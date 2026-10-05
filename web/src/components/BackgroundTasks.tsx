// Hintergrundaufgaben (bash mit run_in_background): Liste im Seitenreiter und Zähler im Chatkopf.
import { useEffect, useRef, useState } from "react"
import { ChevronDownIcon, ChevronRightIcon, CircleStopIcon, Loader2Icon, TerminalSquareIcon } from "lucide-react"
import { toast } from "sonner"
import { api } from "@/api/client"
import type { BackgroundTask } from "@/api/types"
import { Button } from "@/components/ui/button"
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible"
import { useNow } from "@/hooks/useNow"
import {
  backgroundCountLabel,
  backgroundStatus,
  commandPreview,
  formatBackgroundRuntime,
  runningCount,
  sortBackground,
  tailLines,
  type Tone,
} from "@/lib/background"
import { formatBytes } from "@/lib/format"
import { cn } from "@/lib/utils"

const toneClass: Record<Tone, string> = {
  running: "text-sky-700 dark:text-sky-400",
  ok: "text-emerald-700 dark:text-emerald-400",
  error: "text-red-700 dark:text-red-400",
  muted: "text-muted-foreground",
}

/** Seitenreiter „Hintergrund“: je Aufgabe Befehl, Zustand, Laufzeit, letzte Zeilen, Stopp. */
export function BackgroundTasksPanel({
  chatId,
  tasks,
  focus,
}: {
  chatId: string
  tasks: BackgroundTask[]
  /** Aufgabe im Blick (Klick auf eine Karte im Verlauf); n ändert sich je Klick. */
  focus?: { n: number; id?: string }
}) {
  const running = runningCount(tasks)
  const now = useNow(1000, running > 0)
  if (tasks.length === 0) {
    return (
      <p className="text-xs text-muted-foreground">
        Keine Hintergrundaufgaben. Der Agent startet sie mit bash und run_in_background (Server, lange Builds,
        Trainingsläufe) und wird benachrichtigt, wenn sie enden.
      </p>
    )
  }
  return (
    <ul className="flex flex-col gap-1.5">
      {sortBackground(tasks).map((t) => (
        <BackgroundTaskItem key={t.id} chatId={chatId} task={t} now={now} focusN={focus?.id === t.id ? focus.n : undefined} />
      ))}
    </ul>
  )
}

function BackgroundTaskItem({
  chatId,
  task: t,
  now,
  focusN,
}: {
  chatId: string
  task: BackgroundTask
  now: number
  /** Gesetzt, solange diese Aufgabe im Blick ist; ändert sich je Klick. */
  focusN?: number
}) {
  const [open, setOpen] = useState(t.state === "running" || focusN !== undefined)
  // Neuer Klick auf diese Aufgabe: aufklappen und in den Blick rücken
  const [prevFocus, setPrevFocus] = useState(focusN)
  if (prevFocus !== focusN) {
    setPrevFocus(focusN)
    if (focusN !== undefined) setOpen(true)
  }
  const ref = useRef<HTMLLIElement>(null)
  useEffect(() => {
    if (focusN === undefined) return
    const id = window.setTimeout(() => ref.current?.scrollIntoView({ block: "nearest", behavior: "smooth" }), 50)
    return () => window.clearTimeout(id)
  }, [focusN])
  const [stopping, setStopping] = useState(false)
  const status = backgroundStatus(t)
  const lines = tailLines(t.tail, 20)
  const stop = async () => {
    setStopping(true)
    try {
      await api.stopBackground(chatId, t.id)
    } catch (e) {
      toast.error(`Stoppen fehlgeschlagen: ${e instanceof Error ? e.message : String(e)}`)
    } finally {
      setStopping(false)
    }
  }
  return (
    <li ref={ref} className={cn("rounded-md border p-2 text-xs", focusN !== undefined && "border-violet-300 ring-1 ring-violet-200 dark:border-violet-700 dark:ring-violet-900")}>
      <Collapsible open={open} onOpenChange={setOpen}>
        <div className="flex min-w-0 items-start gap-1.5">
          <CollapsibleTrigger
            className="mt-0.5 shrink-0 text-muted-foreground hover:text-foreground"
            aria-label={open ? "Ausgabe zuklappen" : "Ausgabe aufklappen"}
          >
            {open ? <ChevronDownIcon className="size-3.5" /> : <ChevronRightIcon className="size-3.5" />}
          </CollapsibleTrigger>
          <div className="min-w-0 flex-1">
            <div className="flex min-w-0 items-center gap-1.5">
              <span className="font-mono text-muted-foreground">{t.id}</span>
              <span className={cn("flex items-center gap-1 font-medium", toneClass[status.tone])}>
                {t.state === "running" && <Loader2Icon className="size-3 animate-spin" />}
                {status.label}
              </span>
              <span className="ml-auto font-mono text-muted-foreground tabular-nums" title="Laufzeit">
                {formatBackgroundRuntime(t, now)}
              </span>
            </div>
            <div className="mt-0.5 font-mono break-all" title={t.command}>
              {commandPreview(t.command)}
            </div>
            <div className="mt-0.5 text-muted-foreground">
              {t.session !== "main" ? `Subagent ${t.session} · ` : ""}
              {t.output_lines} Zeilen · {formatBytes(t.output_bytes)}
              {t.woke ? " · hat den Agenten geweckt" : ""}
              {t.error && t.state !== "running" ? ` · ${t.error}` : ""}
            </div>
          </div>
          {t.state === "running" && (
            <Button
              size="xs"
              variant="outline"
              className="shrink-0"
              disabled={stopping}
              onClick={() => void stop()}
              title="Beendet den Befehl samt seiner Prozesse; der Agent wird benachrichtigt"
            >
              <CircleStopIcon />
              Stopp
            </Button>
          )}
        </div>
        <CollapsibleContent>
          {lines.length === 0 ? (
            <p className="mt-1.5 text-muted-foreground">Noch keine Ausgabe.</p>
          ) : (
            <pre className="mt-1.5 max-h-60 overflow-auto rounded bg-muted p-1.5 font-mono text-[11px] leading-snug whitespace-pre-wrap break-all">
              {lines.join("\n")}
            </pre>
          )}
          <div className="mt-1 font-mono text-[11px] text-muted-foreground" title="Ganze Ausgabe in der Ausführungs-Sandbox">
            {t.log_path}
          </div>
        </CollapsibleContent>
      </Collapsible>
    </li>
  )
}

/** Kleiner Zähler für den Chatkopf, nur solange Aufgaben laufen; ein Klick öffnet den Reiter. */
export function BackgroundCounter({ count, onOpen }: { count: number; onOpen: () => void }) {
  if (count <= 0) return null
  return (
    <button
      type="button"
      onClick={onOpen}
      className="inline-flex items-center gap-1 rounded-md border px-1.5 py-0.5 text-xs text-sky-700 hover:bg-muted dark:text-sky-400"
      title={`${backgroundCountLabel(count)} – Reiter „Hintergrund“ öffnen`}
    >
      <TerminalSquareIcon className="size-3.5" />
      <span className="tabular-nums">{count}</span>
    </button>
  )
}

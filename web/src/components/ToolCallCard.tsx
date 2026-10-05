import { useState } from "react"
import { ArrowDownToLineIcon, ChevronRightIcon, CircleStopIcon, FileTextIcon, Loader2Icon, TerminalSquareIcon, WrenchIcon } from "lucide-react"
import { toast } from "sonner"
import { api } from "@/api/client"
import type { BackgroundTask } from "@/api/types"
import { EvidenceBadge } from "@/components/badges"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible"
import { useNow } from "@/hooks/useNow"
import {
  type BackgroundCall,
  backgroundCall,
  backgroundCallLabel,
  backgroundStatus,
  formatBackgroundRuntime,
  tailLines,
  type Tone,
} from "@/lib/background"
import type { Evidence } from "@/lib/evidence"
import type { ToolCallBlock, ToolExecution } from "@/lib/stream"
import { formatBytes } from "@/lib/format"
import { preparingInfo } from "@/lib/preparing"
import { formatElapsed, formatStepDuration, toolDurationMs } from "@/lib/runtime"
import { type ArgSection, describeToolArgs } from "@/lib/toolargs"
import { cn } from "@/lib/utils"

type CardProps = {
  block: ToolCallBlock
  exec?: ToolExecution
  /** Abgleich mit dem Protokoll des Orchestrators (E9). */
  evidence?: Evidence
  /** Der Lauf ist fertig; erst dann gelten fehlende Belege als auffällig. */
  settled?: boolean
  /** Hintergrundaufgaben des Chats (für bash mit run_in_background, bg_output, bg_stop). */
  background?: BackgroundTask[]
  /** Öffnet den Reiter „Hintergrund“ mit dieser Aufgabe im Blick. */
  onOpenBackground?: (id?: string) => void
  /** Chat des Aufrufs; mit ihm bekommt ein laufendes bash „Stoppen“ und „In den Hintergrund“. */
  chatId?: string
}

/**
 * Karte eines Werkzeugaufrufs. Hintergrundaufgaben bekommen eine eigene Form: bash mit
 * run_in_background zeigt Kennung, Zustand und Laufzeit der Aufgabe, bg_output und bg_stop eine Zeile.
 */
export function ToolCallCard(props: CardProps) {
  const { block, exec, background } = props
  const name = block.name || exec?.toolName || ""
  const call =
    background && (block.arguments ?? exec?.args) !== undefined
      ? backgroundCall(name, block.arguments ?? exec?.args, block.id, exec?.result, background)
      : undefined
  // Gescheiterter Start (etwa Grenze erreicht): die gewöhnliche Karte zeigt den Fehler.
  if (call?.kind === "start" && exec?.isError !== true) return <BackgroundStartCard {...props} call={call} />
  if (call && call.kind !== "start") return <BackgroundCallLine {...props} call={call} />
  return <GenericToolCallCard {...props} />
}

function GenericToolCallCard({ block, exec, evidence, settled = true, chatId }: CardProps) {
  const [open, setOpen] = useState(false)
  const running = exec?.running ?? false
  const done = exec !== undefined && !running && exec.result !== undefined
  const error = exec?.isError === true
  const name = block.name || exec?.toolName || "Werkzeug"
  // Das Modell schreibt den Aufruf noch (toolcall_delta): Argumente sind halbfertiges JSON.
  const preparing = block.arguments === undefined && block.argsText !== undefined && exec === undefined
  const prep = preparing ? preparingInfo(name, block.argsText ?? "") : undefined
  const view = describeToolArgs(name, block.arguments ?? exec?.args, block.argsText)
  const summary = prep ? (prep.path ?? view.summary) : view.summary
  // Während der Vorbereitung aufgeklappt, damit man sieht, was entsteht; danach wie gewählt.
  const isOpen = preparing || open
  // Dauer: laufend live, fertig aus Start und Ende; nach dem Neuladen ohne Zeitstempel die vom
  // Orchestrator gemessene Ausführungszeit (E9), sonst keine Angabe.
  const live = running && exec?.startedAt !== undefined
  const now = useNow(1000, live)
  const measured = toolDurationMs(exec, now)
  const fallback = !running && measured === undefined && evidence?.executed && evidence.durationMs > 0 ? evidence.durationMs : undefined
  const took = running ? (measured !== undefined ? formatElapsed(measured) : "") : formatStepDuration(measured ?? fallback)
  const tookTitle = measured === undefined && fallback !== undefined ? "Ausführungszeit laut Orchestrator" : "Dauer des Werkzeugaufrufs"

  return (
    <Collapsible
      open={isOpen}
      onOpenChange={setOpen}
      className={cn("rounded-md border bg-background text-sm", error && "border-red-300")}
    >
      <div className="flex min-w-0 items-center hover:bg-muted/50">
        <CollapsibleTrigger className="flex w-full min-w-0 items-center gap-2 px-3 py-1.5 text-left">
          <ChevronRightIcon className={cn("size-3.5 shrink-0 transition-transform", isOpen && "rotate-90")} />
          <WrenchIcon className="size-3.5 shrink-0 text-muted-foreground" />
          <span className="font-mono font-medium">{name}</span>
          {summary && (
            <span className="min-w-0 truncate font-mono text-xs text-muted-foreground" title={summary}>
              {summary}
            </span>
          )}
          <span className="ml-auto flex shrink-0 items-center gap-1.5">
            <EvidenceBadge evidence={evidence} settled={settled} className="hidden sm:inline-flex" />
            {preparing ? (
              <Badge variant="outline" className="border-violet-200 bg-violet-50 text-violet-800" title="Das Modell schreibt diesen Werkzeugaufruf gerade">
                <Loader2Icon className="animate-spin" /> wird vorbereitet · {formatBytes(prep!.bytes)}
                {prep!.lines > 1 ? ` · ${prep!.lines} Zeilen` : ""}
              </Badge>
            ) : running ? (
              <Badge variant="outline" className="border-sky-200 bg-sky-50 text-sky-800">
                <Loader2Icon className="animate-spin" /> läuft
                {took && <span className="tabular-nums">· {took}</span>}
              </Badge>
            ) : error ? (
              <Badge variant="destructive" title={took ? tookTitle : undefined}>
                Fehler{took && <span className="tabular-nums">· {took}</span>}
              </Badge>
            ) : done ? (
              <Badge variant="outline" className="border-emerald-200 bg-emerald-50 text-emerald-800" title={took ? tookTitle : undefined}>
                fertig{took && <span className="tabular-nums">· {took}</span>}
              </Badge>
            ) : (
              <Badge variant="outline">angefordert</Badge>
            )}
          </span>
        </CollapsibleTrigger>
        {running && name === "bash" && chatId && block.id && <ForegroundControls chatId={chatId} toolCallId={block.id} />}
      </div>
      <CollapsibleContent className="space-y-2 border-t px-3 py-2">
        {prep && (
          <Section title={prep.path ? `Entsteht: ${prep.path}` : "Entsteht"} text={prep.preview || "…"} />
        )}
        {!prep && view.sections.map((sec, i) => (
          <ArgBlock key={i} section={sec} />
        ))}
        {running && exec?.output !== undefined && <Section title="Ausgabe (laufend)" text={exec.output} />}
        {exec?.result !== undefined && <Section title={error ? "Fehler" : "Ergebnis"} text={exec.result} error={error} />}
        {evidence && <EvidenceDetails evidence={evidence} settled={settled} />}
      </CollapsibleContent>
    </Collapsible>
  )
}

/**
 * Knöpfe an einem laufenden bash: stoppen (der Agent bekommt „Command stopped by the user“ und
 * arbeitet weiter) oder in den Hintergrund schieben (der Befehl läuft als bg-N weiter; der Agent
 * erfährt es sofort und wird bei seinem Ende benachrichtigt).
 */
function ForegroundControls({ chatId, toolCallId }: { chatId: string; toolCallId: string }) {
  const [busy, setBusy] = useState<"stop" | "bg">()
  const act = async (kind: "stop" | "bg") => {
    setBusy(kind)
    try {
      if (kind === "stop") {
        await api.stopTool(chatId, toolCallId)
      } else {
        const t = await api.backgroundTool(chatId, toolCallId)
        toast.success(`Läuft als ${t.id} im Hintergrund weiter`)
      }
    } catch (e) {
      toast.error(`${kind === "stop" ? "Stoppen" : "In den Hintergrund schieben"} fehlgeschlagen: ${e instanceof Error ? e.message : String(e)}`)
    } finally {
      setBusy(undefined)
    }
  }
  return (
    <span className="flex shrink-0 items-center gap-0.5 pr-1.5">
      <Button
        size="icon-sm"
        variant="ghost"
        disabled={!!busy}
        title="In den Hintergrund schieben: läuft weiter, der Agent arbeitet weiter und wird beim Ende benachrichtigt"
        aria-label="In den Hintergrund schieben"
        onClick={() => void act("bg")}
      >
        {busy === "bg" ? <Loader2Icon className="animate-spin" /> : <ArrowDownToLineIcon />}
      </Button>
      <Button
        size="icon-sm"
        variant="ghost"
        disabled={!!busy}
        title="Befehl stoppen: der Agent erfährt es und arbeitet weiter"
        aria-label="Befehl stoppen"
        onClick={() => void act("stop")}
      >
        {busy === "stop" ? <Loader2Icon className="animate-spin" /> : <CircleStopIcon className="text-red-600" />}
      </Button>
    </span>
  )
}

const toneBadge: Record<Tone, string> = {
  running: "border-sky-200 bg-sky-50 text-sky-800",
  ok: "border-emerald-200 bg-emerald-50 text-emerald-800",
  error: "border-red-200 bg-red-50 text-red-800",
  muted: "",
}

/** Kennung einer Hintergrundaufgabe als Link auf den Reiter „Hintergrund“. */
function BgLink({ id, onOpen, short }: { id: string; onOpen?: (id?: string) => void; short?: boolean }) {
  const text = short ? id : `Hintergrund ${id}`
  if (!onOpen) {
    return (
      <Badge variant="outline" className="font-mono">
        {text}
      </Badge>
    )
  }
  return (
    <button
      type="button"
      onClick={() => onOpen(id)}
      className="inline-flex shrink-0 items-center gap-1 rounded-md border border-violet-200 bg-violet-50 px-1.5 py-0.5 text-xs text-violet-800 hover:bg-violet-100 dark:border-violet-800 dark:bg-violet-950 dark:text-violet-200"
      title={`${id} im Reiter „Hintergrund“ zeigen`}
    >
      <TerminalSquareIcon className="size-3" />
      {short ? (
        <span className="font-mono">{id}</span>
      ) : (
        <>
          <span className="hidden sm:inline">Hintergrund</span>
          <span className="font-mono">{id}</span>
        </>
      )}
    </button>
  )
}

/** bash mit run_in_background: Kennung, Zustand und Laufzeit der Aufgabe; nach dem Ende Exit-Code und letzte Zeilen. */
function BackgroundStartCard({ block, exec, evidence, settled = true, onOpenBackground, call }: CardProps & { call: BackgroundCall }) {
  const [open, setOpen] = useState(false)
  const task = call.task
  const running = task?.state === "running"
  const now = useNow(1000, running)
  const view = describeToolArgs("bash", block.arguments ?? exec?.args, block.argsText)
  const status = task ? backgroundStatus(task) : undefined
  const runtime = task ? formatBackgroundRuntime(task, now) : ""
  const starting = !task && (exec === undefined || exec.running)
  const lines = tailLines(task?.tail, 10)
  return (
    <Collapsible open={open} onOpenChange={setOpen} className="rounded-md border bg-background text-sm">
      <div className="flex min-w-0 items-center gap-1.5 pr-2 hover:bg-muted/50">
        <CollapsibleTrigger className="flex min-w-0 flex-1 items-center gap-2 py-1.5 pl-3 text-left">
          <ChevronRightIcon className={cn("size-3.5 shrink-0 transition-transform", open && "rotate-90")} />
          <WrenchIcon className="size-3.5 shrink-0 text-muted-foreground" />
          <span className="font-mono font-medium">bash</span>
          {view.summary && (
            <span className="min-w-0 truncate font-mono text-xs text-muted-foreground" title={view.summary}>
              {view.summary}
            </span>
          )}
        </CollapsibleTrigger>
        <EvidenceBadge evidence={evidence} settled={settled} className="hidden shrink-0 sm:inline-flex" />
        {call.id ? <BgLink id={call.id} onOpen={onOpenBackground} /> : null}
        {status ? (
          <Badge
            variant="outline"
            className={cn("shrink-0", toneBadge[status.tone])}
            title={task?.state === "running" ? "Läuft im Hintergrund; der Agent wird beim Ende benachrichtigt" : "Zustand der Hintergrundaufgabe"}
          >
            {running && <Loader2Icon className="animate-spin" />}
            <span className="hidden sm:inline">{status.label}</span>
            <span className="sm:hidden">{running ? "läuft" : status.tone === "ok" ? "fertig" : status.tone === "error" ? "Fehler" : "beendet"}</span>
            {runtime && <span className="tabular-nums">· {runtime}</span>}
          </Badge>
        ) : starting ? (
          <Badge variant="outline" className="shrink-0 border-sky-200 bg-sky-50 text-sky-800">
            <Loader2Icon className="animate-spin" /> startet
          </Badge>
        ) : (
          <Badge variant="outline" className="shrink-0">
            gestartet
          </Badge>
        )}
      </div>
      <CollapsibleContent className="space-y-2 border-t px-3 py-2">
        {view.sections.map((sec, i) => (
          <ArgBlock key={i} section={sec} />
        ))}
        {task && (
          <div className="min-w-0">
            <div className="mb-1 text-xs font-medium text-muted-foreground">
              {running ? "Letzte Zeilen (laufend)" : "Letzte Zeilen"}
              {task.output_lines > 0 && ` · ${task.output_lines} Zeilen, ${formatBytes(task.output_bytes)}`}
              {!running && task.exit_code !== undefined && ` · Exit ${task.exit_code}`}
              {task.error && !running ? ` · ${task.error}` : ""}
            </div>
            <pre className="max-h-60 overflow-auto rounded bg-muted p-2 font-mono text-xs whitespace-pre-wrap break-all">
              {lines.length ? lines.join("\n") : running ? "Noch keine Ausgabe." : "Keine Ausgabe."}
            </pre>
            <div className="mt-1 font-mono text-[11px] break-all text-muted-foreground" title="Ganze Ausgabe in der Ausführungs-Sandbox">
              {task.log_path}
            </div>
          </div>
        )}
        {exec?.result !== undefined && <Section title="Rückmeldung an das Modell" text={exec.result} />}
        {evidence && <EvidenceDetails evidence={evidence} settled={settled} />}
      </CollapsibleContent>
    </Collapsible>
  )
}

/** bg_output und bg_stop als eine Zeile („Ausgabe von bg-3 abgerufen“, „bg-3 gestoppt“), aufklappbar. */
function BackgroundCallLine({ exec, evidence, settled = true, onOpenBackground, call }: CardProps & { call: BackgroundCall }) {
  const [open, setOpen] = useState(false)
  const error = exec?.isError === true
  const phase = error ? "error" : exec?.result !== undefined && !exec.running ? "done" : "running"
  const label = backgroundCallLabel(call, phase)
  const Icon = call.kind === "stop" ? CircleStopIcon : FileTextIcon
  return (
    <Collapsible open={open} onOpenChange={setOpen} className="min-w-0 text-xs text-muted-foreground">
      <div className="flex min-w-0 items-center gap-1.5">
        <CollapsibleTrigger className="flex min-w-0 flex-1 items-center gap-1.5 py-0.5 text-left hover:text-foreground">
          <ChevronRightIcon className={cn("size-3 shrink-0 transition-transform", open && "rotate-90")} />
          {phase === "running" ? <Loader2Icon className="size-3 shrink-0 animate-spin" /> : <Icon className="size-3 shrink-0" />}
          <span className={cn("min-w-0 truncate", error && "text-red-700 dark:text-red-400")}>{label}</span>
          <span className="shrink-0 font-mono text-[11px] opacity-70">{call.kind === "stop" ? "bg_stop" : "bg_output"}</span>
        </CollapsibleTrigger>
        {call.id && <BgLink id={call.id} onOpen={onOpenBackground} short />}
      </div>
      <CollapsibleContent className="mt-1 ml-4.5 space-y-1.5">
        {exec?.result !== undefined ? (
          <Section title={error ? "Fehler" : "Rückmeldung an das Modell"} text={exec.result} error={error} />
        ) : (
          <p>Noch keine Rückmeldung.</p>
        )}
        {evidence && <EvidenceDetails evidence={evidence} settled={settled} />}
      </CollapsibleContent>
    </Collapsible>
  )
}

export function Section({
  title,
  text,
  error,
  className,
}: {
  title: React.ReactNode
  text: string
  error?: boolean
  /** Zusätzliche Klassen für den Textblock, etwa eine größere Höchsthöhe. */
  className?: string
}) {
  return (
    <div className="min-w-0">
      <div className="mb-1 text-xs font-medium text-muted-foreground">{title}</div>
      <pre
        className={cn(
          "max-h-72 overflow-auto rounded bg-muted p-2 font-mono text-xs whitespace-pre-wrap break-all",
          error && "bg-red-50 text-red-800",
          className,
        )}
      >
        {text || "(leer)"}
      </pre>
    </div>
  )
}

export function ArgBlock({ section, className }: { section: ArgSection; className?: string }) {
  if (section.kind === "path") {
    return (
      <div className="flex min-w-0 items-baseline gap-2">
        <span className="shrink-0 text-xs font-medium text-muted-foreground">{section.label}</span>
        <code className="min-w-0 rounded bg-sky-50 px-1.5 py-0.5 font-mono text-xs font-semibold break-all text-sky-900">
          {section.text}
        </code>
      </div>
    )
  }
  return (
    <div className="min-w-0">
      <div className="mb-1 text-xs font-medium text-muted-foreground">{section.label}</div>
      <pre
        className={cn(
          "max-h-72 overflow-auto rounded p-2 font-mono text-xs",
          section.kind === "code" ? "bg-zinc-900 whitespace-pre text-zinc-100" : "bg-muted whitespace-pre-wrap break-all",
          className,
        )}
      >
        {section.text || "(leer)"}
      </pre>
    </div>
  )
}

/** Was der Orchestrator zu diesem Aufruf protokolliert hat (E9). */
export function EvidenceDetails({ evidence, settled }: { evidence: Evidence; settled: boolean }) {
  if (evidence.state === "internal") return null
  return (
    <div className="flex min-w-0 flex-wrap items-center gap-x-2 gap-y-1 text-[11px] text-muted-foreground">
      <EvidenceBadge evidence={evidence} settled={settled} />
      {evidence.executed ? (
        <span>
          Orchestrator: {evidence.ops.join(", ")} · {evidence.durationMs} ms
          {evidence.exitCode !== undefined ? ` · Exit ${evidence.exitCode}` : ""}
          {evidence.error ? ` · ${evidence.error}` : ""}
        </span>
      ) : (
        <span>keine Ausführung im Protokoll des Orchestrators</span>
      )}
      <span className="font-mono">{evidence.toolCallId}</span>
    </div>
  )
}

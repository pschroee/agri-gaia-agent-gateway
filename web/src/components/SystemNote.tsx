// Meldungen des Orchestrators im Verlauf (Ende einer Hintergrundaufgabe, Hinweis auf mit der Sandbox
// beendete Aufgaben): schmale graue Zeile statt Nutzerblase, aufklappbar. Erkennung in lib/systemnote.
import { useState } from "react"
import { BellIcon, ChevronRightIcon, SquareArrowOutUpRightIcon } from "lucide-react"
import type { TurnTrigger } from "@/api/types"
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible"
import type { MessagePart } from "@/lib/systemnote"
import { cn } from "@/lib/utils"

type SystemPart = Extract<MessagePart, { kind: "system" }>

export function SystemNoteLine({
  part,
  trigger,
  onOpenBackground,
}: {
  part: SystemPart
  /** Auslöser des Durchgangs laut Server (wake: ohne Zutun des Nutzers) */
  trigger?: TurnTrigger
  onOpenBackground?: (id?: string) => void
}) {
  const [open, setOpen] = useState(false)
  const note = part.note
  const label = note.label
  const woke = trigger === "wake"
  const title = `Meldung des Orchestrators an den Agenten, nicht vom Nutzer (laut Server)${
    woke ? "; Weckruf: hat einen Durchgang ohne Nutzer gestartet" : ""
  }. Befehl und Ausgabe stammen aus der Sandbox und gingen eingezäunt an den Agenten.`
  const bgId = note.refs[0]
  return (
    <Collapsible open={open} onOpenChange={setOpen} className="min-w-0 text-xs text-muted-foreground" role="note">
      <div className="flex min-w-0 items-center gap-1.5">
        <CollapsibleTrigger
          className="flex min-w-0 flex-1 items-center gap-1.5 rounded-md py-0.5 text-left hover:text-foreground"
          title={title}
        >
          <ChevronRightIcon className={cn("size-3 shrink-0 transition-transform", open && "rotate-90")} />
          <BellIcon className="size-3 shrink-0" />
          <span className="min-w-0 truncate">{label}</span>
          {woke && <span className="hidden shrink-0 sm:inline">· Weckruf</span>}
        </CollapsibleTrigger>
        {onOpenBackground && bgId && (
          <button
            type="button"
            className="shrink-0 rounded p-0.5 hover:bg-muted hover:text-foreground"
            title="Im Reiter „Hintergrund“ zeigen"
            aria-label={`${bgId} im Reiter „Hintergrund“ zeigen`}
            onClick={() => onOpenBackground(bgId)}
          >
            <SquareArrowOutUpRightIcon className="size-3" />
          </button>
        )}
      </div>
      <CollapsibleContent>
        <div className="mt-1 ml-4.5 min-w-0 border-l-2 pl-2.5">
          {note.type === "sandbox" ? (
            <div className="break-words">
              {note.summary}
              {note.items && note.items.length > 0 && (
                <ul className="mt-1 font-mono break-all">
                  {note.items.map((it, i) => (
                    <li key={i}>{it}</li>
                  ))}
                </ul>
              )}
            </div>
          ) : (
            <>
              {note.command !== undefined && (
                <div className="font-mono break-all" title="Befehl">
                  {note.command}
                </div>
              )}
              {note.error && <div className="mt-1 break-words">Fehler: {note.error}</div>}
              {note.lines.length > 0 ? (
                <>
                  <div className="mt-1">Letzte Zeilen{note.totalLines !== undefined ? ` (von ${note.totalLines})` : ""}</div>
                  <pre className="mt-0.5 max-h-48 overflow-auto rounded bg-muted p-1.5 font-mono text-[11px] leading-snug whitespace-pre-wrap break-all text-foreground/80">
                    {note.lines.join("\n")}
                  </pre>
                </>
              ) : (
                note.noOutput && <div className="mt-1">Keine Ausgabe.</div>
              )}
              {note.logPath && <div className="mt-0.5 font-mono break-all">{note.logPath}</div>}
            </>
          )}
        </div>
      </CollapsibleContent>
    </Collapsible>
  )
}

/**
 * Nachricht einer Erweiterung in pi an den Agenten, etwa „Subagent fertig“ von pi-subagents: schmale
 * graue Zeile, aufklappbar mit dem vollen Text. Nicht vom Nutzer; mit „Weckruf“, wenn sie einen
 * Durchgang ohne Nutzer begonnen hat.
 */
export function PiNoticeLine({ text, customType, trigger }: { text: string; customType?: string; trigger?: TurnTrigger }) {
  const [open, setOpen] = useState(false)
  const first = text.split("\n").find((l) => l.trim()) ?? ""
  const from = customType?.startsWith("subagent") ? "Subagenten" : "Erweiterung"
  const woke = trigger === "wake"
  return (
    <Collapsible open={open} onOpenChange={setOpen} className="min-w-0 text-xs text-muted-foreground" role="note">
      <CollapsibleTrigger
        className="flex w-full min-w-0 items-center gap-1.5 rounded-md py-0.5 text-left hover:text-foreground"
        title={`Meldung von ${from === "Subagenten" ? "pi-subagents" : "einer Erweiterung in pi"} an den Agenten, nicht vom Nutzer${woke ? "; Weckruf: hat einen Durchgang ohne Nutzer begonnen" : ""}${customType ? ` (${customType})` : ""}`}
      >
        <ChevronRightIcon className={cn("size-3 shrink-0 transition-transform", open && "rotate-90")} />
        <BellIcon className="size-3 shrink-0" />
        <span className="shrink-0">Meldung der {from}:</span>
        <span className="min-w-0 truncate">{first}</span>
        {woke && <span className="hidden shrink-0 sm:inline">· Weckruf</span>}
      </CollapsibleTrigger>
      <CollapsibleContent>
        <pre className="mt-1 ml-4.5 max-h-64 overflow-auto border-l-2 pl-2.5 font-sans text-xs whitespace-pre-wrap break-words">{text}</pre>
      </CollapsibleContent>
    </Collapsible>
  )
}

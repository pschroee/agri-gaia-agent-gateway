// Meldungen des Orchestrators in Nutzernachrichten (Ende einer Hintergrundaufgabe, Hinweis auf mit der
// Sandbox beendete Aufgaben). Der Orchestrator gibt sie pi als Nutzernachricht, allein (Weckruf) oder
// zusammen mit eingereihten Nachrichten des Nutzers (internal/chat/origin.go). Jede Meldung steht in
// einer Hülle: fester Kopf SYSTEM_HEADER, Kopfzeile des Orchestrators, darunter die Daten aus der Sandbox
// in einem Zaun mit zufälliger Marke.
//
// Ob eine Nachricht Meldungen enthält, sagt allein der Server (origin, sources an der gespeicherten
// Nachricht, live über SSE „user_meta“ bzw. „queue“ delivered; Review 3, H1). Zerlegt wird nur entlang
// der Marken, die der Server nennt. Ohne Kennzeichen (auch alte Zeilen) oder mit origin „user“ ist alles
// Nutzertext, wie immer er aussieht.
import type { MessageMeta, MessageSource, QueueEntry } from "@/api/types"

export const SYSTEM_HEADER = "[Meldung des Orchestrators, nicht vom Nutzer]"

/** Lesbare Form einer Meldung; Befehl, Zeilen und Fehler stammen aus der Sandbox (nur anzeigen). */
export type ParsedNote = {
  type: string
  refs: string[]
  /** Kopfzeile des Orchestrators */
  summary: string
  /** Kurzzeile für Verlauf und Warteschlange */
  label: string
  command?: string
  error?: string
  totalLines?: number
  lines: string[]
  logPath?: string
  noOutput: boolean
  /** Hinweis auf beendete Aufgaben: je Aufgabe „bg-1: Befehl“ */
  items?: string[]
  body: string
}

export type MessagePart = { kind: "user"; text: string } | { kind: "system"; text: string; note: ParsedNote; source: MessageSource }

/** Kurzzeile: „Hintergrundaufgabe bg-3 beendet · Exit 0 · 0:08“ bzw. der Hinweis auf beendete Aufgaben. */
export function noteLabel(type: string | undefined, summary: string, refs: string[] = []): string {
  if (type === "sandbox") return `Hinweis an den Agenten: ${refs.join(", ")} mit der vorigen Sandbox beendet`
  let s = summary.trim()
  let runtime = ""
  const rm = /, Laufzeit (\d+:\d{2}(?::\d{2})?)$/.exec(s)
  if (rm) {
    runtime = ` · ${rm[1]}`
    s = s.slice(0, rm.index)
  }
  s = s.replace(/ \(gestartet von Subagent ([^)]*)\)/, " (Subagent $1)").replace(": ", " · ")
  return s + runtime
}

/** Liest die Daten einer Meldung (Befehl, Fehler, letzte Zeilen, Pfad) für die Anzeige. */
function parseNote(type: string, refs: string[], summary: string, body: string): ParsedNote {
  const note: ParsedNote = { type, refs, summary, label: noteLabel(type, summary, refs), lines: [], noOutput: false, body }
  const lines = body ? body.split("\n") : []
  if (type === "sandbox") {
    note.items = lines
    return note
  }
  let i = 0
  if (lines[i]?.startsWith("Befehl: ")) note.command = lines[i++].slice("Befehl: ".length)
  if (lines[i]?.startsWith("Fehler: ")) note.error = lines[i++].slice("Fehler: ".length)
  let end = lines.length
  const last = lines[end - 1]
  if (end > i && last.startsWith("Ganze Ausgabe: ")) {
    note.logPath = last.slice("Ganze Ausgabe: ".length)
    end--
  }
  if (lines[i] === "Keine Ausgabe.") {
    note.noOutput = true
  } else {
    const lm = /^Letzte Zeilen \(von (\d+)\):$/.exec(lines[i] ?? "")
    if (lm) {
      note.totalLines = Number(lm[1])
      i++
    }
    note.lines = lines.slice(i, end)
  }
  return note
}

/**
 * Zerlegt eine Nutzernachricht nach der Herkunft laut Server. Meldungen werden an ihrer Marke gefunden
 * (Zaun `<<<marke` … `marke>>>`), in der Reihenfolge der Quellen; was dazwischen steht, ist Nutzertext.
 */
export function splitMessage(text: string, meta: MessageMeta | undefined): MessagePart[] {
  const whole: MessagePart[] = text ? [{ kind: "user", text }] : []
  if (!meta?.origin || meta.origin === "user" || !meta.sources?.length) return whole
  const parts: MessagePart[] = []
  let cursor = 0
  const pushUser = (s: string) => {
    const t = s.replace(/^\n+|\n+$/g, "")
    if (t.trim()) parts.push({ kind: "user", text: t })
  }
  const head = SYSTEM_HEADER + "\n"
  for (const src of meta.sources) {
    if (src.kind !== "system") continue
    let start: number
    let end: number
    let summary: string
    let body = ""
    if (src.marker) {
      const open = `<<<${src.marker}\n`
      const close = `\n${src.marker}>>>`
      const oi = text.indexOf(open, cursor)
      if (oi < 0) continue
      start = text.lastIndexOf(head, oi)
      const ci = text.indexOf(close, oi + open.length - 1)
      if (start < cursor || ci < 0) continue
      summary = text.slice(start + head.length, oi).split("\n")[0] ?? ""
      body = text.slice(oi + open.length, ci)
      end = ci + close.length
    } else {
      start = text.indexOf(head, cursor)
      if (start < 0) continue
      const nl = text.indexOf("\n", start + head.length)
      end = nl < 0 ? text.length : nl
      summary = text.slice(start + head.length, end)
    }
    pushUser(text.slice(cursor, start))
    parts.push({ kind: "system", text: text.slice(start, end), note: parseNote(src.type ?? "", src.refs ?? [], summary, body), source: src })
    cursor = end
  }
  pushUser(text.slice(cursor))
  return parts.some((p) => p.kind === "system") ? parts : whole
}

/** Kurzzeile für einen Systemeintrag der Warteschlange (erste Zeile ist die Kopfzeile des Orchestrators). */
export function systemEntryLabel(e: Pick<QueueEntry, "kind" | "note" | "refs" | "text">): string | undefined {
  if (e.kind !== "system") return undefined
  return noteLabel(e.note, e.text.split("\n")[0] ?? "", e.refs ?? [])
}

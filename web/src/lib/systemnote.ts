// Orchestrator notes in user messages (end of a background task, notice about tasks ended along with the
// sandbox, the user's preferred language on the first request). The orchestrator hands them to pi as a user message, alone (wake-up) or
// together with queued messages of the user (internal/chat/origin.go). Every note sits in
// an envelope: fixed header SYSTEM_HEADER, the orchestrator's header line, below it the data from the sandbox
// in a fence with a random marker.
//
// Whether a message contains notes is decided by the server alone (origin, sources on the stored
// message, live via SSE "user_meta" or "queue" delivered; Review 3, H1). Splitting happens only along
// the markers the server names. Without a mark (including old rows) or with origin "user", everything is
// user text, whatever it looks like.
import type { MessageMeta, MessageSource, QueueEntry } from "@/api/types"

export const SYSTEM_HEADER = "[Note from the orchestrator, not from the user]"
// German header used before the translation; chats stored earlier still carry it.
export const LEGACY_SYSTEM_HEADER = "[Meldung des Orchestrators, nicht vom Nutzer]"

/** Readable form of a note; command, lines and error come from the sandbox (display only). */
export type ParsedNote = {
  type: string
  refs: string[]
  /** The orchestrator's header line */
  summary: string
  /** Short line for history and queue */
  label: string
  command?: string
  error?: string
  totalLines?: number
  lines: string[]
  logPath?: string
  noOutput: boolean
  /** Notice about ended tasks: per task "bg-1: command" */
  items?: string[]
  body: string
}

export type MessagePart = { kind: "user"; text: string } | { kind: "system"; text: string; note: ParsedNote; source: MessageSource }

/** Short line: "Background task bg-3 finished · exit 0 · 0:08" or the notice about ended tasks. */
export function noteLabel(type: string | undefined, summary: string, refs: string[] = []): string {
  if (type === "sandbox") return `Note to the agent: ${refs.join(", ")} ended with the previous sandbox`
  if (type === "language") return `Note to the agent: preferred language according to the browser ${refs[0] ?? ""}`.trim()
  let s = summary.trim()
  let runtime = ""
  // "Laufzeit", "gestartet von Subagent": German forms used before the translation (stored chats)
  const rm = /, (?:runtime|Laufzeit) (\d+:\d{2}(?::\d{2})?)$/.exec(s)
  if (rm) {
    runtime = ` · ${rm[1]}`
    s = s.slice(0, rm.index)
  }
  s = s.replace(/ \((?:started by subagent|gestartet von Subagent) ([^)]*)\)/, " (subagent $1)").replace(": ", " · ")
  return s + runtime
}

// Body lines of a background note (internal/chat/background.go); each with its German form from before the
// translation, so chats stored earlier still render.
const COMMAND = ["Command: ", "Befehl: "]
const ERROR = ["Error: ", "Fehler: "]
const FULL_OUTPUT = ["Full output: ", "Ganze Ausgabe: "]
const NO_OUTPUT = ["No output.", "Keine Ausgabe."]
const LAST_LINES = /^(?:Last lines \(of|Letzte Zeilen \(von) (\d+)\):$/

/** The rest of `line` after one of the prefixes, or undefined. */
function after(line: string | undefined, prefixes: string[]): string | undefined {
  const p = line === undefined ? undefined : prefixes.find((x) => line.startsWith(x))
  return p === undefined ? undefined : line!.slice(p.length)
}

/** Reads the data of a note (command, error, last lines, path) for display. */
function parseNote(type: string, refs: string[], summary: string, body: string): ParsedNote {
  const note: ParsedNote = { type, refs, summary, label: noteLabel(type, summary, refs), lines: [], noOutput: false, body }
  const lines = body ? body.split("\n") : []
  if (type === "sandbox") {
    note.items = lines
    return note
  }
  let i = 0
  const command = after(lines[i], COMMAND)
  if (command !== undefined) {
    note.command = command
    i++
  }
  const error = after(lines[i], ERROR)
  if (error !== undefined) {
    note.error = error
    i++
  }
  let end = lines.length
  const logPath = end > i ? after(lines[end - 1], FULL_OUTPUT) : undefined
  if (logPath !== undefined) {
    note.logPath = logPath
    end--
  }
  if (NO_OUTPUT.includes(lines[i])) {
    note.noOutput = true
  } else {
    const lm = LAST_LINES.exec(lines[i] ?? "")
    if (lm) {
      note.totalLines = Number(lm[1])
      i++
    }
    note.lines = lines.slice(i, end)
  }
  return note
}

/**
 * Splits a user message by origin according to the server. Notes are found by their marker
 * (fence `<<<marker` … `marker>>>`), in the order of the sources; whatever lies in between is user text.
 * Both the current and the legacy German header are recognised.
 */
const HEADS = [SYSTEM_HEADER + "\n", LEGACY_SYSTEM_HEADER + "\n"]

/** Last header (current or legacy) starting at or before `before`; start -1 if none. */
function lastHeader(text: string, before: number): { start: number; len: number } {
  let best = { start: -1, len: 0 }
  for (const h of HEADS) {
    const i = text.lastIndexOf(h, before)
    if (i > best.start) best = { start: i, len: h.length }
  }
  return best
}

/** First header (current or legacy) starting at or after `from`; start -1 if none. */
function firstHeader(text: string, from: number): { start: number; len: number } {
  let best = { start: -1, len: 0 }
  for (const h of HEADS) {
    const i = text.indexOf(h, from)
    if (i >= 0 && (best.start < 0 || i < best.start)) best = { start: i, len: h.length }
  }
  return best
}

export function splitMessage(text: string, meta: MessageMeta | undefined): MessagePart[] {
  const whole: MessagePart[] = text ? [{ kind: "user", text }] : []
  if (!meta?.origin || meta.origin === "user" || !meta.sources?.length) return whole
  const parts: MessagePart[] = []
  let cursor = 0
  const pushUser = (s: string) => {
    const t = s.replace(/^\n+|\n+$/g, "")
    if (t.trim()) parts.push({ kind: "user", text: t })
  }
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
      const h = lastHeader(text, oi)
      start = h.start
      const ci = text.indexOf(close, oi + open.length - 1)
      if (start < cursor || ci < 0) continue
      summary = text.slice(start + h.len, oi).split("\n")[0] ?? ""
      body = text.slice(oi + open.length, ci)
      end = ci + close.length
    } else {
      const h = firstHeader(text, cursor)
      start = h.start
      if (start < 0) continue
      const nl = text.indexOf("\n", start + h.len)
      end = nl < 0 ? text.length : nl
      summary = text.slice(start + h.len, end)
    }
    pushUser(text.slice(cursor, start))
    parts.push({ kind: "system", text: text.slice(start, end), note: parseNote(src.type ?? "", src.refs ?? [], summary, body), source: src })
    cursor = end
  }
  pushUser(text.slice(cursor))
  return parts.some((p) => p.kind === "system") ? parts : whole
}

/** Short line for a system entry of the queue (the first line is the orchestrator's header line). */
export function systemEntryLabel(e: Pick<QueueEntry, "kind" | "note" | "refs" | "text">): string | undefined {
  if (e.kind !== "system") return undefined
  return noteLabel(e.note, e.text.split("\n")[0] ?? "", e.refs ?? [])
}

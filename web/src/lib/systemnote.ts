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
  const rm = /, runtime (\d+:\d{2}(?::\d{2})?)$/.exec(s)
  if (rm) {
    runtime = ` · ${rm[1]}`
    s = s.slice(0, rm.index)
  }
  s = s.replace(/ \(started by subagent ([^)]*)\)/, " (subagent $1)").replace(": ", " · ")
  return s + runtime
}

// Body lines of a background note (internal/chat/background.go).
const COMMAND = "Command: "
const ERROR = "Error: "
const FULL_OUTPUT = "Full output: "
const NO_OUTPUT = "No output."
const LAST_LINES = /^Last lines \(of (\d+)\):$/

/** The rest of `line` after the prefix, or undefined. */
function after(line: string | undefined, prefix: string): string | undefined {
  return line?.startsWith(prefix) ? line.slice(prefix.length) : undefined
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
  if (lines[i] === NO_OUTPUT) {
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

const HEAD = SYSTEM_HEADER + "\n"

/**
 * Splits a user message by origin according to the server. Notes are found by their marker
 * (fence `<<<marker` … `marker>>>`), in the order of the sources; whatever lies in between is user text.
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
      start = text.lastIndexOf(HEAD, oi)
      const ci = text.indexOf(close, oi + open.length - 1)
      if (start < cursor || ci < 0) continue
      summary = text.slice(start + HEAD.length, oi).split("\n")[0] ?? ""
      body = text.slice(oi + open.length, ci)
      end = ci + close.length
    } else {
      start = text.indexOf(HEAD, cursor)
      if (start < 0) continue
      const nl = text.indexOf("\n", start + HEAD.length)
      end = nl < 0 ? text.length : nl
      summary = text.slice(start + HEAD.length, end)
    }
    pushUser(text.slice(cursor, start))
    parts.push({ kind: "system", text: text.slice(start, end), note: parseNote(src.type ?? "", src.refs ?? [], summary, body), source: src })
    cursor = end
  }
  pushUser(text.slice(cursor))
  return parts.some((p) => p.kind === "system") ? parts : whole
}

/** A note the server marks as context for the model only (audience "agent", e.g. the preferred language). */
export function isAgentOnly(p: MessagePart): boolean {
  return p.kind === "system" && p.source.audience === "agent"
}

/**
 * What the history shows of a user message: without a system part the text exactly as sent (body), otherwise
 * the parts without those meant for the agent alone. Hidden parts are cut out, never shown as user text.
 */
export function visibleParts(parts: MessagePart[] | undefined, body: string): MessagePart[] {
  if (!parts?.some((p) => p.kind !== "user")) return body ? [{ kind: "user", text: body }] : []
  return parts.filter((p) => !isAgentOnly(p))
}

/** Short line for a system entry of the queue (the first line is the orchestrator's header line). */
export function systemEntryLabel(e: Pick<QueueEntry, "kind" | "note" | "refs" | "text">): string | undefined {
  if (e.kind !== "system") return undefined
  return noteLabel(e.note, e.text.split("\n")[0] ?? "", e.refs ?? [])
}

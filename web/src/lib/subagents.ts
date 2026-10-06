// Subagents: group entries from the session files per run, assign them to the main agent's
// tool calls and prepare limit notices. Pure functions, without React.
import type { Chat, SocketCall, SubagentEntry, SubagentRunMeta } from "@/api/types"
import type { TranscriptItem } from "./stream"

export type SubagentRun = {
  runId: string
  agent: string
  entries: SubagentEntry[]
  /** Time of the first and last entry in ms. */
  start: number
  end: number
  /** Task (first task entry). */
  task?: string
  toolCalls: number
  errors: number
  /** Name in the workflow (key for runs.run/runs.all), according to pi-subagents. */
  label?: string
  /** State according to pi-subagents; if missing, the status is estimated from the entries. */
  state?: string
}

const entryKey = (e: SubagentEntry) => `${e.run_id}\u0000${e.entry_id}`

const ms = (iso: string) => {
  const v = Date.parse(iso)
  return Number.isNaN(v) ? 0 : v
}

/** Adds entries; equal (run_id, entry_id) are replaced, the order is kept. */
export function mergeSubagentEntries(list: SubagentEntry[], add: SubagentEntry[]): SubagentEntry[] {
  if (add.length === 0) return list
  const next = list.slice()
  const index = new Map(next.map((e, i) => [entryKey(e), i]))
  for (const e of add) {
    const k = entryKey(e)
    const i = index.get(k)
    if (i === undefined) {
      index.set(k, next.length)
      next.push(e)
    } else next[i] = e
  }
  return next
}

/**
 * Groups entries per run (order within stable by created_at), runs by start.
 * `meta` adds agent, name and state; a run without entries already appears with its metadata.
 */
export function groupRuns(entries: SubagentEntry[], meta: SubagentRunMeta[] = []): SubagentRun[] {
  const byRun = new Map<string, SubagentEntry[]>()
  for (const e of entries) {
    const l = byRun.get(e.run_id)
    if (l) l.push(e)
    else byRun.set(e.run_id, [e])
  }
  const runs: SubagentRun[] = []
  for (const [runId, list] of byRun) {
    const sorted = [...list].sort((a, b) => ms(a.created_at) - ms(b.created_at))
    const times = sorted.map((e) => ms(e.created_at))
    runs.push({
      runId,
      agent: sorted.find((e) => e.agent)?.agent ?? "",
      entries: sorted,
      start: Math.min(...times),
      end: Math.max(...times),
      task: sorted.find((e) => e.kind === "task")?.payload.text,
      toolCalls: sorted.filter((e) => e.kind === "tool_call").length,
      errors: sorted.filter((e) => e.kind === "tool_result" && e.payload.is_error).length,
    })
  }
  const byId = new Map(runs.map((r) => [r.runId, r]))
  for (const m of meta) {
    const r = byId.get(m.run_id)
    const started = m.started_at ? ms(m.started_at) : 0
    const ended = m.ended_at ? ms(m.ended_at) : 0
    if (r) {
      if (m.agent) r.agent = m.agent
      r.label = m.label || undefined
      r.state = m.state || undefined
      if (ended > r.end) r.end = ended
    } else if (started) {
      runs.push({ runId: m.run_id, agent: m.agent, entries: [], start: started, end: Math.max(started, ended), toolCalls: 0, errors: 0,
        label: m.label || undefined, state: m.state || undefined })
    }
  }
  return runs.sort((a, b) => a.start - b.start)
}

/** Short form of the run ID: the first six characters, with "#n" for parallel children. */
export function shortRunId(runId: string): string {
  const [base, idx] = runId.split("#")
  const short = base.length > 6 ? base.slice(0, 6) : base
  return idx !== undefined ? `${short}#${idx}` : short
}

/** Display name: name in the workflow, otherwise agent (e.g. "reid" or "researcher"). */
export function runName(run: Pick<SubagentRun, "agent" | "label">): string {
  return run.label || run.agent || "Subagent"
}

export function runLabel(run: Pick<SubagentRun, "agent" | "runId" | "label">): string {
  const who = run.label ? `${run.label}${run.agent ? ` (${run.agent})` : ""}` : run.agent
  return `Subagent${who ? ` ${who}` : ""} · run ${shortRunId(run.runId)}`
}

/** Whether an entry is verified at the proxy: according to the server or because its response was recorded there. */
export function isEntryConfirmed(e: SubagentEntry, proxyResponseIds: Set<string>): boolean {
  return e.confirmed || (!!e.response_id && proxyResponseIds.has(e.response_id))
}

/** Read the arguments of a tool call (JSON text); anything unreadable stays text. */
export function parseArguments(text: string | undefined): unknown {
  if (text === undefined || text === "") return undefined
  try {
    return JSON.parse(text) as unknown
  } catch {
    return text
  }
}

export function clipText(text: string, max = 800): { text: string; clipped: boolean } {
  return text.length > max ? { text: `${text.slice(0, max)} …`, clipped: true } : { text, clipped: false }
}

type Obj = Record<string, unknown>
const isObj = (v: unknown): v is Obj => typeof v === "object" && v !== null && !Array.isArray(v)

/** Agent names in the arguments of the subagent tool (single, parallel or as a chain). */
export function toolAgents(args: unknown): string[] {
  if (!isObj(args)) return []
  const out: string[] = []
  if (typeof args.agent === "string") out.push(args.agent)
  for (const k of ["tasks", "chain", "parallel"]) {
    const list = args[k]
    if (Array.isArray(list)) for (const t of list) if (isObj(t) && typeof t.agent === "string") out.push(t.agent)
  }
  return out
}

/** Times of the entries; missing ones inherit the predecessor's (live, still without a timestamp). */
export function effectiveTimes(items: TranscriptItem[]): number[] {
  let last = -Infinity
  return items.map((i) => {
    if (i.time !== undefined) last = i.time
    return last
  })
}

/** Index of the last entry whose time is not after `time` (-1: before all). */
export function placeAfter(items: TranscriptItem[], time: number): number {
  const times = effectiveTimes(items)
  let at = -1
  times.forEach((t, i) => {
    if (t <= time) at = i
  })
  return at
}

export type RunAssignment = {
  /** Runs per toolCallId of a subagent call of the main agent. */
  byTool: Record<string, string[]>
  /** Runs without a matching call, per index of the entry they follow (-1: at the front). */
  loose: Record<number, string[]>
}

const push = <K extends string | number>(rec: Record<K, string[]>, k: K, v: string) => {
  ;(rec[k] ??= []).push(v)
}

/**
 * Assigns each run to the main agent's last subagent call before the start of the run
 * (background runs only run after the call). Within one message, the call
 * whose arguments name the run's agent wins, otherwise the first one.
 */
export function assignRuns(
  items: TranscriptItem[],
  runs: Pick<SubagentRun, "runId" | "agent" | "start">[],
): RunAssignment {
  const times = effectiveTimes(items)
  const candidates: { idx: number; time: number; calls: { id: string; agents: string[] }[] }[] = []
  items.forEach((item, idx) => {
    if (item.kind !== "assistant") return
    const calls = item.blocks
      .filter((b) => b.type === "toolCall" && b.name === "subagent" && !!b.id)
      .map((b) => (b.type === "toolCall" ? { id: b.id, agents: toolAgents(b.arguments) } : { id: "", agents: [] }))
    if (calls.length) candidates.push({ idx, time: times[idx], calls })
  })
  const out: RunAssignment = { byTool: {}, loose: {} }
  for (const r of runs) {
    const c = candidates.findLast((x) => x.time <= r.start)
    if (!c) {
      push(out.loose, placeAfter(items, r.start), r.runId)
      continue
    }
    const hit = c.calls.find((x) => x.agents.includes(r.agent)) ?? c.calls[0]
    push(out.byTool, hit.id, r.runId)
  }
  return out
}

export type LimitKind = "agent_limit" | "subagent_limit"

/** Recognises the orchestrator's limit messages (event `error`, from internal/chat/subagents.go). Live events only,
 * so only the English form is matched. */
export function limitErrorKind(message: string | undefined): LimitKind | undefined {
  if (!message) return undefined
  if (/concurrent agents/i.test(message)) return "agent_limit"
  if (/^limit exceeded:.*subagents/i.test(message)) return "subagent_limit"
  return undefined
}

export type LimitNotice = { id: number; op: LimitKind; time: number; count: number; text: string }

function noticeText(op: LimitKind, detail: string): string {
  const d = detail ? ` (${detail})` : ""
  return op === "agent_limit"
    ? `Limit of concurrent agents reached: model call refused at the proxy${d}`
    : `Subagent limit exceeded – aborted${d}`
}

/** Notices for the history from the log; directly consecutive equal ones are counted. */
export function limitNotices(calls: SocketCall[]): LimitNotice[] {
  const out: LimitNotice[] = []
  const sorted = [...calls].sort((a, b) => a.id - b.id)
  for (const c of sorted) {
    if (c.op !== "agent_limit" && c.op !== "subagent_limit") continue
    const text = noticeText(c.op, c.detail)
    const last = out[out.length - 1]
    if (last && last.op === c.op && last.text === text) {
      last.count++
      continue
    }
    out.push({ id: c.id, op: c.op, time: ms(c.created_at), count: 1, text })
  }
  return out
}

/** Subagents running now against the fixed limit of the service (at most this many at the same time). */
export function subagentsRunningLabel(chat: Pick<Chat, "subagents_running" | "max_subagents">): string {
  return `Subagents ${chat.subagents_running ?? 0} / ${chat.max_subagents ?? "–"} running`
}

/** Tooltip of the label: the limit is fixed for the service and counts subagents at the same time. */
export function subagentsRunningTitle(chat: Pick<Chat, "subagents" | "max_subagents">): string {
  const max = chat.max_subagents
  return max === undefined
    ? `${chat.subagents ?? 0} subagents started so far`
    : `At most ${max} subagents run at the same time (fixed for the service). ${chat.subagents ?? 0} started so far.`
}

/** ID of the base run; parallel children carry "#n". */
export function baseRunId(runId: string): string {
  const i = runId.indexOf("#")
  return i < 0 ? runId : runId.slice(0, i)
}

export type RunTreeNode = {
  /** ID of the base run. */
  id: string
  /** The base run itself, if it has entries of its own. */
  run?: SubagentRun
  /** Parallel runs (`id#n`), sorted by n. */
  children: SubagentRun[]
  start: number
  end: number
}

const childIndex = (runId: string) => Number(runId.slice(runId.indexOf("#") + 1)) || 0

/** Tree for the overview: one node per base run, parallel runs below it; nodes by start. */
export function buildRunTree(runs: SubagentRun[]): RunTreeNode[] {
  const nodes = new Map<string, RunTreeNode>()
  for (const r of runs) {
    const id = baseRunId(r.runId)
    let n = nodes.get(id)
    if (!n) {
      n = { id, children: [], start: r.start, end: r.end }
      nodes.set(id, n)
    }
    if (id === r.runId) n.run = r
    else n.children.push(r)
    n.start = Math.min(n.start, r.start)
    n.end = Math.max(n.end, r.end)
  }
  for (const n of nodes.values()) n.children.sort((a, b) => childIndex(a.runId) - childIndex(b.runId))
  return [...nodes.values()].sort((a, b) => a.start - b.start)
}

export type RunStatus = "running" | "idle" | "done" | "stopped"

/** After this long without activity, a run counts as "quiet" while the chat is working. */
export const RUN_IDLE_MS = 90_000

const RUNNING_STATES = new Set(["running", "queued", "pending", "starting", "active", "waiting", "paused"])
const DONE_STATES = new Set(["complete", "completed", "done", "succeeded", "success"])

/**
 * Status of a run. Authoritative is the state according to pi-subagents (`state`); only without it is it
 * estimated: if the run ends with a text response, it is done; if something came recently, it is running (even
 * when the main agent is idle, because background runs keep working); otherwise quiet or ended.
 */
export function runStatus(
  run: Pick<SubagentRun, "entries" | "end" | "state">,
  { chatRunning, now }: { chatRunning: boolean; now: number },
): RunStatus {
  const st = run.state?.toLowerCase()
  if (st) {
    if (DONE_STATES.has(st)) return "done"
    if (RUNNING_STATES.has(st)) return "running"
    return "stopped"
  }
  const last = run.entries[run.entries.length - 1]
  if (last?.kind === "text") return "done"
  if (now - run.end <= RUN_IDLE_MS) return "running"
  return chatRunning ? "idle" : "stopped"
}

const statusLabels: Record<RunStatus, string> = {
  running: "running",
  idle: "quiet",
  done: "done",
  stopped: "ended without response",
}

export const runStatusLabel = (s: RunStatus) => statusLabels[s]

/** How many entries of a run are verified at the proxy. */
export function runConfirmation(run: Pick<SubagentRun, "entries">, proxyIds: Set<string>): { confirmed: number; total: number } {
  return { confirmed: run.entries.filter((e) => isEntryConfirmed(e, proxyIds)).length, total: run.entries.length }
}

export type RunItem =
  | { type: "task"; entry: SubagentEntry }
  | { type: "text"; entry: SubagentEntry }
  /** Tool call with result; either can be missing (open, or without a matching call). */
  | { type: "tool"; call?: SubagentEntry; result?: SubagentEntry }

/**
 * Pairs tool calls and results so that the view shows them as one card, as in the main history.
 * A result belongs to the oldest open call of the same name (without a name: to the
 * oldest open one at all). The entries carry no call ID; this is the best approximation.
 */
export function pairRunEntries(entries: SubagentEntry[]): RunItem[] {
  const items: RunItem[] = []
  const open: { item: Extract<RunItem, { type: "tool" }>; name: string }[] = []
  for (const e of entries) {
    switch (e.kind) {
      case "task":
      case "text":
        items.push({ type: e.kind, entry: e })
        break
      case "tool_call": {
        const item: RunItem = { type: "tool", call: e }
        items.push(item)
        open.push({ item, name: e.payload?.name ?? "" })
        break
      }
      case "tool_result": {
        const name = e.payload?.name
        // exactly via the call ID, otherwise the oldest open call of the same name
        const callId = e.payload?.tool_call_id
        let i = callId ? open.findIndex((o) => o.item.call?.payload?.id === callId) : -1
        if (i < 0) i = open.findIndex((o) => !name || !o.name || o.name === name)
        if (i < 0) items.push({ type: "tool", result: e })
        else {
          open[i].item.result = e
          open.splice(i, 1)
        }
        break
      }
    }
  }
  return items
}

/** true if the session of a socket call is a subagent run (not "main", not empty). */
export const isSubagentSession = (session?: string): session is string => !!session && session !== "main"

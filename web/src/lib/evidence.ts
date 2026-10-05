// Reconciliation of requested and executed tool calls (E9), as on the server
// (internal/chat/reconcile.go). Requested: seen at the LLM proxy in the provider's response.
// Executed: executed by the orchestrator in the execution sandbox (tool_executions). Both
// sources are out of the agent's reach. Pure functions, without React.
import type { LLMCall, SubagentEntry, ToolExecutionRecord } from "@/api/types"
import type { ToolExecution } from "./stream"

/**
 * aborted: the model's response broke off (at the proxy without finish_reason); pi does not execute its
 * tool calls. rejected: refused according to pi's session (invalid arguments,
 * hidden tool, guard); not tamper-proof. Both are harmless causes of
 * "requested, not executed" and do not count as suspicious (M1).
 */
export type EvidenceState = "confirmed" | "unrequested" | "unexecuted" | "mismatch" | "internal" | "aborted" | "rejected"
/** For display: additionally "pending" while a run is not finished yet. */
export type DisplayState = EvidenceState | "pending"

export type Evidence = {
  toolCallId: string
  state: EvidenceState
  /** requested, otherwise executed */
  tool: string
  executedTool?: string
  requested: boolean
  executed: boolean
  /** request in a response of the main session */
  main: boolean
  /** from the execution: "main" or ID of the subagent run */
  session?: string
  ops: string[]
  executions: ToolExecutionRecord[]
  exitCode?: number
  error?: string
  durationMs: number
  /** for "rejected": error message from the session (not tamper-proof) */
  reason?: string
}

export type ReconcileOptions = {
  /** Tools whose execution is verified at the socket: /api/config → executed_tools (L6). */
  executedTools?: Iterable<string>
  /** Error messages from the sessions per toolCallId (rejectionsFrom). */
  rejections?: Map<string, string>
}

export function reconcile(llmCalls: LLMCall[], execs: ToolExecutionRecord[], opts: ReconcileOptions = {}): Map<string, Evidence> {
  const executedTools = new Set(opts.executedTools ?? [])
  const m = new Map<string, Evidence>()
  const incomplete = new Set<string>()
  for (const c of llmCalls) {
    for (const t of c.tool_calls ?? []) {
      if (!t.id || m.has(t.id)) continue
      m.set(t.id, { toolCallId: t.id, state: "internal", tool: t.name, requested: true, executed: false, main: c.main, ops: [], executions: [], durationMs: 0 })
      // Older entries without the field count as complete (as on the server).
      if (c.complete === false) incomplete.add(t.id)
    }
  }
  for (const e of [...execs].sort((a, b) => a.id - b.id)) {
    let ev = m.get(e.tool_call_id)
    if (!ev) {
      ev = { toolCallId: e.tool_call_id, state: "internal", tool: e.tool, requested: false, executed: false, main: false, ops: [], executions: [], durationMs: 0 }
      m.set(e.tool_call_id, ev)
    }
    if (!ev.executed) {
      ev.executed = true
      ev.executedTool = e.tool
      ev.session = e.session
    } else if (!ev.executedTool!.split(",").includes(e.tool)) {
      ev.executedTool += "," + e.tool
    }
    ev.ops.push(e.op)
    ev.executions.push(e)
    ev.durationMs += e.duration_ms ?? 0
    ev.exitCode = e.exit_code
    ev.error = e.error || undefined
  }
  for (const ev of m.values()) {
    if (ev.requested && ev.executed) ev.state = ev.executedTool === ev.tool ? "confirmed" : "mismatch"
    else if (ev.executed) ev.state = "unrequested"
    else if (!executedTools.has(ev.tool)) ev.state = "internal"
    else if (incomplete.has(ev.toolCallId)) ev.state = "aborted"
    else if (opts.rejections?.get(ev.toolCallId)) {
      ev.state = "rejected"
      ev.reason = opts.rejections.get(ev.toolCallId)
    } else ev.state = "unexecuted"
  }
  return m
}

/**
 * Error messages per toolCallId from the main session (tool results in the history) and the
 * subagents' sessions, like store.ToolRejections on the server. The source is pi's session files,
 * so only a hint.
 */
export function rejectionsFrom(tools: Record<string, ToolExecution>, subagentEntries: SubagentEntry[]): Map<string, string> {
  const m = new Map<string, string>()
  const clip = (t: string) => (t.length > 300 ? t.slice(0, 300) + " …" : t)
  for (const t of Object.values(tools)) {
    if (t.isError && t.toolCallId) m.set(t.toolCallId, clip(t.result ?? ""))
  }
  for (const e of subagentEntries) {
    if (e.kind === "tool_result" && e.payload.is_error && e.payload.tool_call_id) m.set(e.payload.tool_call_id, clip(e.payload.text ?? ""))
  }
  return m
}

export type EvidenceSummary = Record<EvidenceState, number> & { flagged: number; total: number }

export function evidenceSummary(m: Map<string, Evidence>): EvidenceSummary {
  const s: EvidenceSummary = { confirmed: 0, unrequested: 0, unexecuted: 0, mismatch: 0, internal: 0, aborted: 0, rejected: 0, flagged: 0, total: 0 }
  for (const ev of m.values()) {
    s[ev.state]++
    s.total++
  }
  s.flagged = s.unrequested + s.unexecuted + s.mismatch
  return s
}

/**
 * State for display. During a run, request (proxy, after the end of the
 * response) and execution (socket) can arrive in any order; evaluation therefore happens
 * only when the run is finished. Verified is verified right away.
 */
export function displayState(ev: Evidence | undefined, { settled }: { settled: boolean }): DisplayState | undefined {
  if (!ev) return undefined
  if (ev.state === "confirmed" || ev.state === "internal") return ev.state
  return settled ? ev.state : "pending"
}

export function evidenceLabel(state: DisplayState): { label: string; tone: "ok" | "bad" | "muted"; title: string } {
  switch (state) {
    case "confirmed":
      return { label: "verified", tone: "ok", title: "executed by the orchestrator: requested at the proxy and executed in the execution sandbox" }
    case "unexecuted":
      return { label: "not executed", tone: "bad", title: "requested at the proxy and delivered completely, but never executed by the orchestrator, with no sign of a refusal by pi (e.g. bypassing the redirection)" }
    case "unrequested":
      return { label: "not requested", tone: "bad", title: "executed by the orchestrator, but never requested at the proxy" }
    case "mismatch":
      return { label: "mismatch", tone: "bad", title: "executed under a different tool than requested" }
    case "internal":
      return { label: "no sandbox", tone: "muted", title: "tool does not run in the execution sandbox (e.g. todo, subagent or an MCP tool at the socket)" }
    case "aborted":
      return { label: "response aborted", tone: "muted", title: "the model's response did not arrive completely at the proxy (no finish_reason); pi does not execute calls from it" }
    case "rejected":
      return { label: "refused by pi", tone: "muted", title: "refused according to pi's session, e.g. because of invalid arguments or a hidden tool (hint from the session, not tamper-proof)" }
    case "pending":
      return { label: "reconciling", tone: "muted", title: "request at the proxy and execution are reconciled after the run" }
  }
}

export function sessionLabel(session: string | undefined): string {
  if (!session) return "–"
  if (session === "main") return "Main agent"
  const [run, n] = session.split("#")
  return `Subagent ${run.slice(0, 8)}${n ? ` #${n}` : ""}`
}

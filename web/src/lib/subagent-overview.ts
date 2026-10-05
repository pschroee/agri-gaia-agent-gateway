// Overview of the subagents for the chat header, switcher and tree in the side tab: title, status, duration,
// metrics and grouping. Pure functions, without React.
import type { LLMCall } from "@/api/types"
import { buildRunTree, runStatus, shortRunId, type RunStatus, type SubagentRun } from "./subagents"

const round = (v: number) => Math.round(v * 1e9) / 1e9

/** Title of a run: first non-empty line of the task, shortened; otherwise the agent name. */
export function runTitle(run: Pick<SubagentRun, "task" | "agent" | "runId" | "label">, max = 80): string {
  if (run.label) return run.label
  const line = (run.task ?? "")
    .split("\n")
    .map((l) => l.trim())
    .find((l) => l !== "")
    // pi-subagents prefixes the task with "Task:"; in the title that is just noise.
    ?.replace(/^task:\s*/i, "")
  if (!line) return `Subagent${run.agent ? ` ${run.agent}` : ""}`
  return line.length > max ? `${line.slice(0, max - 1)}…` : line
}

/** Second line: agent and short run ID. */
export function runSubtitle(run: Pick<SubagentRun, "agent" | "runId" | "task" | "label">): string {
  const base = `${run.agent || "Subagent"} · run ${shortRunId(run.runId)}`
  if (!run.label) return base
  // With a name, the task goes into the second line so that it does not get lost.
  const task = runTitle({ ...run, label: undefined }, 60)
  return task && task !== `Subagent${run.agent ? ` ${run.agent}` : ""}` ? `${base} · ${task}` : base
}

const live = (s: RunStatus) => s === "running" || s === "idle"

/** Duration in ms: when ended from the first to the last entry, otherwise until now. */
export function runDuration(run: Pick<SubagentRun, "start" | "end">, status: RunStatus, now: number): number {
  const end = live(status) ? Math.max(now, run.end) : run.end
  return Math.max(0, end - run.start)
}

export function formatSpan(ms: number): string {
  const s = Math.max(0, Math.round(ms / 1000))
  if (s < 60) return `${s} s`
  const m = Math.floor(s / 60)
  return m < 60 ? `${m} min ${s % 60} s` : `${Math.floor(m / 60)} h ${m % 60} min`
}

const dec1 = new Intl.NumberFormat("en-US", { maximumFractionDigits: 1 })
const dec0 = new Intl.NumberFormat("en-US", { maximumFractionDigits: 0 })

/** Short token count: 999, 1.2k, 12k, 2.5M. */
export function formatTokensShort(n: number): string {
  if (n < 1000) return dec0.format(n)
  if (n < 1_000_000) return `${(n < 10_000 ? dec1 : dec0).format(n / 1000)}k`
  return `${(n < 10_000_000 ? dec1 : dec0).format(n / 1_000_000)}M`
}

export type RunMetrics = {
  /** Model calls recorded at the proxy whose response occurs in the run. */
  llmCalls: number
  input: number
  output: number
  cacheRead: number
  /** Input plus output tokens; missing if no model call is assigned. */
  tokens?: number
  /** Cost by tariff; missing if no model call is assigned. */
  cost?: number
  toolCalls: number
  errors: number
}

/**
 * Metrics of a run. Tokens and cost come from the proxy (tamper-proof); they are assigned
 * via the response_id of the entries, and that comes from the sandbox.
 */
export function runMetrics(
  run: Pick<SubagentRun, "entries" | "toolCalls" | "errors">,
  llmCalls: LLMCall[],
): RunMetrics {
  const ids = new Set(run.entries.map((e) => e.response_id).filter((id): id is string => !!id))
  const m: RunMetrics = { llmCalls: 0, input: 0, output: 0, cacheRead: 0, toolCalls: run.toolCalls, errors: run.errors }
  let cost = 0
  for (const c of llmCalls) {
    if (!c.response_id || !ids.has(c.response_id)) continue
    m.llmCalls++
    m.input += c.input ?? 0
    m.output += c.output ?? 0
    m.cacheRead += c.cache_read ?? 0
    cost += c.cost ?? 0
  }
  if (m.llmCalls > 0) {
    m.tokens = m.input + m.output
    m.cost = round(cost)
  }
  return m
}

export type StatusCounts = Record<RunStatus, number>

export function statusCounts(list: RunStatus[]): StatusCounts {
  const c: StatusCounts = { running: 0, idle: 0, done: 0, stopped: 0 }
  for (const s of list) c[s]++
  return c
}

const countWords: [RunStatus, string][] = [
  ["running", "running"],
  ["idle", "quiet"],
  ["done", "done"],
  ["stopped", "without response"],
]

/** "1 running · 4 done"; empty counters are left out. */
export function statusCountsLabel(c: StatusCounts): string {
  return countWords
    .filter(([k]) => c[k] > 0)
    .map(([k, w]) => `${c[k]} ${w}`)
    .join(" · ")
}

export const subagentCountLabel = (n: number) => `${n} subagent${n === 1 ? "" : "s"}`

export type AgentNode = {
  /** main: main agent (root); run: a run; parallel: parallel runs without their own base run. */
  kind: "main" | "run" | "parallel"
  id: string
  /** Only for kind "run": target of the detail view. */
  runId?: string
  title: string
  subtitle: string
  status: RunStatus
  metrics: RunMetrics
  durationMs: number
  start: number
  children: AgentNode[]
}

const emptyMetrics = (): RunMetrics => ({ llmCalls: 0, input: 0, output: 0, cacheRead: 0, toolCalls: 0, errors: 0 })

function sumMetrics(list: RunMetrics[]): RunMetrics {
  const m = emptyMetrics()
  let tokens: number | undefined
  let cost: number | undefined
  for (const x of list) {
    m.llmCalls += x.llmCalls
    m.input += x.input
    m.output += x.output
    m.cacheRead += x.cacheRead
    m.toolCalls += x.toolCalls
    m.errors += x.errors
    if (x.tokens !== undefined) tokens = (tokens ?? 0) + x.tokens
    if (x.cost !== undefined) cost = round((cost ?? 0) + x.cost)
  }
  if (tokens !== undefined) m.tokens = tokens
  if (cost !== undefined) m.cost = cost
  return m
}

/** Combined status of several runs: running before quiet before without response before done. */
function combinedStatus(list: RunStatus[]): RunStatus {
  for (const s of ["running", "idle", "stopped"] as const) if (list.includes(s)) return s
  return "done"
}

type TreeInput = { chatTitle: string; chatRunning: boolean; runs: SubagentRun[]; llmCalls: LLMCall[]; now: number }

/**
 * Tree for the chat header and side tab: the main agent as root, below it one node per base run,
 * parallel runs (`id#n`) one level deeper. A further level could be attached via `children`.
 */
export function buildAgentTree({ chatTitle, chatRunning, runs, llmCalls, now }: TreeInput): AgentNode {
  const leaf = (r: SubagentRun, children: AgentNode[] = []): AgentNode => {
    const status = runStatus(r, { chatRunning, now })
    return {
      kind: "run",
      id: r.runId,
      runId: r.runId,
      title: runTitle(r),
      subtitle: runSubtitle(r),
      status,
      metrics: runMetrics(r, llmCalls),
      durationMs: runDuration(r, status, now),
      start: r.start,
      children,
    }
  }
  const children = buildRunTree(runs).map((n): AgentNode => {
    const kids = n.children.map((c) => leaf(c))
    if (n.run) return leaf(n.run, kids)
    const start = Math.min(...kids.map((k) => k.start))
    const end = Math.max(...kids.map((k) => k.start + k.durationMs))
    return {
      kind: "parallel",
      id: n.id,
      title: `Parallel runs (${kids.length})`,
      subtitle: `run ${shortRunId(n.id)}`,
      status: combinedStatus(kids.map((k) => k.status)),
      metrics: sumMetrics(kids.map((k) => k.metrics)),
      durationMs: Math.max(0, end - start),
      start,
      children: kids,
    }
  })
  return {
    kind: "main",
    id: "main",
    title: chatTitle || "Untitled",
    subtitle: "Main agent",
    status: chatRunning ? "running" : "done",
    metrics: sumMetrics(children.map((c) => c.metrics)),
    durationMs: 0,
    start: children[0]?.start ?? 0,
    children,
  }
}

/** All runs below the root in tree order; group nodes without a run are left out, their children are not. */
export function flattenAgentTree(root: AgentNode): { node: AgentNode; depth: number }[] {
  const out: { node: AgentNode; depth: number }[] = []
  const walk = (list: AgentNode[], depth: number) => {
    for (const n of list) {
      if (n.runId) out.push({ node: n, depth })
      walk(n.children, depth + 1)
    }
  }
  walk(root.children, 0)
  return out
}

export function containsRun(list: AgentNode[], runId: string | undefined): boolean {
  if (!runId) return false
  return list.some((n) => n.runId === runId || containsRun(n.children, runId))
}

/** How many cards the tree shows individually before combining them into a group card. */
export const GROUP_AFTER = 3

export type NodeGroup =
  | { type: "nodes"; nodes: AgentNode[] }
  | { type: "group"; nodes: AgentNode[]; counts: StatusCounts }

/** Up to `max` nodes stand individually, above that all in a collapsible group with counters. */
export function groupAgentNodes(nodes: AgentNode[], max = GROUP_AFTER): NodeGroup {
  if (nodes.length <= max) return { type: "nodes", nodes }
  return { type: "group", nodes, counts: statusCounts(nodes.map((n) => n.status)) }
}

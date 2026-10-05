// Übersicht der Subagenten für Chatkopf, Umschalter und Baum im Seitenreiter: Titel, Status, Dauer,
// Kennzahlen und Gruppierung. Reine Funktionen, ohne React.
import type { LLMCall } from "@/api/types"
import { buildRunTree, runStatus, shortRunId, type RunStatus, type SubagentRun } from "./subagents"

const round = (v: number) => Math.round(v * 1e9) / 1e9

/** Titel eines Laufs: erste nicht leere Zeile des Auftrags, gekürzt; sonst der Agentenname. */
export function runTitle(run: Pick<SubagentRun, "task" | "agent" | "runId" | "label">, max = 80): string {
  if (run.label) return run.label
  const line = (run.task ?? "")
    .split("\n")
    .map((l) => l.trim())
    .find((l) => l !== "")
    // pi-subagents stellt dem Auftrag „Task:“ voran; im Titel ist das nur Rauschen.
    ?.replace(/^task:\s*/i, "")
  if (!line) return `Subagent${run.agent ? ` ${run.agent}` : ""}`
  return line.length > max ? `${line.slice(0, max - 1)}…` : line
}

/** Zweite Zeile: Agent und kurze Laufkennung. */
export function runSubtitle(run: Pick<SubagentRun, "agent" | "runId" | "task" | "label">): string {
  const base = `${run.agent || "Subagent"} · Lauf ${shortRunId(run.runId)}`
  if (!run.label) return base
  // Mit Namen steht der Auftrag in der zweiten Zeile, damit er nicht verloren geht.
  const task = runTitle({ ...run, label: undefined }, 60)
  return task && task !== `Subagent${run.agent ? ` ${run.agent}` : ""}` ? `${base} · ${task}` : base
}

const live = (s: RunStatus) => s === "running" || s === "idle"

/** Dauer in ms: beendet vom ersten bis zum letzten Eintrag, sonst bis jetzt. */
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

const dec1 = new Intl.NumberFormat("de-DE", { maximumFractionDigits: 1 })
const dec0 = new Intl.NumberFormat("de-DE", { maximumFractionDigits: 0 })

/** Tokenzahl kurz: 999, 1,2k, 12k, 2,5M. */
export function formatTokensShort(n: number): string {
  if (n < 1000) return dec0.format(n)
  if (n < 1_000_000) return `${(n < 10_000 ? dec1 : dec0).format(n / 1000)}k`
  return `${(n < 10_000_000 ? dec1 : dec0).format(n / 1_000_000)}M`
}

export type RunMetrics = {
  /** Am Proxy erfasste Modellaufrufe, deren Antwort im Lauf vorkommt. */
  llmCalls: number
  input: number
  output: number
  cacheRead: number
  /** Ein- plus Ausgabetokens; fehlt, wenn kein Modellaufruf zugeordnet ist. */
  tokens?: number
  /** Kosten nach Tarif; fehlt, wenn kein Modellaufruf zugeordnet ist. */
  cost?: number
  toolCalls: number
  errors: number
}

/**
 * Kennzahlen eines Laufs. Tokens und Kosten kommen vom Proxy (fälschungssicher); zugeordnet werden sie
 * über die response_id der Einträge, und die stammt aus der Sandbox.
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
  ["running", "läuft"],
  ["idle", "still"],
  ["done", "fertig"],
  ["stopped", "ohne Antwort"],
]

/** „1 läuft · 4 fertig“; leere Zähler entfallen. */
export function statusCountsLabel(c: StatusCounts): string {
  return countWords
    .filter(([k]) => c[k] > 0)
    .map(([k, w]) => `${c[k]} ${w}`)
    .join(" · ")
}

export const subagentCountLabel = (n: number) => `${n} Subagent${n === 1 ? "" : "en"}`

export type AgentNode = {
  /** main: Hauptagent (Wurzel); run: ein Lauf; parallel: parallele Läufe ohne eigenen Basis-Lauf. */
  kind: "main" | "run" | "parallel"
  id: string
  /** Nur bei kind "run": Ziel der Detailansicht. */
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

/** Gesamtstatus mehrerer Läufe: läuft vor still vor ohne Antwort vor fertig. */
function combinedStatus(list: RunStatus[]): RunStatus {
  for (const s of ["running", "idle", "stopped"] as const) if (list.includes(s)) return s
  return "done"
}

type TreeInput = { chatTitle: string; chatRunning: boolean; runs: SubagentRun[]; llmCalls: LLMCall[]; now: number }

/**
 * Baum für Chatkopf und Seitenreiter: der Hauptagent als Wurzel, darunter je Basis-Lauf ein Knoten,
 * parallele Läufe (`id#n`) eine Ebene tiefer. Eine weitere Ebene ließe sich über `children` anhängen.
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
      title: `Parallele Läufe (${kids.length})`,
      subtitle: `Lauf ${shortRunId(n.id)}`,
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
    title: chatTitle || "Ohne Titel",
    subtitle: "Hauptagent",
    status: chatRunning ? "running" : "done",
    metrics: sumMetrics(children.map((c) => c.metrics)),
    durationMs: 0,
    start: children[0]?.start ?? 0,
    children,
  }
}

/** Alle Läufe unterhalb der Wurzel in Baumreihenfolge; Gruppenknoten ohne Lauf entfallen, ihre Kinder nicht. */
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

/** Wie viele Karten der Baum einzeln zeigt, bevor er sie zu einer Gruppenkarte zusammenfasst. */
export const GROUP_AFTER = 3

export type NodeGroup =
  | { type: "nodes"; nodes: AgentNode[] }
  | { type: "group"; nodes: AgentNode[]; counts: StatusCounts }

/** Bis `max` Knoten stehen einzeln, darüber alle in einer aufklappbaren Gruppe mit Zählern. */
export function groupAgentNodes(nodes: AgentNode[], max = GROUP_AFTER): NodeGroup {
  if (nodes.length <= max) return { type: "nodes", nodes }
  return { type: "group", nodes, counts: statusCounts(nodes.map((n) => n.status)) }
}

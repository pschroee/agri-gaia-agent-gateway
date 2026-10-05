// Subagenten: Einträge aus den Sitzungsdateien je Lauf gruppieren, den Werkzeugaufrufen des
// Hauptagenten zuordnen und Grenzmeldungen aufbereiten. Reine Funktionen, ohne React.
import type { Chat, SocketCall, SubagentEntry, SubagentRunMeta } from "@/api/types"
import type { TranscriptItem } from "./stream"

export type SubagentRun = {
  runId: string
  agent: string
  entries: SubagentEntry[]
  /** Zeitpunkt des ersten und letzten Eintrags in ms. */
  start: number
  end: number
  /** Auftrag (erster task-Eintrag). */
  task?: string
  toolCalls: number
  errors: number
  /** Name im Workflow (Schlüssel bei runs.run/runs.all), laut pi-subagents. */
  label?: string
  /** Zustand laut pi-subagents; fehlt er, wird der Status aus den Einträgen geschätzt. */
  state?: string
}

const entryKey = (e: SubagentEntry) => `${e.run_id}\u0000${e.entry_id}`

const ms = (iso: string) => {
  const v = Date.parse(iso)
  return Number.isNaN(v) ? 0 : v
}

/** Ergänzt Einträge; gleiche (run_id, entry_id) werden ersetzt, die Reihenfolge bleibt erhalten. */
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
 * Gruppiert Einträge je Lauf (Reihenfolge innerhalb stabil nach created_at), Läufe nach Start.
 * `meta` ergänzt Agent, Namen und Zustand; ein Lauf ohne Einträge erscheint schon mit seinen Metadaten.
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

/** Kurzform der Laufkennung: die ersten sechs Zeichen, bei parallelen Kindern mit „#n“. */
export function shortRunId(runId: string): string {
  const [base, idx] = runId.split("#")
  const short = base.length > 6 ? base.slice(0, 6) : base
  return idx !== undefined ? `${short}#${idx}` : short
}

/** Anzeigename: Name im Workflow, sonst Agent (etwa „reid“ bzw. „researcher“). */
export function runName(run: Pick<SubagentRun, "agent" | "label">): string {
  return run.label || run.agent || "Subagent"
}

export function runLabel(run: Pick<SubagentRun, "agent" | "runId" | "label">): string {
  const who = run.label ? `${run.label}${run.agent ? ` (${run.agent})` : ""}` : run.agent
  return `Subagent${who ? ` ${who}` : ""} · Lauf ${shortRunId(run.runId)}`
}

/** Ob ein Eintrag am Proxy belegt ist: laut Server oder weil seine Antwort dort erfasst wurde. */
export function isEntryConfirmed(e: SubagentEntry, proxyResponseIds: Set<string>): boolean {
  return e.confirmed || (!!e.response_id && proxyResponseIds.has(e.response_id))
}

/** Argumente eines Werkzeugaufrufs (JSON-Text) lesen; Unlesbares bleibt Text. */
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

/** Agentennamen in den Argumenten des subagent-Werkzeugs (einzeln, parallel oder als Kette). */
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

/** Zeiten der Einträge; fehlende erben die des Vorgängers (live noch ohne Zeitstempel). */
export function effectiveTimes(items: TranscriptItem[]): number[] {
  let last = -Infinity
  return items.map((i) => {
    if (i.time !== undefined) last = i.time
    return last
  })
}

/** Index des letzten Eintrags, dessen Zeit nicht nach `time` liegt (-1: vor allen). */
export function placeAfter(items: TranscriptItem[], time: number): number {
  const times = effectiveTimes(items)
  let at = -1
  times.forEach((t, i) => {
    if (t <= time) at = i
  })
  return at
}

export type RunAssignment = {
  /** Läufe je toolCallId eines subagent-Aufrufs des Hauptagenten. */
  byTool: Record<string, string[]>
  /** Läufe ohne passenden Aufruf, je Index des Eintrags, hinter dem sie stehen (-1: vorne). */
  loose: Record<number, string[]>
}

const push = <K extends string | number>(rec: Record<K, string[]>, k: K, v: string) => {
  ;(rec[k] ??= []).push(v)
}

/**
 * Ordnet jeden Lauf dem letzten subagent-Aufruf des Hauptagenten zu, der vor dem Start des Laufs
 * lag (Läufe im Hintergrund laufen erst nach dem Aufruf). Innerhalb einer Nachricht gewinnt der
 * Aufruf, dessen Argumente den Agenten des Laufs nennen, sonst der erste.
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

/** Erkennt die Grenzmeldungen des Orchestrators (Ereignis `error`). */
export function limitErrorKind(message: string | undefined): LimitKind | undefined {
  if (!message) return undefined
  if (/gleichzeitige Agenten/i.test(message)) return "agent_limit"
  if (/^Grenze überschritten:.*Subagenten/i.test(message)) return "subagent_limit"
  return undefined
}

export type LimitNotice = { id: number; op: LimitKind; time: number; count: number; text: string }

function noticeText(op: LimitKind, detail: string): string {
  const d = detail ? ` (${detail})` : ""
  return op === "agent_limit"
    ? `Grenze gleichzeitiger Agenten erreicht: Modellaufruf am Proxy abgewiesen${d}`
    : `Subagenten-Grenze überschritten – abgebrochen${d}`
}

/** Hinweise für den Verlauf aus dem Protokoll; direkt aufeinanderfolgende gleiche werden gezählt. */
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

export function subagentLimitLabel(chat: Pick<Chat, "subagents" | "max_subagents">): string {
  return `Subagenten ${chat.subagents ?? 0} / ${chat.max_subagents ?? "–"}`
}

/** Kennung des Basis-Laufs; parallele Kinder tragen „#n“. */
export function baseRunId(runId: string): string {
  const i = runId.indexOf("#")
  return i < 0 ? runId : runId.slice(0, i)
}

export type RunTreeNode = {
  /** Kennung des Basis-Laufs. */
  id: string
  /** Der Basis-Lauf selbst, falls er eigene Einträge hat. */
  run?: SubagentRun
  /** Parallele Läufe (`id#n`), nach n sortiert. */
  children: SubagentRun[]
  start: number
  end: number
}

const childIndex = (runId: string) => Number(runId.slice(runId.indexOf("#") + 1)) || 0

/** Baum für die Übersicht: je Basis-Lauf ein Knoten, parallele Läufe darunter; Knoten nach Start. */
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

/** Nach so langer Ruhe gilt ein Lauf bei arbeitendem Chat als „still“. */
export const RUN_IDLE_MS = 90_000

const RUNNING_STATES = new Set(["running", "queued", "pending", "starting", "active", "waiting", "paused"])
const DONE_STATES = new Set(["complete", "completed", "done", "succeeded", "success"])

/**
 * Status eines Laufs. Maßgeblich ist der Zustand laut pi-subagents (`state`); nur ohne ihn wird
 * geschätzt: Endet der Lauf mit einer Textantwort, ist er fertig; kam zuletzt etwas, läuft er (auch
 * wenn der Hauptagent ruht, denn Läufe im Hintergrund arbeiten weiter); sonst still bzw. beendet.
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
  running: "läuft",
  idle: "still",
  done: "fertig",
  stopped: "ohne Antwort beendet",
}

export const runStatusLabel = (s: RunStatus) => statusLabels[s]

/** Wie viele Einträge eines Laufs am Proxy belegt sind. */
export function runConfirmation(run: Pick<SubagentRun, "entries">, proxyIds: Set<string>): { confirmed: number; total: number } {
  return { confirmed: run.entries.filter((e) => isEntryConfirmed(e, proxyIds)).length, total: run.entries.length }
}

export type RunItem =
  | { type: "task"; entry: SubagentEntry }
  | { type: "text"; entry: SubagentEntry }
  /** Werkzeugaufruf mit Ergebnis; eines von beiden kann fehlen (offen bzw. ohne passenden Aufruf). */
  | { type: "tool"; call?: SubagentEntry; result?: SubagentEntry }

/**
 * Setzt Werkzeugaufrufe und Ergebnisse zusammen, damit die Ansicht sie wie im Hauptverlauf als eine
 * Karte zeigt. Ein Ergebnis gehört zum ältesten offenen Aufruf gleichen Namens (ohne Namen: zum
 * ältesten offenen überhaupt). Die Einträge tragen keine Aufrufkennung; das ist die beste Näherung.
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
        // Genau über die Aufruf-ID, sonst der älteste offene Aufruf gleichen Namens.
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

/** true, wenn die Sitzung eines Socket-Aufrufs ein Subagenten-Lauf ist (nicht „main“, nicht leer). */
export const isSubagentSession = (session?: string): session is string => !!session && session !== "main"

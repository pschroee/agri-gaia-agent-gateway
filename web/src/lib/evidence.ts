// Abgleich angeforderter und ausgeführter Werkzeugaufrufe (E9), wie auf dem Server
// (internal/chat/reconcile.go). Angefordert: am LLM-Proxy in der Antwort des Anbieters gesehen.
// Ausgeführt: vom Orchestrator in der Ausführungs-Sandbox ausgeführt (tool_executions). Beide
// Quellen liegen außerhalb der Reichweite des Agenten. Reine Funktionen, ohne React.
import type { LLMCall, SubagentEntry, ToolExecutionRecord } from "@/api/types"
import type { ToolExecution } from "./stream"

/**
 * aborted: Die Antwort des Modells brach ab (am Proxy ohne finish_reason); pi führt ihre
 * Werkzeugaufrufe nicht aus. rejected: laut Sitzung von pi abgewiesen (ungültige Argumente,
 * ausgeblendetes Werkzeug, Wächter); nicht fälschungssicher. Beide sind harmlose Ursachen für
 * „angefordert, nicht ausgeführt“ und gelten nicht als auffällig (M1).
 */
export type EvidenceState = "confirmed" | "unrequested" | "unexecuted" | "mismatch" | "internal" | "aborted" | "rejected"
/** Für die Anzeige: dazu „pending“, solange ein Lauf noch nicht fertig ist. */
export type DisplayState = EvidenceState | "pending"

export type Evidence = {
  toolCallId: string
  state: EvidenceState
  /** angefordert, sonst ausgeführt */
  tool: string
  executedTool?: string
  requested: boolean
  executed: boolean
  /** Anforderung in einer Antwort der Hauptsitzung */
  main: boolean
  /** aus der Ausführung: "main" oder Kennung des Subagenten-Laufs */
  session?: string
  ops: string[]
  executions: ToolExecutionRecord[]
  exitCode?: number
  error?: string
  durationMs: number
  /** bei „rejected“: Fehlermeldung aus der Sitzung (nicht fälschungssicher) */
  reason?: string
}

export type ReconcileOptions = {
  /** Werkzeuge, deren Ausführung am Socket belegt wird: /api/config → executed_tools (L6). */
  executedTools?: Iterable<string>
  /** Fehlermeldungen aus den Sitzungen je toolCallId (rejectionsFrom). */
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
      // Ältere Einträge ohne das Feld gelten als vollständig (wie auf dem Server).
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
 * Fehlermeldungen je toolCallId aus der Hauptsitzung (Werkzeugergebnisse im Verlauf) und den
 * Sitzungen der Subagenten, wie store.ToolRejections auf dem Server. Quelle sind Sitzungsdateien
 * von pi, also nur ein Hinweis.
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
 * Zustand für die Anzeige. Während eines Laufs können Anforderung (Proxy, nach dem Ende der
 * Antwort) und Ausführung (Socket) in beliebiger Reihenfolge eintreffen; ausgewertet wird deshalb
 * erst, wenn der Lauf fertig ist. Belegt ist sofort belegt.
 */
export function displayState(ev: Evidence | undefined, { settled }: { settled: boolean }): DisplayState | undefined {
  if (!ev) return undefined
  if (ev.state === "confirmed" || ev.state === "internal") return ev.state
  return settled ? ev.state : "pending"
}

export function evidenceLabel(state: DisplayState): { label: string; tone: "ok" | "bad" | "muted"; title: string } {
  switch (state) {
    case "confirmed":
      return { label: "belegt", tone: "ok", title: "vom Orchestrator ausgeführt: am Proxy angefordert und in der Ausführungs-Sandbox ausgeführt" }
    case "unexecuted":
      return { label: "nicht ausgeführt", tone: "bad", title: "am Proxy angefordert und vollständig geliefert, aber vom Orchestrator nie ausgeführt, ohne Hinweis auf eine Abweisung durch pi (etwa an der Umleitung vorbei)" }
    case "unrequested":
      return { label: "nicht angefordert", tone: "bad", title: "vom Orchestrator ausgeführt, aber am Proxy nie angefordert" }
    case "mismatch":
      return { label: "abweichend", tone: "bad", title: "unter einem anderen Werkzeug ausgeführt als angefordert" }
    case "internal":
      return { label: "ohne Sandbox", tone: "muted", title: "Werkzeug läuft nicht in der Ausführungs-Sandbox (etwa todo, subagent oder ein MCP-Werkzeug am Socket)" }
    case "aborted":
      return { label: "Antwort abgebrochen", tone: "muted", title: "die Antwort des Modells kam am Proxy nicht vollständig an (kein finish_reason); pi führt Aufrufe daraus nicht aus" }
    case "rejected":
      return { label: "von pi abgewiesen", tone: "muted", title: "laut Sitzung von pi abgewiesen, etwa wegen ungültiger Argumente oder eines ausgeblendeten Werkzeugs (Hinweis aus der Sitzung, nicht fälschungssicher)" }
    case "pending":
      return { label: "Abgleich läuft", tone: "muted", title: "Anforderung am Proxy und Ausführung werden nach dem Lauf abgeglichen" }
  }
}

export function sessionLabel(session: string | undefined): string {
  if (!session) return "–"
  if (session === "main") return "Hauptagent"
  const [run, n] = session.split("#")
  return `Subagent ${run.slice(0, 8)}${n ? ` #${n}` : ""}`
}

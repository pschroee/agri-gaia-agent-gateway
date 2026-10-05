import { useState } from "react"
import { EvidenceBadge } from "@/components/badges"
import { displayState, type Evidence, type EvidenceSummary, evidenceLabel, sessionLabel } from "@/lib/evidence"
import { formatTime } from "@/lib/format"
import { cn } from "@/lib/utils"

/**
 * Werkzeugausführungen des Chats (E9): je toolCallId, was am Proxy angefordert und was vom
 * Orchestrator in der Ausführungs-Sandbox ausgeführt wurde. Auffällige Fälle stehen oben.
 */
export function ExecutionsPanel({
  evidence,
  summary,
  settled,
}: {
  evidence: Map<string, Evidence>
  summary: EvidenceSummary
  settled: boolean
}) {
  const [onlyFlagged, setOnlyFlagged] = useState(false)
  const list = [...evidence.values()].filter((e) => e.state !== "internal")
  // Auffällig ist nur, was auf eine Umgehung deuten kann; abgebrochene Antworten und von pi
  // abgewiesene Aufrufe sind grau (M1).
  const flagged = (e: Evidence) => {
    const s = displayState(e, { settled })
    return s === "unexecuted" || s === "unrequested" || s === "mismatch"
  }
  const shown = (onlyFlagged ? list.filter(flagged) : list).sort((a, b) => {
    if (flagged(a) !== flagged(b)) return flagged(a) ? -1 : 1
    return lastId(b) - lastId(a)
  })
  return (
    <div className="flex flex-col gap-2">
      <p className="text-xs text-muted-foreground">
        Jeder Aufruf von bash, read, write, edit, grep, find und ls läuft über den Orchestrator in der Ausführungs-Sandbox.
        Abgeglichen wird je Kennung des Modells: angefordert (Proxy) und ausgeführt (Orchestrator).
      </p>
      <div className="flex flex-wrap items-center gap-1.5 text-xs">
        <span className="rounded-md border border-emerald-200 bg-emerald-50/60 px-1.5 py-0.5 text-emerald-800">
          {summary.confirmed} belegt
        </span>
        {(["unexecuted", "unrequested", "mismatch"] as const).map((k) =>
          summary[k] > 0 ? (
            <span key={k} className="rounded-md border border-red-300 bg-red-50 px-1.5 py-0.5 text-red-800" title={evidenceLabel(k).title}>
              {summary[k]} {evidenceLabel(k).label}
            </span>
          ) : null,
        )}
        {(["aborted", "rejected"] as const).map((k) =>
          summary[k] > 0 ? (
            <span key={k} className="rounded-md border border-border bg-muted/60 px-1.5 py-0.5 text-muted-foreground" title={evidenceLabel(k).title}>
              {summary[k]} {evidenceLabel(k).label}
            </span>
          ) : null,
        )}
        {!settled && summary.flagged > 0 && <span className="text-muted-foreground">(Abgleich nach dem Lauf)</span>}
        <label className="ml-auto flex items-center gap-1 text-muted-foreground">
          <input type="checkbox" checked={onlyFlagged} onChange={(e) => setOnlyFlagged(e.target.checked)} />
          nur auffällige
        </label>
      </div>
      {shown.length === 0 ? (
        <p className="text-xs text-muted-foreground">{onlyFlagged ? "Nichts Auffälliges." : "Noch keine Werkzeugausführungen."}</p>
      ) : (
        <ul className="flex flex-col gap-1.5">
          {shown.map((e) => (
            <li key={e.toolCallId} className={cn("rounded-md border p-2 text-xs", flagged(e) && "border-red-300 bg-red-50/40")}>
              <div className="flex min-w-0 items-center gap-1.5">
                <span className="font-mono font-semibold">{e.tool}</span>
                {e.executedTool && e.executedTool !== e.tool && <span className="font-mono text-red-700">→ {e.executedTool}</span>}
                <span className="truncate text-muted-foreground">{sessionLabel(e.session)}</span>
                <EvidenceBadge evidence={e} settled={settled} className="ml-auto" />
              </div>
              <div className="mt-0.5 truncate font-mono text-muted-foreground" title={e.toolCallId}>
                {e.toolCallId}
              </div>
              {e.executions.length > 0 ? (
                <div className="mt-0.5 text-muted-foreground">
                  {e.ops.join(", ")} · {e.durationMs} ms
                  {e.exitCode !== undefined && ` · Exit ${e.exitCode}`} · {formatTime(e.executions[0].started_at)}
                  {e.error && <div className="break-all text-red-700">{e.error}</div>}
                  {summarizeArgs(e) && <div className="truncate font-mono" title={summarizeArgs(e)}>{summarizeArgs(e)}</div>}
                </div>
              ) : (
                <div className="mt-0.5 text-muted-foreground">
                  keine Ausführung im Protokoll des Orchestrators
                  {e.reason && <div className="break-all">laut Sitzung (nicht fälschungssicher): {e.reason}</div>}
                </div>
              )}
            </li>
          ))}
        </ul>
      )}
    </div>
  )
}

function lastId(e: Evidence): number {
  return e.executions.length > 0 ? e.executions[e.executions.length - 1].id : Number.MAX_SAFE_INTEGER
}

function summarizeArgs(e: Evidence): string {
  const a = e.executions[0]?.args ?? {}
  if (typeof a.command === "string") return a.command
  if (typeof a.path === "string") return a.path
  return ""
}

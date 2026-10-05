import type { LLMCall } from "@/api/types"
import { formatMs, formatTime, formatTokens, formatUsd } from "@/lib/format"
import { llmToolNames, summarizeLlmCalls } from "@/lib/llmcalls"
import { cn } from "@/lib/utils"

/** Reiter „Modellaufrufe“: alle am LLM-Proxy erfassten Aufrufe des Chats, oben die Summe. */
export function LlmCallsPanel({ calls, modelName }: { calls: LLMCall[]; modelName: (id: string) => string }) {
  if (calls.length === 0) {
    return <p className="text-xs text-muted-foreground">Noch keine Modellaufrufe am Proxy erfasst.</p>
  }
  const s = summarizeLlmCalls(calls)
  const sorted = [...calls].sort((a, b) => b.id - a.id)
  return (
    <div className="flex flex-col gap-2">
      <div className="rounded-md border bg-muted/40 p-2 text-xs">
        <div className="flex items-baseline justify-between gap-2">
          <span className="font-semibold">
            {s.count} Aufruf{s.count === 1 ? "" : "e"}
            {s.failed > 0 && <span className="text-red-700"> · {s.failed} mit Fehler</span>}
          </span>
          <span className="font-semibold tabular-nums">{formatUsd(s.cost)}</span>
        </div>
        <div className="mt-0.5 text-muted-foreground tabular-nums">
          {formatTokens(s.input)} ein · {formatTokens(s.output)} aus · {formatTokens(s.cacheRead)} Cache
          {s.cacheWrite > 0 && ` · ${formatTokens(s.cacheWrite)} Cache geschrieben`}
        </div>
        <div className="text-muted-foreground tabular-nums">
          Hauptagent {s.main.count} · {formatUsd(s.main.cost)} — Subagenten u. a. {s.other.count} · {formatUsd(s.other.cost)}
        </div>
        <p className="mt-1 text-[11px] text-muted-foreground">
          Gemessen am LLM-Proxy außerhalb der Sandbox, also fälschungssicher.
        </p>
      </div>
      <ul className="flex flex-col gap-1.5">
        {sorted.map((c) => {
          const tools = llmToolNames(c)
          const failed = c.status !== 200
          return (
            <li key={c.id} className={cn("rounded-md border p-2 text-xs", failed && "border-red-300 bg-red-50/50")}>
              <div className="flex flex-wrap items-center gap-1.5">
                <span className="text-muted-foreground">#{c.id}</span>
                <span
                  className={cn(
                    "rounded border px-1 py-px text-[10px] font-medium",
                    c.main ? "border-sky-200 bg-sky-50 text-sky-800" : "border-violet-200 bg-violet-50 text-violet-800",
                  )}
                  title={c.main ? "Antwort der Hauptsitzung" : "nicht Teil der Hauptantworten: Subagent, Kompaktierung o. Ä."}
                >
                  {c.main ? "Hauptagent" : "Subagent u. a."}
                </span>
                <span
                  className={cn("font-mono", failed ? "font-semibold text-red-700" : "text-muted-foreground")}
                  title="HTTP-Status der Antwort"
                >
                  {c.status}
                </span>
                <span className="ml-auto text-muted-foreground">{formatTime(c.started_at)}</span>
              </div>
              <div className="mt-0.5 truncate" title={c.model}>
                {modelName(c.model)}
              </div>
              <div className="text-muted-foreground tabular-nums">
                {formatTokens(c.input)} ein · {formatTokens(c.output)} aus · {formatTokens(c.cache_read)} Cache
              </div>
              <div className="flex flex-wrap items-center gap-x-1.5 text-muted-foreground tabular-nums">
                <span className="font-medium text-foreground">{formatUsd(c.cost)}</span>
                <span
                  className={cn(
                    "rounded border px-1 py-px text-[10px] font-medium",
                    c.peak ? "border-amber-200 bg-amber-50 text-amber-800" : "border-emerald-200 bg-emerald-50 text-emerald-800",
                  )}
                >
                  {c.peak ? "Spitzentarif" : "Nebentarif"}
                </span>
                <span>· {formatMs(c.duration_ms)}</span>
              </div>
              {tools.length > 0 && (
                <div className="mt-0.5 break-all">
                  <span className="text-muted-foreground">Werkzeuge: </span>
                  <span className="font-mono">{tools.join(", ")}</span>
                </div>
              )}
            </li>
          )
        })}
      </ul>
    </div>
  )
}

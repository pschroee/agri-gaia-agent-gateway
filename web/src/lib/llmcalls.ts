// Am LLM-Proxy erfasste Modellaufrufe zusammenfassen. Reine Funktionen, ohne React.
import type { Chat, LLMCall } from "@/api/types"
import type { TranscriptItem } from "./stream"

export type LlmCallSummary = {
  count: number
  /** Aufrufe mit Status ≠ 200. */
  failed: number
  input: number
  output: number
  cacheRead: number
  cacheWrite: number
  cost: number
  durationMs: number
  main: { count: number; cost: number }
  other: { count: number; cost: number }
}

const round = (v: number) => Math.round(v * 1e9) / 1e9

export function summarizeLlmCalls(calls: LLMCall[]): LlmCallSummary {
  const s: LlmCallSummary = {
    count: calls.length,
    failed: 0,
    input: 0,
    output: 0,
    cacheRead: 0,
    cacheWrite: 0,
    cost: 0,
    durationMs: 0,
    main: { count: 0, cost: 0 },
    other: { count: 0, cost: 0 },
  }
  for (const c of calls) {
    if (c.status !== 200) s.failed++
    s.input += c.input ?? 0
    s.output += c.output ?? 0
    s.cacheRead += c.cache_read ?? 0
    s.cacheWrite += c.cache_write ?? 0
    s.cost += c.cost ?? 0
    s.durationMs += c.duration_ms ?? 0
    const part = c.main ? s.main : s.other
    part.count++
    part.cost += c.cost ?? 0
  }
  s.cost = round(s.cost)
  s.main.cost = round(s.main.cost)
  s.other.cost = round(s.other.cost)
  return s
}

/** Namen der Werkzeuge, die das Modell in diesem Aufruf angefordert hat. */
export function llmToolNames(c: LLMCall): string[] {
  return (c.tool_calls ?? []).map((t) => t.name).filter(Boolean)
}

/** Gesamtkosten des Chats, davon außerhalb der Hauptantworten, und Anzahl Modellaufrufe. */
export function costSplit(chat: Pick<Chat, "cost" | "cost_other" | "llm_calls">) {
  const total = chat.cost ?? 0
  const other = chat.cost_other ?? 0
  return { total, other, main: Math.max(0, round(total - other)), calls: chat.llm_calls ?? 0 }
}

/** Summe der Tarifkosten, die der Verlauf je Antwort und Kompaktierung zeigt. */
export function answerCostSum(items: TranscriptItem[]): number {
  let sum = 0
  for (const i of items) if ((i.kind === "assistant" || i.kind === "compaction") && typeof i.cost === "number") sum += i.cost
  return round(sum)
}

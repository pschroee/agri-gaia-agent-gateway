import { describe, expect, it } from "vitest"
import type { LLMCall } from "@/api/types"
import { answerCostSum, costSplit, llmToolNames, summarizeLlmCalls } from "./llmcalls"

const call = (p: Partial<LLMCall>): LLMCall => ({
  id: 1,
  slot_id: "p",
  source_ip: "10.0.0.2",
  model: "m",
  response_id: "r",
  status: 200,
  input: 0,
  output: 0,
  cache_read: 0,
  cache_write: 0,
  cost: 0,
  peak: false,
  tool_calls: [],
  started_at: "2026-09-29T10:00:00Z",
  duration_ms: 0,
  main: true,
  ...p,
})

describe("summarizeLlmCalls", () => {
  it("sums tokens and cost and separates the main session from the rest", () => {
    const s = summarizeLlmCalls([
      call({ id: 1, input: 100, output: 10, cache_read: 50, cache_write: 5, cost: 0.01, duration_ms: 1000 }),
      call({ id: 2, input: 200, output: 20, cost: 0.02, main: false, duration_ms: 500 }),
      call({ id: 3, status: 429, main: false }),
    ])
    expect(s).toEqual({
      count: 3,
      failed: 1,
      input: 300,
      output: 30,
      cacheRead: 50,
      cacheWrite: 5,
      cost: 0.03,
      durationMs: 1500,
      main: { count: 1, cost: 0.01 },
      other: { count: 2, cost: 0.02 },
    })
  })
  it("tolerates an empty list", () => {
    expect(summarizeLlmCalls([]).count).toBe(0)
  })
})

describe("llmToolNames", () => {
  it("returns the names of the requested tools", () => {
    expect(llmToolNames(call({ tool_calls: [{ name: "bash", arguments: "{}" }, { name: "read", arguments: "{}" }] }))).toEqual([
      "bash",
      "read",
    ])
    expect(llmToolNames(call({ tool_calls: null }))).toEqual([])
  })
})

describe("costSplit", () => {
  it("splits the total cost into main responses and the rest", () => {
    expect(costSplit({ cost: 0.05, cost_other: 0.02, llm_calls: 7 })).toEqual({ total: 0.05, other: 0.02, main: 0.03, calls: 7 })
  })
  it("sets missing values to 0 and never below 0", () => {
    expect(costSplit({ cost: 0.01 })).toEqual({ total: 0.01, other: 0, main: 0.01, calls: 0 })
    expect(costSplit({ cost: 0.01, cost_other: 0.02 }).main).toBe(0)
  })
})

describe("answerCostSum", () => {
  it("sums the tariff costs of the responses and compactions in the history", () => {
    expect(
      answerCostSum([
        { kind: "user", key: "u", text: "x" },
        { kind: "assistant", key: "a", blocks: [], streaming: false, cost: 0.01 },
        { kind: "assistant", key: "b", blocks: [], streaming: false },
        { kind: "compaction", key: "c", reason: "manual", running: false, cost: 0.002 },
      ]),
    ).toBeCloseTo(0.012)
  })
})

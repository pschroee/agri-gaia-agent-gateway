import { describe, expect, it } from "vitest"
import type { ContextUsage } from "@/api/types"
import { cacheHitRate, contextLevel, describeContext, formatPercent } from "./context"

const ctx = (over: Partial<ContextUsage> = {}): ContextUsage => ({
  tokens: 48210,
  window: 1_000_000,
  percent: 4.821,
  threshold_tokens: 983_616,
  reserve_tokens: 16_384,
  keep_recent_tokens: 20_000,
  updated_at: "2026-09-29T10:00:00Z",
  ...over,
})

describe("contextLevel", () => {
  it.each([
    [null, "neutral"],
    [0, "neutral"],
    [59.9, "neutral"],
    [60, "warn"],
    [85, "warn"],
    [85.1, "danger"],
    [120, "danger"],
  ] as const)("%s → %s", (p, level) => {
    expect(contextLevel(p)).toBe(level)
  })
})

describe("formatPercent", () => {
  it.each([
    [4.821, "5 %"],
    [0.4, "< 1 %"],
    [0, "0 %"],
    [99.6, "100 %"],
    [null, "–"],
    [undefined, "–"],
  ] as const)("%s → %s", (p, text) => {
    expect(formatPercent(p)).toBe(text)
  })
})

describe("describeContext", () => {
  it("names used tokens, window, remainder and threshold", () => {
    expect(describeContext(ctx())).toEqual({
      measured: true,
      percent: "5 %",
      used: "48,210 / 1,000,000 tokens",
      remaining: "951,790 tokens free",
      threshold: "983,616 tokens",
      level: "neutral",
      ratio: expect.closeTo(0.04821),
    })
  })
  it("reports after a compaction that it will be measured again", () => {
    const d = describeContext(ctx({ tokens: null, percent: null }))
    expect(d.measured).toBe(false)
    expect(d.percent).toBe("–")
    expect(d.used).toBe("measured again after the next response")
    expect(d.remaining).toBe("")
    expect(d.ratio).toBe(0)
  })
  it("caps remainder and share on overflow", () => {
    const d = describeContext(ctx({ tokens: 1_100_000, percent: 110 }))
    expect(d.remaining).toBe("0 tokens free")
    expect(d.ratio).toBe(1)
    expect(d.level).toBe("danger")
  })
})

describe("cacheHitRate", () => {
  it("is cacheRead / (input + cacheRead)", () => {
    expect(cacheHitRate(100, 900)).toBeCloseTo(0.9)
  })
  it("returns undefined without input tokens", () => {
    expect(cacheHitRate(0, 0)).toBeUndefined()
    expect(cacheHitRate(undefined, undefined)).toBeUndefined()
  })
  it("counts missing cacheRead as 0", () => {
    expect(cacheHitRate(50, undefined)).toBe(0)
  })
})

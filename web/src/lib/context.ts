/** Preparation of the context usage and the cache hit rate. */
import type { ContextUsage } from "@/api/types"

export type ContextLevel = "neutral" | "warn" | "danger"

const intFmt = new Intl.NumberFormat("en-US", { maximumFractionDigits: 0 })

/** Colour level: below 60 % neutral, up to 85 % yellow, above red. */
export function contextLevel(percent: number | null | undefined): ContextLevel {
  if (percent === null || percent === undefined) return "neutral"
  if (percent > 85) return "danger"
  if (percent >= 60) return "warn"
  return "neutral"
}

/** Percentage (0–100) as an integer; small values above 0 as "< 1 %". */
export function formatPercent(percent: number | null | undefined): string {
  if (percent === null || percent === undefined || Number.isNaN(percent)) return "–"
  if (percent > 0 && percent < 1) return "< 1 %"
  return `${Math.round(percent)} %`
}

export type ContextView = {
  measured: boolean
  percent: string
  used: string
  remaining: string
  threshold: string
  level: ContextLevel
  /** Share 0–1 for ring or bar, capped at 1. */
  ratio: number
}

export function describeContext(c: ContextUsage): ContextView {
  const threshold = `${intFmt.format(c.threshold_tokens)} tokens`
  if (c.tokens === null || c.percent === null) {
    return {
      measured: false,
      percent: "–",
      used: "measured again after the next response",
      remaining: "",
      threshold,
      level: "neutral",
      ratio: 0,
    }
  }
  return {
    measured: true,
    percent: formatPercent(c.percent),
    used: `${intFmt.format(c.tokens)} / ${intFmt.format(c.window)} tokens`,
    remaining: `${intFmt.format(Math.max(0, c.window - c.tokens))} tokens free`,
    threshold,
    level: contextLevel(c.percent),
    ratio: Math.min(1, Math.max(0, c.percent / 100)),
  }
}

/** Cache hit rate cacheRead / (input + cacheRead); undefined without input. */
export function cacheHitRate(input: number | undefined, cacheRead: number | undefined): number | undefined {
  const total = (input ?? 0) + (cacheRead ?? 0)
  if (total <= 0) return undefined
  return (cacheRead ?? 0) / total
}

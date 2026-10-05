/** Aufbereitung der Kontextauslastung und der Cache-Trefferquote. */
import type { ContextUsage } from "@/api/types"

export type ContextLevel = "neutral" | "warn" | "danger"

const intFmt = new Intl.NumberFormat("de-DE", { maximumFractionDigits: 0 })

/** Farbstufe: unter 60 % neutral, bis 85 % gelb, darüber rot. */
export function contextLevel(percent: number | null | undefined): ContextLevel {
  if (percent === null || percent === undefined) return "neutral"
  if (percent > 85) return "danger"
  if (percent >= 60) return "warn"
  return "neutral"
}

/** Prozentwert (0–100) als ganze Zahl; kleine Werte über 0 als „< 1 %“. */
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
  /** Anteil 0–1 für Ring oder Balken, auf 1 begrenzt. */
  ratio: number
}

export function describeContext(c: ContextUsage): ContextView {
  const threshold = `${intFmt.format(c.threshold_tokens)} Tokens`
  if (c.tokens === null || c.percent === null) {
    return {
      measured: false,
      percent: "–",
      used: "wird nach der nächsten Antwort neu gemessen",
      remaining: "",
      threshold,
      level: "neutral",
      ratio: 0,
    }
  }
  return {
    measured: true,
    percent: formatPercent(c.percent),
    used: `${intFmt.format(c.tokens)} / ${intFmt.format(c.window)} Tokens`,
    remaining: `${intFmt.format(Math.max(0, c.window - c.tokens))} Tokens frei`,
    threshold,
    level: contextLevel(c.percent),
    ratio: Math.min(1, Math.max(0, c.percent / 100)),
  }
}

/** Cache-Trefferquote cacheRead / (input + cacheRead); undefined ohne Eingabe. */
export function cacheHitRate(input: number | undefined, cacheRead: number | undefined): number | undefined {
  const total = (input ?? 0) + (cacheRead ?? 0)
  if (total <= 0) return undefined
  return (cacheRead ?? 0) / total
}

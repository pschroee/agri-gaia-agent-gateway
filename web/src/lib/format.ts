import type { Activity, Model, Pricing, Usage } from "@/api/types"

export function formatActivity(a: Activity | undefined): string {
  if (!a) return "–"
  switch (a.kind) {
    case "thinking":
      return "Denkt"
    case "writing":
      return "Schreibt"
    case "tool":
      return a.tool ? `Führt ${a.tool} aus` : "Führt ein Werkzeug aus"
    case "preparing":
      return a.tool ? `Bereitet ${a.tool} vor` : "Bereitet einen Werkzeugaufruf vor"
    case "compacting":
      return "Fasst den Kontext zusammen"
    case "waiting_approval":
      return "Wartet auf Bestätigung"
    case "starting":
      return "Startet"
    case "idle":
      return "Wartet"
    default:
      return a.kind
  }
}

export function formatDuration(since: string | undefined, now: number = Date.now()): string {
  if (!since) return "–"
  const t = Date.parse(since)
  if (Number.isNaN(t)) return "–"
  const secs = Math.max(0, Math.floor((now - t) / 1000))
  if (secs < 60) return `${secs} s`
  const mins = Math.floor(secs / 60)
  if (mins < 60) return `${mins} min ${secs % 60} s`
  const hours = Math.floor(mins / 60)
  return `${hours} h ${mins % 60} min`
}

const usdFmt = new Intl.NumberFormat("de-DE", { minimumFractionDigits: 4, maximumFractionDigits: 4 })
const intFmt = new Intl.NumberFormat("de-DE", { maximumFractionDigits: 0 })
const oneDecimal = new Intl.NumberFormat("de-DE", { minimumFractionDigits: 1, maximumFractionDigits: 1 })
const priceFmt = new Intl.NumberFormat("de-DE", { minimumFractionDigits: 2, maximumFractionDigits: 4 })

export function formatUsd(v: number | undefined | null): string {
  if (v === undefined || v === null || Number.isNaN(v)) return "–"
  return `$${usdFmt.format(v)}`
}

/** Preis je 1 Mio. Tokens. */
export function formatPrice(v: number | undefined): string {
  if (v === undefined || Number.isNaN(v)) return "–"
  return `$${priceFmt.format(v)}`
}

export function formatTokens(v: number | undefined | null): string {
  return intFmt.format(v ?? 0)
}

export function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`
  const units = ["KiB", "MiB", "GiB", "TiB"]
  let v = n / 1024
  let u = 0
  while (v >= 1024 && u < units.length - 1) {
    v /= 1024
    u++
  }
  return `${oneDecimal.format(v)} ${units[u]}`
}

export function shortHash(h: string, len = 12): string {
  return h.length > len ? `${h.slice(0, len)}…` : h
}

export function formatTime(iso: string | undefined): string {
  if (!iso) return "–"
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return "–"
  return d.toLocaleString("de-DE", { dateStyle: "short", timeStyle: "medium" })
}

/**
 * Tokens und Kosten einer Assistenten-Nachricht. `cost` ist der Wert des Orchestrators nach Tarif
 * und hat Vorrang; `usage.cost.total` (pis Einheitspreis) dient nur als Rückfall. Mit
 * `provisional` (Antwort live, Tarifwert steht noch aus) wird der Rückfall als vorläufig markiert.
 */
export function formatUsage(u: Usage | undefined, cost?: number, opts?: { provisional?: boolean }): string {
  if (!u) return cost !== undefined ? formatUsd(cost) : ""
  const parts = [`${formatTokens(u.input)} ein`, `${formatTokens(u.output)} aus`]
  if (u.cacheRead) parts.push(`${formatTokens(u.cacheRead)} Cache`)
  if (cost !== undefined) parts.push(formatUsd(cost))
  else if (u.cost?.total) parts.push(opts?.provisional ? `≈ ${formatUsd(u.cost.total)} (vorläufig)` : formatUsd(u.cost.total))
  return parts.join(" · ")
}

/** Dauer in Millisekunden: unter 1 s in ms, unter 1 min mit einer Nachkommastelle. */
export function formatMs(ms: number | undefined | null): string {
  if (ms === undefined || ms === null || Number.isNaN(ms)) return "–"
  if (ms < 1000) return `${Math.round(ms)} ms`
  if (ms < 60_000) return `${oneDecimal.format(ms / 1000)} s`
  const secs = Math.round(ms / 1000)
  return `${Math.floor(secs / 60)} min ${secs % 60} s`
}

/** Kalenderdatum als TT.MM.JJJJ; nimmt das Datum so, wie es im ISO-Text steht (keine Zeitzonenverschiebung). */
export function formatDate(iso: string | undefined): string {
  const m = iso ? /^(\d{4})-(\d{2})-(\d{2})/.exec(iso) : null
  return m ? `${m[3]}.${m[2]}.${m[1]}` : "–"
}

const socketResults: Record<string, string> = {
  approved: "bestätigt",
  rejected: "abgelehnt",
  expired: "abgelaufen",
}

/** Übersetzt das Feld `result` eines Socket-Aufrufs; unbekannte Werte bleiben unverändert. */
export function socketResultLabel(result: string): string {
  return socketResults[result.trim().toLowerCase()] ?? result
}

const socketOps: Record<string, string> = {
  upload: "Upload",
  list: "Liste",
  get: "Abruf",
  ping: "Ping",
  agent_limit: "Grenze gleichzeitiger Agenten",
  subagent_limit: "Subagenten-Grenze überschritten – abgebrochen",
  extension_ui: "Rückfrage einer Extension abgelehnt",
}

/** Lesbarer Name der Socket-Operation; unbekannte Werte bleiben unverändert. */
export function socketOpLabel(op: string): string {
  return socketOps[op.trim().toLowerCase()] ?? op
}

/**
 * Link und Stand der Preisquelle. `source` ist laut API.md nur die URL, `retrieved` ein ISO-Datum;
 * die ältere Form „<URL>, Abruf TT.MM.JJJJ“ in `source` wird ebenfalls verstanden.
 * Nur http(s)-Adressen werden als Link übernommen.
 */
export function pricingSource(p: Pick<Pricing, "source" | "retrieved"> | undefined): {
  href?: string
  retrieved?: string
} {
  const out: { href?: string; retrieved?: string } = {}
  if (!p) return out
  const url = p.source ? /https?:\/\/[^\s,]+/.exec(p.source)?.[0] : undefined
  if (url) out.href = url
  if (p.retrieved) {
    const d = formatDate(p.retrieved)
    if (d !== "–") out.retrieved = d
  } else if (p.source) {
    const m = /Abruf\s+(\d{2}\.\d{2}\.\d{4})/.exec(p.source)
    if (m) out.retrieved = m[1]
  }
  return out
}

/** Quelle der Preise eines Modells: die der Preise, sonst die des Tarifs. */
export function modelPriceSource(m: Pick<Model, "pricing" | "tariff">): { href?: string; retrieved?: string } {
  const p = pricingSource(m.pricing)
  if (p.href) return p
  const t = pricingSource(m.tariff)
  if (t.href) return t
  return {}
}

/** Linktext „Quelle · Stand TT.MM.JJJJ“ (Teile entfallen, wenn sie fehlen). */
export function sourceLabel(src: { href?: string; retrieved?: string }): string {
  return [src.href ? "Quelle" : "", src.retrieved ? `Stand ${src.retrieved}` : ""].filter(Boolean).join(" · ")
}

/** Ob ein Thinking-Block sichtbaren Text hat (Leerraum und unsichtbare Zeichen zählen nicht). */
export function hasThinkingText(text: unknown): boolean {
  return typeof text === "string" && text.replace(/[\s\u200b-\u200d\u2060\ufeff]/g, "") !== ""
}

/** Hat der Nutzer die Antwort abgebrochen? pi meldet das teils als Fehler („This operation was aborted“). */
export function isUserAbort(m: { stopReason?: string; errorMessage?: string }): boolean {
  if (m.stopReason === "aborted") return true
  return m.stopReason === "error" && /\b(operation|request) was aborted\b/i.test(m.errorMessage ?? "")
}

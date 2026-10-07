import type { Activity, Model, Pricing, Usage } from "@/api/types"

export function formatActivity(a: Activity | undefined): string {
  if (!a) return "–"
  switch (a.kind) {
    case "thinking":
      return "Thinking"
    case "writing":
      return "Writing"
    case "tool":
      return a.tool ? `Running ${a.tool}` : "Running a tool"
    case "preparing":
      return a.tool ? `Preparing ${a.tool}` : "Preparing a tool call"
    case "compacting":
      return "Summarizing the context"
    case "waiting_approval":
      return "Waiting for approval"
    case "starting":
      return "Starting"
    case "idle":
      return "Waiting"
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

const usdFmt = new Intl.NumberFormat("en-US", { minimumFractionDigits: 4, maximumFractionDigits: 4 })
const intFmt = new Intl.NumberFormat("en-US", { maximumFractionDigits: 0 })
const oneDecimal = new Intl.NumberFormat("en-US", { minimumFractionDigits: 1, maximumFractionDigits: 1 })
const priceFmt = new Intl.NumberFormat("en-US", { minimumFractionDigits: 2, maximumFractionDigits: 4 })

export function formatUsd(v: number | undefined | null): string {
  if (v === undefined || v === null || Number.isNaN(v)) return "–"
  return `$${usdFmt.format(v)}`
}

/** Price per 1M tokens. */
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
  return d.toLocaleString("en-US", { dateStyle: "short", timeStyle: "medium", hourCycle: "h23" })
}

/**
 * Tokens and cost of an assistant message. `cost` is the orchestrator's value by tariff
 * and takes precedence; `usage.cost.total` (pi's flat price) only serves as a fallback. With
 * `provisional` (response live, tariff value still pending) the fallback is marked as provisional.
 */
export function formatUsage(u: Usage | undefined, cost?: number, opts?: { provisional?: boolean }): string {
  if (!u) return cost !== undefined ? formatUsd(cost) : ""
  const parts = [`${formatTokens(u.input)} in`, `${formatTokens(u.output)} out`]
  if (u.cacheRead) parts.push(`${formatTokens(u.cacheRead)} cache`)
  if (cost !== undefined) parts.push(formatUsd(cost))
  else if (u.cost?.total) parts.push(opts?.provisional ? `≈ ${formatUsd(u.cost.total)} (provisional)` : formatUsd(u.cost.total))
  return parts.join(" · ")
}

/** Duration in milliseconds: below 1 s in ms, below 1 min with one decimal place. */
export function formatMs(ms: number | undefined | null): string {
  if (ms === undefined || ms === null || Number.isNaN(ms)) return "–"
  if (ms < 1000) return `${Math.round(ms)} ms`
  if (ms < 60_000) return `${oneDecimal.format(ms / 1000)} s`
  const secs = Math.round(ms / 1000)
  return `${Math.floor(secs / 60)} min ${secs % 60} s`
}

/** Calendar date as YYYY-MM-DD; takes the date as it stands in the ISO text (no time zone shift). */
export function formatDate(iso: string | undefined): string {
  const m = iso ? /^(\d{4})-(\d{2})-(\d{2})/.exec(iso) : null
  return m ? `${m[1]}-${m[2]}-${m[3]}` : "–"
}

const socketResults: Record<string, string> = {
  approved: "approved",
  rejected: "rejected",
  expired: "expired",
}

/** Translates the `result` field of a socket call; unknown values stay unchanged. */
export function socketResultLabel(result: string): string {
  return socketResults[result.trim().toLowerCase()] ?? result
}

const socketOps: Record<string, string> = {
  upload: "Upload",
  list: "List",
  get: "Fetch",
  ping: "Ping",
  internet: "Internet request",
  internet_off: "Internet off",
  internet_set: "Internet switch by the user",
  agent_limit: "Limit of concurrent agents",
  subagent_limit: "Subagent limit exceeded – aborted",
  extension_ui: "Extension prompt declined",
}

/** Readable name of the socket operation; unknown values stay unchanged. */
export function socketOpLabel(op: string): string {
  return socketOps[op.trim().toLowerCase()] ?? op
}

/**
 * Link and date of the price source. According to API.md `source` is just the URL, `retrieved` an ISO date;
 * the older German form "<URL>, Abruf DD.MM.YYYY" in `source` is understood as well.
 * Only http(s) addresses are taken over as a link.
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
    const m = /Abruf\s+(\d{2})\.(\d{2})\.(\d{4})/.exec(p.source)
    if (m) out.retrieved = `${m[3]}-${m[2]}-${m[1]}`
  }
  return out
}

/** Price source of a model: that of the prices, otherwise that of the tariff. */
export function modelPriceSource(m: Pick<Model, "pricing" | "tariff">): { href?: string; retrieved?: string } {
  const p = pricingSource(m.pricing)
  if (p.href) return p
  const t = pricingSource(m.tariff)
  if (t.href) return t
  return {}
}

/** Link text "Source · as of YYYY-MM-DD" (parts are left out when missing). */
export function sourceLabel(src: { href?: string; retrieved?: string }): string {
  return [src.href ? "Source" : "", src.retrieved ? `as of ${src.retrieved}` : ""].filter(Boolean).join(" · ")
}

/** Whether a thinking block has visible text (whitespace and invisible characters do not count). */
export function hasThinkingText(text: unknown): boolean {
  return typeof text === "string" && text.replace(/[\s\u200b-\u200d\u2060\ufeff]/g, "") !== ""
}

/** Did the user abort the response? pi sometimes reports this as an error ("This operation was aborted"). */
export function isUserAbort(m: { stopReason?: string; errorMessage?: string }): boolean {
  if (m.stopReason === "aborted") return true
  return m.stopReason === "error" && /\b(operation|request) was aborted\b/i.test(m.errorMessage ?? "")
}

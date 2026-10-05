/** Ganze Zahl in [min, max]; Unlesbares ergibt min. */
export function clampInt(v: number | string, min: number, max: number): number {
  const n = typeof v === "string" ? (v.trim() === "" ? Number.NaN : Number(v)) : v
  if (!Number.isFinite(n)) return min
  return Math.min(max, Math.max(min, Math.round(n)))
}

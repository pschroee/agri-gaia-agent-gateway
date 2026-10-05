import type { ServerEvent } from "@/api/types"

/** Liest eine SSE-`data:`-Zeile des Orchestrators; liefert null bei Unlesbarem. */
export function parseServerEvent(raw: string): ServerEvent | null {
  try {
    const v = JSON.parse(raw) as unknown
    if (typeof v !== "object" || v === null) return null
    const o = v as { kind?: unknown; data?: unknown }
    if (typeof o.kind !== "string") return null
    return { kind: o.kind, data: o.data } as ServerEvent
  } catch {
    return null
  }
}

/** Ersetzt den Eintrag mit gleichem Schlüssel oder hängt ihn an. */
export function upsert<T, K>(list: T[], item: T, key: (x: T) => K): T[] {
  const k = key(item)
  const idx = list.findIndex((x) => key(x) === k)
  if (idx < 0) return [...list, item]
  const next = list.slice()
  next[idx] = item
  return next
}

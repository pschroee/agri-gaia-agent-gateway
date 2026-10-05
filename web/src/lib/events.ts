import type { ServerEvent } from "@/api/types"

/** Reads an SSE `data:` line of the orchestrator; returns null if unreadable. */
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

/** Replaces the entry with the same key or appends it. */
export function upsert<T, K>(list: T[], item: T, key: (x: T) => K): T[] {
  const k = key(item)
  const idx = list.findIndex((x) => key(x) === k)
  if (idx < 0) return [...list, item]
  const next = list.slice()
  next[idx] = item
  return next
}

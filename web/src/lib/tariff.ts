/** Show a tariff's peak hours (UTC) in local time. */

import type { PeakWindow } from "@/api/types"

const dayKeys = ["mon", "tue", "wed", "thu", "fri", "sat", "sun"]
const dayNames = ["Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"]

/** Day spec like "mon-fri", "sat", "mon,wed" or "daily" as a set of weekdays (0 = Monday). */
function parseDays(days: string): number[] | undefined {
  const d = days.trim().toLowerCase()
  if (d === "daily" || d === "all" || d === "*" || d === "mon-sun") return [0, 1, 2, 3, 4, 5, 6]
  const out = new Set<number>()
  for (const part of d.split(",").map((p) => p.trim())) {
    const range = part.split("-").map((p) => dayKeys.indexOf(p.slice(0, 3)))
    if (range.some((i) => i < 0) || range.length > 2 || part === "") return undefined
    const [a, b = a] = range
    for (let i = a; ; i = (i + 1) % 7) {
      out.add(i)
      if (i === b) break
    }
  }
  return [...out].sort((x, y) => x - y)
}

function formatDays(set: number[]): string {
  if (set.length === 7) return "daily"
  if (set.length === 1) return dayNames[set[0]]
  // contiguous range, also across the week boundary (Sun–Thu)
  const inSet = new Set(set)
  const start = set.find((d) => !inSet.has((d + 6) % 7))
  if (start !== undefined) {
    let len = 0
    while (inSet.has((start + len) % 7)) len++
    if (len === set.length && len >= 2) return `${dayNames[start]}–${dayNames[(start + len - 1) % 7]}`
  }
  return set.map((d) => dayNames[d]).join(", ")
}

/** Offset of the time zone from UTC in minutes at time `at`. */
function tzOffsetMinutes(timeZone: string, at: Date): number {
  const parts = new Intl.DateTimeFormat("en-US", {
    timeZone,
    hourCycle: "h23",
    year: "numeric",
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
  }).formatToParts(at)
  const get = (t: string) => Number(parts.find((p) => p.type === t)?.value)
  const asUtc = Date.UTC(get("year"), get("month") - 1, get("day"), get("hour"), get("minute"))
  return Math.round((asUtc - Math.floor(at.getTime() / 60000) * 60000) / 60000)
}

const toMin = (hhmm: string) => {
  const [h, m] = hhmm.split(":").map(Number)
  return h * 60 + (m || 0)
}
const fmtMin = (min: number) => {
  const m = ((min % 1440) + 1440) % 1440
  return `${String(Math.floor(m / 60)).padStart(2, "0")}:${String(m % 60).padStart(2, "0")}`
}

/**
 * Peak hours in local time, e.g. "Mon–Fri 03:00–06:00 and 08:00–12:00". The time zone offset
 * applies at time `at` (summer/winter time). If a window starts on a different day in local
 * time, the weekdays shift along.
 */
export function formatPeakWindows(windows: PeakWindow[], timeZone = "Europe/Berlin", at: Date = new Date()): string {
  const offset = tzOffsetMinutes(timeZone, at)
  const groups = new Map<string, string[]>()
  for (const w of windows) {
    const from = toMin(w.from) + offset
    const to = toMin(w.to) + offset
    const shift = Math.floor(from / 1440)
    const days = parseDays(w.days)
    const label = days ? formatDays(days.map((d) => (((d + shift) % 7) + 7) % 7).sort((a, b) => a - b)) : w.days
    const range = `${fmtMin(from)}–${fmtMin(to)}`
    groups.set(label, [...(groups.get(label) ?? []), range])
  }
  return [...groups].map(([days, ranges]) => `${days} ${ranges.join(" and ")}`).join("; ")
}

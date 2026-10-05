import { useEffect, useState } from "react"

/**
 * Current time in ms, updated every `intervalMs` – for running durations. With `active = false` the
 * timer stands still (e.g. while nothing is running); when switched on again it catches up immediately.
 */
export function useNow(intervalMs: number, active = true): number {
  const [now, setNow] = useState(() => Date.now())
  useEffect(() => {
    if (!active) return
    const tick = () => setNow(Date.now())
    const first = setTimeout(tick, 0)
    const t = setInterval(tick, intervalMs)
    return () => {
      clearTimeout(first)
      clearInterval(t)
    }
  }, [intervalMs, active])
  return now
}

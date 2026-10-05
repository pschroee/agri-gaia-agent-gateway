import { useEffect, useState } from "react"

/**
 * Aktuelle Zeit in ms, alle `intervalMs` aktualisiert – für laufende Dauern. Mit `active = false` steht der
 * Zeitgeber still (etwa, solange nichts läuft); beim Wiedereinschalten holt er die Zeit sofort nach.
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

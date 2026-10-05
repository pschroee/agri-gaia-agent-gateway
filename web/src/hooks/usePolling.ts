import { useCallback, useEffect, useRef, useState } from "react"

/** Polls `fn` every `intervalMs`; errors are reported, the last data stays. */
export function usePolling<T>(fn: () => Promise<T>, intervalMs: number) {
  const [data, setData] = useState<T | undefined>(undefined)
  const [error, setError] = useState<string | undefined>(undefined)
  const fnRef = useRef(fn)
  useEffect(() => {
    fnRef.current = fn
  })

  const reload = useCallback(async () => {
    try {
      const v = await fnRef.current()
      setData(v)
      setError(undefined)
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    }
  }, [])

  useEffect(() => {
    let stopped = false
    let timer: ReturnType<typeof setTimeout> | undefined
    const tick = async () => {
      await reload()
      if (!stopped) timer = setTimeout(tick, intervalMs)
    }
    void tick()
    return () => {
      stopped = true
      if (timer) clearTimeout(timer)
    }
  }, [reload, intervalMs])

  return { data, error, reload }
}

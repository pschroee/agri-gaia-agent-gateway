import { useCallback, useEffect, useState } from "react"
import { api } from "@/api/client"
import type { Config, Model, Variant } from "@/api/types"

export type Meta = { models: Model[]; variants: Variant[]; config?: Config; error?: string }

/** Modelle, Varianten und Voreinstellungen – ändern sich zur Laufzeit nicht. */
export function useMeta() {
  const [meta, setMeta] = useState<Meta>({ models: [], variants: [] })
  const reload = useCallback(async () => {
    try {
      const [models, variants, config] = await Promise.all([api.models(), api.variants(), api.config()])
      setMeta({ models, variants, config })
    } catch (e) {
      setMeta((m) => ({ ...m, error: e instanceof Error ? e.message : String(e) }))
    }
  }, [])
  useEffect(() => {
    let cancelled = false
    Promise.all([api.models(), api.variants(), api.config()]).then(
      ([models, variants, config]) => !cancelled && setMeta({ models, variants, config }),
      (e: unknown) => !cancelled && setMeta((m) => ({ ...m, error: e instanceof Error ? e.message : String(e) })),
    )
    return () => {
      cancelled = true
    }
  }, [])
  return { ...meta, reload }
}

export const modelName = (models: Model[], id: string) => models.find((m) => m.id === id)?.name ?? id

import { useState } from "react"
import { toast } from "sonner"
import { api, type ContextTooLarge } from "@/api/client"
import type { Chat } from "@/api/types"
import { effortLabels } from "@/lib/commands"
import { contextTooLarge } from "@/lib/modelswitch"

const errText = (e: unknown) => (e instanceof Error ? e.message : String(e))

/**
 * Modell- und Denkstufenwechsel samt Rückfrage, wenn der Kontext nicht ins neue Modell passt:
 * Dann fragt ein Dialog, ob erst kompaktiert werden soll; der Orchestrator wechselt danach selbst.
 */
export function useModelSwitch(chat: Chat, onChat: (c: Chat) => void, modelName: (id: string) => string) {
  const [tooLarge, setTooLarge] = useState<ContextTooLarge>()
  const [busy, setBusy] = useState(false)

  const setModel = async (model: string) => {
    if (model === chat.model) return
    setBusy(true)
    try {
      onChat(await api.setModel(chat.id, model))
      toast.success(`Modell: ${modelName(model)}`)
    } catch (e) {
      const d = contextTooLarge(e)
      if (d) setTooLarge(d)
      else toast.error(`Modellwechsel fehlgeschlagen: ${errText(e)}`)
    } finally {
      setBusy(false)
    }
  }

  const setEffort = async (level: string) => {
    if (level === chat.thinking_level) return
    setBusy(true)
    try {
      onChat(await api.setEffort(chat.id, level))
      toast.success(`Denkstufe: ${effortLabels[level] ?? level}`)
    } catch (e) {
      toast.error(`Denkstufe nicht gesetzt: ${errText(e)}`)
    } finally {
      setBusy(false)
    }
  }

  const compactAndSwitch = async () => {
    if (!tooLarge) return
    const model = tooLarge.model
    setBusy(true)
    try {
      onChat(await api.setModel(chat.id, model, true))
      toast.success(`Wird kompaktiert; danach geht es mit ${modelName(model)} weiter.`)
      setTooLarge(undefined)
    } catch (e) {
      toast.error(`Kompaktieren fehlgeschlagen: ${errText(e)}`)
    } finally {
      setBusy(false)
    }
  }

  return { setModel, setEffort, showTooLarge: setTooLarge, tooLarge, compactAndSwitch, cancel: () => setTooLarge(undefined), busy }
}

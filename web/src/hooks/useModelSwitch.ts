import { useState } from "react"
import { toast } from "sonner"
import { api, type ContextTooLarge } from "@/api/client"
import type { Chat } from "@/api/types"
import { effortLabels } from "@/lib/commands"
import { contextTooLarge } from "@/lib/modelswitch"

const errText = (e: unknown) => (e instanceof Error ? e.message : String(e))

/**
 * Model and thinking level switch, including a prompt if the context does not fit into the new model:
 * then a dialog asks whether to compact first; the orchestrator switches by itself afterwards.
 */
export function useModelSwitch(chat: Chat, onChat: (c: Chat) => void, modelName: (id: string) => string) {
  const [tooLarge, setTooLarge] = useState<ContextTooLarge>()
  const [busy, setBusy] = useState(false)

  const setModel = async (model: string) => {
    if (model === chat.model) return
    setBusy(true)
    try {
      onChat(await api.setModel(chat.id, model))
      toast.success(`Model: ${modelName(model)}`)
    } catch (e) {
      const d = contextTooLarge(e)
      if (d) setTooLarge(d)
      else toast.error(`Model switch failed: ${errText(e)}`)
    } finally {
      setBusy(false)
    }
  }

  const setEffort = async (level: string) => {
    if (level === chat.thinking_level) return
    setBusy(true)
    try {
      onChat(await api.setEffort(chat.id, level))
      toast.success(`Thinking level: ${effortLabels[level] ?? level}`)
    } catch (e) {
      toast.error(`Thinking level not set: ${errText(e)}`)
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
      toast.success(`Compacting; afterwards it continues with ${modelName(model)}.`)
      setTooLarge(undefined)
    } catch (e) {
      toast.error(`Compaction failed: ${errText(e)}`)
    } finally {
      setBusy(false)
    }
  }

  return { setModel, setEffort, showTooLarge: setTooLarge, tooLarge, compactAndSwitch, cancel: () => setTooLarge(undefined), busy }
}

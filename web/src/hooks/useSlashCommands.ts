import { useState } from "react"
import type { Command, CommandOption } from "@/api/types"
import { filterCommands, filterOptions, slashArg, slashQuery } from "@/lib/commands"

/** Ein Eintrag im Popover: ein Befehl oder ein Argument eines Befehls (/model, /effort). */
export type SlashItem = { command: Command; option?: CommandOption }

export type SlashState = {
  open: boolean
  items: SlashItem[]
  /** Gesetzt, wenn Argumente eines Befehls vorgeschlagen werden. */
  argsOf?: Command
  active: number
  setActive: (i: number) => void
  pick: (item: SlashItem) => void
  close: () => void
  /** Tastenbehandlung im Eingabefeld; true, wenn die Taste verbraucht wurde. */
  onKeyDown: (e: React.KeyboardEvent) => boolean
}

/** Einträge fürs Popover: bei „/name“ die Befehle, bei „/name arg“ die Argumente des Befehls.
 * Ist das Argument schon vollständig (ein Wert passt genau), gibt es nichts mehr vorzuschlagen,
 * damit Enter sendet. */
export function slashItems(commands: Command[], text: string): { items: SlashItem[]; argsOf?: Command } {
  const query = slashQuery(text)
  if (query !== undefined) return { items: filterCommands(commands, query).map((command) => ({ command })) }
  const arg = slashArg(text)
  const command = arg && commands.find((c) => c.name.toLowerCase() === arg.name && c.options?.length)
  if (!arg || !command?.options) return { items: [] }
  if (command.options.some((o) => o.value === arg.query)) return { items: [] }
  return { items: filterOptions(command.options, arg.query).map((option) => ({ command, option })), argsOf: command }
}

/** Zustand des Befehls-Popovers: öffnet bei „/“ am Anfang, filtert beim Tippen. */
export function useSlashCommands(commands: Command[], text: string, setText: (t: string) => void): SlashState {
  const [active, setActive] = useState(0)
  const [dismissed, setDismissed] = useState<string>()
  const { items, argsOf } = slashItems(commands, text)
  const open = items.length > 0 && dismissed !== text
  const idx = Math.min(active, Math.max(0, items.length - 1))

  const pick = (item: SlashItem) => {
    setText(item.option ? `/${item.command.name} ${item.option.value}` : `/${item.command.name} `)
    setActive(0)
  }

  const onKeyDown = (e: React.KeyboardEvent) => {
    if (!open || e.nativeEvent.isComposing) return false
    switch (e.key) {
      case "ArrowDown":
        setActive((idx + 1) % items.length)
        break
      case "ArrowUp":
        setActive((idx - 1 + items.length) % items.length)
        break
      case "Enter":
      case "Tab":
        if (e.shiftKey) return false
        pick(items[idx])
        break
      case "Escape":
        setDismissed(text)
        break
      default:
        if (active !== 0 && e.key.length === 1) setActive(0)
        return false
    }
    e.preventDefault()
    return true
  }

  return { open, items, argsOf, active: idx, setActive, pick, close: () => setDismissed(text), onKeyDown }
}

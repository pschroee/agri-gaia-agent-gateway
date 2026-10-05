/** Slash commands in the input field. */
import type { Chat, Command, CommandOption } from "@/api/types"

/** Search text for the command popover while only "/name" without spaces has been typed. */
export function slashQuery(text: string): string | undefined {
  const m = /^\/(\S*)$/.exec(text)
  return m ? m[1].toLowerCase() : undefined
}

/** Filters commands: prefix matches in the name, then partial matches in the name, then (from 3 characters) in the description. */
// Commands that only take effect in pi's terminal UI (output via notify/widget) and show nothing in the web UI.
const TERMINAL_ONLY = new Set(["todos"])

export function filterCommands(all: Command[], query: string): Command[] {
  const list = all.filter((c) => !TERMINAL_ONLY.has(c.name))
  const q = query.trim().toLowerCase()
  if (!q) return list
  const prefix: Command[] = []
  const infix: Command[] = []
  const desc: Command[] = []
  for (const c of list) {
    const name = c.name.toLowerCase()
    if (name.startsWith(q)) prefix.push(c)
    else if (name.includes(q)) infix.push(c)
    else if (q.length >= 3 && c.description?.toLowerCase().includes(q)) desc.push(c)
  }
  return [...prefix, ...infix, ...desc]
}

/** An input that goes to /commands as a slash command instead of as a message. */
export function isSlashCommand(text: string): boolean {
  return /^\/[\p{L}\p{N}]/u.test(text.trim())
}

/** Switch value of "/autocompact on|off" (like the orchestrator: on/an/ein/true, off/aus/false; the German words are accepted too); otherwise undefined. */
export function autoCompactSwitch(text: string): boolean | undefined {
  const m = /^\/autocompact\s+(\S+)$/i.exec(text.trim())
  if (!m) return undefined
  const v = m[1].toLowerCase()
  if (["on", "an", "ein", "true"].includes(v)) return true
  if (["off", "aus", "false"].includes(v)) return false
  return undefined
}

/** Built-in commands of the orchestrator (/compact, /autocompact, /rename); they create no user message. */
export function isBuiltinCommand(text: string): boolean {
  return /^\/(compact|autocompact|rename|model|effort)(\s|$)/i.test(text.trim())
}

/** Command and started argument while "/name arg" with exactly one argument word is typed. */
export function slashArg(text: string): { name: string; query: string } | undefined {
  const m = /^\/(\S+)\s+(\S*)$/.exec(text)
  return m ? { name: m[1].toLowerCase(), query: m[2] } : undefined
}

/** Filters arguments: prefix matches in the value, then partial matches in value or label. */
export function filterOptions(options: CommandOption[], query: string): CommandOption[] {
  const q = query.toLowerCase()
  if (!q) return options
  const prefix = options.filter((o) => o.value.toLowerCase().startsWith(q))
  const rest = options.filter(
    (o) => !prefix.includes(o) && (o.value.toLowerCase().includes(q) || o.label?.toLowerCase().includes(q)),
  )
  return [...prefix, ...rest]
}

export const effortLabels: Record<string, string> = {
  off: "off",
  minimal: "minimal",
  low: "low",
  medium: "medium",
  high: "high",
  xhigh: "very high",
  max: "maximum",
}

/** Puts the chat's current values into the suggestions of /model and /effort (the command list
 * is loaded less often than the chat); /effort gets the levels pi reports for the model. */
export function withLiveOptions(commands: Command[], chat: Pick<Chat, "model" | "thinking_level" | "thinking_levels">): Command[] {
  return commands.map((c) => {
    if (c.name === "model" && c.options) {
      return { ...c, options: c.options.map((o) => ({ ...o, current: o.value === chat.model })) }
    }
    if (c.name === "effort") {
      const levels = chat.thinking_levels?.length ? chat.thinking_levels : c.options?.map((o) => o.value)
      if (!levels) return c
      return {
        ...c,
        options: levels.map((l) => ({ value: l, label: effortLabels[l] ?? l, current: l === chat.thinking_level })),
      }
    }
    return c
  })
}

/** New name from "/rename Name" (whitespace collapsed); otherwise undefined. */
export function renameTitle(text: string): string | undefined {
  const m = /^\/rename\s+(.+)$/is.exec(text.trim())
  return m ? m[1].split(/\s+/).join(" ") : undefined
}

/** Slash-Befehle im Eingabefeld. */
import type { Chat, Command, CommandOption } from "@/api/types"

/** Suchtext für das Befehls-Popover, solange nur „/name“ ohne Leerzeichen eingegeben ist. */
export function slashQuery(text: string): string | undefined {
  const m = /^\/(\S*)$/.exec(text)
  return m ? m[1].toLowerCase() : undefined
}

/** Filtert Befehle: Präfixtreffer im Namen, dann Teiltreffer im Namen, dann (ab 3 Zeichen) in der Beschreibung. */
// Befehle, die nur in pis Terminal-Oberfläche wirken (Ausgabe per notify/Widget) und in der Web-UI nichts zeigen.
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

/** Eine Eingabe, die als Slash-Befehl an /commands geht statt als Nachricht. */
export function isSlashCommand(text: string): boolean {
  return /^\/[\p{L}\p{N}]/u.test(text.trim())
}

/** Schaltwert von „/autocompact on|off“ (wie der Orchestrator: on/an/ein/true, off/aus/false); sonst undefined. */
export function autoCompactSwitch(text: string): boolean | undefined {
  const m = /^\/autocompact\s+(\S+)$/i.exec(text.trim())
  if (!m) return undefined
  const v = m[1].toLowerCase()
  if (["on", "an", "ein", "true"].includes(v)) return true
  if (["off", "aus", "false"].includes(v)) return false
  return undefined
}

/** Eingebaute Befehle des Orchestrators (/compact, /autocompact, /rename); sie erzeugen keine Nutzernachricht. */
export function isBuiltinCommand(text: string): boolean {
  return /^\/(compact|autocompact|rename|model|effort)(\s|$)/i.test(text.trim())
}

/** Befehl und angefangenes Argument, solange „/name arg“ mit genau einem Argumentwort getippt ist. */
export function slashArg(text: string): { name: string; query: string } | undefined {
  const m = /^\/(\S+)\s+(\S*)$/.exec(text)
  return m ? { name: m[1].toLowerCase(), query: m[2] } : undefined
}

/** Filtert Argumente: Präfixtreffer im Wert, dann Teiltreffer in Wert oder Beschriftung. */
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
  off: "aus",
  minimal: "minimal",
  low: "niedrig",
  medium: "mittel",
  high: "hoch",
  xhigh: "sehr hoch",
  max: "maximal",
}

/** Setzt die aktuellen Werte des Chats in die Vorschläge von /model und /effort (die Befehlsliste
 * wird seltener geladen als der Chat); /effort bekommt die Stufen, die pi für das Modell meldet. */
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

/** Neuer Name aus „/rename Name“ (Leerraum zusammengefasst); sonst undefined. */
export function renameTitle(text: string): string | undefined {
  const m = /^\/rename\s+(.+)$/is.exec(text.trim())
  return m ? m[1].split(/\s+/).join(" ") : undefined
}

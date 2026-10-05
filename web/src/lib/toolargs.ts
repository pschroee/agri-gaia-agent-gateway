/** Aufbereitung der Werkzeugargumente für die Werkzeugkarten. */

export type ArgSection = { label: string; kind: "code" | "path" | "json"; text: string }
export type ToolArgsView = { summary: string; sections: ArgSection[] }

type Obj = Record<string, unknown>

const isObj = (v: unknown): v is Obj => typeof v === "object" && v !== null && !Array.isArray(v)

/** Erste nichtleere Zeile; bei mehrzeiligem Text mit „…" markiert. */
export function firstLine(text: string): string {
  const lines = text.split("\n")
  const idx = lines.findIndex((l) => l.trim() !== "")
  if (idx < 0) return ""
  const line = lines[idx].trim()
  const more = lines.slice(idx + 1).some((l) => l.trim() !== "")
  return more ? `${line} …` : line
}

function toJson(v: unknown): string {
  if (v === undefined) return ""
  try {
    return JSON.stringify(v, null, 2) ?? String(v)
  } catch {
    return String(v)
  }
}

/** Argumente ohne Werte (null/undefined) tragen nichts bei und werden nicht angezeigt. */
function withoutEmpty(o: Obj): Obj {
  return Object.fromEntries(Object.entries(o).filter(([, v]) => v !== null && v !== undefined))
}

function without(o: Obj, keys: string[]): Obj | undefined {
  const rest = Object.fromEntries(Object.entries(withoutEmpty(o)).filter(([k]) => !keys.includes(k)))
  return Object.keys(rest).length > 0 ? rest : undefined
}

function restSection(o: Obj, used: string[]): ArgSection[] {
  const rest = without(o, used)
  return rest ? [{ label: "Weitere Argumente", kind: "json", text: toJson(rest) }] : []
}

const summaryKeys = ["command", "path", "file_path", "name", "query", "url"]

function genericSummary(o: Obj): string {
  for (const k of summaryKeys) {
    const v = o[k]
    if (typeof v === "string") return firstLine(v)
  }
  return ""
}

function editSections(o: Obj): ArgSection[] | undefined {
  const pair = (e: unknown): e is { oldText: string; newText: string } =>
    isObj(e) && typeof e.oldText === "string" && typeof e.newText === "string"
  let edits: unknown = o.edits
  if (typeof edits === "string") {
    try {
      edits = JSON.parse(edits)
    } catch {
      return undefined
    }
  }
  if (pair(edits)) edits = [edits]
  if (Array.isArray(edits) && edits.every(pair)) {
    const sections: ArgSection[] = []
    edits.forEach((e, i) => {
      const n = edits.length === 1 && !Array.isArray(o.edits) ? "" : ` ${i + 1}`
      sections.push({ label: `Ersetzung${n}: alt`, kind: "code", text: e.oldText })
      sections.push({ label: `Ersetzung${n}: neu`, kind: "code", text: e.newText })
    })
    return [...sections, ...restSection(o, ["path", "edits"])]
  }
  if (pair(o)) {
    return [
      { label: "Ersetzung: alt", kind: "code", text: o.oldText },
      { label: "Ersetzung: neu", kind: "code", text: o.newText },
      ...restSection(o, ["path", "oldText", "newText"]),
    ]
  }
  return undefined
}

/**
 * Zerlegt die Argumente eines Werkzeugaufrufs in lesbare Abschnitte.
 * `rawText` ist das Roh-JSON, solange die Argumente noch gestreamt werden.
 */
export function describeToolArgs(name: string, args: unknown, rawText?: string): ToolArgsView {
  if (!isObj(args)) {
    const text = args === undefined ? (rawText ?? "") : toJson(args)
    return { summary: "", sections: [{ label: "Argumente", kind: "json", text }] }
  }

  const tool = name.toLowerCase()
  if (tool === "bash" && typeof args.command === "string") {
    return {
      summary: firstLine(args.command),
      sections: [{ label: "Befehl", kind: "code", text: args.command }, ...restSection(args, ["command"])],
    }
  }
  if ((tool === "write" || tool === "edit") && typeof args.path === "string") {
    const pathSection: ArgSection = { label: "Pfad", kind: "path", text: args.path }
    if (tool === "write" && typeof args.content === "string") {
      return {
        summary: firstLine(args.path),
        sections: [pathSection, { label: "Inhalt", kind: "code", text: args.content }, ...restSection(args, ["path", "content"])],
      }
    }
    const edits = tool === "edit" ? editSections(args) : undefined
    if (edits) return { summary: firstLine(args.path), sections: [pathSection, ...edits] }
  }

  return { summary: genericSummary(args), sections: [{ label: "Argumente", kind: "json", text: toJson(withoutEmpty(args)) }] }
}

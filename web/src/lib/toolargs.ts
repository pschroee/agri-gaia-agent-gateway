/** Preparation of the tool arguments for the tool cards. */

export type ArgSection = { label: string; kind: "code" | "path" | "json"; text: string }
export type ToolArgsView = { summary: string; sections: ArgSection[] }

type Obj = Record<string, unknown>

const isObj = (v: unknown): v is Obj => typeof v === "object" && v !== null && !Array.isArray(v)

/** First non-empty line; marked with "…" for multi-line text. */
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

/** Arguments without values (null/undefined) add nothing and are not shown. */
function withoutEmpty(o: Obj): Obj {
  return Object.fromEntries(Object.entries(o).filter(([, v]) => v !== null && v !== undefined))
}

function without(o: Obj, keys: string[]): Obj | undefined {
  const rest = Object.fromEntries(Object.entries(withoutEmpty(o)).filter(([k]) => !keys.includes(k)))
  return Object.keys(rest).length > 0 ? rest : undefined
}

function restSection(o: Obj, used: string[]): ArgSection[] {
  const rest = without(o, used)
  return rest ? [{ label: "Further arguments", kind: "json", text: toJson(rest) }] : []
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
      sections.push({ label: `Replacement${n}: old`, kind: "code", text: e.oldText })
      sections.push({ label: `Replacement${n}: new`, kind: "code", text: e.newText })
    })
    return [...sections, ...restSection(o, ["path", "edits"])]
  }
  if (pair(o)) {
    return [
      { label: "Replacement: old", kind: "code", text: o.oldText },
      { label: "Replacement: new", kind: "code", text: o.newText },
      ...restSection(o, ["path", "oldText", "newText"]),
    ]
  }
  return undefined
}

/**
 * Splits the arguments of a tool call into readable sections.
 * `rawText` is the raw JSON while the arguments are still being streamed.
 */
export function describeToolArgs(name: string, args: unknown, rawText?: string): ToolArgsView {
  if (!isObj(args)) {
    const text = args === undefined ? (rawText ?? "") : toJson(args)
    return { summary: "", sections: [{ label: "Arguments", kind: "json", text }] }
  }

  const tool = name.toLowerCase()
  if (tool === "bash" && typeof args.command === "string") {
    return {
      summary: firstLine(args.command),
      sections: [{ label: "Command", kind: "code", text: args.command }, ...restSection(args, ["command"])],
    }
  }
  if ((tool === "write" || tool === "edit") && typeof args.path === "string") {
    const pathSection: ArgSection = { label: "Path", kind: "path", text: args.path }
    if (tool === "write" && typeof args.content === "string") {
      return {
        summary: firstLine(args.path),
        sections: [pathSection, { label: "Content", kind: "code", text: args.content }, ...restSection(args, ["path", "content"])],
      }
    }
    const edits = tool === "edit" ? editSections(args) : undefined
    if (edits) return { summary: firstLine(args.path), sections: [pathSection, ...edits] }
  }

  return { summary: genericSummary(args), sections: [{ label: "Arguments", kind: "json", text: toJson(withoutEmpty(args)) }] }
}

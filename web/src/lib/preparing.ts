// Display of a tool call the model is still writing (toolcall_delta):
// the arguments are then half-finished JSON. Only what is reliably recognisable is read.

export type PreparingInfo = {
  bytes: number
  lines: number
  path?: string
  /** End of the content being written (for write/edit the text, for bash the command). */
  preview: string
}

const MAX_PREVIEW = 1200

// Main field per tool whose content is shown as a preview.
const MAIN_FIELD: Record<string, string> = { write: "content", bash: "command", edit: "newText" }

/** Reads a (possibly incomplete) JSON string from `start` (after the opening quote). */
function readPartialString(src: string, start: number): { value: string; complete: boolean } {
  let out = ""
  for (let i = start; i < src.length; i++) {
    const c = src[i]
    if (c === '"') return { value: out, complete: true }
    if (c !== "\\") {
      out += c
      continue
    }
    const n = src[i + 1]
    if (n === undefined) break // escape not yet complete
    i++
    switch (n) {
      case "n": out += "\n"; break
      case "t": out += "\t"; break
      case "r": out += "\r"; break
      case "b": out += "\b"; break
      case "f": out += "\f"; break
      case "u": {
        const hex = src.slice(i + 1, i + 5)
        if (hex.length < 4) return { value: out, complete: false }
        out += String.fromCharCode(parseInt(hex, 16))
        i += 4
        break
      }
      default: out += n // \" \\ \/
    }
  }
  return { value: out, complete: false }
}

/** Last value of a string field in the half-finished JSON. */
function field(src: string, name: string): { value: string; complete: boolean } | undefined {
  const key = `"${name}"`
  const at = src.lastIndexOf(key)
  if (at < 0) return undefined
  let i = at + key.length
  while (i < src.length && /\s/.test(src[i])) i++
  if (src[i] !== ":") return undefined
  i++
  while (i < src.length && /\s/.test(src[i])) i++
  if (src[i] !== '"') return undefined
  return readPartialString(src, i + 1)
}

export function preparingInfo(tool: string, argsText: string): PreparingInfo {
  const bytes = argsText.length
  const path = field(argsText, "path")
  const main = MAIN_FIELD[tool] ? field(argsText, MAIN_FIELD[tool]) : undefined
  let preview = main ? main.value : argsText
  if (preview.length > MAX_PREVIEW) preview = "…" + preview.slice(preview.length - MAX_PREVIEW + 1)
  const lines = main ? main.value.split("\n").length : 0
  return { bytes, lines, path: path?.complete ? path.value : undefined, preview }
}

// Mermaid diagrams in responses: detection of the code blocks, streaming state and a renderer that loads the
// library only at the first diagram (separate chunk, see mermaid-load.ts). Without React.
import type { MermaidConfig } from "mermaid"

export type MermaidTheme = "light" | "dark"
export type MermaidOutcome = { ok: true; svg: string } | { ok: false; error: string }

/** The part of the mermaid API the UI uses (mocked in tests). */
export type MermaidApi = {
  initialize: (config: MermaidConfig) => void
  render: (id: string, code: string) => Promise<{ svg: string }>
}
export type MermaidModule = { api: MermaidApi; sanitize: (svg: string) => string }

/** Whether a code block (class from react-markdown, e.g. `language-mermaid`) is a Mermaid diagram. */
export function isMermaidClass(className: unknown): boolean {
  const list = Array.isArray(className) ? className : typeof className === "string" ? className.split(/\s+/) : []
  return list.some((c) => typeof c === "string" && c.toLowerCase() === "language-mermaid")
}

const fenceRe = /^( {0,3})(`{3,}|~{3,})(.*)$/

/**
 * Start (character position of the line) of a still open code block at the end of the text, otherwise undefined. While streaming,
 * the Markdown parser closes open blocks itself; a diagram should only be drawn once
 * its block is really closed. Rules as in CommonMark: fence of at least three ` or ~, indented by at most three
 * spaces; closed with the same character in at least the same length.
 */
export function openFenceStart(text: string): number | undefined {
  let open: { start: number; char: string; len: number } | undefined
  let pos = 0
  for (const line of text.split("\n")) {
    const m = fenceRe.exec(line.replace(/\r$/, ""))
    if (m) {
      const fence = m[2]
      if (!open) {
        // for backtick fences the info string must not contain backticks
        if (!(fence[0] === "`" && m[3].includes("`"))) open = { start: pos, char: fence[0], len: fence.length }
      } else if (fence[0] === open.char && fence.length >= open.len && m[3].trim() === "") {
        open = undefined
      }
    }
    pos += line.length + 1
  }
  return open?.start
}

/**
 * Starts (character position of the line) of all Mermaid code blocks of a text in order; the UI uses
 * them to count the diagrams of a message (MERMAID_AUTO_MAX).
 */
export function mermaidFenceStarts(text: string): number[] {
  const out: number[] = []
  let open: { char: string; len: number } | undefined
  let pos = 0
  for (const line of text.split("\n")) {
    const m = fenceRe.exec(line.replace(/\r$/, ""))
    if (m) {
      const fence = m[2]
      if (!open) {
        if (!(fence[0] === "`" && m[3].includes("`"))) {
          open = { char: fence[0], len: fence.length }
          if ((m[3].trim().split(/\s+/)[0] ?? "").toLowerCase() === "mermaid") out.push(pos)
        }
      } else if (fence[0] === open.char && fence.length >= open.len && m[3].trim() === "") {
        open = undefined
      }
    }
    pos += line.length + 1
  }
  return out
}

/**
 * Limits against blocking diagrams (Review 3, N4: about 3.4 s per large diagram on the main thread): from
 * MERMAID_LARGE_CHARS characters or MERMAID_LARGE_EDGES edges, drawing happens only on click, and per
 * message at most MERMAID_AUTO_MAX diagrams are drawn on their own.
 */
export const MERMAID_AUTO_MAX = 5
export const MERMAID_LARGE_CHARS = 4000
export const MERMAID_LARGE_EDGES = 150

// Edges: arrows and lines of the common diagram types (flowchart, sequence, class, state, er).
const edgeRe = /<?(?:-{2,}|={2,}|-\.+-?|~{3})[->xo)|]*|->>?|-[x)]|\|\|--|\}o--/g

/** Size of a diagram: characters and (estimated) edges. */
export function mermaidSize(code: string): { chars: number; edges: number } {
  let edges = 0
  for (const line of code.split("\n")) {
    const l = line.trim()
    if (!l || l.startsWith("%%")) continue
    const m = l.match(edgeRe)
    if (m) edges += m.length
  }
  return { chars: code.length, edges }
}

export function isLargeDiagram(code: string): boolean {
  const s = mermaidSize(code)
  return s.chars > MERMAID_LARGE_CHARS || s.edges > MERMAID_LARGE_EDGES
}

/** Is a diagram drawn without a click? index: position in the message (from 0). */
export function autoRender(p: { index: number; large: boolean }): boolean {
  return !p.large && p.index < MERMAID_AUTO_MAX
}

export type DiagramView = "source" | "deferred" | "loading" | "diagram" | "error"

/**
 * What a Mermaid block shows: the source while the block is still open during streaming or the
 * user wants to see it; a deferred diagram (large or too many) as source with a button;
 * otherwise the diagram, "drawing" until then, and on errors the source with a notice.
 */
export function diagramView(p: { ready: boolean; outcome?: MermaidOutcome; showSource?: boolean; deferred?: boolean }): DiagramView {
  if (!p.ready) return "source"
  if (p.deferred) return "deferred"
  if (!p.outcome) return "loading"
  if (!p.outcome.ok) return "error"
  return p.showSource ? "source" : "diagram"
}

/**
 * SVG as a data: address for <img>. Embedded as an image, the browser runs no script in the SVG and loads
 * no external resources (image context); the CSP allows `img-src data:`.
 */
export function svgDataUrl(svg: string): string {
  return `data:image/svg+xml;charset=utf-8,${encodeURIComponent(svg)}`
}

/**
 * Configuration: `securityLevel: "strict"` (labels are sanitized, no click directives, no JS),
 * no HTML labels (otherwise <foreignObject> with HTML), no custom fonts from the network. `secure`
 * forbids the diagram to override these keys via `%%{init: …}%%`.
 */
export function mermaidConfig(theme: MermaidTheme): MermaidConfig {
  return {
    startOnLoad: false,
    securityLevel: "strict",
    htmlLabels: false,
    theme: theme === "dark" ? "dark" : "default",
    darkMode: theme === "dark",
    fontFamily: "ui-sans-serif, system-ui, sans-serif",
    suppressErrorRendering: true,
    maxTextSize: 50_000,
    secure: [
      "secure",
      "securityLevel",
      "startOnLoad",
      "maxTextSize",
      "suppressErrorRendering",
      "maxEdges",
      "htmlLabels",
      "themeCSS",
      "fontFamily",
      "altFontFamily",
      "theme",
      "themeVariables",
      "darkMode",
    ],
  }
}

/**
 * Renderer with cache (per theme and source). `load` fetches the library on the first call; drawing
 * happens one after another because mermaid has global state (initialize, temporary nodes in the document).
 */
export function createMermaidRenderer(load: () => Promise<MermaidModule>) {
  let mod: Promise<MermaidModule> | undefined
  let queue: Promise<unknown> = Promise.resolve()
  let theme: MermaidTheme | undefined
  let seq = 0
  const cache = new Map<string, Promise<MermaidOutcome>>()

  const draw = async (code: string, t: MermaidTheme): Promise<MermaidOutcome> => {
    try {
      mod ??= load()
      const { api, sanitize } = await mod
      if (theme !== t) {
        api.initialize(mermaidConfig(t))
        theme = t
      }
      const { svg } = await api.render(`agw-mermaid-${++seq}`, code)
      return { ok: true, svg: sanitize(svg) }
    } catch (e) {
      // retry a failed load at the next diagram
      if (mod && (await mod.then(() => false, () => true))) mod = undefined
      return { ok: false, error: e instanceof Error ? e.message : String(e) }
    }
  }

  return {
    render(code: string, t: MermaidTheme): Promise<MermaidOutcome> {
      const key = `${t}\n${code}`
      let p = cache.get(key)
      if (!p) {
        p = queue.then(() => draw(code, t))
        queue = p
        cache.set(key, p)
      }
      return p
    },
  }
}

/** Shared renderer of the UI; the library comes as a separate chunk (dynamic import). */
export const mermaidRenderer = createMermaidRenderer(() => import("./mermaid-load").then((m) => m.loadMermaid()))

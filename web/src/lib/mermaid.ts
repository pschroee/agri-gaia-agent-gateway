// Mermaid-Diagramme in Antworten: Erkennung der Codeblöcke, Streaming-Zustand und ein Renderer, der die
// Bibliothek erst beim ersten Diagramm lädt (eigener Chunk, siehe mermaid-load.ts). Ohne React.
import type { MermaidConfig } from "mermaid"

export type MermaidTheme = "light" | "dark"
export type MermaidOutcome = { ok: true; svg: string } | { ok: false; error: string }

/** Der Ausschnitt der mermaid-API, den die UI nutzt (in Tests attrappiert). */
export type MermaidApi = {
  initialize: (config: MermaidConfig) => void
  render: (id: string, code: string) => Promise<{ svg: string }>
}
export type MermaidModule = { api: MermaidApi; sanitize: (svg: string) => string }

/** Ob ein Codeblock (Klasse von react-markdown, etwa `language-mermaid`) ein Mermaid-Diagramm ist. */
export function isMermaidClass(className: unknown): boolean {
  const list = Array.isArray(className) ? className : typeof className === "string" ? className.split(/\s+/) : []
  return list.some((c) => typeof c === "string" && c.toLowerCase() === "language-mermaid")
}

const fenceRe = /^( {0,3})(`{3,}|~{3,})(.*)$/

/**
 * Anfang (Zeichenposition der Zeile) eines noch offenen Codeblocks am Ende des Textes, sonst undefined. Beim Streamen
 * schließt der Markdown-Parser offene Blöcke selbst; ein Diagramm soll aber erst gezeichnet werden, wenn
 * sein Block wirklich zu ist. Regeln wie CommonMark: Zaun aus mindestens drei ` oder ~, höchstens drei
 * Leerzeichen eingerückt; geschlossen wird mit demselben Zeichen in mindestens gleicher Länge.
 */
export function openFenceStart(text: string): number | undefined {
  let open: { start: number; char: string; len: number } | undefined
  let pos = 0
  for (const line of text.split("\n")) {
    const m = fenceRe.exec(line.replace(/\r$/, ""))
    if (m) {
      const fence = m[2]
      if (!open) {
        // Bei Backtick-Zäunen darf die Info-Zeile keine Backticks enthalten
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
 * Anfänge (Zeichenposition der Zeile) aller Mermaid-Codeblöcke eines Textes in Reihenfolge; die UI zählt
 * damit die Diagramme einer Nachricht (MERMAID_AUTO_MAX).
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
 * Grenzen gegen blockierende Diagramme (Review 3, N4: rund 3,4 s je großem Diagramm im Hauptfaden): Ab
 * MERMAID_LARGE_CHARS Zeichen oder MERMAID_LARGE_EDGES Kanten wird erst auf Klick gezeichnet, und je
 * Nachricht höchstens MERMAID_AUTO_MAX Diagramme von selbst.
 */
export const MERMAID_AUTO_MAX = 5
export const MERMAID_LARGE_CHARS = 4000
export const MERMAID_LARGE_EDGES = 150

// Kanten: Pfeile und Linien der gängigen Diagrammarten (flowchart, sequence, class, state, er).
const edgeRe = /<?(?:-{2,}|={2,}|-\.+-?|~{3})[->xo)|]*|->>?|-[x)]|\|\|--|\}o--/g

/** Größe eines Diagramms: Zeichen und (geschätzte) Kanten. */
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

/** Wird ein Diagramm ohne Klick gezeichnet? index: Position in der Nachricht (ab 0). */
export function autoRender(p: { index: number; large: boolean }): boolean {
  return !p.large && p.index < MERMAID_AUTO_MAX
}

export type DiagramView = "source" | "deferred" | "loading" | "diagram" | "error"

/**
 * Was ein Mermaid-Block zeigt: den Quelltext, solange der Block beim Streamen noch offen ist oder der
 * Nutzer ihn sehen will; ein zurückgestelltes Diagramm (groß oder zu viele) als Quelltext mit Knopf;
 * sonst das Diagramm, bis dahin „wird gezeichnet“, bei Fehlern den Quelltext mit Hinweis.
 */
export function diagramView(p: { ready: boolean; outcome?: MermaidOutcome; showSource?: boolean; deferred?: boolean }): DiagramView {
  if (!p.ready) return "source"
  if (p.deferred) return "deferred"
  if (!p.outcome) return "loading"
  if (!p.outcome.ok) return "error"
  return p.showSource ? "source" : "diagram"
}

/**
 * SVG als data:-Adresse für <img>. Als Bild eingebunden führt der Browser kein Skript im SVG aus und lädt
 * keine externen Ressourcen (Bild-Kontext); die CSP erlaubt `img-src data:`.
 */
export function svgDataUrl(svg: string): string {
  return `data:image/svg+xml;charset=utf-8,${encodeURIComponent(svg)}`
}

/**
 * Konfiguration: `securityLevel: "strict"` (Labels werden gesäubert, keine click-Direktiven, kein JS),
 * keine HTML-Labels (sonst <foreignObject> mit HTML), keine eigenen Schriften aus dem Netz. `secure`
 * verbietet dem Diagramm, diese Schlüssel per `%%{init: …}%%` zu überschreiben.
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
 * Renderer mit Cache (je Theme und Quelltext). `load` holt die Bibliothek beim ersten Aufruf; gezeichnet
 * wird nacheinander, weil mermaid globalen Zustand hat (initialize, temporäre Knoten im Dokument).
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
      // Ein gescheitertes Laden beim nächsten Diagramm erneut versuchen
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

/** Gemeinsamer Renderer der UI; die Bibliothek kommt als eigener Chunk (dynamischer Import). */
export const mermaidRenderer = createMermaidRenderer(() => import("./mermaid-load").then((m) => m.loadMermaid()))

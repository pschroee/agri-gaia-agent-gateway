import { describe, expect, it, vi } from "vitest"
import {
  autoRender,
  createMermaidRenderer,
  diagramView,
  isLargeDiagram,
  isMermaidClass,
  MERMAID_AUTO_MAX,
  MERMAID_LARGE_CHARS,
  MERMAID_LARGE_EDGES,
  mermaidFenceStarts,
  mermaidSize,
  openFenceStart,
  svgDataUrl,
  type MermaidApi,
} from "./mermaid"

describe("isMermaidClass", () => {
  it("erkennt Codeblöcke mit Sprache mermaid", () => {
    expect(isMermaidClass("language-mermaid")).toBe(true)
    expect(isMermaidClass(["language-mermaid"])).toBe(true)
    expect(isMermaidClass("hljs language-Mermaid")).toBe(true)
  })
  it("nimmt andere Sprachen und fehlende Angaben nicht", () => {
    for (const c of [undefined, "", "language-python", "language-mermaidx", "mermaid", ["language-js"]]) {
      expect(isMermaidClass(c), String(c)).toBe(false)
    }
  })
})

describe("openFenceStart", () => {
  it("liefert nichts, wenn alle Codeblöcke geschlossen sind", () => {
    expect(openFenceStart("Text ohne Code")).toBeUndefined()
    expect(openFenceStart("a\n```mermaid\ngraph TD\nA-->B\n```\nb")).toBeUndefined()
    expect(openFenceStart("~~~\nx\n~~~")).toBeUndefined()
  })
  it("liefert den Anfang des offenen Blocks beim Streamen", () => {
    const text = "Hier:\n\n```mermaid\ngraph TD\n  A-->"
    expect(openFenceStart(text)).toBe(text.indexOf("```"))
  })
  it("schließt nur mit gleichem Zeichen und mindestens gleicher Länge", () => {
    expect(openFenceStart("````\n```\nnoch offen")).toBe(0)
    expect(openFenceStart("```\n~~~\n")).toBe(0)
    expect(openFenceStart("```\nx\n`````")).toBeUndefined()
  })
  it("zählt einen zweiten, offenen Block nach einem geschlossenen", () => {
    const text = "```js\n1\n```\n\n```mermaid\nsequenceDiagram"
    expect(openFenceStart(text)).toBe(text.indexOf("```mermaid"))
  })
  it("ignoriert Backticks mitten in der Zeile und eingerückten Code", () => {
    expect(openFenceStart("Text mit ``` in der Zeile")).toBeUndefined()
    expect(openFenceStart("    ```\n    eingerückt")).toBeUndefined()
  })
})

describe("diagramView", () => {
  it("zeigt beim Streamen eines offenen Blocks den Quelltext", () => {
    expect(diagramView({ ready: false })).toBe("source")
  })
  it("wartet auf das Ergebnis, sobald der Block fertig ist", () => {
    expect(diagramView({ ready: true })).toBe("loading")
  })
  it("zeigt das Diagramm oder den Fehler", () => {
    expect(diagramView({ ready: true, outcome: { ok: true, svg: "<svg/>" } })).toBe("diagram")
    expect(diagramView({ ready: true, outcome: { ok: false, error: "Parse error" } })).toBe("error")
  })
  it("zeigt auf Wunsch den Quelltext statt des Diagramms", () => {
    expect(diagramView({ ready: true, outcome: { ok: true, svg: "<svg/>" }, showSource: true })).toBe("source")
  })
})

describe("svgDataUrl", () => {
  it("kodiert das SVG als data:-Adresse (für <img>, dort läuft kein Skript)", () => {
    const url = svgDataUrl('<svg xmlns="http://www.w3.org/2000/svg"><text>Ä & #</text></svg>')
    expect(url.startsWith("data:image/svg+xml;charset=utf-8,")).toBe(true)
    expect(url).not.toContain("#")
    expect(decodeURIComponent(url.slice(url.indexOf(",") + 1))).toContain("<text>Ä & #</text>")
  })
})

describe("createMermaidRenderer (Bibliothek attrappiert)", () => {
  const fake = (render: MermaidApi["render"]) => {
    const api: MermaidApi = { initialize: vi.fn(), render: vi.fn(render) }
    const load = vi.fn(async () => ({ api, sanitize: (svg: string) => `sauber:${svg}` }))
    return { api, load }
  }

  it("lädt die Bibliothek erst beim ersten Diagramm und nur einmal", async () => {
    const { api, load } = fake(async (id, code) => ({ svg: `<svg id="${id}">${code}</svg>` }))
    const r = createMermaidRenderer(load)
    expect(load).not.toHaveBeenCalled()
    await r.render("graph TD\nA-->B", "light")
    await r.render("graph TD\nB-->C", "light")
    expect(load).toHaveBeenCalledTimes(1)
    expect(api.render).toHaveBeenCalledTimes(2)
  })

  it("setzt securityLevel strict und keine HTML-Labels, und das Diagramm kann es nicht überschreiben", async () => {
    const { api, load } = fake(async () => ({ svg: "<svg/>" }))
    await createMermaidRenderer(load).render("graph TD\nA-->B", "dark")
    const cfg = vi.mocked(api.initialize).mock.calls[0][0]
    expect(cfg.securityLevel).toBe("strict")
    expect(cfg.htmlLabels).toBe(false)
    expect(cfg.startOnLoad).toBe(false)
    expect(cfg.theme).toBe("dark")
    expect(cfg.secure).toEqual(expect.arrayContaining(["securityLevel", "htmlLabels", "themeCSS", "startOnLoad"]))
  })

  it("säubert das Ergebnis und merkt es sich je Quelltext und Theme", async () => {
    const { api, load } = fake(async () => ({ svg: "<svg/>" }))
    const r = createMermaidRenderer(load)
    expect(await r.render("graph TD\nA-->B", "light")).toEqual({ ok: true, svg: "sauber:<svg/>" })
    await r.render("graph TD\nA-->B", "light")
    expect(api.render).toHaveBeenCalledTimes(1)
    await r.render("graph TD\nA-->B", "dark")
    expect(api.render).toHaveBeenCalledTimes(2)
  })

  it("meldet einen Syntaxfehler als Ergebnis statt als Ausnahme", async () => {
    const { load } = fake(async () => {
      throw new Error("Parse error on line 2")
    })
    const out = await createMermaidRenderer(load).render("graph TD\nA-->", "light")
    expect(out).toEqual({ ok: false, error: "Parse error on line 2" })
  })

  it("meldet einen Fehler beim Laden der Bibliothek", async () => {
    const r = createMermaidRenderer(async () => {
      throw new Error("Chunk nicht geladen")
    })
    expect(await r.render("graph TD", "light")).toEqual({ ok: false, error: "Chunk nicht geladen" })
  })

  it("zeichnet nacheinander, nie gleichzeitig (mermaid ist dafür nicht gebaut)", async () => {
    let active = 0
    let max = 0
    const { load } = fake(async () => {
      active++
      max = Math.max(max, active)
      await new Promise((r) => setTimeout(r, 5))
      active--
      return { svg: "<svg/>" }
    })
    const r = createMermaidRenderer(load)
    await Promise.all([r.render("a", "light"), r.render("b", "light"), r.render("c", "light")])
    expect(max).toBe(1)
  })
})

// Review 3, N4: Große Diagramme blockieren den Browser (rund 3,4 s je Diagramm). Ab einer Größe und ab dem
// sechsten Diagramm einer Nachricht wird erst auf Klick gezeichnet.
describe("große und viele Diagramme", () => {
  const graph = (edges: number) => ["graph TD", ...Array.from({ length: edges }, (_, i) => `  n${i} --> n${i + 1}`)].join("\n")
  it("misst Zeichen und Kanten", () => {
    expect(mermaidSize(graph(3))).toEqual({ chars: graph(3).length, edges: 3 })
    expect(mermaidSize("sequenceDiagram\n  A->>B: hi\n  B-->>A: ok\n  A-)B: x")).toMatchObject({ edges: 3 })
    expect(isLargeDiagram(graph(20))).toBe(false)
    expect(isLargeDiagram(graph(MERMAID_LARGE_EDGES + 1))).toBe(true)
    expect(isLargeDiagram("graph TD\n" + "%% " + "x".repeat(MERMAID_LARGE_CHARS))).toBe(true)
  })
  it("zeichnet höchstens MERMAID_AUTO_MAX je Nachricht von selbst, große nie", () => {
    expect(MERMAID_AUTO_MAX).toBe(5)
    expect(autoRender({ index: 0, large: false })).toBe(true)
    expect(autoRender({ index: 4, large: false })).toBe(true)
    expect(autoRender({ index: 5, large: false })).toBe(false)
    expect(autoRender({ index: 0, large: true })).toBe(false)
  })
  it("zeigt zurückgestellte Diagramme als Quelltext mit Knopf", () => {
    expect(diagramView({ ready: true, deferred: true })).toBe("deferred")
    expect(diagramView({ ready: false, deferred: true })).toBe("source")
    expect(diagramView({ ready: true, deferred: false })).toBe("loading")
  })
  it("findet die Mermaid-Blöcke einer Nachricht in Reihenfolge", () => {
    const text = "a\n```mermaid\ngraph TD\nA-->B\n```\n\n```js\nx\n```\n\n  ~~~ Mermaid\ngraph LR\n~~~\n"
    const starts = mermaidFenceStarts(text)
    expect(starts).toEqual([2, text.indexOf("  ~~~")])
  })
})

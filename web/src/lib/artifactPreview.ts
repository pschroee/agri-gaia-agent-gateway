import type { Artifact } from "@/api/types"

/** Wie ein Artefakt im Verlauf erscheint: Bild direkt, PDF mit Vorschau per Klick, sonst als Datei. */
export type PreviewKind = "image" | "pdf" | "file"

const imageTypes = new Set(["image/png", "image/jpeg", "image/gif", "image/webp"])
const imageExt = /\.(png|jpe?g|gif|webp)$/i

/**
 * Einordnung nach Inhaltstyp (vom Orchestrator aus Inhalt und Endung bestimmt) und Endung; beide müssen
 * passen, damit ein umbenanntes HTML nicht als Bild oder PDF geöffnet wird. SVG gilt als Datei.
 */
export function previewKind(a: Pick<Artifact, "name" | "content_type">): PreviewKind {
  const ct = a.content_type.split(";")[0].trim().toLowerCase()
  if (imageTypes.has(ct) && imageExt.test(a.name)) return "image"
  if (ct === "application/pdf" && /\.pdf$/i.test(a.name)) return "pdf"
  return "file"
}

/** Ergebnisse, die ein Werkzeugaufruf hochgeladen hat (neueste zuletzt). */
export function artifactsOfCall(list: Artifact[] | undefined, toolCallId: string | undefined): Artifact[] {
  if (!list || !toolCallId) return []
  return list
    .filter((a) => a.kind === "output" && a.tool_call_id === toolCallId)
    .sort((x, y) => x.created_at.localeCompare(y.created_at))
}

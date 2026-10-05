// Attachments of a user message: the orchestrator appends them to the text as a fixed block
// (see "Attachments to messages" in poc/API.md). Here the block is split off again.

export const ATTACHMENTS_HEADER = "[Attachments in /workspace/inputs/]"
const ONLY_ATTACHMENTS = "See attachments."

export function splitAttachments(message: string): { text: string; files: string[] } {
  const at = message.lastIndexOf(ATTACHMENTS_HEADER)
  if (at < 0) return { text: message, files: [] }
  const lines = message.slice(at + ATTACHMENTS_HEADER.length).split("\n").slice(1)
  if (lines.length === 0 || !lines.every((l) => l.startsWith("- ") && l.length > 2)) return { text: message, files: [] }
  let text = message.slice(0, at).replace(/\s+$/, "")
  if (text === ONLY_ATTACHMENTS) text = ""
  return { text, files: lines.map((l) => l.slice(2)) }
}

// Raster images shown as a preview. SVG is left out: it is text that may
// contain script and is shown as a tile like any other file.
const PREVIEW_EXT = /\.(png|jpe?g|gif|webp|avif)$/i

export function isPreviewImage(name: string): boolean {
  return PREVIEW_EXT.test(name)
}

/**
 * Buttons in the input field as in ChatGPT: during a run it shows "Stop";
 * once something has been typed, "Send" (enqueues the message) joins it.
 */
export function composerButtons(s: { running: boolean; hasContent: boolean }) {
  const stop = s.running
  const send = !stop || s.hasContent
  return { stop, send, sendEnabled: send && s.hasContent }
}

/**
 * Text of a message with attachments as the orchestrator hands it to pi (for immediate
 * display before the server answers): without text "See attachments.", then the fixed block.
 */
export function withAttachments(text: string, files: string[]): string {
  const t = text.trim()
  if (files.length === 0) return t
  return `${t || ONLY_ATTACHMENTS}\n\n${ATTACHMENTS_HEADER}\n${files.map((f) => `- ${f}`).join("\n")}`
}

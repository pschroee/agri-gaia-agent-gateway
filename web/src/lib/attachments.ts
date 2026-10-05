// Anhänge einer Nutzernachricht: Der Orchestrator hängt sie als festen Block an den Text
// (siehe „Anhänge an Nachrichten“ in poc/API.md). Hier wird der Block wieder abgetrennt.

export const ATTACHMENTS_HEADER = "[Anhänge unter /workspace/inputs/]"
const ONLY_ATTACHMENTS = "Siehe Anhänge."

export function splitAttachments(message: string): { text: string; files: string[] } {
  const at = message.lastIndexOf(ATTACHMENTS_HEADER)
  if (at < 0) return { text: message, files: [] }
  const lines = message.slice(at + ATTACHMENTS_HEADER.length).split("\n").slice(1)
  if (lines.length === 0 || !lines.every((l) => l.startsWith("- ") && l.length > 2)) return { text: message, files: [] }
  let text = message.slice(0, at).replace(/\s+$/, "")
  if (text === ONLY_ATTACHMENTS) text = ""
  return { text, files: lines.map((l) => l.slice(2)) }
}

// Rasterbilder, die als Vorschau gezeigt werden. SVG bleibt außen vor: Es ist Text mit
// möglichem Skript und wird wie jede andere Datei als Kachel angezeigt.
const PREVIEW_EXT = /\.(png|jpe?g|gif|webp|avif)$/i

export function isPreviewImage(name: string): boolean {
  return PREVIEW_EXT.test(name)
}

/**
 * Knöpfe im Eingabefeld wie bei ChatGPT: Während eines Laufs steht dort „Stoppen“;
 * hat man etwas geschrieben, kommt „Senden“ (reiht die Nachricht ein) dazu.
 */
export function composerButtons(s: { running: boolean; hasContent: boolean }) {
  const stop = s.running
  const send = !stop || s.hasContent
  return { stop, send, sendEnabled: send && s.hasContent }
}

/**
 * Text einer Nachricht mit Anhängen, wie der Orchestrator ihn an pi gibt (für die sofortige
 * Anzeige vor der Antwort des Servers): ohne Text „Siehe Anhänge.“, dann der feste Block.
 */
export function withAttachments(text: string, files: string[]): string {
  const t = text.trim()
  if (files.length === 0) return t
  return `${t || ONLY_ATTACHMENTS}\n\n${ATTACHMENTS_HEADER}\n${files.map((f) => `- ${f}`).join("\n")}`
}

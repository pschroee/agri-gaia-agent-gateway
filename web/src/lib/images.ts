// Bilder in Antworten des Agenten (siehe „Anzeige-Bilder“ in poc/README.md). Fremde Adressen lädt die UI
// nie: Über die Bildadresse könnten Daten aus der Sandbox zu einem fremden Server gelangen, ohne dass der
// Nutzer Internet bestätigt hat (Markdown-Image-Exfiltration, Review K1). Lokale Pfade aus der Sandbox holt
// der Orchestrator selbst, prüft sie und liefert sie aus; data:-Bilder bleiben im Browser.
// Die Regeln entsprechen internal/chat/images.go (NormalizeImagePath, MessageImageKey).
import { urls } from "@/api/client"

const ROOTS = ["/workspace", "/tmp", "/home/agent"]
const SCHEME = /^[a-z][a-z0-9+.-]*:/i
const DATA_IMAGE = /^data:image\/(png|jpeg|gif|webp);base64,[A-Za-z0-9+/=\s]+$/
const MSG_ID = /^[A-Za-z0-9._:-]{1,128}$/

function decode(s: string): string {
  if (!s.includes("%")) return s
  try {
    return decodeURIComponent(s)
  } catch {
    return s
  }
}

/** Bereinigt einen absoluten POSIX-Pfad (`.`, `..`, doppelte `/`). */
function cleanPath(p: string): string {
  const out: string[] = []
  for (const part of p.split("/")) {
    if (part === "" || part === ".") continue
    if (part === "..") out.pop()
    else out.push(part)
  }
  return "/" + out.join("/")
}

/**
 * Lokaler Bildpfad in der Sandbox, absolut und bereinigt; relative Pfade gelten ab /workspace.
 * `undefined` für fremde Adressen (Schema, `//…`) und Orte außerhalb von /workspace, /tmp, /home/agent.
 */
export function sandboxImagePath(src: string): string | undefined {
  let p = decode(src.trim())
  if (p.startsWith("file://")) {
    p = p.slice("file://".length)
    if (!p.startsWith("/")) return undefined
  }
  // Steuerzeichen gezielt abweisen
  // oxlint-disable-next-line no-control-regex
  if (!p || p.length > 1024 || p.startsWith("//") || SCHEME.test(p) || /[\u0000-\u001f\u007f]/.test(p)) return undefined
  if (!p.startsWith("/")) p = "/workspace/" + p
  p = cleanPath(p)
  return ROOTS.some((r) => p.startsWith(r + "/")) ? p : undefined
}

export type ImageSource =
  /** Aus der Sandbox, über den Orchestrator. */
  | { kind: "sandbox"; url: string; path: string }
  /** Eingebettet (data:), nur Rasterformate. */
  | { kind: "data"; url: string }
  /** Lokaler Pfad, aber die Antwort ist noch nicht fertig (keine Kennung). */
  | { kind: "pending"; path: string }
  /** Wird nicht geladen. */
  | { kind: "blocked"; src: string }

export function imageSource(src: string | undefined, ctx?: { chatId?: string; msgId?: string }): ImageSource {
  const s = (src ?? "").trim()
  if (DATA_IMAGE.test(s)) return { kind: "data", url: s }
  const path = s ? sandboxImagePath(s) : undefined
  if (!path) return { kind: "blocked", src: s }
  if (!ctx?.chatId || !ctx.msgId) return { kind: "pending", path }
  return { kind: "sandbox", path, url: urls.image(ctx.chatId, path, ctx.msgId) }
}

/** Kennung einer Antwort für ihre Bilder: responseId, sonst ts-<timestamp> (wie der Orchestrator). */
export function messageImageKey(m: { responseId?: unknown; timestamp?: unknown }): string | undefined {
  if (typeof m.responseId === "string" && MSG_ID.test(m.responseId)) return m.responseId
  if (typeof m.timestamp === "number" && Number.isFinite(m.timestamp) && m.timestamp > 0) return `ts-${m.timestamp}`
  return undefined
}

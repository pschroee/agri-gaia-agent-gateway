// Images in the agent's responses (see "Display images in the chat" in docs/design.md). The UI never loads
// foreign addresses: through the image address, data from the sandbox could reach a foreign server without the
// user having approved internet access (Markdown image exfiltration, Review K1). Local paths from the sandbox are
// fetched by the orchestrator itself, which checks and serves them; data: images stay in the browser.
// The rules match internal/chat/images.go (NormalizeImagePath, MessageImageKey).
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

/** Cleans an absolute POSIX path (`.`, `..`, double `/`). */
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
 * Local image path in the sandbox, absolute and cleaned; relative paths start at /workspace.
 * `undefined` for foreign addresses (scheme, `//…`) and locations outside /workspace, /tmp, /home/agent.
 */
export function sandboxImagePath(src: string): string | undefined {
  let p = decode(src.trim())
  if (p.startsWith("file://")) {
    p = p.slice("file://".length)
    if (!p.startsWith("/")) return undefined
  }
  // reject control characters on purpose
  // oxlint-disable-next-line no-control-regex
  if (!p || p.length > 1024 || p.startsWith("//") || SCHEME.test(p) || /[\u0000-\u001f\u007f]/.test(p)) return undefined
  if (!p.startsWith("/")) p = "/workspace/" + p
  p = cleanPath(p)
  return ROOTS.some((r) => p.startsWith(r + "/")) ? p : undefined
}

export type ImageSource =
  /** From the sandbox, via the orchestrator. */
  | { kind: "sandbox"; url: string; path: string }
  /** Embedded (data:), raster formats only. */
  | { kind: "data"; url: string }
  /** Local path, but the response is not finished yet (no ID). */
  | { kind: "pending"; path: string }
  /** Not loaded. */
  | { kind: "blocked"; src: string }

export function imageSource(src: string | undefined, ctx?: { chatId?: string; msgId?: string }): ImageSource {
  const s = (src ?? "").trim()
  if (DATA_IMAGE.test(s)) return { kind: "data", url: s }
  const path = s ? sandboxImagePath(s) : undefined
  if (!path) return { kind: "blocked", src: s }
  if (!ctx?.chatId || !ctx.msgId) return { kind: "pending", path }
  return { kind: "sandbox", path, url: urls.image(ctx.chatId, path, ctx.msgId) }
}

/** ID of a response for its images: responseId, otherwise ts-<timestamp> (like the orchestrator). */
export function messageImageKey(m: { responseId?: unknown; timestamp?: unknown }): string | undefined {
  if (typeof m.responseId === "string" && MSG_ID.test(m.responseId)) return m.responseId
  if (typeof m.timestamp === "number" && Number.isFinite(m.timestamp) && m.timestamp > 0) return `ts-${m.timestamp}`
  return undefined
}

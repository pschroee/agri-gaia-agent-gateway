// Anzeige des Fortsetzens eines ruhenden Chats (SSE „resume“, siehe poc/API.md): Beschriftungen
// der Schritte und die Zeile, auf die der Block nach dem Abschluss zusammenklappt.
import type { ResumePhase } from "@/api/types"
import { formatBytes, formatMs } from "@/lib/format"
import type { ResumeItem, ResumeStepView } from "@/lib/stream"

export const resumePhaseLabel: Record<Exclude<ResumePhase, "ready" | "failed">, string> = {
  acquire: "Sandbox aus dem Pool holen",
  session: "Sitzung einspielen",
  settings: "Einstellungen setzen",
  workspace: "Arbeitsbereich einspielen",
  inputs: "Eingaben bereitstellen",
}

export const stepLabel = (phase: ResumePhase) =>
  (resumePhaseLabel as Record<string, string>)[phase] ?? phase

const files = (n: number) => (n === 1 ? "1 Datei" : `${n} Dateien`)

/** Ergänzung hinter einem Schritt, etwa „1,2 MiB, 14 Dateien“; leer, solange er nicht fertig ist. */
export function stepDetail(s: ResumeStepView): string | undefined {
  if (s.status === "pending" || s.status === "running") return undefined
  const parts: string[] = []
  if ((s.phase === "workspace" || s.phase === "inputs") && s.files !== undefined && s.size !== undefined) {
    parts.push(s.files === 0 ? "keine Dateien" : `${formatBytes(s.size)}, ${files(s.files)}`)
  } else if (s.phase === "session" && s.size !== undefined) {
    parts.push(formatBytes(s.size))
  }
  // Beim Platz ist detail die interne Kennung; sie steht im Tooltip, nicht in der Zeile.
  if (s.detail && s.phase !== "acquire") parts.push(s.detail)
  return parts.length > 0 ? parts.join(" · ") : undefined
}

/** Kopfzeile des Blocks; nach dem Abschluss die eine Zeile, die im Verlauf stehen bleibt. */
export function resumeSummary(r: ResumeItem): string {
  if (r.state === "running") return "Chat wird fortgesetzt …"
  if (r.state === "failed") return `Fortsetzen gescheitert${r.error ? `: ${r.error}` : ""}`
  const warn = r.steps.some((s) => s.status === "warning") ? " · mit Hinweis" : ""
  return `Fortgesetzt in frischer Sandbox${r.totalMs !== undefined ? ` · ${formatMs(r.totalMs)}` : ""}${warn}`
}

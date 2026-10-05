// Display of resuming an idle chat (SSE "resume", see poc/API.md): labels of the
// steps and the line the block collapses to once finished.
import type { ResumePhase } from "@/api/types"
import { formatBytes, formatMs } from "@/lib/format"
import type { ResumeItem, ResumeStepView } from "@/lib/stream"

export const resumePhaseLabel: Record<Exclude<ResumePhase, "ready" | "failed">, string> = {
  acquire: "Take sandbox from the pool",
  session: "Restore session",
  settings: "Apply settings",
  workspace: "Restore workspace",
  inputs: "Provide inputs",
}

export const stepLabel = (phase: ResumePhase) =>
  (resumePhaseLabel as Record<string, string>)[phase] ?? phase

const files = (n: number) => (n === 1 ? "1 file" : `${n} files`)

/** Addition after a step, e.g. "1.2 MiB, 14 files"; empty while it is not finished. */
export function stepDetail(s: ResumeStepView): string | undefined {
  if (s.status === "pending" || s.status === "running") return undefined
  const parts: string[] = []
  if ((s.phase === "workspace" || s.phase === "inputs") && s.files !== undefined && s.size !== undefined) {
    parts.push(s.files === 0 ? "no files" : `${formatBytes(s.size)}, ${files(s.files)}`)
  } else if (s.phase === "session" && s.size !== undefined) {
    parts.push(formatBytes(s.size))
  }
  // For the slot, detail is the internal ID; it goes into the tooltip, not the line.
  if (s.detail && s.phase !== "acquire") parts.push(s.detail)
  return parts.length > 0 ? parts.join(" · ") : undefined
}

/** Header line of the block; once finished, the one line that stays in the history. */
export function resumeSummary(r: ResumeItem): string {
  if (r.state === "running") return "Resuming chat …"
  if (r.state === "failed") return `Resuming failed${r.error ? `: ${r.error}` : ""}`
  const warn = r.steps.some((s) => s.status === "warning") ? " · with warning" : ""
  return `Resumed in a fresh sandbox${r.totalMs !== undefined ? ` · ${formatMs(r.totalMs)}` : ""}${warn}`
}

import type { WorkspaceBackup } from "@/api/types"
import { formatBytes } from "@/lib/format"

const timeOnly = new Intl.DateTimeFormat("en-US", { hour: "2-digit", minute: "2-digit", hourCycle: "h23" })
const dateTime = new Intl.DateTimeFormat("en-US", { dateStyle: "short", timeStyle: "short", hourCycle: "h23" })

/** Time of day, with the date if on a different day than `now`. */
export function shortWhen(iso: string, now = new Date()): string {
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return "–"
  return d.toDateString() === now.toDateString() ? timeOnly.format(d) : dateTime.format(d)
}

/**
 * Short description of the /workspace backup for the side tab:
 * "Backed up: 1.2 MiB, 14 files, 17:05", plus a warning if a backup was skipped.
 */
export function workspaceSummary(w: WorkspaceBackup | undefined, now = new Date()): { text: string; warning?: string } {
  const text = w?.saved_at
    ? `Backed up: ${formatBytes(w.size)}, ${w.files === 1 ? "1 file" : `${w.files} files`}, ${shortWhen(w.saved_at, now)}`
    : "Not backed up yet."
  if (!w?.skipped_reason) return { text }
  const when = w.skipped_at ? ` (${shortWhen(w.skipped_at, now)})` : ""
  const rest = w.saved_at ? "On resume, the backup above applies." : "If the chat goes idle, the files are lost."
  return { text, warning: `Last backup skipped${when}: ${w.skipped_reason}. ${rest}` }
}

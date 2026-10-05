import type { WorkspaceBackup } from "@/api/types"
import { formatBytes } from "@/lib/format"

const timeOnly = new Intl.DateTimeFormat("de-DE", { hour: "2-digit", minute: "2-digit" })
const dateTime = new Intl.DateTimeFormat("de-DE", { dateStyle: "short", timeStyle: "short" })

/** Uhrzeit, bei einem anderen Tag als `now` mit Datum. */
export function shortWhen(iso: string, now = new Date()): string {
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return "–"
  return d.toDateString() === now.toDateString() ? timeOnly.format(d) : dateTime.format(d)
}

/**
 * Kurzbeschreibung der Sicherung von /workspace für den Seitenreiter:
 * „Gesichert: 1,2 MiB, 14 Dateien, 17:05“, bei ausgelassener Sicherung dazu eine Warnung.
 */
export function workspaceSummary(w: WorkspaceBackup | undefined, now = new Date()): { text: string; warning?: string } {
  const text = w?.saved_at
    ? `Gesichert: ${formatBytes(w.size)}, ${w.files === 1 ? "1 Datei" : `${w.files} Dateien`}, ${shortWhen(w.saved_at, now)}`
    : "Noch nicht gesichert."
  if (!w?.skipped_reason) return { text }
  const when = w.skipped_at ? ` (${shortWhen(w.skipped_at, now)})` : ""
  const rest = w.saved_at ? "Beim Fortsetzen gilt die Sicherung oben." : "Ruht der Chat, gehen die Dateien verloren."
  return { text, warning: `Zuletzt nicht gesichert${when}: ${w.skipped_reason}. ${rest}` }
}

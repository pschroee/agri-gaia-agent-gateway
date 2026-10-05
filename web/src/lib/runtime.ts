// Laufzeiten wie in Claude Code: wie lange der aktuelle Lauf, ein Werkzeug oder eine Antwort gedauert hat.
// Die Zeiten stammen aus den Ereignissen (lib/stream: runStart, startedAt/endedAt, durationMs); was sich nicht
// belegen lässt, bleibt leer statt geschätzt.
import type { Chat } from "@/api/types"
import type { ToolExecution, TranscriptState } from "@/lib/stream"

const pad = (n: number) => String(n).padStart(2, "0")

/** Dauer in ms: unter 60 s „12 s“, darüber „1:05“, ab einer Stunde „1:02:03“. */
export function formatElapsed(ms: number): string {
  if (!Number.isFinite(ms)) return ""
  const secs = Math.floor(Math.max(0, ms) / 1000)
  if (secs < 60) return `${secs} s`
  const h = Math.floor(secs / 3600)
  const m = Math.floor((secs % 3600) / 60)
  const s = secs % 60
  return h > 0 ? `${h}:${pad(m)}:${pad(s)}` : `${m}:${pad(s)}`
}

/** Abgeschlossene Dauer: unter einer Sekunde „< 1 s“ statt „0 s“; ohne Wert leer. */
export function formatStepDuration(ms: number | undefined): string {
  if (ms === undefined || !Number.isFinite(ms)) return ""
  return ms < 1000 ? "< 1 s" : formatElapsed(ms)
}

/**
 * Beginn des laufenden Durchgangs: live der Empfang von agent_start. Wurde der Start nicht beobachtet
 * (Seite während des Laufs neu geladen), die Zeit der letzten Nutzernachricht, denn mit ihr beginnt der
 * Lauf. Arbeitet der Agent nicht, nichts.
 */
export function runStartOf(state: TranscriptState, running: boolean): number | undefined {
  if (!running) return undefined
  if (state.runStart !== undefined) return state.runStart
  for (let i = state.items.length - 1; i >= 0; i--) {
    const item = state.items[i]
    if (item.kind === "user") return item.time
  }
  return undefined
}

/**
 * Beginn des Laufs eines Chats für Kopf und Liste: der Wert des Orchestrators (`running_since`), sobald er
 * ihn liefert, sonst der aus dem Verlauf (runStartOf). Ruht der Chat oder ist nichts bekannt: nichts.
 */
export function chatRunSince(chat: Pick<Chat, "running" | "running_since"> | undefined, state?: TranscriptState): number | undefined {
  if (!chat?.running) return undefined
  const server = chat.running_since ? Date.parse(chat.running_since) : NaN
  if (!Number.isNaN(server)) return server
  return state ? runStartOf(state, true) : undefined
}

/** Dauer eines Werkzeugaufrufs: laufend bis `now`, fertig von Start bis Ende; ohne Start nichts. */
export function toolDurationMs(t: ToolExecution | undefined, now: number): number | undefined {
  if (!t || t.startedAt === undefined) return undefined
  if (t.endedAt !== undefined) return Math.max(0, t.endedAt - t.startedAt)
  return t.running ? Math.max(0, now - t.startedAt) : undefined
}

/** Was der Agent gerade tut, für die Statuszeile („Denkt“, „Schreibt“, „Führt bash aus“ …). */
export function liveActivity(state: TranscriptState): string {
  const running = Object.values(state.tools).find((t) => t.running)
  if (running) return running.toolName ? `Führt ${running.toolName} aus` : "Führt ein Werkzeug aus"
  const item = state.streamingKey ? state.items.find((i) => i.key === state.streamingKey) : undefined
  const last = item?.kind === "assistant" ? item.blocks.at(-1) : undefined
  if (last?.type === "toolCall") return last.name ? `Bereitet ${last.name} vor` : "Bereitet einen Werkzeugaufruf vor"
  if (last?.type === "text") return "Schreibt"
  // Nach dem Neuladen fehlen tool_execution_start und streamingKey: Endet der Verlauf mit einer
  // Antwort, deren Werkzeugaufruf noch kein Ergebnis hat, läuft genau dieses Werkzeug.
  const tail = state.items.at(-1)
  if (!item && tail?.kind === "assistant") {
    const open = tail.blocks.find((b) => b.type === "toolCall" && b.id && !state.tools[b.id]?.result && !state.tools[b.id]?.isError)
    if (open?.type === "toolCall") return open.name ? `Führt ${open.name} aus` : "Führt ein Werkzeug aus"
  }
  return "Denkt"
}

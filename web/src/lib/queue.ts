// Queue of a chat: messages sent during a run (or while resuming). The orchestrator
// holds them (poc/API.md, "Queue"); the UI shows them above the
// input field until they are handed over.
import type { AutoHeldEvent, Chat, HoldReason, QueueEntry } from "@/api/types"
import { systemEntryLabel } from "@/lib/systemnote"

/** Queued, but the server's response is still pending (optimistic). */
export type LocalQueued = { key: string; text: string; attachments: string[] }

/**
 * `system`: orchestrator note (kind "system", e.g. the end of a background task);
 * `label`: its short line, if readable. Only the server's mark counts, never the text.
 */
export type QueueRow = { key: string; id?: string; text: string; attachments: string[]; sending: boolean; system: boolean; label?: string }

/** Server entries in order, followed by the not yet confirmed ones. */
export function queueRows(server: QueueEntry[], local: LocalQueued[]): QueueRow[] {
  return [
    ...server.map((e) => {
      const system = e.kind === "system"
      const row: QueueRow = { key: e.id, id: e.id, text: e.text, attachments: e.attachments ?? [], sending: false, system }
      if (system) row.label = systemEntryLabel(e)
      return row
    }),
    ...local.map((l) => ({ key: l.key, text: l.text, attachments: l.attachments, sending: true, system: false })),
  ]
}

/**
 * Will a new message probably be queued? Then the UI shows it right away in the
 * queue instead of the history. The server's response (`queued`) remains authoritative.
 */
export function expectQueued(chat: Pick<Chat, "running" | "resuming">, inFlight: number): boolean {
  return chat.running || !!chat.resuming || inFlight > 0
}

/** Single-line preview of a queued text. */
export function queuePreview(text: string, max = 140): string {
  const one = text.split(/\s+/).filter(Boolean).join(" ")
  return one.length > max ? `${one.slice(0, max).trimEnd()} …` : one
}

/** Why queued entries are held back (hold_reason on the chat), for the line above the input field. */
export function holdReasonText(r: HoldReason | undefined): string | undefined {
  switch (r) {
    case "abort":
      return "paused after abort"
    case "wake_limit":
      return "limit of wake-ups per hour reached"
    case "auto_turns":
      return "limit of turns without the user reached"
  }
  return undefined
}

/** Notice for SSE "auto_held": notes stay queued because a limit has been reached. */
export function autoHeldText(e: AutoHeldEvent): string {
  const why =
    e.reason === "auto_turns"
      ? `${e.count} turns without the user in a row (limit ${e.limit})`
      : `limit of wake-ups per hour reached (${e.limit})`
  return `Notes to the agent stay queued: ${why}. They go out with the next message or via "Send now".`
}

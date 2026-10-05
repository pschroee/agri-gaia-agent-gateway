// Warteschlange eines Chats: Nachrichten, die während eines Laufs (oder beim Fortsetzen) gesendet
// wurden. Der Orchestrator hält sie (poc/API.md, „Warteschlange“); die UI zeigt sie über dem
// Eingabefeld, bis sie übergeben sind.
import type { AutoHeldEvent, Chat, HoldReason, QueueEntry } from "@/api/types"
import { systemEntryLabel } from "@/lib/systemnote"

/** Eingereiht, aber die Antwort des Servers steht noch aus (optimistisch). */
export type LocalQueued = { key: string; text: string; attachments: string[] }

/**
 * `system`: Meldung des Orchestrators (kind "system", etwa das Ende einer Hintergrundaufgabe);
 * `label`: deren Kurzzeile, sofern lesbar. Nur das Serverkennzeichen zählt, nie der Text.
 */
export type QueueRow = { key: string; id?: string; text: string; attachments: string[]; sending: boolean; system: boolean; label?: string }

/** Server-Einträge in Reihenfolge, danach die noch unbestätigten. */
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
 * Wird eine neue Nachricht voraussichtlich eingereiht? Dann zeigt die UI sie gleich in der
 * Warteschlange statt im Verlauf. Maßgeblich bleibt die Antwort des Servers (`queued`).
 */
export function expectQueued(chat: Pick<Chat, "running" | "resuming">, inFlight: number): boolean {
  return chat.running || !!chat.resuming || inFlight > 0
}

/** Einzeilige Vorschau eines eingereihten Texts. */
export function queuePreview(text: string, max = 140): string {
  const one = text.split(/\s+/).filter(Boolean).join(" ")
  return one.length > max ? `${one.slice(0, max).trimEnd()} …` : one
}

/** Warum Eingereihtes zurückgehalten ist (hold_reason am Chat), für die Zeile über dem Eingabefeld. */
export function holdReasonText(r: HoldReason | undefined): string | undefined {
  switch (r) {
    case "abort":
      return "angehalten nach Abbruch"
    case "wake_limit":
      return "Grenze der Weckrufe je Stunde erreicht"
    case "auto_turns":
      return "Grenze der Durchgänge ohne Nutzer erreicht"
  }
  return undefined
}

/** Hinweis bei SSE „auto_held“: Meldungen bleiben eingereiht, weil eine Grenze erreicht ist. */
export function autoHeldText(e: AutoHeldEvent): string {
  const why =
    e.reason === "auto_turns"
      ? `${e.count} Durchgänge ohne Nutzer in Folge (Grenze ${e.limit})`
      : `Grenze der Weckrufe je Stunde erreicht (${e.limit})`
  return `Meldungen an den Agenten bleiben eingereiht: ${why}. Sie gehen mit der nächsten Nachricht oder über „Jetzt senden“.`
}

import { describe, expect, it } from "vitest"
import type { PiEvent, StoredMessage } from "@/api/types"
import { chatRunSince, formatElapsed, formatStepDuration, liveActivity, runStartOf, toolDurationMs } from "./runtime"
import { applyPiEvent, emptyTranscript, hydrate, type TranscriptState } from "./stream"

const ev = (type: string, rest: Record<string, unknown> = {}): PiEvent => ({ type, ...rest })
/** Ereignisse mit Empfangszeit (ms) anwenden. */
const at = (events: [number, PiEvent][], start: TranscriptState = emptyTranscript()) =>
  events.reduce((s, [t, e]) => applyPiEvent(s, e, t), start)
const stored = (seq: number, created: string, message: StoredMessage["message"]): StoredMessage => ({
  seq,
  role: message.role,
  message,
  created_at: `2026-09-29T10:${created}Z`,
})
const T0 = Date.parse("2026-09-29T10:00:00Z")

describe("formatElapsed", () => {
  it("zeigt unter einer Minute Sekunden", () => {
    expect(formatElapsed(0)).toBe("0 s")
    expect(formatElapsed(999)).toBe("0 s")
    expect(formatElapsed(12_400)).toBe("12 s")
    expect(formatElapsed(59_999)).toBe("59 s")
  })
  it("zeigt ab einer Minute m:ss und ab einer Stunde h:mm:ss", () => {
    expect(formatElapsed(60_000)).toBe("1:00")
    expect(formatElapsed(65_000)).toBe("1:05")
    expect(formatElapsed(59 * 60_000 + 59_000)).toBe("59:59")
    expect(formatElapsed(3_723_000)).toBe("1:02:03")
    expect(formatElapsed(36_000_000)).toBe("10:00:00")
  })
  it("macht aus negativen und ungültigen Werten nichts Falsches", () => {
    expect(formatElapsed(-500)).toBe("0 s")
    expect(formatElapsed(Number.NaN)).toBe("")
  })
})

describe("formatStepDuration", () => {
  it("zeigt kurze Schritte als „< 1 s“, sonst wie formatElapsed", () => {
    expect(formatStepDuration(300)).toBe("< 1 s")
    expect(formatStepDuration(1_500)).toBe("1 s")
    expect(formatStepDuration(65_000)).toBe("1:05")
    expect(formatStepDuration(undefined)).toBe("")
  })
})

describe("Laufzeit: live aus Ereignissen", () => {
  it("merkt sich den Start bei agent_start und vergisst ihn bei agent_settled", () => {
    let s = at([[T0, ev("agent_start")]])
    expect(runStartOf(s, true)).toBe(T0)
    s = at([[T0 + 9000, ev("agent_settled")]], s)
    expect(s.runStart).toBeUndefined()
    expect(runStartOf(s, false)).toBeUndefined()
  })

  it("misst Werkzeuge von tool_execution_start bis _end; laufende zählen bis jetzt", () => {
    let s = at([
      [T0, ev("agent_start")],
      [T0 + 1000, ev("tool_execution_start", { toolCallId: "t1", toolName: "bash" })],
    ])
    expect(toolDurationMs(s.tools.t1, T0 + 4000)).toBe(3000)
    s = at([[T0 + 6500, ev("tool_execution_end", { toolCallId: "t1", toolName: "bash", result: "ok" })]], s)
    expect(toolDurationMs(s.tools.t1, T0 + 99_000)).toBe(5500)
  })

  it("gibt der Antwort, die den Lauf beendet, die Gesamtdauer seit agent_start", () => {
    const s = at([
      [T0, ev("agent_start")],
      [T0 + 100, ev("message_end", { message: { role: "user", content: "Hallo" } })],
      [T0 + 2000, ev("message_end", { message: { role: "assistant", content: [], stopReason: "toolUse" } })],
      [T0 + 34_000, ev("message_end", { message: { role: "assistant", content: [], stopReason: "stop" } })],
    ])
    const [, zwischen, ende] = s.items
    if (zwischen.kind !== "assistant" || ende.kind !== "assistant") throw new Error()
    expect(zwischen.durationMs).toBeUndefined()
    expect(ende.durationMs).toBe(34_000)
  })

  it("rät keine Dauer, wenn der Start des Laufs nicht beobachtet wurde", () => {
    const s = at([[T0, ev("message_end", { message: { role: "assistant", content: [], stopReason: "stop" } })]])
    const item = s.items[0]
    if (item.kind !== "assistant") throw new Error()
    expect(item.durationMs).toBeUndefined()
  })

  it("behält den Start beim Neuladen während des Laufs", () => {
    const s = hydrate(at([[T0, ev("agent_start")]]), [])
    expect(s.runStart).toBe(T0)
  })
})

describe("Laufzeit: nach dem Neuladen aus Zeitstempeln", () => {
  const history = [
    stored(1, "00:00", { role: "user", content: [{ type: "text", text: "Rechne" }] }),
    stored(2, "00:03", { role: "assistant", stopReason: "toolUse", content: [{ type: "toolCall", id: "t1", name: "bash", arguments: {} }] }),
    stored(3, "00:10", { role: "toolResult", toolCallId: "t1", toolName: "bash", content: [{ type: "text", text: "42" }] }),
    stored(4, "00:12", {
      role: "assistant",
      stopReason: "toolUse",
      content: [
        { type: "toolCall", id: "t2", name: "read", arguments: {} },
        { type: "toolCall", id: "t3", name: "read", arguments: {} },
      ],
    }),
    stored(5, "00:13", { role: "toolResult", toolCallId: "t2", toolName: "read", content: "a" }),
    stored(6, "00:14", { role: "toolResult", toolCallId: "t3", toolName: "read", content: "b" }),
    stored(7, "00:34", { role: "assistant", stopReason: "stop", content: [{ type: "text", text: "42" }] }),
  ]

  it("rekonstruiert die Gesamtdauer der Antwort aus Nutzernachricht und Antwortende", () => {
    const s = hydrate(emptyTranscript(), history)
    const last = s.items.at(-1)
    if (last?.kind !== "assistant") throw new Error()
    expect(last.durationMs).toBe(34_000)
    const mid = s.items[1]
    if (mid.kind !== "assistant") throw new Error()
    expect(mid.durationMs).toBeUndefined()
  })

  it("rekonstruiert die Werkzeugdauer nur, wenn die Antwort genau einen Aufruf hatte", () => {
    const s = hydrate(emptyTranscript(), history)
    expect(toolDurationMs(s.tools.t1, 0)).toBe(7000)
    // zwei Aufrufe in einer Antwort: Start je Aufruf unbekannt, also keine Angabe
    expect(toolDurationMs(s.tools.t2, 0)).toBeUndefined()
    expect(toolDurationMs(s.tools.t3, 0)).toBeUndefined()
  })

  it("zeigt nichts ohne Nutzernachricht vor der Antwort oder ohne Zeitstempel", () => {
    const s = hydrate(emptyTranscript(), [stored(1, "00:05", { role: "assistant", stopReason: "stop", content: [] })])
    const a = s.items[0]
    if (a.kind !== "assistant") throw new Error()
    expect(a.durationMs).toBeUndefined()
    const bad = hydrate(emptyTranscript(), [
      { ...stored(1, "00:00", { role: "user", content: "x" }), created_at: "kaputt" },
      stored(2, "00:05", { role: "assistant", stopReason: "stop", content: [] }),
    ])
    const b = bad.items[1]
    if (b.kind !== "assistant") throw new Error()
    expect(b.durationMs).toBeUndefined()
  })

  it("nimmt während eines Laufs nach dem Neuladen die letzte Nutzernachricht als Start", () => {
    const s = hydrate(emptyTranscript(), history.slice(0, 3))
    expect(runStartOf(s, true)).toBe(T0)
    expect(runStartOf(s, false)).toBeUndefined()
  })
})

describe("chatRunSince", () => {
  it("nimmt running_since des Orchestrators, sonst den Start aus dem Verlauf, und nichts, wenn der Chat ruht", () => {
    const s = at([[T0, ev("agent_start")]])
    expect(chatRunSince({ running: true, running_since: "2026-09-29T09:59:00Z" }, s)).toBe(T0 - 60_000)
    expect(chatRunSince({ running: true }, s)).toBe(T0)
    expect(chatRunSince({ running: true, running_since: "kaputt" }, s)).toBe(T0)
    expect(chatRunSince({ running: true })).toBeUndefined()
    expect(chatRunSince({ running: false, running_since: "2026-09-29T09:59:00Z" }, s)).toBeUndefined()
  })
})

describe("liveActivity", () => {
  it("nach dem Neuladen mitten in einem Werkzeugaufruf: führt das Werkzeug aus, statt zu denken", () => {
    const s = hydrate(emptyTranscript(), [
      stored(1, "00:00", { role: "user", content: [{ type: "text", text: "los" }] }),
      stored(2, "00:02", {
        role: "assistant",
        content: [{ type: "toolCall", id: "t9", name: "bash", arguments: { command: "sleep 12" } }],
        stopReason: "toolUse",
      }),
    ])
    expect(liveActivity(s)).toBe("Führt bash aus")
  })
  it("benennt, was der Agent gerade tut", () => {
    let s = at([[T0, ev("agent_start")]])
    expect(liveActivity(s)).toBe("Denkt")
    s = at([[T0, ev("message_start", { message: { role: "assistant", content: [] } })]], s)
    s = at([[T0, ev("message_update", { assistantMessageEvent: { type: "text_delta", delta: "Hal" } })]], s)
    expect(liveActivity(s)).toBe("Schreibt")
    s = at([[T0, ev("message_update", { assistantMessageEvent: { type: "toolcall_start", contentIndex: 1, id: "t1", toolName: "bash" } })]], s)
    expect(liveActivity(s)).toBe("Bereitet bash vor")
    s = at([[T0, ev("message_end", { message: { role: "assistant", content: [], stopReason: "toolUse" } })]], s)
    s = at([[T0, ev("tool_execution_start", { toolCallId: "t1", toolName: "bash" })]], s)
    expect(liveActivity(s)).toBe("Führt bash aus")
  })
})

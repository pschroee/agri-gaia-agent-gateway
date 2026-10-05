import { describe, expect, it } from "vitest"
import type { PiEvent, StoredMessage } from "@/api/types"
import {
  addPending,
  applyCompactionError,
  applyPiEvent,
  applyQueueDelivered,
  applyUserMeta,
  compactionNotice,
  emptyTranscript,
  hydrate,
  type TranscriptState,
} from "./stream"

const ev = (type: string, rest: Record<string, unknown> = {}): PiEvent => ({ type, ...rest })
const upd = (assistantMessageEvent: Record<string, unknown>) =>
  ev("message_update", { assistantMessageEvent })
const run = (events: PiEvent[], start: TranscriptState = emptyTranscript()) =>
  events.reduce((s, e) => applyPiEvent(s, e), start)

const stored = (seq: number, message: StoredMessage["message"]): StoredMessage => ({
  seq,
  role: message.role,
  message,
  created_at: "2026-09-29T10:00:00Z",
})

describe("applyPiEvent: Text", () => {
  it("setzt Text-Deltas Token für Token zusammen", () => {
    const s = run([
      ev("message_start", { message: { role: "assistant", content: [] } }),
      upd({ type: "text_delta", contentIndex: 0, delta: "Hal" }),
      upd({ type: "text_delta", contentIndex: 0, delta: "lo" }),
      upd({ type: "text_delta", contentIndex: 0, delta: " Welt" }),
    ])
    expect(s.items).toHaveLength(1)
    const item = s.items[0]
    expect(item.kind).toBe("assistant")
    if (item.kind !== "assistant") throw new Error()
    expect(item.streaming).toBe(true)
    expect(item.blocks).toEqual([{ type: "text", text: "Hallo Welt" }])
  })

  it("hängt ohne contentIndex an den letzten Block gleichen Typs an", () => {
    const s = run([
      ev("message_start", { message: { role: "assistant", content: [] } }),
      upd({ type: "text_delta", delta: "a" }),
      upd({ type: "text_delta", delta: "b" }),
    ])
    const item = s.items[0]
    if (item.kind !== "assistant") throw new Error()
    expect(item.blocks).toEqual([{ type: "text", text: "ab" }])
  })

  it("legt eine Antwort an, wenn message_start verpasst wurde", () => {
    const s = run([upd({ type: "text_delta", contentIndex: 0, delta: "x" })])
    expect(s.items).toHaveLength(1)
    expect(s.items[0].kind).toBe("assistant")
  })
})

describe("applyPiEvent: Thinking", () => {
  it("führt Thinking und Text als getrennte Blöcke", () => {
    const s = run([
      ev("message_start", { message: { role: "assistant", content: [] } }),
      upd({ type: "thinking_delta", contentIndex: 0, delta: "Ich über" }),
      upd({ type: "thinking_delta", contentIndex: 0, delta: "lege" }),
      upd({ type: "text_delta", contentIndex: 1, delta: "Antwort" }),
    ])
    const item = s.items[0]
    if (item.kind !== "assistant") throw new Error()
    expect(item.blocks).toEqual([
      { type: "thinking", thinking: "Ich überlege" },
      { type: "text", text: "Antwort" },
    ])
  })
})

describe("applyPiEvent: Werkzeugaufrufe", () => {
  it("baut einen toolCall aus start, delta und end", () => {
    const s = run([
      ev("message_start", { message: { role: "assistant", content: [] } }),
      upd({ type: "toolcall_start", contentIndex: 0, id: "c1", toolName: "bash" }),
      upd({ type: "toolcall_delta", contentIndex: 0, delta: '{"command":' }),
      upd({ type: "toolcall_delta", contentIndex: 0, delta: '"ls"}' }),
    ])
    let item = s.items[0]
    if (item.kind !== "assistant") throw new Error()
    expect(item.blocks[0]).toMatchObject({ type: "toolCall", id: "c1", name: "bash", argsText: '{"command":"ls"}' })

    const s2 = applyPiEvent(
      s,
      upd({ type: "toolcall_end", contentIndex: 0, toolCall: { type: "toolCall", id: "c1", name: "bash", arguments: { command: "ls" } } }),
    )
    item = s2.items[0]
    if (item.kind !== "assistant") throw new Error()
    expect(item.blocks[0]).toMatchObject({ type: "toolCall", id: "c1", name: "bash", arguments: { command: "ls" } })
  })

  it("liest id und Name aus partial, wenn sie nicht direkt am Ereignis stehen", () => {
    const s = run([
      ev("message_start", { message: { role: "assistant", content: [] } }),
      upd({
        type: "toolcall_start",
        contentIndex: 0,
        partial: { role: "assistant", content: [{ type: "toolCall", id: "c9", name: "read", arguments: {} }] },
      }),
    ])
    const item = s.items[0]
    if (item.kind !== "assistant") throw new Error()
    expect(item.blocks[0]).toMatchObject({ type: "toolCall", id: "c9", name: "read" })
  })

  it("verfolgt die Ausführung: start, update ersetzt Ausgabe, end setzt Ergebnis", () => {
    let s = run([
      ev("tool_execution_start", { toolCallId: "c1", toolName: "bash", args: { command: "ls" } }),
      ev("tool_execution_update", { toolCallId: "c1", toolName: "bash", partialResult: { content: [{ type: "text", text: "a\n" }] } }),
      ev("tool_execution_update", { toolCallId: "c1", toolName: "bash", partialResult: { content: [{ type: "text", text: "a\nb\n" }] } }),
    ])
    expect(s.tools.c1).toMatchObject({ toolName: "bash", running: true, output: "a\nb\n" })

    s = applyPiEvent(
      s,
      ev("tool_execution_end", { toolCallId: "c1", toolName: "bash", result: { content: [{ type: "text", text: "fertig" }] }, isError: false }),
    )
    expect(s.tools.c1).toMatchObject({ running: false, result: "fertig", isError: false })
  })

  it("übernimmt einen Fehler aus tool_execution_end", () => {
    const s = run([
      ev("tool_execution_start", { toolCallId: "c2", toolName: "bash", args: {} }),
      ev("tool_execution_end", { toolCallId: "c2", toolName: "bash", result: { content: [{ type: "text", text: "exit 1" }] }, isError: true }),
    ])
    expect(s.tools.c2).toMatchObject({ running: false, result: "exit 1", isError: true })
  })

  it("übernimmt toolResult-Nachrichten in die Werkzeugtabelle, ohne einen Eintrag im Verlauf", () => {
    const s = run([
      ev("message_end", {
        message: { role: "toolResult", toolCallId: "c3", toolName: "bash", content: [{ type: "text", text: "ok" }], isError: false },
      }),
    ])
    expect(s.items).toHaveLength(0)
    expect(s.tools.c3).toMatchObject({ result: "ok", isError: false, running: false })
  })
})

describe("applyPiEvent: message_end", () => {
  it("ersetzt das Zusammengesetzte durch die maßgebliche Nachricht", () => {
    const final = {
      role: "assistant",
      content: [
        { type: "thinking", thinking: "t" },
        { type: "text", text: "Endgültig" },
      ],
      usage: { input: 10, output: 5, cacheRead: 0, cost: { total: 0.001 } },
      stopReason: "stop",
    }
    const s = run([
      ev("message_start", { message: { role: "assistant", content: [] } }),
      upd({ type: "text_delta", contentIndex: 0, delta: "Vorläu" }),
      ev("message_end", { message: final }),
    ])
    expect(s.items).toHaveLength(1)
    const item = s.items[0]
    if (item.kind !== "assistant") throw new Error()
    expect(item.streaming).toBe(false)
    expect(item.blocks).toEqual(final.content)
    expect(item.usage?.cost?.total).toBe(0.001)
    expect(item.stopReason).toBe("stop")
  })

  it("hängt eine Nutzernachricht aus message_end an", () => {
    const s = run([
      ev("message_start", { message: { role: "user", content: [{ type: "text", text: "Hi" }] } }),
      ev("message_end", { message: { role: "user", content: [{ type: "text", text: "Hi" }] } }),
    ])
    expect(s.items).toEqual([expect.objectContaining({ kind: "user", text: "Hi" })])
  })

  it("akzeptiert Nutzertext als Zeichenkette", () => {
    const s = run([ev("message_end", { message: { role: "user", content: "direkt" } })])
    expect(s.items).toEqual([expect.objectContaining({ kind: "user", text: "direkt" })])
  })

  it("zwei Antworten nacheinander ergeben zwei Einträge", () => {
    const s = run([
      ev("message_start", { message: { role: "assistant", content: [] } }),
      ev("message_end", { message: { role: "assistant", content: [{ type: "text", text: "1" }] } }),
      ev("message_start", { message: { role: "assistant", content: [] } }),
      upd({ type: "text_delta", contentIndex: 0, delta: "2" }),
    ])
    expect(s.items).toHaveLength(2)
  })
})

describe("applyPiEvent: system und Unbekanntes", () => {
  it("ignoriert system-Nachrichten", () => {
    const s = run([
      ev("message_start", { message: { role: "system", content: [{ type: "text", text: "geheim" }] } }),
      ev("message_end", { message: { role: "system", content: [{ type: "text", text: "geheim" }] } }),
    ])
    expect(s.items).toHaveLength(0)
  })

  it("ignoriert Deltas, die zu einer system-Nachricht gehören", () => {
    const s = run([
      ev("message_start", { message: { role: "system", content: [] } }),
      upd({ type: "text_delta", contentIndex: 0, delta: "x" }),
      ev("message_end", { message: { role: "system", content: [] } }),
    ])
    expect(s.items).toHaveLength(0)
  })

  it("lässt unbekannte Ereignisse den Zustand unverändert", () => {
    const start = emptyTranscript()
    expect(applyPiEvent(start, ev("turn_start"))).toBe(start)
    expect(applyPiEvent(start, ev("auto_retry_start"))).toBe(start)
  })

  it("beendet bei agent_end ein hängengebliebenes Streaming", () => {
    const s = run([
      ev("message_start", { message: { role: "assistant", content: [] } }),
      upd({ type: "text_delta", contentIndex: 0, delta: "x" }),
      ev("agent_end"),
    ])
    const item = s.items[0]
    if (item.kind !== "assistant") throw new Error()
    expect(item.streaming).toBe(false)
  })

  it("markiert laufende Werkzeuge bei agent_end als nicht mehr laufend", () => {
    const s = run([ev("tool_execution_start", { toolCallId: "c1", toolName: "bash", args: {} }), ev("agent_end")])
    expect(s.tools.c1.running).toBe(false)
  })
})

describe("hydrate: Historie und Live gemischt", () => {
  const history: StoredMessage[] = [
    stored(1, { role: "user", content: [{ type: "text", text: "Liste Dateien" }] }),
    stored(2, {
      role: "assistant",
      content: [{ type: "toolCall", id: "c1", name: "bash", arguments: { command: "ls" } }],
      stopReason: "toolUse",
    }),
    stored(3, { role: "toolResult", toolCallId: "c1", toolName: "bash", content: [{ type: "text", text: "a.txt" }], isError: false }),
    stored(4, { role: "system", content: [{ type: "text", text: "x" }] }),
  ]

  it("baut den Verlauf aus der Historie; toolResult landet in der Werkzeugtabelle, system entfällt", () => {
    const s = hydrate(emptyTranscript(), history)
    expect(s.items.map((i) => i.kind)).toEqual(["user", "assistant"])
    expect(s.tools.c1).toMatchObject({ result: "a.txt", running: false, isError: false })
  })

  it("setzt Live-Ereignisse nach der Historie fort", () => {
    const s = run(
      [
        ev("message_start", { message: { role: "assistant", content: [] } }),
        upd({ type: "text_delta", contentIndex: 0, delta: "Da liegt a.txt" }),
      ],
      hydrate(emptyTranscript(), history),
    )
    expect(s.items.map((i) => i.kind)).toEqual(["user", "assistant", "assistant"])
    const last = s.items[2]
    if (last.kind !== "assistant") throw new Error()
    expect(last.streaming).toBe(true)
    expect(last.blocks).toEqual([{ type: "text", text: "Da liegt a.txt" }])
  })

  it("behält beim Neuladen eine noch laufende Antwort und laufende Werkzeuge", () => {
    const live = run([
      ev("message_start", { message: { role: "assistant", content: [] } }),
      upd({ type: "text_delta", contentIndex: 0, delta: "halb" }),
      ev("tool_execution_start", { toolCallId: "c7", toolName: "bash", args: {} }),
    ])
    const s = hydrate(live, history)
    expect(s.items.map((i) => i.kind)).toEqual(["user", "assistant", "assistant"])
    expect(s.tools.c7?.running).toBe(true)
  })

  it("ersetzt beim Neuladen bereits abgeschlossene Live-Einträge durch die Historie", () => {
    const live = run([ev("message_end", { message: { role: "user", content: "Liste Dateien" } })])
    const s = hydrate(live, history)
    expect(s.items.filter((i) => i.kind === "user")).toHaveLength(1)
  })
})

describe("hydrate: Tarifkosten je Antwort", () => {
  it("übernimmt cost und peak aus der gespeicherten Nachricht", () => {
    const s = hydrate(emptyTranscript(), [
      {
        seq: 1,
        role: "assistant",
        created_at: "2026-09-29T10:00:00Z",
        cost: 0.0012,
        peak: false,
        message: { role: "assistant", content: [{ type: "text", text: "hi" }] },
      },
    ])
    expect(s.items[0]).toMatchObject({ kind: "assistant", cost: 0.0012, peak: false })
  })
})

describe("Kompaktierung", () => {
  it("zeigt eine laufende Kompaktierung und ersetzt sie durch das Ergebnis", () => {
    const s1 = run([ev("compaction_start", { reason: "threshold" })])
    expect(s1.items).toEqual([expect.objectContaining({ kind: "compaction", reason: "threshold", running: true })])
    const s2 = run(
      [
        ev("compaction_end", {
          reason: "threshold",
          aborted: false,
          willRetry: false,
          result: { summary: "## Ziel\nDaten", tokensBefore: 152000, estimatedTokensAfter: 32000, usage: { input: 10 } },
        }),
      ],
      s1,
    )
    expect(s2.items).toHaveLength(1)
    expect(s2.items[0]).toMatchObject({
      kind: "compaction",
      running: false,
      reason: "threshold",
      summary: "## Ziel\nDaten",
      tokensBefore: 152000,
      tokensAfter: 32000,
      aborted: false,
    })
  })

  it("hält Abbruch und Fehler fest", () => {
    const s = run([
      ev("compaction_start", { reason: "manual" }),
      ev("compaction_end", { reason: "manual", aborted: true, errorMessage: "abgebrochen vom Nutzer" }),
    ])
    expect(s.items[0]).toMatchObject({ kind: "compaction", running: false, aborted: true, errorMessage: "abgebrochen vom Nutzer" })
  })

  it("legt bei compaction_end ohne Start einen Eintrag an", () => {
    const s = run([ev("compaction_end", { reason: "overflow", aborted: false, result: { summary: "x", tokensBefore: 5 } })])
    expect(s.items[0]).toMatchObject({ kind: "compaction", running: false, reason: "overflow", tokensBefore: 5 })
  })

  it("baut Kompaktierungen aus der Historie auf, samt Kosten", () => {
    const s = hydrate(emptyTranscript(), [
      stored(1, { role: "user", content: [{ type: "text", text: "a" }] }),
      {
        seq: 2,
        role: "compaction",
        created_at: "2026-09-29T10:00:00Z",
        cost: 0.003,
        message: { role: "compaction", reason: "manual", summary: "S", tokensBefore: 100, estimatedTokensAfter: 20 },
      },
    ])
    expect(s.items[1]).toMatchObject({
      kind: "compaction",
      key: "seq-2",
      reason: "manual",
      running: false,
      summary: "S",
      tokensBefore: 100,
      tokensAfter: 20,
      cost: 0.003,
    })
  })

  it("behält beim Neuladen eine noch laufende Kompaktierung", () => {
    const live = run([ev("compaction_start", { reason: "manual" })])
    const s = hydrate(live, [stored(1, { role: "user", content: [{ type: "text", text: "a" }] })])
    expect(s.items.map((i) => i.kind)).toEqual(["user", "compaction"])
    const s2 = applyPiEvent(s, ev("compaction_end", { reason: "manual", aborted: false, result: { summary: "x", tokensBefore: 1 } }))
    expect(s2.items).toHaveLength(2)
    expect(s2.items[1]).toMatchObject({ running: false, summary: "x" })
  })
})

describe("Kompaktierung: fehlgeschlagen", () => {
  it("übersetzt bekannte Meldungen von pi", () => {
    expect(compactionNotice("Nothing to compact (session too small)")).toBe(
      "Kompaktierung nicht möglich: noch zu wenig Verlauf zum Zusammenfassen",
    )
    expect(compactionNotice("Kompaktierung fehlgeschlagen: Already compacted")).toBe(
      "Kompaktierung nicht nötig: bereits zusammengefasst",
    )
    expect(compactionNotice("Kompaktierung fehlgeschlagen: timeout")).toBe("Kompaktierung fehlgeschlagen: timeout")
    expect(compactionNotice("boom")).toBe("Kompaktierung fehlgeschlagen: boom")
    expect(compactionNotice(undefined)).toBe("Kompaktierung fehlgeschlagen")
  })

  it("markiert compaction_end mit errorMessage als fehlgeschlagen", () => {
    const s = run([
      ev("compaction_start", { reason: "manual" }),
      ev("compaction_end", { reason: "manual", aborted: false, result: null, errorMessage: "Nothing to compact (session too small)" }),
    ])
    expect(s.items).toHaveLength(1)
    expect(s.items[0]).toMatchObject({ kind: "compaction", running: false, failed: true })
  })

  it("wertet compaction_end mit result:null als fehlgeschlagen", () => {
    const s = run([ev("compaction_end", { reason: "manual", aborted: false, result: null })])
    expect(s.items[0]).toMatchObject({ kind: "compaction", failed: true })
  })

  it("legt bei einem error-Ereignis einen Hinweis an und verdoppelt ihn nicht", () => {
    const msg = "Kompaktierung fehlgeschlagen: Nothing to compact (session too small)"
    let s = applyCompactionError(emptyTranscript(), msg)
    expect(s.items).toEqual([expect.objectContaining({ kind: "compaction", failed: true, errorMessage: msg, running: false })])
    s = applyPiEvent(s, ev("compaction_end", { reason: "manual", result: null, errorMessage: "Nothing to compact (session too small)" }))
    expect(s.items).toHaveLength(1)
    s = applyCompactionError(s, msg)
    expect(s.items).toHaveLength(1)
  })

  it("beendet eine laufende Kompaktierung beim error-Ereignis", () => {
    let s = run([ev("compaction_start", { reason: "manual" })])
    s = applyCompactionError(s, "Kompaktierung fehlgeschlagen: kaputt")
    expect(s.items).toHaveLength(1)
    expect(s.items[0]).toMatchObject({ running: false, failed: true, errorMessage: "Kompaktierung fehlgeschlagen: kaputt" })
    expect(s.compactingKey).toBeUndefined()
  })

  it("behält den Hinweis beim Neuladen an seiner Stelle", () => {
    const hist = [
      stored(1, { role: "user", content: [{ type: "text", text: "a" }] }),
      stored(2, { role: "assistant", content: [{ type: "text", text: "b" }] }),
    ]
    let s = hydrate(emptyTranscript(), hist)
    s = applyPiEvent(s, ev("compaction_end", { reason: "manual", result: null, errorMessage: "Already compacted" }))
    s = hydrate(s, hist)
    expect(s.items.map((i) => i.kind)).toEqual(["user", "assistant", "compaction"])
    // später kommen weitere Nachrichten dazu; der Hinweis bleibt hinter seq 2
    s = hydrate(s, [
      ...hist,
      stored(3, { role: "user", content: [{ type: "text", text: "c" }] }),
    ])
    expect(s.items.map((i) => i.key)).toEqual(["seq-1", "seq-2", expect.stringMatching(/^live-/), "seq-3"])
  })

  it("übernimmt erfolgreiche Live-Kompaktierungen nicht (die kommen aus der Historie)", () => {
    let s = run([ev("compaction_end", { reason: "manual", result: { summary: "x", tokensBefore: 1 } })])
    s = hydrate(s, [])
    expect(s.items).toHaveLength(0)
  })
})

describe("Zeitpunkte der Einträge", () => {
  it("übernimmt created_at aus der Historie als Zeit", () => {
    const s = hydrate(emptyTranscript(), [
      { ...stored(1, { role: "user", content: [{ type: "text", text: "a" }] }), created_at: "2026-09-29T10:00:00Z" },
      { ...stored(2, { role: "assistant", content: [] }), created_at: "2026-09-29T10:00:05Z" },
    ])
    expect(s.items.map((i) => i.time)).toEqual([Date.parse("2026-09-29T10:00:00Z"), Date.parse("2026-09-29T10:00:05Z")])
  })

  it("nimmt live den timestamp von pi", () => {
    const s = run([
      ev("message_start", { message: { role: "assistant", content: [], timestamp: 1000 } }),
      ev("message_end", { message: { role: "assistant", content: [], timestamp: 1000 } }),
      ev("message_end", { message: { role: "user", content: "x", timestamp: 2000 } }),
    ])
    expect(s.items.map((i) => i.time)).toEqual([1000, 2000])
  })
})

describe("Kennung der Antwort für Bilder (msgKey)", () => {
  it("ist live nach message_end und nach dem Neuladen dieselbe, während des Streamens leer", () => {
    const msg = { role: "assistant", responseId: "resp-7", timestamp: 1700000000000, content: [{ type: "text", text: "![a](a.png)" }] }
    let s = run([ev("message_start", { message: { role: "assistant", content: [] } }), upd({ type: "text_delta", delta: "x" })])
    const live = s.items[0]
    if (live.kind !== "assistant") throw new Error()
    expect(live.msgKey).toBeUndefined()
    s = run([ev("message_end", { message: msg })], s)
    const done = s.items[0]
    if (done.kind !== "assistant") throw new Error()
    expect(done.msgKey).toBe("resp-7")
    const h = hydrate(emptyTranscript(), [stored(2, msg)]).items[0]
    if (h.kind !== "assistant") throw new Error()
    expect(h.msgKey).toBe("resp-7")
  })
})

// Review 3, H1: Herkunft der Nutzernachricht kommt vom Server (user_meta live, Felder der Historie).
describe("Herkunft der Nutzernachricht", () => {
  const meta = { turn_id: 7, trigger: "wake" as const, origin: "system" as const, sources: [{ kind: "system" as const, type: "background", refs: ["bg-1"], marker: "agw-0123456789abcdef" }] }
  it("user_meta gilt für die nächste Nutzernachricht, danach nicht mehr", () => {
    let s = applyUserMeta(emptyTranscript(), meta)
    s = run([ev("message_end", { message: { role: "user", content: "Meldung" } }), ev("message_end", { message: { role: "user", content: "danach" } })], s)
    const users = s.items.filter((i) => i.kind === "user")
    expect(users[0]).toMatchObject({ origin: "system", trigger: "wake", turnId: 7, sources: meta.sources })
    expect(users[1]).not.toHaveProperty("origin")
    expect(s.nextUserMeta).toBeUndefined()
  })
  it("Historie trägt origin, sources und trigger", () => {
    const s = hydrate(emptyTranscript(), [{ ...stored(1, { role: "user", content: "x" }), turn_id: 7, trigger: "wake", origin: "system", sources: meta.sources }])
    expect(s.items[0]).toMatchObject({ kind: "user", origin: "system", trigger: "wake", turnId: 7 })
  })
  it("übergebene Warteschlange zeigt die Herkunft schon vor der Bestätigung durch pi", () => {
    const s = applyQueueDelivered(emptyTranscript(), "q-1", "Meldung", { origin: "system", sources: meta.sources })
    expect(s.pending[0]).toMatchObject({ text: "Meldung", origin: "system", sources: meta.sources })
    const t = applyQueueDelivered(addPending(emptyTranscript(), "p", "weiter"), "q-2", "Meldung\n\nweiter", { origin: "mixed", sources: meta.sources })
    expect(t.pending).toHaveLength(1)
    expect(t.pending[0]).toMatchObject({ key: "p", origin: "mixed" })
  })
})

describe("Meldungen von Erweiterungen (Rolle custom)", () => {
  it("live und nach dem Neuladen als eigene Zeile, nicht als Nutzernachricht", () => {
    const live = run([ev("message_end", { message: { role: "custom", customType: "subagent-notify", content: "Subagent fertig\nBericht liegt vor" } })])
    expect(live.items).toMatchObject([{ kind: "notice", text: "Subagent fertig\nBericht liegt vor", customType: "subagent-notify" }])
    const h = hydrate(emptyTranscript(), [
      { ...stored(1, { role: "custom", customType: "subagent-notify", content: [{ type: "text", text: "fertig" }] }), trigger: "wake" },
    ])
    expect(h.items).toMatchObject([{ kind: "notice", text: "fertig", trigger: "wake", seq: 1 }])
  })
})

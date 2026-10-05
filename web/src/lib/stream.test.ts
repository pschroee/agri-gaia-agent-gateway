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

describe("applyPiEvent: text", () => {
  it("assembles text deltas token by token", () => {
    const s = run([
      ev("message_start", { message: { role: "assistant", content: [] } }),
      upd({ type: "text_delta", contentIndex: 0, delta: "Hel" }),
      upd({ type: "text_delta", contentIndex: 0, delta: "lo" }),
      upd({ type: "text_delta", contentIndex: 0, delta: " world" }),
    ])
    expect(s.items).toHaveLength(1)
    const item = s.items[0]
    expect(item.kind).toBe("assistant")
    if (item.kind !== "assistant") throw new Error()
    expect(item.streaming).toBe(true)
    expect(item.blocks).toEqual([{ type: "text", text: "Hello world" }])
  })

  it("appends to the last block of the same type without contentIndex", () => {
    const s = run([
      ev("message_start", { message: { role: "assistant", content: [] } }),
      upd({ type: "text_delta", delta: "a" }),
      upd({ type: "text_delta", delta: "b" }),
    ])
    const item = s.items[0]
    if (item.kind !== "assistant") throw new Error()
    expect(item.blocks).toEqual([{ type: "text", text: "ab" }])
  })

  it("creates a response if message_start was missed", () => {
    const s = run([upd({ type: "text_delta", contentIndex: 0, delta: "x" })])
    expect(s.items).toHaveLength(1)
    expect(s.items[0].kind).toBe("assistant")
  })
})

describe("applyPiEvent: thinking", () => {
  it("keeps thinking and text as separate blocks", () => {
    const s = run([
      ev("message_start", { message: { role: "assistant", content: [] } }),
      upd({ type: "thinking_delta", contentIndex: 0, delta: "Let me th" }),
      upd({ type: "thinking_delta", contentIndex: 0, delta: "ink" }),
      upd({ type: "text_delta", contentIndex: 1, delta: "Answer" }),
    ])
    const item = s.items[0]
    if (item.kind !== "assistant") throw new Error()
    expect(item.blocks).toEqual([
      { type: "thinking", thinking: "Let me think" },
      { type: "text", text: "Answer" },
    ])
  })
})

describe("applyPiEvent: tool calls", () => {
  it("builds a toolCall from start, delta and end", () => {
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

  it("reads id and name from partial if they are not on the event directly", () => {
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

  it("tracks the execution: start, update replaces output, end sets the result", () => {
    let s = run([
      ev("tool_execution_start", { toolCallId: "c1", toolName: "bash", args: { command: "ls" } }),
      ev("tool_execution_update", { toolCallId: "c1", toolName: "bash", partialResult: { content: [{ type: "text", text: "a\n" }] } }),
      ev("tool_execution_update", { toolCallId: "c1", toolName: "bash", partialResult: { content: [{ type: "text", text: "a\nb\n" }] } }),
    ])
    expect(s.tools.c1).toMatchObject({ toolName: "bash", running: true, output: "a\nb\n" })

    s = applyPiEvent(
      s,
      ev("tool_execution_end", { toolCallId: "c1", toolName: "bash", result: { content: [{ type: "text", text: "done" }] }, isError: false }),
    )
    expect(s.tools.c1).toMatchObject({ running: false, result: "done", isError: false })
  })

  it("takes over an error from tool_execution_end", () => {
    const s = run([
      ev("tool_execution_start", { toolCallId: "c2", toolName: "bash", args: {} }),
      ev("tool_execution_end", { toolCallId: "c2", toolName: "bash", result: { content: [{ type: "text", text: "exit 1" }] }, isError: true }),
    ])
    expect(s.tools.c2).toMatchObject({ running: false, result: "exit 1", isError: true })
  })

  it("takes toolResult messages into the tool table without an entry in the history", () => {
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
  it("replaces the assembled content with the authoritative message", () => {
    const final = {
      role: "assistant",
      content: [
        { type: "thinking", thinking: "t" },
        { type: "text", text: "Final" },
      ],
      usage: { input: 10, output: 5, cacheRead: 0, cost: { total: 0.001 } },
      stopReason: "stop",
    }
    const s = run([
      ev("message_start", { message: { role: "assistant", content: [] } }),
      upd({ type: "text_delta", contentIndex: 0, delta: "Provis" }),
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

  it("appends a user message from message_end", () => {
    const s = run([
      ev("message_start", { message: { role: "user", content: [{ type: "text", text: "Hi" }] } }),
      ev("message_end", { message: { role: "user", content: [{ type: "text", text: "Hi" }] } }),
    ])
    expect(s.items).toEqual([expect.objectContaining({ kind: "user", text: "Hi" })])
  })

  it("accepts user text as a string", () => {
    const s = run([ev("message_end", { message: { role: "user", content: "direct" } })])
    expect(s.items).toEqual([expect.objectContaining({ kind: "user", text: "direct" })])
  })

  it("two responses in a row give two entries", () => {
    const s = run([
      ev("message_start", { message: { role: "assistant", content: [] } }),
      ev("message_end", { message: { role: "assistant", content: [{ type: "text", text: "1" }] } }),
      ev("message_start", { message: { role: "assistant", content: [] } }),
      upd({ type: "text_delta", contentIndex: 0, delta: "2" }),
    ])
    expect(s.items).toHaveLength(2)
  })
})

describe("applyPiEvent: system and unknown", () => {
  it("ignores system messages", () => {
    const s = run([
      ev("message_start", { message: { role: "system", content: [{ type: "text", text: "secret" }] } }),
      ev("message_end", { message: { role: "system", content: [{ type: "text", text: "secret" }] } }),
    ])
    expect(s.items).toHaveLength(0)
  })

  it("ignores deltas belonging to a system message", () => {
    const s = run([
      ev("message_start", { message: { role: "system", content: [] } }),
      upd({ type: "text_delta", contentIndex: 0, delta: "x" }),
      ev("message_end", { message: { role: "system", content: [] } }),
    ])
    expect(s.items).toHaveLength(0)
  })

  it("leaves the state unchanged for unknown events", () => {
    const start = emptyTranscript()
    expect(applyPiEvent(start, ev("turn_start"))).toBe(start)
    expect(applyPiEvent(start, ev("auto_retry_start"))).toBe(start)
  })

  it("ends a stuck streaming at agent_end", () => {
    const s = run([
      ev("message_start", { message: { role: "assistant", content: [] } }),
      upd({ type: "text_delta", contentIndex: 0, delta: "x" }),
      ev("agent_end"),
    ])
    const item = s.items[0]
    if (item.kind !== "assistant") throw new Error()
    expect(item.streaming).toBe(false)
  })

  it("marks running tools as no longer running at agent_end", () => {
    const s = run([ev("tool_execution_start", { toolCallId: "c1", toolName: "bash", args: {} }), ev("agent_end")])
    expect(s.tools.c1.running).toBe(false)
  })
})

describe("hydrate: history and live mixed", () => {
  const history: StoredMessage[] = [
    stored(1, { role: "user", content: [{ type: "text", text: "List files" }] }),
    stored(2, {
      role: "assistant",
      content: [{ type: "toolCall", id: "c1", name: "bash", arguments: { command: "ls" } }],
      stopReason: "toolUse",
    }),
    stored(3, { role: "toolResult", toolCallId: "c1", toolName: "bash", content: [{ type: "text", text: "a.txt" }], isError: false }),
    stored(4, { role: "system", content: [{ type: "text", text: "x" }] }),
  ]

  it("builds the history from the stored messages; toolResult goes into the tool table, system is dropped", () => {
    const s = hydrate(emptyTranscript(), history)
    expect(s.items.map((i) => i.kind)).toEqual(["user", "assistant"])
    expect(s.tools.c1).toMatchObject({ result: "a.txt", running: false, isError: false })
  })

  it("continues live events after the history", () => {
    const s = run(
      [
        ev("message_start", { message: { role: "assistant", content: [] } }),
        upd({ type: "text_delta", contentIndex: 0, delta: "There is a.txt" }),
      ],
      hydrate(emptyTranscript(), history),
    )
    expect(s.items.map((i) => i.kind)).toEqual(["user", "assistant", "assistant"])
    const last = s.items[2]
    if (last.kind !== "assistant") throw new Error()
    expect(last.streaming).toBe(true)
    expect(last.blocks).toEqual([{ type: "text", text: "There is a.txt" }])
  })

  it("keeps a response still running and running tools on reload", () => {
    const live = run([
      ev("message_start", { message: { role: "assistant", content: [] } }),
      upd({ type: "text_delta", contentIndex: 0, delta: "half" }),
      ev("tool_execution_start", { toolCallId: "c7", toolName: "bash", args: {} }),
    ])
    const s = hydrate(live, history)
    expect(s.items.map((i) => i.kind)).toEqual(["user", "assistant", "assistant"])
    expect(s.tools.c7?.running).toBe(true)
  })

  it("replaces already completed live entries with the history on reload", () => {
    const live = run([ev("message_end", { message: { role: "user", content: "List files" } })])
    const s = hydrate(live, history)
    expect(s.items.filter((i) => i.kind === "user")).toHaveLength(1)
  })
})

describe("hydrate: tariff cost per response", () => {
  it("takes cost and peak from the stored message", () => {
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

describe("compaction", () => {
  it("shows a running compaction and replaces it with the result", () => {
    const s1 = run([ev("compaction_start", { reason: "threshold" })])
    expect(s1.items).toEqual([expect.objectContaining({ kind: "compaction", reason: "threshold", running: true })])
    const s2 = run(
      [
        ev("compaction_end", {
          reason: "threshold",
          aborted: false,
          willRetry: false,
          result: { summary: "## Goal\nData", tokensBefore: 152000, estimatedTokensAfter: 32000, usage: { input: 10 } },
        }),
      ],
      s1,
    )
    expect(s2.items).toHaveLength(1)
    expect(s2.items[0]).toMatchObject({
      kind: "compaction",
      running: false,
      reason: "threshold",
      summary: "## Goal\nData",
      tokensBefore: 152000,
      tokensAfter: 32000,
      aborted: false,
    })
  })

  it("records abort and error", () => {
    const s = run([
      ev("compaction_start", { reason: "manual" }),
      ev("compaction_end", { reason: "manual", aborted: true, errorMessage: "aborted by the user" }),
    ])
    expect(s.items[0]).toMatchObject({ kind: "compaction", running: false, aborted: true, errorMessage: "aborted by the user" })
  })

  it("creates an entry at compaction_end without a start", () => {
    const s = run([ev("compaction_end", { reason: "overflow", aborted: false, result: { summary: "x", tokensBefore: 5 } })])
    expect(s.items[0]).toMatchObject({ kind: "compaction", running: false, reason: "overflow", tokensBefore: 5 })
  })

  it("builds compactions from the history, including cost", () => {
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

  it("keeps a compaction still running on reload", () => {
    const live = run([ev("compaction_start", { reason: "manual" })])
    const s = hydrate(live, [stored(1, { role: "user", content: [{ type: "text", text: "a" }] })])
    expect(s.items.map((i) => i.kind)).toEqual(["user", "compaction"])
    const s2 = applyPiEvent(s, ev("compaction_end", { reason: "manual", aborted: false, result: { summary: "x", tokensBefore: 1 } }))
    expect(s2.items).toHaveLength(2)
    expect(s2.items[1]).toMatchObject({ running: false, summary: "x" })
  })
})

describe("compaction: failed", () => {
  // the orchestrator's prefix "Compaction failed:" (internal/chat/manager.go); the German one is from before the translation
  it("rephrases known messages from pi", () => {
    expect(compactionNotice("Nothing to compact (session too small)")).toBe(
      "Compaction not possible: not enough history to summarize yet",
    )
    expect(compactionNotice("Compaction failed: Already compacted")).toBe(
      "Compaction not needed: already summarized",
    )
    expect(compactionNotice("Compaction failed: timeout")).toBe("Compaction failed: timeout")
    expect(compactionNotice("Kompaktierung fehlgeschlagen: timeout")).toBe("Compaction failed: timeout")
    expect(compactionNotice("boom")).toBe("Compaction failed: boom")
    expect(compactionNotice(undefined)).toBe("Compaction failed")
  })

  it("marks compaction_end with errorMessage as failed", () => {
    const s = run([
      ev("compaction_start", { reason: "manual" }),
      ev("compaction_end", { reason: "manual", aborted: false, result: null, errorMessage: "Nothing to compact (session too small)" }),
    ])
    expect(s.items).toHaveLength(1)
    expect(s.items[0]).toMatchObject({ kind: "compaction", running: false, failed: true })
  })

  it("treats compaction_end with result:null as failed", () => {
    const s = run([ev("compaction_end", { reason: "manual", aborted: false, result: null })])
    expect(s.items[0]).toMatchObject({ kind: "compaction", failed: true })
  })

  it("creates a notice on an error event and does not duplicate it", () => {
    const msg = "Compaction failed: Nothing to compact (session too small)"
    let s = applyCompactionError(emptyTranscript(), msg)
    expect(s.items).toEqual([expect.objectContaining({ kind: "compaction", failed: true, errorMessage: msg, running: false })])
    s = applyPiEvent(s, ev("compaction_end", { reason: "manual", result: null, errorMessage: "Nothing to compact (session too small)" }))
    expect(s.items).toHaveLength(1)
    s = applyCompactionError(s, msg)
    expect(s.items).toHaveLength(1)
  })

  it("ends a running compaction on the error event", () => {
    let s = run([ev("compaction_start", { reason: "manual" })])
    s = applyCompactionError(s, "Compaction failed: broken")
    expect(s.items).toHaveLength(1)
    expect(s.items[0]).toMatchObject({ running: false, failed: true, errorMessage: "Compaction failed: broken" })
    expect(s.compactingKey).toBeUndefined()
  })

  it("keeps the notice in its place on reload", () => {
    const hist = [
      stored(1, { role: "user", content: [{ type: "text", text: "a" }] }),
      stored(2, { role: "assistant", content: [{ type: "text", text: "b" }] }),
    ]
    let s = hydrate(emptyTranscript(), hist)
    s = applyPiEvent(s, ev("compaction_end", { reason: "manual", result: null, errorMessage: "Already compacted" }))
    s = hydrate(s, hist)
    expect(s.items.map((i) => i.kind)).toEqual(["user", "assistant", "compaction"])
    // later more messages are added; the notice stays behind seq 2
    s = hydrate(s, [
      ...hist,
      stored(3, { role: "user", content: [{ type: "text", text: "c" }] }),
    ])
    expect(s.items.map((i) => i.key)).toEqual(["seq-1", "seq-2", expect.stringMatching(/^live-/), "seq-3"])
  })

  it("does not take over successful live compactions (they come from the history)", () => {
    let s = run([ev("compaction_end", { reason: "manual", result: { summary: "x", tokensBefore: 1 } })])
    s = hydrate(s, [])
    expect(s.items).toHaveLength(0)
  })
})

describe("times of the entries", () => {
  it("takes created_at from the history as the time", () => {
    const s = hydrate(emptyTranscript(), [
      { ...stored(1, { role: "user", content: [{ type: "text", text: "a" }] }), created_at: "2026-09-29T10:00:00Z" },
      { ...stored(2, { role: "assistant", content: [] }), created_at: "2026-09-29T10:00:05Z" },
    ])
    expect(s.items.map((i) => i.time)).toEqual([Date.parse("2026-09-29T10:00:00Z"), Date.parse("2026-09-29T10:00:05Z")])
  })

  it("takes pi's timestamp live", () => {
    const s = run([
      ev("message_start", { message: { role: "assistant", content: [], timestamp: 1000 } }),
      ev("message_end", { message: { role: "assistant", content: [], timestamp: 1000 } }),
      ev("message_end", { message: { role: "user", content: "x", timestamp: 2000 } }),
    ])
    expect(s.items.map((i) => i.time)).toEqual([1000, 2000])
  })
})

describe("response ID for images (msgKey)", () => {
  it("is the same live after message_end and after a reload, empty while streaming", () => {
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

// Review 3, H1: the origin of the user message comes from the server (user_meta live, fields of the history).
describe("origin of the user message", () => {
  const meta = { turn_id: 7, trigger: "wake" as const, origin: "system" as const, sources: [{ kind: "system" as const, type: "background", refs: ["bg-1"], marker: "agw-0123456789abcdef" }] }
  it("user_meta applies to the next user message, not after that", () => {
    let s = applyUserMeta(emptyTranscript(), meta)
    s = run([ev("message_end", { message: { role: "user", content: "Note" } }), ev("message_end", { message: { role: "user", content: "afterwards" } })], s)
    const users = s.items.filter((i) => i.kind === "user")
    expect(users[0]).toMatchObject({ origin: "system", trigger: "wake", turnId: 7, sources: meta.sources })
    expect(users[1]).not.toHaveProperty("origin")
    expect(s.nextUserMeta).toBeUndefined()
  })
  it("the history carries origin, sources and trigger", () => {
    const s = hydrate(emptyTranscript(), [{ ...stored(1, { role: "user", content: "x" }), turn_id: 7, trigger: "wake", origin: "system", sources: meta.sources }])
    expect(s.items[0]).toMatchObject({ kind: "user", origin: "system", trigger: "wake", turnId: 7 })
  })
  it("a handed-over queue shows the origin even before pi confirms", () => {
    const s = applyQueueDelivered(emptyTranscript(), "q-1", "Note", { origin: "system", sources: meta.sources })
    expect(s.pending[0]).toMatchObject({ text: "Note", origin: "system", sources: meta.sources })
    const t = applyQueueDelivered(addPending(emptyTranscript(), "p", "continue"), "q-2", "Note\n\ncontinue", { origin: "mixed", sources: meta.sources })
    expect(t.pending).toHaveLength(1)
    expect(t.pending[0]).toMatchObject({ key: "p", origin: "mixed" })
  })
})

describe("messages from extensions (role custom)", () => {
  it("live and after a reload as a line of its own, not as a user message", () => {
    const live = run([ev("message_end", { message: { role: "custom", customType: "subagent-notify", content: "Subagent done\nreport available" } })])
    expect(live.items).toMatchObject([{ kind: "notice", text: "Subagent done\nreport available", customType: "subagent-notify" }])
    const h = hydrate(emptyTranscript(), [
      { ...stored(1, { role: "custom", customType: "subagent-notify", content: [{ type: "text", text: "done" }] }), trigger: "wake" },
    ])
    expect(h.items).toMatchObject([{ kind: "notice", text: "done", trigger: "wake", seq: 1 }])
  })
})

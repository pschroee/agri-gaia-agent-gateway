import { describe, expect, it } from "vitest"
import type { PiEvent, StoredMessage } from "@/api/types"
import { chatRunSince, formatElapsed, formatStepDuration, liveActivity, runStartOf, toolDurationMs } from "./runtime"
import { applyPiEvent, emptyTranscript, hydrate, type TranscriptState } from "./stream"

const ev = (type: string, rest: Record<string, unknown> = {}): PiEvent => ({ type, ...rest })
/** Apply events with receipt time (ms). */
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
  it("shows seconds below one minute", () => {
    expect(formatElapsed(0)).toBe("0 s")
    expect(formatElapsed(999)).toBe("0 s")
    expect(formatElapsed(12_400)).toBe("12 s")
    expect(formatElapsed(59_999)).toBe("59 s")
  })
  it("shows m:ss from one minute and h:mm:ss from one hour", () => {
    expect(formatElapsed(60_000)).toBe("1:00")
    expect(formatElapsed(65_000)).toBe("1:05")
    expect(formatElapsed(59 * 60_000 + 59_000)).toBe("59:59")
    expect(formatElapsed(3_723_000)).toBe("1:02:03")
    expect(formatElapsed(36_000_000)).toBe("10:00:00")
  })
  it("turns negative and invalid values into nothing wrong", () => {
    expect(formatElapsed(-500)).toBe("0 s")
    expect(formatElapsed(Number.NaN)).toBe("")
  })
})

describe("formatStepDuration", () => {
  it("shows short steps as \"< 1 s\", otherwise like formatElapsed", () => {
    expect(formatStepDuration(300)).toBe("< 1 s")
    expect(formatStepDuration(1_500)).toBe("1 s")
    expect(formatStepDuration(65_000)).toBe("1:05")
    expect(formatStepDuration(undefined)).toBe("")
  })
})

describe("duration: live from events", () => {
  it("remembers the start at agent_start and forgets it at agent_settled", () => {
    let s = at([[T0, ev("agent_start")]])
    expect(runStartOf(s, true)).toBe(T0)
    s = at([[T0 + 9000, ev("agent_settled")]], s)
    expect(s.runStart).toBeUndefined()
    expect(runStartOf(s, false)).toBeUndefined()
  })

  it("measures tools from tool_execution_start to _end; running ones count until now", () => {
    let s = at([
      [T0, ev("agent_start")],
      [T0 + 1000, ev("tool_execution_start", { toolCallId: "t1", toolName: "bash" })],
    ])
    expect(toolDurationMs(s.tools.t1, T0 + 4000)).toBe(3000)
    s = at([[T0 + 6500, ev("tool_execution_end", { toolCallId: "t1", toolName: "bash", result: "ok" })]], s)
    expect(toolDurationMs(s.tools.t1, T0 + 99_000)).toBe(5500)
  })

  it("gives the response that ends the run the total duration since agent_start", () => {
    const s = at([
      [T0, ev("agent_start")],
      [T0 + 100, ev("message_end", { message: { role: "user", content: "Hello" } })],
      [T0 + 2000, ev("message_end", { message: { role: "assistant", content: [], stopReason: "toolUse" } })],
      [T0 + 34_000, ev("message_end", { message: { role: "assistant", content: [], stopReason: "stop" } })],
    ])
    const [, between, end] = s.items
    if (between.kind !== "assistant" || end.kind !== "assistant") throw new Error()
    expect(between.durationMs).toBeUndefined()
    expect(end.durationMs).toBe(34_000)
  })

  it("guesses no duration if the start of the run was not observed", () => {
    const s = at([[T0, ev("message_end", { message: { role: "assistant", content: [], stopReason: "stop" } })]])
    const item = s.items[0]
    if (item.kind !== "assistant") throw new Error()
    expect(item.durationMs).toBeUndefined()
  })

  it("keeps the start when reloading during the run", () => {
    const s = hydrate(at([[T0, ev("agent_start")]]), [])
    expect(s.runStart).toBe(T0)
  })
})

describe("duration: after a reload from timestamps", () => {
  const history = [
    stored(1, "00:00", { role: "user", content: [{ type: "text", text: "Calculate" }] }),
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

  it("reconstructs the total duration of the response from user message and end of response", () => {
    const s = hydrate(emptyTranscript(), history)
    const last = s.items.at(-1)
    if (last?.kind !== "assistant") throw new Error()
    expect(last.durationMs).toBe(34_000)
    const mid = s.items[1]
    if (mid.kind !== "assistant") throw new Error()
    expect(mid.durationMs).toBeUndefined()
  })

  it("reconstructs the tool duration only if the response had exactly one call", () => {
    const s = hydrate(emptyTranscript(), history)
    expect(toolDurationMs(s.tools.t1, 0)).toBe(7000)
    // two calls in one response: start per call unknown, so no value
    expect(toolDurationMs(s.tools.t2, 0)).toBeUndefined()
    expect(toolDurationMs(s.tools.t3, 0)).toBeUndefined()
  })

  it("shows nothing without a user message before the response or without timestamps", () => {
    const s = hydrate(emptyTranscript(), [stored(1, "00:05", { role: "assistant", stopReason: "stop", content: [] })])
    const a = s.items[0]
    if (a.kind !== "assistant") throw new Error()
    expect(a.durationMs).toBeUndefined()
    const bad = hydrate(emptyTranscript(), [
      { ...stored(1, "00:00", { role: "user", content: "x" }), created_at: "broken" },
      stored(2, "00:05", { role: "assistant", stopReason: "stop", content: [] }),
    ])
    const b = bad.items[1]
    if (b.kind !== "assistant") throw new Error()
    expect(b.durationMs).toBeUndefined()
  })

  it("takes the last user message as the start after a reload during a run", () => {
    const s = hydrate(emptyTranscript(), history.slice(0, 3))
    expect(runStartOf(s, true)).toBe(T0)
    expect(runStartOf(s, false)).toBeUndefined()
  })
})

describe("chatRunSince", () => {
  it("takes the orchestrator's running_since, otherwise the start from the history, and nothing when the chat is idle", () => {
    const s = at([[T0, ev("agent_start")]])
    expect(chatRunSince({ running: true, running_since: "2026-09-29T09:59:00Z" }, s)).toBe(T0 - 60_000)
    expect(chatRunSince({ running: true }, s)).toBe(T0)
    expect(chatRunSince({ running: true, running_since: "broken" }, s)).toBe(T0)
    expect(chatRunSince({ running: true })).toBeUndefined()
    expect(chatRunSince({ running: false, running_since: "2026-09-29T09:59:00Z" }, s)).toBeUndefined()
  })
})

describe("liveActivity", () => {
  it("after a reload in the middle of a tool call: runs the tool instead of thinking", () => {
    const s = hydrate(emptyTranscript(), [
      stored(1, "00:00", { role: "user", content: [{ type: "text", text: "go" }] }),
      stored(2, "00:02", {
        role: "assistant",
        content: [{ type: "toolCall", id: "t9", name: "bash", arguments: { command: "sleep 12" } }],
        stopReason: "toolUse",
      }),
    ])
    expect(liveActivity(s)).toBe("Running bash")
  })
  it("names what the agent is doing right now", () => {
    let s = at([[T0, ev("agent_start")]])
    expect(liveActivity(s)).toBe("Thinking")
    s = at([[T0, ev("message_start", { message: { role: "assistant", content: [] } })]], s)
    s = at([[T0, ev("message_update", { assistantMessageEvent: { type: "text_delta", delta: "Hal" } })]], s)
    expect(liveActivity(s)).toBe("Writing")
    s = at([[T0, ev("message_update", { assistantMessageEvent: { type: "toolcall_start", contentIndex: 1, id: "t1", toolName: "bash" } })]], s)
    expect(liveActivity(s)).toBe("Preparing bash")
    s = at([[T0, ev("message_end", { message: { role: "assistant", content: [], stopReason: "toolUse" } })]], s)
    s = at([[T0, ev("tool_execution_start", { toolCallId: "t1", toolName: "bash" })]], s)
    expect(liveActivity(s)).toBe("Running bash")
  })
})

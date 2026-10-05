import { describe, expect, it } from "vitest"
import type { SocketCall, SubagentEntry } from "@/api/types"
import {
  assignRuns,
  clipText,
  effectiveTimes,
  groupRuns,
  isEntryConfirmed,
  limitErrorKind,
  limitNotices,
  mergeSubagentEntries,
  parseArguments,
  placeAfter,
  runLabel,
  shortRunId,
  subagentLimitLabel,
  toolAgents,
} from "./subagents"
import type { TranscriptItem } from "./stream"

const entry = (p: Partial<SubagentEntry> & Pick<SubagentEntry, "run_id" | "entry_id">): SubagentEntry => ({
  chat_id: "c",
  agent: "scout",
  kind: "text",
  payload: {},
  confirmed: false,
  created_at: "2026-09-29T10:00:00Z",
  ...p,
})
const t = (s: string) => Date.parse(`2026-09-29T${s}Z`)
const iso = (s: string) => `2026-09-29T${s}Z`

describe("mergeSubagentEntries", () => {
  it("appends new entries and replaces equal (run_id, entry_id)", () => {
    const a = entry({ run_id: "r1", entry_id: "e1" })
    const b = entry({ run_id: "r1", entry_id: "e2" })
    const a2 = { ...a, confirmed: true }
    expect(mergeSubagentEntries([a], [b, a2])).toEqual([a2, b])
  })
  it("distinguishes equal entry_id in different runs", () => {
    const a = entry({ run_id: "r1", entry_id: "e1" })
    const b = entry({ run_id: "r2", entry_id: "e1" })
    expect(mergeSubagentEntries([a], [b])).toHaveLength(2)
  })
})

describe("groupRuns", () => {
  it("groups per run_id, sorts by start and counts", () => {
    const runs = groupRuns([
      entry({ run_id: "r2", entry_id: "x", created_at: iso("10:05:00"), agent: "worker", kind: "task", payload: { text: "B" } }),
      entry({ run_id: "r1", entry_id: "a", created_at: iso("10:00:00"), kind: "task", payload: { text: "Finde X" } }),
      entry({ run_id: "r1", entry_id: "b", created_at: iso("10:00:02"), kind: "tool_call", payload: { name: "bash", arguments: "{}" } }),
      entry({ run_id: "r1", entry_id: "c", created_at: iso("10:00:03"), kind: "tool_result", payload: { name: "bash", text: "err", is_error: true } }),
    ])
    expect(runs.map((r) => r.runId)).toEqual(["r1", "r2"])
    expect(runs[0]).toMatchObject({ agent: "scout", task: "Finde X", toolCalls: 1, errors: 1, start: t("10:00:00"), end: t("10:00:03") })
    expect(runs[0].entries.map((e) => e.entry_id)).toEqual(["a", "b", "c"])
    expect(runs[1].agent).toBe("worker")
  })
  it("returns an empty list without entries", () => {
    expect(groupRuns([])).toEqual([])
  })
})

describe("shortRunId and runLabel", () => {
  it("shortens a UUID to six characters and keeps the index of parallel children", () => {
    expect(shortRunId("abc123de-0000-4000-8000-000000000000")).toBe("abc123")
    expect(shortRunId("abc123de-0000-4000-8000-000000000000#2")).toBe("abc123#2")
    expect(shortRunId("short")).toBe("short")
  })
  it("builds the heading", () => {
    expect(runLabel({ agent: "scout", runId: "abc123de-0000" })).toBe("Subagent scout · run abc123")
    expect(runLabel({ agent: "", runId: "abc123de-0000" })).toBe("Subagent · run abc123")
  })
})

describe("isEntryConfirmed", () => {
  it("takes confirmed from the server or a response_id known at the proxy", () => {
    expect(isEntryConfirmed(entry({ run_id: "r", entry_id: "e", confirmed: true }), new Set())).toBe(true)
    expect(isEntryConfirmed(entry({ run_id: "r", entry_id: "e", response_id: "resp1" }), new Set(["resp1"]))).toBe(true)
    expect(isEntryConfirmed(entry({ run_id: "r", entry_id: "e", response_id: "resp1" }), new Set(["x"]))).toBe(false)
    expect(isEntryConfirmed(entry({ run_id: "r", entry_id: "e" }), new Set([""]))).toBe(false)
  })
})

describe("parseArguments", () => {
  it("reads JSON and otherwise falls back to the text", () => {
    expect(parseArguments('{"command":"ls"}')).toEqual({ command: "ls" })
    expect(parseArguments("no json")).toBe("no json")
    expect(parseArguments(undefined)).toBeUndefined()
    expect(parseArguments("")).toBeUndefined()
  })
})

describe("clipText", () => {
  it("shortens long texts and reports it", () => {
    expect(clipText("abc", 10)).toEqual({ text: "abc", clipped: false })
    expect(clipText("abcdefghijkl", 5)).toEqual({ text: "abcde …", clipped: true })
  })
})

describe("toolAgents", () => {
  it("finds agent names in single and parallel calls", () => {
    expect(toolAgents({ agent: "scout", task: "x" })).toEqual(["scout"])
    expect(toolAgents({ tasks: [{ agent: "a" }, { agent: "b" }] })).toEqual(["a", "b"])
    expect(toolAgents({ chain: [{ agent: "c" }] })).toEqual(["c"])
    expect(toolAgents("x")).toEqual([])
  })
})

describe("effectiveTimes and placeAfter", () => {
  const items: TranscriptItem[] = [
    { kind: "user", key: "u1", text: "a", time: t("10:00:00") },
    { kind: "assistant", key: "a1", blocks: [], streaming: false, time: t("10:00:10") },
    { kind: "assistant", key: "a2", blocks: [], streaming: true },
  ]
  it("inherits missing times from the predecessor", () => {
    expect(effectiveTimes(items)).toEqual([t("10:00:00"), t("10:00:10"), t("10:00:10")])
  })
  it("places after the last entry that is not later", () => {
    expect(placeAfter(items, t("09:59:00"))).toBe(-1)
    expect(placeAfter(items, t("10:00:05"))).toBe(0)
    expect(placeAfter(items, t("10:00:20"))).toBe(2)
  })
})

describe("assignRuns", () => {
  const call = (id: string, args: unknown) => ({ type: "toolCall" as const, id, name: "subagent", arguments: args })
  const items: TranscriptItem[] = [
    { kind: "user", key: "u1", text: "go", time: t("10:00:00") },
    {
      kind: "assistant",
      key: "a1",
      blocks: [call("tc1", { agent: "scout", task: "A" }), call("tc2", { agent: "worker", task: "B" })],
      streaming: false,
      time: t("10:00:05"),
    },
    { kind: "assistant", key: "a2", blocks: [{ type: "text", text: "done" }], streaming: false, time: t("10:01:00") },
    { kind: "assistant", key: "a3", blocks: [call("tc3", { agent: "scout" })], streaming: false, time: t("10:02:00") },
  ]
  const run = (runId: string, agent: string, at: string) => ({ runId, agent, start: t(at) })

  it("assigns a run to the last subagent call before it, preferring a matching agent", () => {
    const r = assignRuns(items, [run("r1", "worker", "10:00:07"), run("r2", "scout", "10:00:08"), run("r3", "scout", "10:02:03")])
    expect(r.byTool).toEqual({ tc2: ["r1"], tc1: ["r2"], tc3: ["r3"] })
    expect(r.loose).toEqual({})
  })
  it("takes the first call of the message without a matching agent", () => {
    const r = assignRuns(items, [run("r1", "fremd", "10:00:30")])
    expect(r.byTool).toEqual({ tc1: ["r1"] })
  })
  it("places runs before any subagent call loosely by time into the history", () => {
    const r = assignRuns(items, [run("r0", "scout", "10:00:01")])
    expect(r.byTool).toEqual({})
    expect(r.loose).toEqual({ 0: ["r0"] })
  })
})

describe("limitErrorKind", () => {
  // the orchestrator's messages are still German (internal/chat/subagents.go)
  it("recognises the orchestrator's two limit messages", () => {
    expect(limitErrorKind("model call refused: at most 3 concurrent agents (main agent and 2 subagents) allowed")).toBe(
      "agent_limit",
    )
    expect(limitErrorKind("limit exceeded: 3 subagents started, 2 allowed. The turn was aborted.")).toBe(
      "subagent_limit",
    )
    expect(limitErrorKind("compaction failed: x")).toBeUndefined()
    expect(limitErrorKind(undefined)).toBeUndefined()
  })
})

describe("limitNotices", () => {
  const sc = (id: number, op: string, detail: string, at: string): SocketCall => ({
    id,
    slot_id: "p",
    via: "proxy",
    op,
    detail,
    result: "refused",
    created_at: iso(at),
  })
  it("turns limit entries into notices and combines directly consecutive equal ones", () => {
    const n = limitNotices([
      sc(1, "upload", "x", "10:00:00"),
      sc(2, "agent_limit", "at most 3 concurrent agents", "10:00:01"),
      sc(3, "agent_limit", "at most 3 concurrent agents", "10:00:02"),
      sc(4, "subagent_limit", "3 started, 2 allowed", "10:00:03"),
    ])
    expect(n).toEqual([
      {
        id: 2,
        op: "agent_limit",
        time: t("10:00:01"),
        count: 2,
        text: "Limit of concurrent agents reached: model call refused at the proxy (at most 3 concurrent agents)",
      },
      {
        id: 4,
        op: "subagent_limit",
        time: t("10:00:03"),
        count: 1,
        text: "Subagent limit exceeded – aborted (3 started, 2 allowed)",
      },
    ])
  })
})

describe("subagentLimitLabel", () => {
  it("shows started and allowed subagents", () => {
    expect(subagentLimitLabel({ subagents: 1, max_subagents: 2 })).toBe("Subagents 1 / 2")
    expect(subagentLimitLabel({})).toBe("Subagents 0 / –")
  })
})

import { describe, expect, it } from "vitest"
import type { SubagentEntry } from "@/api/types"
import {
  baseRunId,
  buildRunTree,
  groupRuns,
  pairRunEntries,
  runConfirmation,
  runStatus,
  runStatusLabel,
} from "./subagents"

const entry = (p: Partial<SubagentEntry> & Pick<SubagentEntry, "run_id" | "entry_id">): SubagentEntry => ({
  chat_id: "c",
  agent: "scout",
  kind: "text",
  payload: {},
  confirmed: false,
  created_at: "2026-09-29T10:00:00Z",
  ...p,
})
const iso = (s: string) => `2026-09-29T${s}Z`
const t = (s: string) => Date.parse(iso(s))

describe("baseRunId", () => {
  it("cuts off the suffix of parallel runs", () => {
    expect(baseRunId("abc123#2")).toBe("abc123")
    expect(baseRunId("abc123")).toBe("abc123")
  })
})

describe("buildRunTree", () => {
  it("returns an empty list without runs", () => {
    expect(buildRunTree([])).toEqual([])
  })

  it("turns single runs into one node each without children, sorted by start", () => {
    const runs = groupRuns([
      entry({ run_id: "b", entry_id: "1", created_at: iso("10:05:00") }),
      entry({ run_id: "a", entry_id: "1", created_at: iso("10:00:00") }),
    ])
    const tree = buildRunTree(runs)
    expect(tree.map((n) => n.id)).toEqual(["a", "b"])
    expect(tree[0].run?.runId).toBe("a")
    expect(tree[0].children).toEqual([])
  })

  it("groups parallel runs (#n) under their base run, sorted numerically", () => {
    const runs = groupRuns([
      entry({ run_id: "p#10", entry_id: "1", created_at: iso("10:00:01") }),
      entry({ run_id: "p#2", entry_id: "1", created_at: iso("10:00:03") }),
      entry({ run_id: "p", entry_id: "1", created_at: iso("10:00:00") }),
      entry({ run_id: "q", entry_id: "1", created_at: iso("10:00:02") }),
    ])
    const tree = buildRunTree(runs)
    expect(tree.map((n) => n.id)).toEqual(["p", "q"])
    expect(tree[0].run?.runId).toBe("p")
    expect(tree[0].children.map((r) => r.runId)).toEqual(["p#2", "p#10"])
  })

  it("creates a group node without its own run if only children exist", () => {
    const runs = groupRuns([
      entry({ run_id: "x#1", entry_id: "1", created_at: iso("10:00:05"), agent: "worker" }),
      entry({ run_id: "x#2", entry_id: "1", created_at: iso("10:00:04"), agent: "reviewer" }),
    ])
    const tree = buildRunTree(runs)
    expect(tree).toHaveLength(1)
    expect(tree[0]).toMatchObject({ id: "x", start: t("10:00:04"), end: t("10:00:05") })
    expect(tree[0].run).toBeUndefined()
    expect(tree[0].children.map((r) => r.runId)).toEqual(["x#1", "x#2"])
  })
})

describe("runStatus", () => {
  const now = t("10:10:00")
  const run = (last: Partial<SubagentEntry>) =>
    groupRuns([
      entry({ run_id: "r", entry_id: "1", kind: "task", created_at: iso("10:00:00"), payload: { text: "A" } }),
      entry({ run_id: "r", entry_id: "2", created_at: iso("10:09:50"), ...last }),
    ])[0]

  it("is done if the last entry is a text response", () => {
    expect(runStatus(run({ kind: "text", payload: { text: "Result" } }), { chatRunning: true, now })).toBe("done")
  })
  it("is running while the chat is working and the last entry is fresh", () => {
    expect(runStatus(run({ kind: "tool_call", payload: { name: "bash" } }), { chatRunning: true, now })).toBe("running")
  })
  it("is quiet if the chat is working but nothing came for a long time", () => {
    const r = run({ kind: "tool_call", payload: { name: "bash" } })
    expect(runStatus(r, { chatRunning: true, now: t("10:20:00") })).toBe("idle")
  })
  it("keeps running without state while something came recently, even if the main agent is idle (background)", () => {
    expect(runStatus(run({ kind: "tool_result", payload: { name: "bash", text: "" } }), { chatRunning: false, now })).toBe(
      "running",
    )
  })
  it("is ended without state and without response if nothing came for a long time and the chat is idle", () => {
    const r = run({ kind: "tool_result", payload: { name: "bash", text: "" } })
    expect(runStatus(r, { chatRunning: false, now: t("10:20:00") })).toBe("stopped")
  })
  it("follows the state according to pi-subagents when it is known", () => {
    const meta = (state: string) => [{ chat_id: "c", run_id: "r", agent: "researcher", label: "reid", state, updated_at: iso("10:09:55") }]
    const mk = (state: string) =>
      groupRuns(
        [
          entry({ run_id: "r", entry_id: "1", kind: "task", created_at: iso("10:00:00"), payload: { text: "A" } }),
          entry({ run_id: "r", entry_id: "2", kind: "text", created_at: iso("10:00:05"), payload: { text: "Zwischenstand" } }),
        ],
        meta(state),
      )[0]
    // text as the last entry would otherwise mean "done"; pi-subagents says it is still running
    expect(runStatus(mk("running"), { chatRunning: false, now: t("10:30:00") })).toBe("running")
    expect(runStatus(mk("complete"), { chatRunning: true, now })).toBe("done")
    expect(runStatus(mk("failed"), { chatRunning: true, now })).toBe("stopped")
    expect(mk("running").label).toBe("reid")
  })
  it("shows a run with metadata even before the first entry", () => {
    const runs = groupRuns([], [{ chat_id: "c", run_id: "x", agent: "scout", label: "daten", state: "running", started_at: iso("10:00:00"), updated_at: iso("10:00:00") }])
    expect(runs).toHaveLength(1)
    expect(runs[0]).toMatchObject({ runId: "x", agent: "scout", label: "daten" })
  })
  it("counts as done with errors if a text response comes after errors", () => {
    const r = groupRuns([
      entry({ run_id: "r", entry_id: "1", kind: "tool_result", payload: { is_error: true } }),
      entry({ run_id: "r", entry_id: "2", kind: "text", created_at: iso("10:00:01"), payload: { text: "ok" } }),
    ])[0]
    expect(runStatus(r, { chatRunning: false, now })).toBe("done")
  })
  it("has labels", () => {
    expect(runStatusLabel("running")).toBe("running")
    expect(runStatusLabel("done")).toBe("done")
    expect(runStatusLabel("idle")).toBe("quiet")
    expect(runStatusLabel("stopped")).toBe("ended without response")
  })
})

describe("runConfirmation", () => {
  it("counts verified entries according to server or proxy", () => {
    const r = groupRuns([
      entry({ run_id: "r", entry_id: "1", confirmed: true }),
      entry({ run_id: "r", entry_id: "2", response_id: "resp-1" }),
      entry({ run_id: "r", entry_id: "3" }),
    ])[0]
    expect(runConfirmation(r, new Set(["resp-1"]))).toEqual({ confirmed: 2, total: 3 })
  })
})

describe("pairRunEntries", () => {
  it("pairs call and result into one tool entry", () => {
    const list = [
      entry({ run_id: "r", entry_id: "t", kind: "task", payload: { text: "Task" } }),
      entry({ run_id: "r", entry_id: "c1", kind: "tool_call", payload: { name: "bash", arguments: '{"command":"ls"}' } }),
      entry({ run_id: "r", entry_id: "c2", kind: "tool_call", payload: { name: "read", arguments: "{}" } }),
      entry({ run_id: "r", entry_id: "r2", kind: "tool_result", payload: { name: "read", text: "Content" } }),
      entry({ run_id: "r", entry_id: "r1", kind: "tool_result", payload: { name: "bash", text: "a b" } }),
      entry({ run_id: "r", entry_id: "x", kind: "text", payload: { text: "done" } }),
    ]
    const items = pairRunEntries(list)
    expect(items.map((i) => i.type)).toEqual(["task", "tool", "tool", "text"])
    const [, bash, read] = items
    expect(bash.type === "tool" && [bash.call?.entry_id, bash.result?.entry_id]).toEqual(["c1", "r1"])
    expect(read.type === "tool" && [read.call?.entry_id, read.result?.entry_id]).toEqual(["c2", "r2"])
  })

  it("assigns a result without a name to the oldest open call", () => {
    const items = pairRunEntries([
      entry({ run_id: "r", entry_id: "c1", kind: "tool_call", payload: { name: "bash" } }),
      entry({ run_id: "r", entry_id: "r1", kind: "tool_result", payload: { text: "x" } }),
    ])
    expect(items).toHaveLength(1)
    expect(items[0].type === "tool" && items[0].result?.entry_id).toBe("r1")
  })

  it("shows a result without a matching call as an entry of its own", () => {
    const items = pairRunEntries([entry({ run_id: "r", entry_id: "r1", kind: "tool_result", payload: { name: "bash" } })])
    expect(items).toHaveLength(1)
    expect(items[0].type === "tool" && items[0].call).toBeUndefined()
    expect(items[0].type === "tool" && items[0].result?.entry_id).toBe("r1")
  })

  it("leaves an open call without result in place", () => {
    const items = pairRunEntries([entry({ run_id: "r", entry_id: "c1", kind: "tool_call", payload: { name: "bash" } })])
    expect(items[0].type === "tool" && items[0].result).toBeUndefined()
  })
})

describe("pairRunEntries with call ID", () => {
  it("assigns parallel calls of the same name via the ID", () => {
    const e = (kind: SubagentEntry["kind"], payload: SubagentEntry["payload"], id: string): SubagentEntry => ({
      chat_id: "c", run_id: "r", entry_id: id, agent: "scout", kind, payload, confirmed: false, created_at: "2026-09-29T12:00:00Z",
    })
    const items = pairRunEntries([
      e("tool_call", { name: "bash", arguments: '{"command":"a"}', id: "c1" }, "1"),
      e("tool_call", { name: "bash", arguments: '{"command":"b"}', id: "c2" }, "2"),
      e("tool_result", { name: "bash", text: "B", tool_call_id: "c2" }, "3"),
      e("tool_result", { name: "bash", text: "A", tool_call_id: "c1" }, "4"),
    ])
    const tools = items.filter((i) => i.type === "tool")
    expect(tools.map((t) => [t.call?.payload?.arguments, t.result?.payload?.text])).toEqual([
      ['{"command":"a"}', "A"],
      ['{"command":"b"}', "B"],
    ])
  })
})

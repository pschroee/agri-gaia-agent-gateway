import { describe, expect, it } from "vitest"
import type { LLMCall, ToolExecutionRecord } from "@/api/types"
import type { ToolExecution } from "./stream"
import { displayState, evidenceLabel, evidenceSummary, reconcile as reconcileWith, rejectionsFrom, sessionLabel } from "./evidence"

// Wie /api/config → executed_tools (L6: die Liste kommt vom Server)
const EXECUTED = ["bash", "edit", "find", "grep", "ls", "mcp_upload_artifact", "read", "write"]
const reconcile = (l: LLMCall[], e: ToolExecutionRecord[], rejections?: Map<string, string>) =>
  reconcileWith(l, e, { executedTools: EXECUTED, rejections })

const call = (id: number, tools: { id?: string; name: string }[], main = true, at = "2026-09-29T12:00:00Z", complete = true): LLMCall => ({
  id,
  slot_id: "p",
  source_ip: "10.0.0.2",
  model: "deepseek/deepseek-flash",
  response_id: `r${id}`,
  status: 200,
  input: 0,
  output: 0,
  cache_read: 0,
  cache_write: 0,
  cost: 0,
  peak: false,
  tool_calls: tools.map((t) => ({ ...t, arguments: "{}" })),
  started_at: at,
  duration_ms: 1,
  main,
  complete,
  finish_reason: complete ? "tool_calls" : "",
})

const exec = (id: number, toolCallId: string, tool: string, op: string, extra: Partial<ToolExecutionRecord> = {}): ToolExecutionRecord => ({
  id,
  chat_id: "c",
  slot_id: "p",
  session: "main",
  tool_call_id: toolCallId,
  tool,
  op,
  args: {},
  output_bytes: 0,
  started_at: "2026-09-29T12:00:01Z",
  duration_ms: 2,
  ...extra,
})

describe("reconcile", () => {
  it("gleicht angeforderte und ausgeführte Aufrufe je toolCallId ab", () => {
    const m = reconcile(
      [
        call(1, [{ id: "a", name: "bash" }, { id: "b", name: "read" }, { id: "c", name: "todo" }, { name: "ohne_id" }]),
        call(2, [{ id: "d", name: "write" }], false),
      ],
      [
        exec(1, "a", "bash", "bash", { exit_code: 0 }),
        exec(2, "d", "bash", "bash", { session: "run-1" }),
        exec(3, "x", "read", "read"),
        exec(4, "e", "edit", "read"),
      ],
    )
    expect(m.get("a")?.state).toBe("confirmed")
    expect(m.get("a")?.exitCode).toBe(0)
    expect(m.get("b")?.state).toBe("unexecuted")
    expect(m.get("c")?.state).toBe("internal")
    expect(m.get("d")?.state).toBe("mismatch")
    expect(m.get("d")?.session).toBe("run-1")
    expect(m.get("x")?.state).toBe("unrequested")
    expect(m.has("ohne_id")).toBe(false)
  })

  it("sammelt mehrere Operationen eines Aufrufs", () => {
    const m = reconcile(
      [call(1, [{ id: "e", name: "edit" }])],
      [exec(1, "e", "edit", "read"), exec(2, "e", "edit", "write", { error: "EACCES: nope" })],
    )
    const e = m.get("e")!
    expect(e.state).toBe("confirmed")
    expect(e.ops).toEqual(["read", "write"])
    expect(e.executions).toHaveLength(2)
    expect(e.error).toBe("EACCES: nope")
    expect(e.durationMs).toBe(4)
  })

  it("zählt die Zustände", () => {
    const m = reconcile([call(1, [{ id: "a", name: "bash" }, { id: "b", name: "read" }])], [exec(1, "a", "bash", "bash"), exec(2, "z", "ls", "stat")])
    expect(evidenceSummary(m)).toEqual({ confirmed: 1, unrequested: 1, unexecuted: 1, mismatch: 0, internal: 0, aborted: 0, rejected: 0, flagged: 2, total: 3 })
  })

  it("trennt harmlose Ursachen von einer Umgehung (M1): abgebrochene Antwort, von pi abgewiesen", () => {
    const m = reconcile(
      [call(1, [{ id: "a", name: "bash" }], true, "2026-09-29T12:00:00Z", false), call(2, [{ id: "b", name: "grep" }, { id: "c", name: "read" }])],
      [],
      new Map([["b", "Tool grep not found"]]),
    )
    expect(m.get("a")?.state).toBe("aborted")
    expect(m.get("b")?.state).toBe("rejected")
    expect(m.get("b")?.reason).toBe("Tool grep not found")
    expect(m.get("c")?.state).toBe("unexecuted")
    const s = evidenceSummary(m)
    expect(s.flagged).toBe(1)
    expect(s.aborted).toBe(1)
    expect(s.rejected).toBe(1)
  })

  it("führt ein doppelt ausgeführtes Werkzeug nur einmal (wie der Server, L6)", () => {
    const m = reconcile([call(1, [{ id: "a", name: "read" }])], [exec(1, "a", "read", "read"), exec(2, "a", "bash", "bash"), exec(3, "a", "read", "stat")])
    expect(m.get("a")?.executedTool).toBe("read,bash")
    expect(m.get("a")?.state).toBe("mismatch")
  })

  it("ohne Liste der ausgeführten Werkzeuge (Konfiguration noch nicht geladen) ist nichts auffällig", () => {
    const m = reconcileWith([call(1, [{ id: "b", name: "read" }])], [])
    expect(m.get("b")?.state).toBe("internal")
  })
})

describe("rejectionsFrom", () => {
  it("sammelt Fehlermeldungen aus der Hauptsitzung und den Subagenten", () => {
    const tools: Record<string, ToolExecution> = {
      a: { toolCallId: "a", toolName: "grep", running: false, isError: true, result: "Tool grep not found" },
      b: { toolCallId: "b", toolName: "bash", running: false, isError: false, result: "ok" },
    }
    const m = rejectionsFrom(tools, [
      { chat_id: "c", run_id: "r", entry_id: "e", agent: "worker", kind: "tool_result", confirmed: false, created_at: "",
        payload: { tool_call_id: "c", is_error: true, text: "Validation failed" } },
      { chat_id: "c", run_id: "r", entry_id: "f", agent: "worker", kind: "tool_result", confirmed: false, created_at: "",
        payload: { tool_call_id: "d", is_error: false, text: "ok" } },
    ])
    expect([...m.entries()]).toEqual([["a", "Tool grep not found"], ["c", "Validation failed"]])
  })
})

describe("displayState", () => {
  it("wertet erst nach dem Lauf aus, vorher gilt ein Abgleich als ausstehend", () => {
    const m = reconcile([call(1, [{ id: "b", name: "read" }])], [exec(1, "x", "read", "read")])
    expect(displayState(m.get("b"), { settled: false })).toBe("pending")
    expect(displayState(m.get("x"), { settled: false })).toBe("pending")
    expect(displayState(m.get("b"), { settled: true })).toBe("unexecuted")
    expect(displayState(m.get("x"), { settled: true })).toBe("unrequested")
    const r = reconcile([call(1, [{ id: "g", name: "grep" }])], [], new Map([["g", "x"]]))
    expect(displayState(r.get("g"), { settled: false })).toBe("pending")
    expect(displayState(r.get("g"), { settled: true })).toBe("rejected")
  })

  it("belegt ist belegt, auch während des Laufs; ohne Angaben kein Zustand", () => {
    const m = reconcile([call(1, [{ id: "a", name: "bash" }])], [exec(1, "a", "bash", "bash")])
    expect(displayState(m.get("a"), { settled: false })).toBe("confirmed")
    expect(displayState(undefined, { settled: true })).toBeUndefined()
  })

  it("eine ausgeführte, aber noch nicht am Proxy gemeldete ID ist während des Laufs ausstehend", () => {
    const m = reconcile([], [exec(1, "a", "bash", "bash")])
    expect(displayState(m.get("a"), { settled: false })).toBe("pending")
  })
})

describe("evidenceLabel und sessionLabel", () => {
  it("benennt die Zustände deutsch und markiert Auffälliges", () => {
    expect(evidenceLabel("confirmed")).toMatchObject({ label: "belegt", tone: "ok" })
    expect(evidenceLabel("confirmed").title).toContain("vom Orchestrator ausgeführt")
    expect(evidenceLabel("unexecuted")).toMatchObject({ label: "nicht ausgeführt", tone: "bad" })
    expect(evidenceLabel("unrequested")).toMatchObject({ label: "nicht angefordert", tone: "bad" })
    expect(evidenceLabel("mismatch")).toMatchObject({ label: "abweichend", tone: "bad" })
    expect(evidenceLabel("internal").tone).toBe("muted")
    expect(evidenceLabel("pending").tone).toBe("muted")
    expect(evidenceLabel("aborted")).toMatchObject({ label: "Antwort abgebrochen", tone: "muted" })
    expect(evidenceLabel("rejected")).toMatchObject({ label: "von pi abgewiesen", tone: "muted" })
    expect(evidenceLabel("rejected").title).toContain("nicht fälschungssicher")
  })

  it("benennt die Sitzung", () => {
    expect(sessionLabel("main")).toBe("Hauptagent")
    expect(sessionLabel("9017da63-d08d-43ab-b978-e86cb7afc45b")).toBe("Subagent 9017da63")
    expect(sessionLabel("9017da63-d08d-43ab-b978-e86cb7afc45b#2")).toBe("Subagent 9017da63 #2")
    expect(sessionLabel(undefined)).toBe("–")
  })
})

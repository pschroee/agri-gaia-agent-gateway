import { describe, expect, it } from "vitest"
import type { StoredMessage, SubagentEntry } from "@/api/types"
import { applyPiEvent, emptyTranscript, hydrate, type TranscriptState } from "./stream"
import {
  applyTodoArgs,
  groupTodoBlocks,
  taskCounts,
  taskCountLabel,
  tasksFromDetails,
  tasksFromSubagentEntries,
  todoGroupSummary,
  todoTimeline,
  type Task,
} from "./tasks"

const t = (id: number, subject: string, status: Task["status"] = "pending", extra: Partial<Task> = {}): Task => ({
  id,
  subject,
  status,
  ...extra,
})

const details = (tasks: Task[], extra: Record<string, unknown> = {}) => ({
  action: "update",
  params: {},
  tasks,
  nextId: tasks.length + 1,
  ...extra,
})

let seq = 0
const stored = (message: Record<string, unknown>): StoredMessage =>
  ({ seq: ++seq, role: String(message.role), message, created_at: "2026-09-29T10:00:00Z" }) as unknown as StoredMessage

const assistant = (...calls: { id: string; args: Record<string, unknown>; name?: string }[]) =>
  stored({
    role: "assistant",
    content: calls.map((c) => ({ type: "toolCall", id: c.id, name: c.name ?? "todo", arguments: c.args })),
  })

const result = (id: string, text: string, det?: unknown, name = "todo", isError = false) =>
  stored({ role: "toolResult", toolCallId: id, toolName: name, content: [{ type: "text", text }], details: det, isError })

describe("tasksFromDetails", () => {
  it("liest den vollständigen Stand aus details", () => {
    const d = details([t(1, "A", "completed", { activeForm: "macht A" }), t(2, "B", "in_progress")])
    expect(tasksFromDetails(d)).toEqual([t(1, "A", "completed", { activeForm: "macht A" }), t(2, "B", "in_progress")])
  })
  it("verwirft unbrauchbare Angaben", () => {
    expect(tasksFromDetails(undefined)).toBeUndefined()
    expect(tasksFromDetails({ tasks: "x" })).toBeUndefined()
    expect(tasksFromDetails({ tasks: [], nextId: "1" })).toBeUndefined()
    // einzelne kaputte Einträge fallen heraus, der Rest bleibt
    expect(tasksFromDetails({ tasks: [{ id: 1 }, t(2, "B")], nextId: 3 })).toEqual([t(2, "B")])
  })
})

describe("applyTodoArgs (Rückfall ohne details)", () => {
  it("legt an, ändert, löscht und leert", () => {
    let s = applyTodoArgs({ tasks: [], nextId: 1 }, { action: "create", subject: "A", description: "lang" })
    s = applyTodoArgs(s, { action: "create", subject: "B", activeForm: "macht B" })
    expect(s.tasks).toEqual([t(1, "A", "pending", { description: "lang" }), t(2, "B", "pending", { activeForm: "macht B" })])
    s = applyTodoArgs(s, { action: "update", id: 1, status: "in_progress", activeForm: "macht A" })
    expect(s.tasks[0]).toMatchObject({ status: "in_progress", activeForm: "macht A" })
    s = applyTodoArgs(s, { action: "delete", id: 2 })
    expect(s.tasks[1].status).toBe("deleted")
    s = applyTodoArgs(s, { action: "list" })
    expect(s.tasks).toHaveLength(2)
    s = applyTodoArgs(s, { action: "clear" })
    expect(s).toEqual({ tasks: [], nextId: 1 })
  })
  it("ignoriert unbekannte IDs und Unsinn", () => {
    const s = { tasks: [t(1, "A")], nextId: 2 }
    expect(applyTodoArgs(s, { action: "update", id: 9, status: "completed" })).toEqual(s)
    expect(applyTodoArgs(s, "kaputt")).toEqual(s)
    expect(applyTodoArgs(s, { action: "create" })).toEqual(s)
  })
})

describe("todoTimeline", () => {
  it("rekonstruiert aus der gespeicherten Historie; der neueste Stand gewinnt", () => {
    const hist = [
      stored({ role: "user", content: "los" }),
      assistant({ id: "c1", args: { action: "create", subject: "A" } }, { id: "c2", args: { action: "create", subject: "B" } }),
      result("c1", "Created #1: A (pending)", details([t(1, "A")])),
      result("c2", "Created #2: B (pending)", details([t(1, "A"), t(2, "B")])),
      assistant({ id: "x", name: "bash", args: { command: "ls" } }),
      result("x", "a.txt", undefined, "bash"),
      assistant(
        { id: "c3", args: { action: "update", id: 1, status: "completed" } },
        { id: "c4", args: { action: "update", id: 2, status: "in_progress" } },
      ),
      result("c3", "Updated #1 (pending → completed)", details([t(1, "A", "completed"), t(2, "B")])),
      result("c4", "Updated #2 (pending → in_progress)", details([t(1, "A", "completed"), t(2, "B", "in_progress")])),
    ]
    const tr = hydrate(emptyTranscript(), hist)
    const tl = todoTimeline(tr)
    expect(tl.tasks).toEqual([t(1, "A", "completed"), t(2, "B", "in_progress")])
    expect(Object.keys(tl.calls)).toEqual(["c1", "c2", "c3", "c4"])
    expect(tl.calls.c1.before).toEqual([])
    expect(tl.calls.c3.change).toBe("#1 A: erledigt")
    expect(tl.calls.c4.change).toBe("#2 B: in Arbeit")
    expect(tl.calls.c2.change).toBe("Neu: #2 B")
  })

  it("nimmt live den Stand aus tool_execution_end (result.details)", () => {
    let s: TranscriptState = emptyTranscript()
    const ev = (e: Record<string, unknown>) => (s = applyPiEvent(s, e as never))
    ev({ type: "message_start", message: { role: "assistant", content: [] } })
    ev({
      type: "message_end",
      message: { role: "assistant", content: [{ type: "toolCall", id: "c1", name: "todo", arguments: { action: "create", subject: "A" } }] },
    })
    ev({ type: "tool_execution_start", toolCallId: "c1", toolName: "todo", args: { action: "create", subject: "A" } })
    // noch kein Ergebnis: nichts erfinden
    expect(todoTimeline(s).tasks).toEqual([])
    expect(todoTimeline(s).calls.c1.running).toBe(true)
    ev({
      type: "tool_execution_end",
      toolCallId: "c1",
      toolName: "todo",
      result: { content: [{ type: "text", text: "Created #1: A (pending)" }], details: details([t(1, "A")]) },
      isError: false,
    })
    expect(todoTimeline(s).tasks).toEqual([t(1, "A")])
  })

  it("fällt ohne details auf die Argumente zurück und überspringt Fehler", () => {
    const hist = [
      assistant({ id: "c1", args: { action: "create", subject: "A" } }),
      result("c1", "Created #1: A (pending)"),
      assistant({ id: "c2", args: { action: "update", id: 1, status: "in_progress" } }),
      result("c2", "Error: illegal transition", undefined, "todo", true),
      assistant({ id: "c3", args: { action: "update", id: 1, status: "completed" } }),
      result("c3", "Error: something", details([t(1, "A")], { error: "something" })),
    ]
    const tl = todoTimeline(hydrate(emptyTranscript(), hist))
    expect(tl.tasks).toEqual([t(1, "A")])
    expect(tl.calls.c2.error).toBe("Error: illegal transition")
    expect(tl.calls.c3.error).toBe("something")
  })

  it("ohne Aufrufe: leer", () => {
    expect(todoTimeline(emptyTranscript())).toEqual({ tasks: [], calls: {} })
  })
})

describe("Zählung und Beschriftung", () => {
  const list = [t(1, "A", "completed"), t(2, "B", "in_progress"), t(3, "C"), t(4, "D", "deleted")]
  it("zählt ohne gelöschte", () => {
    expect(taskCounts(list)).toEqual({ total: 3, completed: 1, inProgress: 1, pending: 1 })
    expect(taskCountLabel(taskCounts(list))).toBe("1/3 Aufgaben")
    expect(taskCountLabel(taskCounts([t(1, "A", "completed")]))).toBe("1/1 Aufgabe")
  })
  it("fasst eine Gruppe zusammen", () => {
    expect(todoGroupSummary(["create", "create"], list)).toBe("Aufgaben angelegt: 1 erledigt, 1 in Arbeit, 1 offen")
    expect(todoGroupSummary(["update"], [t(1, "A", "completed"), t(2, "B", "completed")])).toBe(
      "Aufgaben aktualisiert: 2 erledigt",
    )
    expect(todoGroupSummary(["list"], list)).toBe("Aufgaben abgefragt: 1 erledigt, 1 in Arbeit, 1 offen")
    expect(todoGroupSummary(["clear"], [])).toBe("Aufgabenliste geleert")
  })
})

describe("groupTodoBlocks", () => {
  it("fasst aufeinanderfolgende todo-Aufrufe zusammen", () => {
    const b = (name: string, id: string) => ({ type: "toolCall" as const, id, name })
    const blocks = [{ type: "text" as const, text: "x" }, b("todo", "1"), b("todo", "2"), b("bash", "3"), b("todo", "4")]
    expect(groupTodoBlocks(blocks)).toEqual([
      { kind: "block", index: 0 },
      { kind: "todo", indices: [1, 2] },
      { kind: "block", index: 3 },
      { kind: "todo", indices: [4] },
    ])
  })
})

describe("tasksFromSubagentEntries", () => {
  const e = (kind: SubagentEntry["kind"], payload: SubagentEntry["payload"]): SubagentEntry =>
    ({ chat_id: "c", run_id: "r", entry_id: String(++seq), agent: "worker", kind, payload, created_at: "" }) as SubagentEntry
  it("spielt die Argumente ab, überspringt fehlgeschlagene Aufrufe", () => {
    const list = [
      e("tool_call", { name: "todo", id: "a", arguments: JSON.stringify({ action: "create", subject: "A" }) }),
      e("tool_result", { name: "todo", tool_call_id: "a", text: "Created #1: A (pending)" }),
      e("tool_call", { name: "todo", id: "b", arguments: JSON.stringify({ action: "update", id: 1, status: "completed" }) }),
      e("tool_result", { name: "todo", tool_call_id: "b", text: "Updated #1" }),
      e("tool_call", { name: "todo", id: "c", arguments: JSON.stringify({ action: "create", subject: "B" }) }),
      e("tool_result", { name: "todo", tool_call_id: "c", text: "Error: kaputt" }),
      e("tool_call", { name: "bash", id: "d", arguments: "{}" }),
    ]
    expect(tasksFromSubagentEntries(list)).toEqual([t(1, "A", "completed")])
  })
  it("ohne todo-Aufrufe: undefined", () => {
    expect(tasksFromSubagentEntries([e("tool_call", { name: "bash", id: "x", arguments: "{}" })])).toBeUndefined()
  })
})

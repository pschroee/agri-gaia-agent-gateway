// Reconstruct the agent's task list from the calls of the `todo` tool (pi extension rpiv-todo).
// Every result carries the complete state after the call in `details.tasks`;
// the last one wins. If the details are missing (subagent entries, old messages), the
// arguments are replayed. Pure functions, without React.
import type { SubagentEntry } from "@/api/types"
import type { TranscriptState, ViewBlock } from "./stream"

export const TODO_TOOL = "todo"

export type TaskStatus = "pending" | "in_progress" | "completed" | "deleted"
export type Task = {
  id: number
  subject: string
  status: TaskStatus
  description?: string
  /** Label while the task is in progress ("writing tests"). */
  activeForm?: string
  blockedBy?: number[]
  owner?: string
}
export type TaskState = { tasks: Task[]; nextId: number }

type Obj = Record<string, unknown>
const isObj = (v: unknown): v is Obj => typeof v === "object" && v !== null && !Array.isArray(v)
const str = (v: unknown) => (typeof v === "string" && v.trim() !== "" ? v : undefined)
const STATUSES: TaskStatus[] = ["pending", "in_progress", "completed", "deleted"]
const isStatus = (v: unknown): v is TaskStatus => STATUSES.includes(v as TaskStatus)
const nums = (v: unknown) => (Array.isArray(v) ? v.filter((x): x is number => typeof x === "number") : undefined)

function toTask(v: unknown): Task | undefined {
  if (!isObj(v) || typeof v.id !== "number" || !str(v.subject) || !isStatus(v.status)) return undefined
  const t: Task = { id: v.id, subject: v.subject as string, status: v.status }
  if (str(v.description)) t.description = v.description as string
  if (str(v.activeForm)) t.activeForm = v.activeForm as string
  const blocked = nums(v.blockedBy)
  if (blocked?.length) t.blockedBy = blocked
  if (str(v.owner)) t.owner = v.owner as string
  return t
}

/** State from the `details` of a todo result; undefined if the shape does not match. */
export function tasksFromDetails(details: unknown): Task[] | undefined {
  if (!isObj(details) || !Array.isArray(details.tasks) || typeof details.nextId !== "number") return undefined
  return details.tasks.map(toTask).filter((t): t is Task => t !== undefined)
}

function parseArgs(args: unknown): Obj | undefined {
  if (isObj(args)) return args
  if (typeof args !== "string") return undefined
  try {
    const v: unknown = JSON.parse(args)
    return isObj(v) ? v : undefined
  } catch {
    return undefined
  }
}

/**
 * Applies a todo call to the state (fallback when there is no result with details).
 * Reproduces only what is needed of rpiv-todo; invalid calls leave the state unchanged.
 */
export function applyTodoArgs(state: TaskState, rawArgs: unknown): TaskState {
  const a = parseArgs(rawArgs)
  if (!a) return state
  const patch = (id: unknown, fn: (t: Task) => Task): TaskState => {
    if (typeof id !== "number" || !state.tasks.some((t) => t.id === id)) return state
    return { ...state, tasks: state.tasks.map((t) => (t.id === id ? fn(t) : t)) }
  }
  switch (a.action) {
    case "create": {
      const t = toTask({ ...a, id: state.nextId, status: "pending" })
      return t ? { tasks: [...state.tasks, t], nextId: state.nextId + 1 } : state
    }
    case "update":
      return patch(a.id, (t) => {
        const next = { ...t }
        if (str(a.subject)) next.subject = a.subject as string
        if (str(a.description)) next.description = a.description as string
        if (str(a.activeForm)) next.activeForm = a.activeForm as string
        if (str(a.owner)) next.owner = a.owner as string
        if (isStatus(a.status)) next.status = a.status
        return next
      })
    case "delete":
      return patch(a.id, (t) => ({ ...t, status: "deleted" }))
    case "clear":
      return { tasks: [], nextId: 1 }
    default:
      return state
  }
}

export type TodoCall = {
  action: string
  /** State before and after the call. */
  before: Task[]
  after: Task[]
  /** Short description of the change ("#2 Tests: done"). */
  change: string
  running: boolean
  error?: string
}
export type TodoTimeline = { tasks: Task[]; calls: Record<string, TodoCall> }

export const statusLabel: Record<TaskStatus, string> = {
  pending: "pending",
  in_progress: "in progress",
  completed: "done",
  deleted: "removed",
}

function describe(action: string, args: Obj | undefined, before: Task[], after: Task[]): string {
  const find = (list: Task[]) => list.find((t) => t.id === args?.id)
  const name = (t: Task | undefined) => (t ? `#${t.id} ${t.subject}` : `#${String(args?.id ?? "?")}`)
  switch (action) {
    case "create": {
      const t = after.find((x) => !before.some((b) => b.id === x.id))
      return `New: ${t ? name(t) : (str(args?.subject) ?? "task")}`
    }
    case "update": {
      const t = find(after) ?? find(before)
      const was = find(before)
      if (t && was && t.status !== was.status) return `${name(t)}: ${statusLabel[t.status]}`
      return `${name(t)} changed`
    }
    case "delete":
      return `Removed: ${name(find(before) ?? find(after))}`
    case "clear":
      return "List cleared"
    case "get":
      return `${name(find(after))} queried`
    default:
      return "List queried"
  }
}

/**
 * Walks through the todo calls in the order of the history (blocks of the responses) and returns
 * per call the state before and after as well as the current state. A result with details is
 * authoritative; without details the arguments of a successful call are replayed.
 */
export function todoTimeline(transcript: TranscriptState): TodoTimeline {
  let state: TaskState = { tasks: [], nextId: 1 }
  const calls: Record<string, TodoCall> = {}
  for (const item of transcript.items) {
    if (item.kind !== "assistant") continue
    for (const b of item.blocks) {
      if (b.type !== "toolCall" || b.name !== TODO_TOOL || !b.id || calls[b.id]) continue
      const exec = transcript.tools[b.id]
      const args = parseArgs(b.arguments ?? exec?.args)
      const action = str(args?.action) ?? "?"
      const before = state.tasks
      const done = exec !== undefined && !exec.running && exec.result !== undefined
      const detailErr = isObj(exec?.details) ? str(exec.details.error) : undefined
      let error: string | undefined
      if (done && exec.isError) error = exec.result || "Error"
      else if (detailErr) error = detailErr
      else if (done && /^Error:/.test(exec.result ?? "")) error = exec.result
      if (done) {
        const snap = tasksFromDetails(exec.details)
        if (snap && !exec.isError) {
          const nextId = (exec.details as Obj).nextId as number
          state = { tasks: snap, nextId }
        } else if (!error) state = applyTodoArgs(state, args)
      }
      calls[b.id] = {
        action,
        before,
        after: state.tasks,
        change: error ? `Error: ${error.replace(/^Error:\s*/, "")}` : describe(action, args, before, state.tasks),
        running: !done,
        error,
      }
    }
  }
  return { tasks: state.tasks, calls }
}

export type TaskCounts = { total: number; completed: number; inProgress: number; pending: number }

/** Visible tasks (without removed ones). */
export const visibleTasks = (tasks: Task[]) => tasks.filter((t) => t.status !== "deleted")

export function taskCounts(tasks: Task[]): TaskCounts {
  const v = visibleTasks(tasks)
  return {
    total: v.length,
    completed: v.filter((t) => t.status === "completed").length,
    inProgress: v.filter((t) => t.status === "in_progress").length,
    pending: v.filter((t) => t.status === "pending").length,
  }
}

/** "3/7 tasks". */
export const taskCountLabel = (c: TaskCounts) => `${c.completed}/${c.total} ${c.total === 1 ? "task" : "tasks"}`

function countsText(c: TaskCounts): string {
  const parts: string[] = []
  if (c.completed) parts.push(`${c.completed} done`)
  if (c.inProgress) parts.push(`${c.inProgress} in progress`)
  if (c.pending) parts.push(`${c.pending} pending`)
  return parts.join(", ")
}

/** Line for a group of todo calls in the history, with the state afterwards. */
export function todoGroupSummary(actions: string[], after: Task[]): string {
  const c = taskCounts(after)
  if (c.total === 0) return actions.includes("clear") ? "Task list cleared" : "Task list empty"
  const verb = actions.every((a) => a === "create")
    ? "created"
    : actions.every((a) => a === "list" || a === "get")
      ? "queried"
      : "updated"
  return `Tasks ${verb}: ${countsText(c)}`
}

export type BlockGroup = { kind: "block"; index: number } | { kind: "todo"; indices: number[] }

/** Combines consecutive todo calls of a response into one group. */
export function groupTodoBlocks(blocks: ViewBlock[]): BlockGroup[] {
  const out: BlockGroup[] = []
  blocks.forEach((b, i) => {
    if (b.type === "toolCall" && b.name === TODO_TOOL) {
      const last = out[out.length - 1]
      if (last?.kind === "todo") last.indices.push(i)
      else out.push({ kind: "todo", indices: [i] })
    } else out.push({ kind: "block", index: i })
  })
  return out
}

/**
 * Tasks of a subagent run from its entries. The entries only carry arguments and
 * result text, so the arguments of successful calls are replayed. undefined if
 * the run did not call todo.
 */
export function tasksFromSubagentEntries(entries: SubagentEntry[]): Task[] | undefined {
  const failed = new Set(
    entries
      .filter((e) => e.kind === "tool_result" && e.payload.name === TODO_TOOL)
      .filter((e) => e.payload.is_error || /^Error:/.test(e.payload.text ?? ""))
      .map((e) => e.payload.tool_call_id),
  )
  let state: TaskState = { tasks: [], nextId: 1 }
  let seen = false
  for (const e of entries) {
    if (e.kind !== "tool_call" || e.payload.name !== TODO_TOOL) continue
    seen = true
    if (e.payload.id && failed.has(e.payload.id)) continue
    state = applyTodoArgs(state, e.payload.arguments)
  }
  return seen ? state.tasks : undefined
}

// Background tasks (bash with run_in_background): state from the API and the SSE events
// "background", labels and durations for the side tab and the chat header.
import type { BackgroundEvent, BackgroundState, BackgroundTask } from "@/api/types"
import { formatElapsed } from "@/lib/runtime"

/** Applies an event. A throttled "output" does not overwrite an end that has already happened. */
export function applyBackgroundEvent(list: BackgroundTask[], ev: BackgroundEvent): BackgroundTask[] {
  const t = ev.task
  if (!t?.id) return list
  const idx = list.findIndex((x) => x.id === t.id)
  if (idx < 0) return [...list, t]
  if (ev.change === "output" && list[idx].state !== "running") return list
  const next = list.slice()
  next[idx] = { ...list[idx], ...t }
  return next
}

/** Running ones first (oldest on top), then ended ones (newest on top). */
export function sortBackground(list: BackgroundTask[]): BackgroundTask[] {
  return [...list].sort((a, b) => {
    const ra = a.state === "running" ? 0 : 1
    const rb = b.state === "running" ? 0 : 1
    if (ra !== rb) return ra - rb
    return ra === 0 ? a.seq - b.seq : b.seq - a.seq
  })
}

export const runningCount = (list: BackgroundTask[]) => list.filter((t) => t.state === "running").length

export type Tone = "running" | "ok" | "error" | "muted"

/** Label and colour tone of a state. */
export function backgroundStatus(t: Pick<BackgroundTask, "state" | "exit_code" | "stopped_by" | "error">): { label: string; tone: Tone } {
  const labels: Record<BackgroundState, string> = {
    running: "running",
    exited: "ended",
    failed: "failed",
    timeout: "time limit",
    stopped: t.stopped_by === "user" ? "stopped by the user" : "stopped by the agent",
    lost: "lost with the sandbox",
    suspended: "ended when idling",
    closed: "ended with the chat",
  }
  switch (t.state) {
    case "running":
      return { label: labels.running, tone: "running" }
    case "exited":
      return t.exit_code === 0
        ? { label: "ended (exit 0)", tone: "ok" }
        : { label: `ended (exit ${t.exit_code ?? "?"})`, tone: "error" }
    case "failed":
    case "timeout":
      return { label: labels[t.state], tone: "error" }
    default:
      return { label: labels[t.state] ?? t.state, tone: "muted" }
  }
}

/** Runtime: until `now` while the task is running; otherwise until the end; nothing without times. */
export function backgroundRuntimeMs(t: Pick<BackgroundTask, "state" | "started_at" | "ended_at">, now: number): number | undefined {
  const start = Date.parse(t.started_at)
  if (Number.isNaN(start)) return undefined
  if (t.state === "running") return Math.max(0, now - start)
  const end = t.ended_at ? Date.parse(t.ended_at) : NaN
  return Number.isNaN(end) ? undefined : Math.max(0, end - start)
}

export function formatBackgroundRuntime(t: Pick<BackgroundTask, "state" | "started_at" | "ended_at">, now: number): string {
  const ms = backgroundRuntimeMs(t, now)
  return ms === undefined ? "" : formatElapsed(ms)
}

/** The last n lines of the output (without the trailing line break). */
export function tailLines(tail: string | undefined, n: number): string[] {
  const s = (tail ?? "").replace(/\n+$/, "")
  if (!s) return []
  const lines = s.split("\n")
  return lines.slice(Math.max(0, lines.length - n))
}

/** Short form of the command for one line. */
export function commandPreview(cmd: string, max = 120): string {
  const one = cmd.split(/\s+/).filter(Boolean).join(" ")
  return one.length > max ? `${one.slice(0, max).trimEnd()} …` : one
}

/** Label of the counter in the chat header. */
export function backgroundCountLabel(n: number): string {
  return n === 1 ? "1 background task running" : `${n} background tasks running`
}

/** Tool call that starts, queries or stops a background task. */
export type BackgroundCall = { kind: "start" | "output" | "stop"; id?: string; task?: BackgroundTask }

const isObj = (v: unknown): v is Record<string, unknown> => typeof v === "object" && v !== null && !Array.isArray(v)

/**
 * Recognises bash with run_in_background (task via tool_call_id, otherwise the ID from the result
 * "Background task bg-3 started …") as well as bg_output and bg_stop; otherwise undefined.
 */
export function backgroundCall(
  name: string,
  args: unknown,
  toolCallId: string | undefined,
  result: string | undefined,
  tasks: BackgroundTask[],
): BackgroundCall | undefined {
  const a = isObj(args) ? args : {}
  if (name === "bash") {
    const task = toolCallId ? tasks.find((t) => t.tool_call_id === toolCallId) : undefined
    if (!task && a.run_in_background !== true) return undefined
    const id = task?.id ?? /\bBackground task (bg-\d+) started\b/.exec(result ?? "")?.[1]
    const out: BackgroundCall = { kind: "start" }
    if (id) out.id = id
    if (task) out.task = task
    return out
  }
  if (name === "bg_output" || name === "bg_stop") {
    const out: BackgroundCall = { kind: name === "bg_output" ? "output" : "stop" }
    if (typeof a.id === "string" && a.id) {
      out.id = a.id
      const task = tasks.find((t) => t.id === a.id)
      if (task) out.task = task
    }
    return out
  }
  return undefined
}

/** Compact line for bg_output and bg_stop. */
export function backgroundCallLabel(c: Pick<BackgroundCall, "kind" | "id">, phase: "running" | "done" | "error"): string {
  const id = c.id ?? "background task"
  if (c.kind === "output") {
    return phase === "running" ? `Fetching output of ${id} …` : phase === "error" ? `Output of ${id} not fetched` : `Fetched output of ${id}`
  }
  return phase === "running" ? `Stopping ${id} …` : phase === "error" ? `${id} not stopped` : `${id} stopped`
}

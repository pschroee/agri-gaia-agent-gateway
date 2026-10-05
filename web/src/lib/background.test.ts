import { describe, expect, it } from "vitest"
import type { BackgroundTask } from "@/api/types"
import {
  applyBackgroundEvent,
  backgroundCountLabel,
  backgroundRuntimeMs,
  backgroundStatus,
  commandPreview,
  formatBackgroundRuntime,
  runningCount,
  sortBackground,
  tailLines,
  backgroundCall,
  backgroundCallLabel,
} from "@/lib/background"

const task = (over: Partial<BackgroundTask>): BackgroundTask => ({
  id: "bg-1",
  seq: 1,
  chat_id: "c",
  session: "main",
  tool_call_id: "call_1",
  command: "sleep 8; echo done-bg",
  log_path: "/tmp/agw-bg/bg-1.log",
  state: "running",
  started_at: "2026-09-29T20:00:00Z",
  output_bytes: 0,
  output_lines: 0,
  ...over,
})

describe("background tasks", () => {
  it("applies start, output and end; a late output does not overwrite the end", () => {
    let l = applyBackgroundEvent([], { change: "started", task: task({}) })
    l = applyBackgroundEvent(l, { change: "output", task: task({ tail: "a\n", output_bytes: 2 }) })
    expect(l).toHaveLength(1)
    expect(l[0].tail).toBe("a\n")
    l = applyBackgroundEvent(l, { change: "ended", task: task({ state: "exited", exit_code: 0, tail: "a\ndone-bg\n", ended_at: "2026-09-29T20:00:08Z" }) })
    l = applyBackgroundEvent(l, { change: "output", task: task({ tail: "old\n" }) })
    expect(l[0].state).toBe("exited")
    expect(l[0].tail).toBe("a\ndone-bg\n")
    expect(applyBackgroundEvent(l, { change: "started", task: task({ id: "bg-2", seq: 2 }) })).toHaveLength(2)
  })

  it("sorts running ones first and counts them", () => {
    const l = [
      task({ id: "bg-1", seq: 1, state: "exited" }),
      task({ id: "bg-3", seq: 3, state: "running" }),
      task({ id: "bg-2", seq: 2, state: "running" }),
      task({ id: "bg-4", seq: 4, state: "stopped" }),
    ]
    expect(sortBackground(l).map((t) => t.id)).toEqual(["bg-2", "bg-3", "bg-4", "bg-1"])
    expect(runningCount(l)).toBe(2)
    expect(backgroundCountLabel(1)).toBe("1 background task running")
    expect(backgroundCountLabel(3)).toBe("3 background tasks running")
  })

  it("labels the states", () => {
    expect(backgroundStatus({ state: "running" })).toEqual({ label: "running", tone: "running" })
    expect(backgroundStatus({ state: "exited", exit_code: 0 }).tone).toBe("ok")
    expect(backgroundStatus({ state: "exited", exit_code: 2 })).toEqual({ label: "ended (exit 2)", tone: "error" })
    expect(backgroundStatus({ state: "stopped", stopped_by: "user" }).label).toBe("stopped by the user")
    expect(backgroundStatus({ state: "stopped", stopped_by: "agent" }).label).toBe("stopped by the agent")
    expect(backgroundStatus({ state: "suspended" })).toEqual({ label: "ended when idling", tone: "muted" })
    expect(backgroundStatus({ state: "failed" }).tone).toBe("error")
  })

  it("counts the runtime up live and stops it at the end", () => {
    const now = Date.parse("2026-09-29T20:01:05Z")
    expect(backgroundRuntimeMs(task({}), now)).toBe(65_000)
    expect(formatBackgroundRuntime(task({}), now)).toBe("1:05")
    const done = task({ state: "exited", ended_at: "2026-09-29T20:00:08Z" })
    expect(formatBackgroundRuntime(done, now)).toBe("8 s")
    expect(backgroundRuntimeMs(task({ state: "exited" }), now)).toBeUndefined()
    expect(backgroundRuntimeMs(task({ started_at: "broken" }), now)).toBeUndefined()
  })

  it("returns the last lines and a short command line", () => {
    expect(tailLines("a\nb\nc\n\n", 2)).toEqual(["b", "c"])
    expect(tailLines("", 3)).toEqual([])
    expect(tailLines(undefined, 3)).toEqual([])
    expect(commandPreview("python  train.py\n --epochs 3")).toBe("python train.py --epochs 3")
    expect(commandPreview("x".repeat(200), 10)).toBe("xxxxxxxxxx …")
  })
})

describe("tool calls for background tasks", () => {
  const tasks = [task({ id: "bg-3", seq: 3, tool_call_id: "call_9" })]

  it("bash with run_in_background: task via tool_call_id", () => {
    expect(backgroundCall("bash", { command: "x", run_in_background: true }, "call_9", undefined, tasks)).toMatchObject({
      kind: "start",
      id: "bg-3",
      task: { id: "bg-3" },
    })
  })

  it("ID from the result while the task is not known yet", () => {
    const r = "Background task bg-7 started. You will be notified when it ends; do not poll or sleep."
    expect(backgroundCall("bash", { command: "x", run_in_background: true }, "call_x", r, tasks)).toEqual({ kind: "start", id: "bg-7" })
    expect(backgroundCall("bash", { command: "x", run_in_background: true }, "call_x", undefined, [])).toEqual({ kind: "start" })
  })

  it("ordinary bash is not a background call", () => {
    expect(backgroundCall("bash", { command: "x" }, "call_x", "ok", tasks)).toBeUndefined()
    expect(backgroundCall("read", { path: "a" }, "call_9", "ok", tasks)).toBeUndefined()
  })

  it("bg_output and bg_stop", () => {
    expect(backgroundCall("bg_output", { id: "bg-3" }, "c", undefined, tasks)).toMatchObject({ kind: "output", id: "bg-3", task: { id: "bg-3" } })
    expect(backgroundCall("bg_stop", { id: "bg-4" }, "c", undefined, tasks)).toEqual({ kind: "stop", id: "bg-4" })
    expect(backgroundCall("bg_stop", {}, "c", undefined, tasks)).toEqual({ kind: "stop" })
  })

  it("label per state", () => {
    expect(backgroundCallLabel({ kind: "output", id: "bg-3" }, "done")).toBe("Fetched output of bg-3")
    expect(backgroundCallLabel({ kind: "output", id: "bg-3" }, "running")).toBe("Fetching output of bg-3 …")
    expect(backgroundCallLabel({ kind: "output", id: "bg-3" }, "error")).toBe("Output of bg-3 not fetched")
    expect(backgroundCallLabel({ kind: "stop", id: "bg-3" }, "done")).toBe("bg-3 stopped")
    expect(backgroundCallLabel({ kind: "stop", id: "bg-3" }, "running")).toBe("Stopping bg-3 …")
    expect(backgroundCallLabel({ kind: "stop" }, "error")).toBe("background task not stopped")
  })
})

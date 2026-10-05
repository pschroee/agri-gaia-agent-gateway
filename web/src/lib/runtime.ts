// Durations as in Claude Code: how long the current run, a tool or a response took.
// The times come from the events (lib/stream: runStart, startedAt/endedAt, durationMs); whatever cannot
// be verified stays empty instead of being estimated.
import type { Chat } from "@/api/types"
import type { ToolExecution, TranscriptState } from "@/lib/stream"

const pad = (n: number) => String(n).padStart(2, "0")

/** Duration in ms: below 60 s "12 s", above "1:05", from one hour "1:02:03". */
export function formatElapsed(ms: number): string {
  if (!Number.isFinite(ms)) return ""
  const secs = Math.floor(Math.max(0, ms) / 1000)
  if (secs < 60) return `${secs} s`
  const h = Math.floor(secs / 3600)
  const m = Math.floor((secs % 3600) / 60)
  const s = secs % 60
  return h > 0 ? `${h}:${pad(m)}:${pad(s)}` : `${m}:${pad(s)}`
}

/** Completed duration: below one second "< 1 s" instead of "0 s"; empty without a value. */
export function formatStepDuration(ms: number | undefined): string {
  if (ms === undefined || !Number.isFinite(ms)) return ""
  return ms < 1000 ? "< 1 s" : formatElapsed(ms)
}

/**
 * Start of the current turn: live, the receipt of agent_start. If the start was not observed
 * (page reloaded during the run), the time of the last user message, since the run begins
 * with it. If the agent is not working, nothing.
 */
export function runStartOf(state: TranscriptState, running: boolean): number | undefined {
  if (!running) return undefined
  if (state.runStart !== undefined) return state.runStart
  for (let i = state.items.length - 1; i >= 0; i--) {
    const item = state.items[i]
    if (item.kind === "user") return item.time
  }
  return undefined
}

/**
 * Start of a chat's run for header and list: the orchestrator's value (`running_since`) as soon as it
 * provides it, otherwise the one from the history (runStartOf). If the chat is idle or nothing is known: nothing.
 */
export function chatRunSince(chat: Pick<Chat, "running" | "running_since"> | undefined, state?: TranscriptState): number | undefined {
  if (!chat?.running) return undefined
  const server = chat.running_since ? Date.parse(chat.running_since) : NaN
  if (!Number.isNaN(server)) return server
  return state ? runStartOf(state, true) : undefined
}

/** Duration of a tool call: running until `now`, finished from start to end; nothing without a start. */
export function toolDurationMs(t: ToolExecution | undefined, now: number): number | undefined {
  if (!t || t.startedAt === undefined) return undefined
  if (t.endedAt !== undefined) return Math.max(0, t.endedAt - t.startedAt)
  return t.running ? Math.max(0, now - t.startedAt) : undefined
}

/** What the agent is doing right now, for the status line ("Thinking", "Writing", "Running bash" …). */
export function liveActivity(state: TranscriptState): string {
  const running = Object.values(state.tools).find((t) => t.running)
  if (running) return running.toolName ? `Running ${running.toolName}` : "Running a tool"
  const item = state.streamingKey ? state.items.find((i) => i.key === state.streamingKey) : undefined
  const last = item?.kind === "assistant" ? item.blocks.at(-1) : undefined
  if (last?.type === "toolCall") return last.name ? `Preparing ${last.name}` : "Preparing a tool call"
  if (last?.type === "text") return "Writing"
  // After a reload tool_execution_start and streamingKey are missing: if the history ends with a
  // response whose tool call has no result yet, exactly this tool is running.
  const tail = state.items.at(-1)
  if (!item && tail?.kind === "assistant") {
    const open = tail.blocks.find((b) => b.type === "toolCall" && b.id && !state.tools[b.id]?.result && !state.tools[b.id]?.isError)
    if (open?.type === "toolCall") return open.name ? `Running ${open.name}` : "Running a tool"
  }
  return "Thinking"
}

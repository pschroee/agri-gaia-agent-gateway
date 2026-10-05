// Assembles a chat's history from the stored messages and pi's live events
// (see "Assembling the stream" in poc/API.md). Pure functions, without React.
import type { ContentBlock, MessageMeta, MessageOrigin, MessageSource, PiEvent, ResumePhase, ResumeStep, StoredMessage, TurnTrigger, Usage } from "@/api/types"
import { splitAttachments } from "@/lib/attachments"
import { messageImageKey } from "@/lib/images"

export type ToolCallBlock = {
  type: "toolCall"
  id: string
  name: string
  arguments?: unknown
  /** Raw JSON of the arguments while they are still being streamed. */
  argsText?: string
}
export type ViewBlock =
  | { type: "text"; text: string }
  | { type: "thinking"; thinking: string }
  | ToolCallBlock
  | { type: "image"; mimeType?: string }

/**
 * `time`: time in ms (history: created_at, live: pi's timestamp), if known. `origin`,
 * `sources`, `trigger`, `turnId`: origin according to the server (Review 3, H1); missing on old rows.
 */
export type UserItem = {
  kind: "user"
  key: string
  text: string
  seq?: number
  time?: number
  origin?: MessageOrigin
  sources?: MessageSource[]
  trigger?: TurnTrigger
  turnId?: number
}
export type AssistantItem = {
  kind: "assistant"
  key: string
  blocks: ViewBlock[]
  streaming: boolean
  seq?: number
  time?: number
  usage?: Usage
  /** Cost by tariff (from the orchestrator, only from the history). */
  cost?: number
  /** Response fell into peak hours. */
  peak?: boolean
  stopReason?: string
  errorMessage?: string
  model?: string
  /** ID of the finished response for its images (responseId or ts-<timestamp>); missing while streaming. */
  msgKey?: string
  /**
   * Total duration of the run in ms, only on the response that ends it (stopReason not "toolUse"). Live, measured
   * from agent_start; after a reload from created_at of user message and response; otherwise empty.
   */
  durationMs?: number
}
export type CompactionReason = "manual" | "threshold" | "overflow"
export type CompactionItem = {
  kind: "compaction"
  key: string
  seq?: number
  time?: number
  reason: CompactionReason | string
  /** Compaction still running (between compaction_start and compaction_end). */
  running: boolean
  summary?: string
  tokensBefore?: number
  tokensAfter?: number
  usage?: Usage
  cost?: number
  aborted?: boolean
  errorMessage?: string
  /**
   * Compaction aborted or failed. Such entries exist only live (the history
   * does not store them); on reload they stay behind the message `afterSeq`.
   */
  failed?: boolean
  afterSeq?: number
}
/** Step while resuming, as the block in the history shows it; "pending" has not started yet. */
export type ResumeStepView = {
  phase: ResumePhase
  status: "pending" | ResumeStep["status"]
  detail?: string
  size?: number
  files?: number
  ms?: number
}
/**
 * Resuming in a fresh sandbox (SSE "resume"). Only known live; on reload the entry stays
 * behind the message `afterSeq` (with `withUser` behind the user message that followed).
 */
export type ResumeItem = {
  kind: "resume"
  key: string
  id: string
  state: "running" | "done" | "failed"
  steps: ResumeStepView[]
  totalMs?: number
  error?: string
  time?: number
  seq?: undefined
  afterSeq?: number
  withUser?: boolean
}
/**
 * Message from an extension in pi to the agent (role custom), e.g. the end of a subagent in the
 * background (pi-subagents). Not from the user; `trigger` wake if it started a turn.
 */
export type NoticeItem = {
  kind: "notice"
  key: string
  seq?: number
  time?: number
  text: string
  customType?: string
  trigger?: TurnTrigger
}
export type TranscriptItem = UserItem | AssistantItem | CompactionItem | ResumeItem | NoticeItem

/**
 * Sent user message that pi has not confirmed yet (optimistic). It stands at the end of the
 * history and is replaced by the real message (message_end, role user). `failed`: not
 * sent (error while sending or resuming).
 */
export type PendingUser = {
  key: string
  text: string
  failed?: boolean
  afterSeq?: number
  /** Origin according to the server (handed-over queue) */
  origin?: MessageOrigin
  sources?: MessageSource[]
}

export type ToolExecution = {
  toolCallId: string
  toolName: string
  args?: unknown
  running: boolean
  /** Output so far from tool_execution_update (replaced, not appended). */
  output?: string
  result?: string
  isError?: boolean
  /** Structured result of the tool (`details`), e.g. the state of the task list for todo. */
  details?: unknown
  /** Start and end in ms: live on receipt of tool_execution_start/_end, after a reload from created_at. */
  startedAt?: number
  endedAt?: number
}

export type TranscriptState = {
  items: TranscriptItem[]
  tools: Record<string, ToolExecution>
  /** Role of the message between message_start and message_end. */
  currentRole?: string
  /** Key of the response currently being streamed. */
  streamingKey?: string
  /** Key of the compaction currently running. */
  compactingKey?: string
  /** Sent, not yet confirmed by pi; appears below the history. */
  pending: PendingUser[]
  /**
   * Running or just finished resume. It stands below the sent messages until pi's
   * user message arrives, and then moves into the history directly behind it.
   */
  resume?: ResumeItem
  /** Start of the current turn (receipt of agent_start, ms); until agent_settled. */
  runStart?: number
  /** Origin of pi's next user message (SSE "user_meta" arrives right before it). */
  nextUserMeta?: MessageMeta
  nextKey: number
}

export const emptyTranscript = (): TranscriptState => ({ items: [], tools: {}, pending: [], nextKey: 1 })

type Obj = Record<string, unknown>
const isObj = (v: unknown): v is Obj => typeof v === "object" && v !== null
const str = (v: unknown): string | undefined => (typeof v === "string" ? v : undefined)
const msgTime = (m: Obj): number | undefined =>
  typeof m.timestamp === "number" && Number.isFinite(m.timestamp) ? m.timestamp : undefined
const isoTime = (iso: string | undefined): number | undefined => {
  const v = iso ? Date.parse(iso) : NaN
  return Number.isNaN(v) ? undefined : v
}

/** Turns message content (string or block list) into text. */
export function contentToText(content: unknown): string {
  if (typeof content === "string") return content
  if (!Array.isArray(content)) return ""
  return content
    .map((b) => {
      if (!isObj(b)) return ""
      if (b.type === "text") return str(b.text) ?? ""
      if (b.type === "image") return "[Bild]"
      return ""
    })
    .join("")
}

/** Result or partial result of a tool execution as text. */
export function resultToText(result: unknown): string {
  if (result === undefined || result === null) return ""
  if (typeof result === "string") return result
  if (isObj(result) && "content" in result) return contentToText(result.content)
  if (Array.isArray(result)) return contentToText(result)
  try {
    return JSON.stringify(result, null, 2)
  } catch {
    return String(result)
  }
}

function toViewBlocks(content: unknown): ViewBlock[] {
  if (!Array.isArray(content)) return []
  return (content as ContentBlock[]).filter(isObj) as ViewBlock[]
}

function newKey(state: TranscriptState, prefix: string): [string, number] {
  return [`${prefix}-${state.nextKey}`, state.nextKey + 1]
}

function setTool(state: TranscriptState, id: string, patch: Partial<ToolExecution>): TranscriptState {
  const prev = state.tools[id] ?? { toolCallId: id, toolName: "", running: false }
  return { ...state, tools: { ...state.tools, [id]: { ...prev, ...patch } } }
}

function applyToolResultMessage(state: TranscriptState, m: Obj): TranscriptState {
  const id = str(m.toolCallId)
  if (!id) return state
  return setTool(state, id, {
    toolName: str(m.toolName) ?? state.tools[id]?.toolName ?? "",
    running: false,
    result: contentToText(m.content),
    isError: m.isError === true,
    details: m.details,
  })
}

/** Returns the state with a response currently being streamed (creates it if needed). */
function ensureStreaming(state: TranscriptState): [TranscriptState, number] {
  if (state.streamingKey) {
    const idx = state.items.findIndex((i) => i.key === state.streamingKey)
    if (idx >= 0) return [state, idx]
  }
  state = flushResume(state)
  const [key, nextKey] = newKey(state, "live")
  const item: AssistantItem = { kind: "assistant", key, blocks: [], streaming: true }
  const next = { ...state, items: [...state.items, item], streamingKey: key, currentRole: "assistant", nextKey }
  return [next, next.items.length - 1]
}

function updateBlocks(
  state: TranscriptState,
  fn: (blocks: ViewBlock[]) => ViewBlock[],
): TranscriptState {
  const [s, idx] = ensureStreaming(state)
  const item = s.items[idx] as AssistantItem
  const items = s.items.slice()
  items[idx] = { ...item, blocks: fn(item.blocks) }
  return { ...s, items }
}

/** Index of the block a delta goes to: contentIndex, otherwise the last block of the type. */
function blockIndex(blocks: ViewBlock[], ev: Obj, type: ViewBlock["type"]): number {
  if (typeof ev.contentIndex === "number") return ev.contentIndex
  const last = blocks.length - 1
  return last >= 0 && blocks[last].type === type ? last : blocks.length
}

function withBlock(blocks: ViewBlock[], idx: number, fn: (b: ViewBlock | undefined) => ViewBlock): ViewBlock[] {
  const next = blocks.slice()
  // fill gaps (missed *_start events) with empty text
  while (next.length < idx) next.push({ type: "text", text: "" })
  next[idx] = fn(next[idx])
  return next
}

function applyAssistantEvent(state: TranscriptState, ev: Obj): TranscriptState {
  const t = str(ev.type)
  switch (t) {
    case "text_start":
    case "text_delta": {
      const delta = str(ev.delta) ?? ""
      return updateBlocks(state, (blocks) =>
        withBlock(blocks, blockIndex(blocks, ev, "text"), (b) => ({
          type: "text",
          text: (b?.type === "text" ? b.text : "") + delta,
        })),
      )
    }
    case "thinking_start":
    case "thinking_delta": {
      const delta = str(ev.delta) ?? ""
      return updateBlocks(state, (blocks) =>
        withBlock(blocks, blockIndex(blocks, ev, "thinking"), (b) => ({
          type: "thinking",
          thinking: (b?.type === "thinking" ? b.thinking : "") + delta,
        })),
      )
    }
    case "toolcall_start": {
      let id = str(ev.id)
      let name = str(ev.toolName)
      const partial = isObj(ev.partial) && Array.isArray(ev.partial.content) ? ev.partial.content : undefined
      if (partial && typeof ev.contentIndex === "number") {
        const pb = partial[ev.contentIndex]
        if (isObj(pb)) {
          id ??= str(pb.id)
          name ??= str(pb.name)
        }
      }
      return updateBlocks(state, (blocks) =>
        withBlock(blocks, typeof ev.contentIndex === "number" ? ev.contentIndex : blocks.length, () => ({
          type: "toolCall",
          id: id ?? "",
          name: name ?? "",
          argsText: "",
        })),
      )
    }
    case "toolcall_delta": {
      const delta = str(ev.delta) ?? ""
      return updateBlocks(state, (blocks) =>
        withBlock(blocks, blockIndex(blocks, ev, "toolCall"), (b) =>
          b?.type === "toolCall"
            ? { ...b, argsText: (b.argsText ?? "") + delta }
            : { type: "toolCall", id: "", name: "", argsText: delta },
        ),
      )
    }
    case "toolcall_end": {
      const tc = isObj(ev.toolCall) ? ev.toolCall : undefined
      if (!tc) return state
      return updateBlocks(state, (blocks) =>
        withBlock(blocks, blockIndex(blocks, ev, "toolCall"), () => ({
          type: "toolCall",
          id: str(tc.id) ?? "",
          name: str(tc.name) ?? "",
          arguments: tc.arguments,
        })),
      )
    }
    default:
      return state
  }
}

function finishAssistant(state: TranscriptState, m: Obj, now: number): TranscriptState {
  const endsRun = str(m.stopReason) !== "toolUse"
  const final: Omit<AssistantItem, "key"> = {
    kind: "assistant",
    blocks: toViewBlocks(m.content),
    streaming: false,
    usage: isObj(m.usage) ? (m.usage as Usage) : undefined,
    stopReason: str(m.stopReason),
    errorMessage: str(m.errorMessage),
    model: str(m.model),
    time: msgTime(m),
    msgKey: messageImageKey(m),
    durationMs: endsRun && state.runStart !== undefined ? Math.max(0, now - state.runStart) : undefined,
  }
  const idx = state.streamingKey ? state.items.findIndex((i) => i.key === state.streamingKey) : -1
  if (idx >= 0) {
    const items = state.items.slice()
    const prev = state.items[idx]
    items[idx] = { ...final, key: prev.key, time: final.time ?? prev.time }
    return { ...state, items, streamingKey: undefined, currentRole: undefined }
  }
  const flushed = flushResume(state)
  const [key, nextKey] = newKey(flushed, "live")
  return { ...flushed, items: [...flushed.items, { ...final, key }], nextKey, streamingKey: undefined, currentRole: undefined }
}

const num = (v: unknown): number | undefined => (typeof v === "number" && Number.isFinite(v) ? v : undefined)

/** Fields of a compaction from pi's CompactionResult or the stored compaction message. */
function compactionFields(r: Obj): Partial<CompactionItem> {
  return {
    summary: str(r.summary),
    tokensBefore: num(r.tokensBefore),
    tokensAfter: num(r.estimatedTokensAfter),
    usage: isObj(r.usage) ? (r.usage as Usage) : undefined,
  }
}

// The orchestrator's error text (internal/chat/manager.go); the German form is the one used before the translation.
const compactPrefix = /^(Compaction failed|Kompaktierung fehlgeschlagen):\s*/i

/**
 * Notice text for a failed compaction. pi's messages "Nothing to compact" and
 * "Already compacted" are rephrased; the original message belongs in the tooltip.
 */
export function compactionNotice(message: string | undefined): string {
  const msg = (message ?? "").replace(compactPrefix, "").trim()
  if (/nothing to compact/i.test(msg)) return "Compaction not possible: not enough history to summarize yet"
  if (/already compacted/i.test(msg)) return "Compaction not needed: already summarized"
  return msg ? `Compaction failed: ${msg}` : "Compaction failed"
}

/** Whether an error message of the orchestrator (event `error`) concerns a compaction. */
export const isCompactionError = (message: string | undefined) => !!message && compactPrefix.test(message)

const lastSeq = (items: TranscriptItem[]) =>
  items.reduce<number | undefined>((m, i) => (i.seq !== undefined && (m === undefined || i.seq > m) ? i.seq : m), undefined)

/** A failed live entry at the end of the history with the same cause (against duplicate notices). */
function duplicateFailure(state: TranscriptState, message: string | undefined): number {
  const idx = state.items.length - 1
  const last = state.items[idx]
  if (!last || last.kind !== "compaction" || !last.failed || last.seq !== undefined) return -1
  return compactionNotice(last.errorMessage) === compactionNotice(message) ? idx : -1
}

function applyCompactionEnd(state: TranscriptState, ev: PiEvent): TranscriptState {
  const reason = str(ev.reason) ?? "manual"
  const aborted = ev.aborted === true
  const errorMessage = str(ev.errorMessage)
  const failed = aborted || !!errorMessage || !isObj(ev.result)
  const patch: Partial<CompactionItem> = {
    ...(isObj(ev.result) ? compactionFields(ev.result) : {}),
    running: false,
    aborted,
    errorMessage,
    failed,
    afterSeq: failed ? lastSeq(state.items) : undefined,
  }
  const key = state.compactingKey
  if (key && state.items.some((i) => i.key === key)) {
    const items = state.items.map((i) => (i.key === key && i.kind === "compaction" ? { ...i, ...patch, reason } : i))
    return { ...state, items, compactingKey: undefined }
  }
  if (failed && !aborted && duplicateFailure(state, errorMessage) >= 0) return { ...state, compactingKey: undefined }
  const [newK, nextKey] = newKey(state, "live")
  const item: CompactionItem = { kind: "compaction", key: newK, reason, ...patch, running: false }
  return { ...state, items: [...state.items, item], nextKey, compactingKey: undefined }
}

/**
 * The orchestrator's error message "Compaction failed: …" (event `error`): ends
 * a running compaction as failed or creates a notice, unless the same
 * error already arrived as compaction_end.
 */
export function applyCompactionError(state: TranscriptState, message: string): TranscriptState {
  const patch: Partial<CompactionItem> = {
    running: false,
    failed: true,
    errorMessage: message,
    afterSeq: lastSeq(state.items),
  }
  const key = state.compactingKey
  if (key && state.items.some((i) => i.key === key)) {
    const items = state.items.map((i) => (i.key === key && i.kind === "compaction" ? { ...i, ...patch } : i))
    return { ...state, items, compactingKey: undefined }
  }
  if (duplicateFailure(state, message) >= 0) return state
  const [newK, nextKey] = newKey(state, "live")
  const item: CompactionItem = { kind: "compaction", key: newK, reason: "manual", ...patch, running: false }
  return { ...state, items: [...state.items, item], nextKey }
}

/** Reducer: apply a pi event to the history. `now`: receipt time (for durations). */
export function applyPiEvent(state: TranscriptState, ev: PiEvent, now: number = Date.now()): TranscriptState {
  switch (ev.type) {
    case "agent_start":
      return { ...state, runStart: now }
    case "message_start": {
      const m = isObj(ev.message) ? ev.message : {}
      const role = str(m.role) ?? ""
      if (role !== "assistant") return { ...state, currentRole: role }
      const s = flushResume(state)
      const [key, nextKey] = newKey(s, "live")
      const item: AssistantItem = { kind: "assistant", key, blocks: toViewBlocks(m.content), streaming: true, time: msgTime(m) }
      return { ...s, items: [...s.items, item], streamingKey: key, currentRole: "assistant", nextKey }
    }
    case "message_update": {
      if (state.currentRole !== undefined && state.currentRole !== "assistant") return state
      const ame = ev.assistantMessageEvent
      return isObj(ame) ? applyAssistantEvent(state, ame) : state
    }
    case "message_end": {
      const m = isObj(ev.message) ? ev.message : {}
      const role = str(m.role)
      if (role === "assistant") return finishAssistant(state, m, now)
      const base = { ...state, currentRole: undefined }
      if (role === "user") {
        const text = contentToText(m.content)
        const [key, nextKey] = newKey(state, "live")
        const item: UserItem = { kind: "user", key, text, time: msgTime(m), ...metaFields(state.nextUserMeta) }
        const s = { ...base, pending: consumePending(state.pending, text), items: [...state.items, item], nextKey, nextUserMeta: undefined }
        return flushResume(s, true)
      }
      if (role === "toolResult") return applyToolResultMessage(base, m)
      if (role === "custom") {
        const [key, nextKey] = newKey(state, "live")
        const item: NoticeItem = { kind: "notice", key, text: contentToText(m.content), customType: str(m.customType), time: msgTime(m) }
        return { ...base, items: [...state.items, item], nextKey }
      }
      return base // system and other roles are not shown
    }
    case "tool_execution_start": {
      const id = str(ev.toolCallId)
      if (!id) return state
      return setTool(state, id, { toolName: str(ev.toolName) ?? "", args: ev.args, running: true, startedAt: now, endedAt: undefined })
    }
    case "tool_execution_update": {
      const id = str(ev.toolCallId)
      if (!id) return state
      return setTool(state, id, { running: true, output: resultToText(ev.partialResult) })
    }
    case "tool_execution_end": {
      const id = str(ev.toolCallId)
      if (!id) return state
      return setTool(state, id, {
        toolName: str(ev.toolName) ?? state.tools[id]?.toolName ?? "",
        running: false,
        result: resultToText(ev.result),
        isError: ev.isError === true,
        details: isObj(ev.result) ? ev.result.details : undefined,
        endedAt: now,
      })
    }
    case "compaction_start": {
      const s = flushResume(state)
      const [key, nextKey] = newKey(s, "live")
      const item: CompactionItem = { kind: "compaction", key, reason: str(ev.reason) ?? "manual", running: true }
      return { ...s, items: [...s.items, item], nextKey, compactingKey: key }
    }
    case "compaction_end":
      return applyCompactionEnd(state, ev)
    case "agent_end":
    case "agent_settled": {
      if (ev.type === "agent_settled" && state.runStart !== undefined) state = { ...state, runStart: undefined }
      const hasStreaming = state.items.some((i) => i.kind === "assistant" && i.streaming)
      const hasRunning = Object.values(state.tools).some((t) => t.running)
      if (!hasStreaming && !hasRunning && !state.streamingKey) return state
      const items = state.items.map((i) => (i.kind === "assistant" && i.streaming ? { ...i, streaming: false } : i))
      const tools = Object.fromEntries(
        Object.entries(state.tools).map(([k, t]) => [k, t.running ? { ...t, running: false } : t]),
      )
      return { ...state, items, tools, streamingKey: undefined, currentRole: undefined }
    }
    default:
      return state
  }
}

/**
 * Rebuilds the history from the stored messages. Completed live entries are
 * replaced by the history; a response still running and running tools are kept.
 */
export function hydrate(prev: TranscriptState, history: StoredMessage[]): TranscriptState {
  let state: TranscriptState = { items: [], tools: {}, pending: [], nextKey: prev.nextKey, runStart: prev.runStart }
  const sorted = [...history].sort((a, b) => a.seq - b.seq)
  // end of the response that called a tool, if it contained exactly one call (start of the tool)
  const toolStart = new Map<string, number>()
  // total duration per run: from the user message to the response that ends it (both created_at of the
  // orchestrator). If not a user message but the end of an earlier run precedes it: no value.
  let runFrom: number | undefined
  for (const sm of sorted) {
    const m = sm.message as Obj
    const role = str(m.role) ?? sm.role
    const key = `seq-${sm.seq}`
    const time = isoTime(sm.created_at) ?? msgTime(m)
    if (role === "user") {
      runFrom = isoTime(sm.created_at)
      const meta = metaFields({ origin: sm.origin, sources: sm.sources, trigger: sm.trigger, turn_id: sm.turn_id })
      state = { ...state, items: [...state.items, { kind: "user", key, seq: sm.seq, time, text: contentToText(m.content), ...meta }] }
    } else if (role === "assistant") {
      const end = isoTime(sm.created_at)
      let durationMs: number | undefined
      if (str(m.stopReason) !== "toolUse") {
        durationMs = runFrom !== undefined && end !== undefined && end >= runFrom ? end - runFrom : undefined
        runFrom = undefined
      }
      const item: AssistantItem = {
        kind: "assistant",
        key,
        seq: sm.seq,
        time,
        blocks: toViewBlocks(m.content),
        streaming: false,
        usage: isObj(m.usage) ? (m.usage as Usage) : undefined,
        cost: typeof sm.cost === "number" ? sm.cost : undefined,
        peak: typeof sm.peak === "boolean" ? sm.peak : undefined,
        stopReason: str(m.stopReason),
        errorMessage: str(m.errorMessage),
        model: str(m.model),
        msgKey: messageImageKey(m),
        durationMs,
      }
      state = { ...state, items: [...state.items, item] }
      const calls = item.blocks.filter((b) => b.type === "toolCall")
      if (calls.length === 1 && calls[0].type === "toolCall" && calls[0].id && end !== undefined) toolStart.set(calls[0].id, end)
    } else if (role === "toolResult") {
      state = applyToolResultMessage(state, m)
      const id = str(m.toolCallId)
      if (id) {
        // times measured live take precedence; otherwise from the timestamps, where unambiguous
        const live = prev.tools[id]
        const times =
          live?.startedAt !== undefined && live.endedAt !== undefined
            ? { startedAt: live.startedAt, endedAt: live.endedAt }
            : { startedAt: toolStart.get(id), endedAt: toolStart.has(id) ? isoTime(sm.created_at) : undefined }
        state = setTool(state, id, times)
      }
    } else if (role === "custom") {
      const item: NoticeItem = {
        kind: "notice",
        key,
        seq: sm.seq,
        time,
        text: contentToText(m.content),
        customType: str(m.customType),
        trigger: sm.trigger as TurnTrigger | undefined,
      }
      state = { ...state, items: [...state.items, item] }
    } else if (role === "compaction") {
      const item: CompactionItem = {
        kind: "compaction",
        key,
        seq: sm.seq,
        time,
        reason: str(m.reason) ?? "manual",
        running: false,
        ...compactionFields(m),
        cost: typeof sm.cost === "number" ? sm.cost : undefined,
      }
      state = { ...state, items: [...state.items, item] }
    }
  }
  // re-insert failed compactions and resumes (only known live) behind their message
  for (const f of prev.items) {
    const liveOnly = (f.kind === "compaction" && f.failed) || f.kind === "resume"
    if (!liveOnly || f.seq !== undefined) continue
    const at = f.afterSeq === undefined ? 0 : state.items.findLastIndex((i) => i.seq !== undefined && i.seq <= f.afterSeq!) + 1
    // line up behind notices already inserted at the same position
    let pos = at
    while (pos < state.items.length && state.items[pos].seq === undefined) pos++
    // a resume stands behind the user message that triggered it
    if (f.kind === "resume" && f.withUser && state.items[pos]?.kind === "user") pos++
    state = { ...state, items: [...state.items.slice(0, pos), f, ...state.items.slice(pos)] }
  }
  // do not show sent messages twice that are in the history by now
  state = { ...state, pending: prunePending(prev.pending ?? [], sorted), resume: prev.resume }
  // take over running tools without a stored result
  for (const [id, t] of Object.entries(prev.tools)) {
    if (t.running && !state.tools[id]) state = { ...state, tools: { ...state.tools, [id]: t } }
  }
  const compacting = prev.compactingKey ? prev.items.find((i) => i.key === prev.compactingKey) : undefined
  if (compacting && compacting.kind === "compaction" && compacting.running) {
    state = { ...state, items: [...state.items, compacting], compactingKey: compacting.key }
  }
  const streaming = prev.streamingKey ? prev.items.find((i) => i.key === prev.streamingKey) : undefined
  if (streaming && streaming.kind === "assistant" && streaming.streaming) {
    state = { ...state, items: [...state.items, streaming], streamingKey: streaming.key, currentRole: "assistant" }
  }
  return state
}

// --- Sent messages and resuming ---

/** Last known message number in the history (anchor for entries only known live). */
export const lastKnownSeq = (state: TranscriptState) => lastSeq(state.items)

/**
 * Show a message optimistically before the server answers. Failed sends and a
 * failed resume disappear in the process (their text was back in the input field).
 */
export function addPending(state: TranscriptState, key: string, text: string): TranscriptState {
  const pending = [...state.pending.filter((p) => !p.failed), { key, text, afterSeq: lastSeq(state.items) }]
  const resume = state.resume?.state === "failed" ? undefined : state.resume
  return { ...state, pending, resume }
}

/**
 * Queued messages have been handed over (SSE "queue", change "delivered"): the request appears
 * right away as a sent message. If a just-sent message went along (it stands at the end
 * of the request), its display is extended instead of creating a second one.
 */
export function applyQueueDelivered(state: TranscriptState, key: string, text: string, meta?: Pick<MessageMeta, "origin" | "sources">): TranscriptState {
  const body = splitAttachments(text).text
  const origin = meta?.origin ? { origin: meta.origin, sources: meta.sources } : {}
  const idx = state.pending.findIndex((p) => !p.failed && body.endsWith(splitAttachments(p.text).text))
  if (idx >= 0) {
    const pending = state.pending.slice()
    pending[idx] = { ...pending[idx], text, ...origin }
    return { ...state, pending }
  }
  return { ...state, pending: [...state.pending, { key, text, afterSeq: lastSeq(state.items), ...origin }] }
}

/** SSE "user_meta": origin of the user message pi reports next. */
export function applyUserMeta(state: TranscriptState, meta: MessageMeta): TranscriptState {
  return { ...state, nextUserMeta: meta }
}

/** Origin fields for an entry in the history (only what the server states). */
function metaFields(meta: MessageMeta | undefined): Partial<UserItem> {
  if (!meta) return {}
  const out: Partial<UserItem> = {}
  if (meta.origin) out.origin = meta.origin
  if (meta.sources) out.sources = meta.sources
  if (meta.trigger) out.trigger = meta.trigger
  if (meta.turn_id !== undefined && meta.turn_id !== null) out.turnId = meta.turn_id
  return out
}

/** Sending failed: the message stays visible, marked as not sent. */
export function failPending(state: TranscriptState, key: string): TranscriptState {
  if (!state.pending.some((p) => p.key === key)) return state
  return { ...state, pending: state.pending.map((p) => (p.key === key ? { ...p, failed: true } : p)) }
}

/** Remove an optimistic message (e.g. because it was queued after all). */
export function dropPending(state: TranscriptState, key: string): TranscriptState {
  if (!state.pending.some((p) => p.key === key)) return state
  return { ...state, pending: state.pending.filter((p) => p.key !== key) }
}

/**
 * pi's user message replaces the oldest matching sent one (same text); otherwise the
 * oldest not yet failed one, because pi can change the text (skills, templates).
 */
function consumePending(pending: PendingUser[], text: string): PendingUser[] {
  if (pending.length === 0) return pending
  let idx = pending.findIndex((p) => !p.failed && p.text === text)
  if (idx < 0) idx = pending.findIndex((p) => !p.failed)
  return idx < 0 ? pending : [...pending.slice(0, idx), ...pending.slice(idx + 1)]
}

/** Sent messages already in the history (after their anchor, same text) are dropped. */
function prunePending(pending: PendingUser[], history: StoredMessage[]): PendingUser[] {
  const used = new Set<number>()
  return pending.filter((p) => {
    if (p.failed) return true
    const hit = history.find(
      (sm) =>
        !used.has(sm.seq) &&
        (p.afterSeq === undefined || sm.seq > p.afterSeq) &&
        (str((sm.message as Obj).role) ?? sm.role) === "user" &&
        contentToText((sm.message as Obj).content) === p.text,
    )
    if (!hit) return true
    used.add(hit.seq)
    return false
  })
}

const RESUME_PHASES: ResumePhase[] = ["acquire", "session", "settings", "workspace", "inputs"]

function newResume(state: TranscriptState, step: ResumeStep): TranscriptState {
  const [key, nextKey] = newKey(state, "resume")
  const t = Date.parse(step.at)
  const item: ResumeItem = {
    kind: "resume",
    key,
    id: step.id,
    state: "running",
    steps: RESUME_PHASES.map((phase) => ({ phase, status: "pending" })),
    time: Number.isNaN(t) ? undefined : t,
    afterSeq: state.pending.find((p) => !p.failed)?.afterSeq ?? lastSeq(state.items),
  }
  return { ...state, resume: item, nextKey }
}

/** Reducer for a "resume" event (steps while resuming, see poc/API.md). */
export function applyResumeStep(state: TranscriptState, step: ResumeStep): TranscriptState {
  let s = state
  if (!s.resume || s.resume.id !== step.id) {
    // first move an earlier, finished resume into the history
    s = newResume(flushResume(s), step)
  }
  const r = s.resume!
  let next: ResumeItem
  if (step.phase === "ready") {
    next = { ...r, state: "done", totalMs: step.ms }
  } else if (step.phase === "failed") {
    next = { ...r, state: "failed", totalMs: step.ms, error: step.detail }
  } else {
    const view: ResumeStepView = { phase: step.phase, status: step.status, detail: step.detail, size: step.size, files: step.files, ms: step.ms }
    const steps = r.steps.some((x) => x.phase === step.phase)
      ? r.steps.map((x) => (x.phase === step.phase ? { ...x, ...view } : x))
      : [...r.steps, view]
    next = { ...r, steps }
  }
  return { ...s, resume: next }
}

/**
 * A finished resume moves into the history: behind the user message that triggered
 * it (afterUser), otherwise before the next entry. A running one stays in place.
 */
function flushResume(state: TranscriptState, afterUser = false): TranscriptState {
  const r = state.resume
  if (!r || r.state === "running") return state
  if (r.state === "failed" && !afterUser) return state
  return { ...state, items: [...state.items, { ...r, withUser: afterUser }], resume: undefined }
}

/** Whether the chat is waiting for the first response right now (for "Thinking …"). */
export function awaitingAnswer(state: TranscriptState): boolean {
  if (state.streamingKey) return false
  if (Object.values(state.tools).some((t) => t.running)) return false
  if (state.resume?.state === "running") return false
  return true
}

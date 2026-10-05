// Setzt den Verlauf eines Chats aus der gespeicherten Historie und den Live-Ereignissen von pi
// zusammen (siehe „Streaming zusammensetzen" in poc/API.md). Reine Funktionen, ohne React.
import type { ContentBlock, MessageMeta, MessageOrigin, MessageSource, PiEvent, ResumePhase, ResumeStep, StoredMessage, TurnTrigger, Usage } from "@/api/types"
import { splitAttachments } from "@/lib/attachments"
import { messageImageKey } from "@/lib/images"

export type ToolCallBlock = {
  type: "toolCall"
  id: string
  name: string
  arguments?: unknown
  /** Roh-JSON der Argumente, solange sie noch gestreamt werden. */
  argsText?: string
}
export type ViewBlock =
  | { type: "text"; text: string }
  | { type: "thinking"; thinking: string }
  | ToolCallBlock
  | { type: "image"; mimeType?: string }

/**
 * `time`: Zeitpunkt in ms (Historie: created_at, live: timestamp von pi), sofern bekannt. `origin`,
 * `sources`, `trigger`, `turnId`: Herkunft laut Server (Review 3, H1); fehlen bei alten Zeilen.
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
  /** Kosten nach Tarif (vom Orchestrator, nur aus der Historie). */
  cost?: number
  /** Antwort fiel in die Spitzenzeit. */
  peak?: boolean
  stopReason?: string
  errorMessage?: string
  model?: string
  /** Kennung der fertigen Antwort für ihre Bilder (responseId bzw. ts-<timestamp>); fehlt beim Streamen. */
  msgKey?: string
  /**
   * Gesamtdauer des Laufs in ms, nur an der Antwort, die ihn beendet (stopReason nicht „toolUse“). Live ab
   * agent_start gemessen, nach dem Neuladen aus created_at von Nutzernachricht und Antwort; sonst leer.
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
  /** Kompaktierung läuft noch (zwischen compaction_start und compaction_end). */
  running: boolean
  summary?: string
  tokensBefore?: number
  tokensAfter?: number
  usage?: Usage
  cost?: number
  aborted?: boolean
  errorMessage?: string
  /**
   * Kompaktierung abgebrochen oder fehlgeschlagen. Solche Einträge gibt es nur live (die Historie
   * speichert sie nicht); beim Neuladen bleiben sie hinter der Nachricht `afterSeq` stehen.
   */
  failed?: boolean
  afterSeq?: number
}
/** Schritt beim Fortsetzen, wie ihn der Block im Verlauf zeigt; „pending“ ist noch nicht begonnen. */
export type ResumeStepView = {
  phase: ResumePhase
  status: "pending" | ResumeStep["status"]
  detail?: string
  size?: number
  files?: number
  ms?: number
}
/**
 * Fortsetzen in frischer Sandbox (SSE „resume“). Nur live bekannt; beim Neuladen bleibt der Eintrag
 * hinter der Nachricht `afterSeq` stehen (mit `withUser` hinter der Nutzernachricht, die folgte).
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
 * Nachricht einer Erweiterung in pi an den Agenten (Rolle custom), etwa das Ende eines Subagenten im
 * Hintergrund (pi-subagents). Nicht vom Nutzer; `trigger` wake, wenn sie einen Durchgang begonnen hat.
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
 * Gesendete Nutzernachricht, die pi noch nicht bestätigt hat (optimistisch). Sie steht am Ende des
 * Verlaufs und wird durch die echte Nachricht (message_end, role user) ersetzt. `failed`: nicht
 * gesendet (Fehler beim Senden oder Fortsetzen).
 */
export type PendingUser = {
  key: string
  text: string
  failed?: boolean
  afterSeq?: number
  /** Herkunft laut Server (übergebene Warteschlange) */
  origin?: MessageOrigin
  sources?: MessageSource[]
}

export type ToolExecution = {
  toolCallId: string
  toolName: string
  args?: unknown
  running: boolean
  /** Bisherige Ausgabe aus tool_execution_update (wird ersetzt, nicht angehängt). */
  output?: string
  result?: string
  isError?: boolean
  /** Strukturiertes Ergebnis des Werkzeugs (`details`), etwa der Stand der Aufgabenliste bei todo. */
  details?: unknown
  /** Beginn und Ende in ms: live beim Empfang von tool_execution_start/_end, nach dem Neuladen aus created_at. */
  startedAt?: number
  endedAt?: number
}

export type TranscriptState = {
  items: TranscriptItem[]
  tools: Record<string, ToolExecution>
  /** Rolle der Nachricht zwischen message_start und message_end. */
  currentRole?: string
  /** Schlüssel der gerade gestreamten Antwort. */
  streamingKey?: string
  /** Schlüssel der gerade laufenden Kompaktierung. */
  compactingKey?: string
  /** Gesendet, von pi noch nicht bestätigt; erscheint unter dem Verlauf. */
  pending: PendingUser[]
  /**
   * Laufendes oder gerade beendetes Fortsetzen. Es steht unter den gesendeten Nachrichten, bis die
   * Nutzernachricht von pi kommt, und wandert dann direkt hinter sie in den Verlauf.
   */
  resume?: ResumeItem
  /** Beginn des laufenden Durchgangs (Empfang von agent_start, ms); bis agent_settled. */
  runStart?: number
  /** Herkunft der nächsten Nutzernachricht von pi (SSE „user_meta“ kommt unmittelbar davor). */
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

/** Wandelt Nachrichteninhalt (Zeichenkette oder Blockliste) in Text. */
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

/** Ergebnis oder Teilergebnis einer Werkzeugausführung als Text. */
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

/** Liefert den Zustand mit einer gerade gestreamten Antwort (legt sie bei Bedarf an). */
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

/** Index des Blocks, an den ein Delta geht: contentIndex, sonst letzter Block des Typs. */
function blockIndex(blocks: ViewBlock[], ev: Obj, type: ViewBlock["type"]): number {
  if (typeof ev.contentIndex === "number") return ev.contentIndex
  const last = blocks.length - 1
  return last >= 0 && blocks[last].type === type ? last : blocks.length
}

function withBlock(blocks: ViewBlock[], idx: number, fn: (b: ViewBlock | undefined) => ViewBlock): ViewBlock[] {
  const next = blocks.slice()
  // Lücken (verpasste *_start-Ereignisse) mit leerem Text füllen
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

/** Felder einer Kompaktierung aus pis CompactionResult bzw. der gespeicherten compaction-Nachricht. */
function compactionFields(r: Obj): Partial<CompactionItem> {
  return {
    summary: str(r.summary),
    tokensBefore: num(r.tokensBefore),
    tokensAfter: num(r.estimatedTokensAfter),
    usage: isObj(r.usage) ? (r.usage as Usage) : undefined,
  }
}

const compactPrefix = /^Kompaktierung fehlgeschlagen:\s*/i

/**
 * Hinweistext zu einer fehlgeschlagenen Kompaktierung. pis Meldungen „Nothing to compact“ und
 * „Already compacted“ werden übersetzt; die Originalmeldung gehört in den Tooltip.
 */
export function compactionNotice(message: string | undefined): string {
  const msg = (message ?? "").replace(compactPrefix, "").trim()
  if (/nothing to compact/i.test(msg)) return "Kompaktierung nicht möglich: noch zu wenig Verlauf zum Zusammenfassen"
  if (/already compacted/i.test(msg)) return "Kompaktierung nicht nötig: bereits zusammengefasst"
  return msg ? `Kompaktierung fehlgeschlagen: ${msg}` : "Kompaktierung fehlgeschlagen"
}

/** Ob eine Fehlermeldung des Orchestrators (Ereignis `error`) eine Kompaktierung betrifft. */
export const isCompactionError = (message: string | undefined) => !!message && compactPrefix.test(message)

const lastSeq = (items: TranscriptItem[]) =>
  items.reduce<number | undefined>((m, i) => (i.seq !== undefined && (m === undefined || i.seq > m) ? i.seq : m), undefined)

/** Ein fehlgeschlagener Live-Eintrag am Ende des Verlaufs mit derselben Ursache (gegen Doppelmeldungen). */
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
 * Fehlermeldung „Kompaktierung fehlgeschlagen: …“ des Orchestrators (Ereignis `error`): beendet
 * eine laufende Kompaktierung als fehlgeschlagen oder legt einen Hinweis an, sofern derselbe
 * Fehler nicht schon als compaction_end angekommen ist.
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

/** Reducer: ein pi-Ereignis auf den Verlauf anwenden. `now`: Empfangszeit (für Laufzeiten). */
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
      return base // system und andere Rollen werden nicht angezeigt
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
 * Baut den Verlauf aus der gespeicherten Historie neu auf. Abgeschlossene Live-Einträge werden
 * durch die Historie ersetzt; eine noch laufende Antwort und laufende Werkzeuge bleiben erhalten.
 */
export function hydrate(prev: TranscriptState, history: StoredMessage[]): TranscriptState {
  let state: TranscriptState = { items: [], tools: {}, pending: [], nextKey: prev.nextKey, runStart: prev.runStart }
  const sorted = [...history].sort((a, b) => a.seq - b.seq)
  // Ende der Antwort, die ein Werkzeug aufrief, sofern sie genau einen Aufruf enthielt (Start des Werkzeugs)
  const toolStart = new Map<string, number>()
  // Gesamtdauer je Lauf: von der Nutzernachricht bis zur Antwort, die ihn beendet (beides created_at des
  // Orchestrators). Steht davor keine Nutzernachricht, sondern das Ende eines früheren Laufs: keine Angabe.
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
        // live gemessene Zeiten haben Vorrang; sonst aus den Zeitstempeln, soweit eindeutig
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
  // fehlgeschlagene Kompaktierungen und Fortsetzungen (nur live bekannt) hinter ihrer Nachricht wieder einsetzen
  for (const f of prev.items) {
    const liveOnly = (f.kind === "compaction" && f.failed) || f.kind === "resume"
    if (!liveOnly || f.seq !== undefined) continue
    const at = f.afterSeq === undefined ? 0 : state.items.findLastIndex((i) => i.seq !== undefined && i.seq <= f.afterSeq!) + 1
    // hinter bereits eingesetzten Hinweisen derselben Stelle einreihen
    let pos = at
    while (pos < state.items.length && state.items[pos].seq === undefined) pos++
    // ein Fortsetzen steht hinter der Nutzernachricht, die es ausgelöst hat
    if (f.kind === "resume" && f.withUser && state.items[pos]?.kind === "user") pos++
    state = { ...state, items: [...state.items.slice(0, pos), f, ...state.items.slice(pos)] }
  }
  // Gesendete Nachrichten, die inzwischen in der Historie stehen, nicht doppelt zeigen
  state = { ...state, pending: prunePending(prev.pending ?? [], sorted), resume: prev.resume }
  // laufende Werkzeuge ohne gespeichertes Ergebnis übernehmen
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

// --- Gesendete Nachrichten und Fortsetzen ---

/** Letzte bekannte Nachrichtennummer im Verlauf (Anker für nur live bekannte Einträge). */
export const lastKnownSeq = (state: TranscriptState) => lastSeq(state.items)

/**
 * Nachricht optimistisch anzeigen, bevor der Server antwortet. Fehlgeschlagene Sendungen und ein
 * gescheitertes Fortsetzen verschwinden dabei (ihr Text stand wieder im Eingabefeld).
 */
export function addPending(state: TranscriptState, key: string, text: string): TranscriptState {
  const pending = [...state.pending.filter((p) => !p.failed), { key, text, afterSeq: lastSeq(state.items) }]
  const resume = state.resume?.state === "failed" ? undefined : state.resume
  return { ...state, pending, resume }
}

/**
 * Eingereihte Nachrichten sind übergeben (SSE „queue“, change „delivered“): Der Auftrag erscheint
 * sofort als gesendete Nachricht. Ging dabei eine eben gesendete Nachricht mit (sie steht am Ende
 * des Auftrags), wird deren Anzeige erweitert statt eine zweite anzulegen.
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

/** SSE „user_meta“: Herkunft der Nutzernachricht, die pi als Nächstes meldet. */
export function applyUserMeta(state: TranscriptState, meta: MessageMeta): TranscriptState {
  return { ...state, nextUserMeta: meta }
}

/** Felder der Herkunft für einen Eintrag im Verlauf (nur, was der Server angibt). */
function metaFields(meta: MessageMeta | undefined): Partial<UserItem> {
  if (!meta) return {}
  const out: Partial<UserItem> = {}
  if (meta.origin) out.origin = meta.origin
  if (meta.sources) out.sources = meta.sources
  if (meta.trigger) out.trigger = meta.trigger
  if (meta.turn_id !== undefined && meta.turn_id !== null) out.turnId = meta.turn_id
  return out
}

/** Senden gescheitert: Die Nachricht bleibt sichtbar, als nicht gesendet markiert. */
export function failPending(state: TranscriptState, key: string): TranscriptState {
  if (!state.pending.some((p) => p.key === key)) return state
  return { ...state, pending: state.pending.map((p) => (p.key === key ? { ...p, failed: true } : p)) }
}

/** Optimistische Nachricht entfernen (etwa, weil sie doch eingereiht wurde). */
export function dropPending(state: TranscriptState, key: string): TranscriptState {
  if (!state.pending.some((p) => p.key === key)) return state
  return { ...state, pending: state.pending.filter((p) => p.key !== key) }
}

/**
 * Die Nutzernachricht von pi ersetzt die älteste passende gesendete (gleicher Text); sonst die
 * älteste noch nicht gescheiterte, denn pi kann den Text verändern (Skills, Vorlagen).
 */
function consumePending(pending: PendingUser[], text: string): PendingUser[] {
  if (pending.length === 0) return pending
  let idx = pending.findIndex((p) => !p.failed && p.text === text)
  if (idx < 0) idx = pending.findIndex((p) => !p.failed)
  return idx < 0 ? pending : [...pending.slice(0, idx), ...pending.slice(idx + 1)]
}

/** Gesendete, die schon in der Historie stehen (nach ihrem Anker, gleicher Text), entfallen. */
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

/** Reducer für ein Ereignis „resume“ (Schritte beim Fortsetzen, siehe poc/API.md). */
export function applyResumeStep(state: TranscriptState, step: ResumeStep): TranscriptState {
  let s = state
  if (!s.resume || s.resume.id !== step.id) {
    // Ein früheres, abgeschlossenes Fortsetzen vorher in den Verlauf übernehmen
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
 * Ein abgeschlossenes Fortsetzen wandert in den Verlauf: hinter die Nutzernachricht, die es
 * ausgelöst hat (afterUser), sonst vor den nächsten Eintrag. Ein laufendes bleibt stehen.
 */
function flushResume(state: TranscriptState, afterUser = false): TranscriptState {
  const r = state.resume
  if (!r || r.state === "running") return state
  if (r.state === "failed" && !afterUser) return state
  return { ...state, items: [...state.items, { ...r, withUser: afterUser }], resume: undefined }
}

/** Ob der Chat gerade auf die erste Antwort wartet (für „Denkt …“). */
export function awaitingAnswer(state: TranscriptState): boolean {
  if (state.streamingKey) return false
  if (Object.values(state.tools).some((t) => t.running)) return false
  if (state.resume?.state === "running") return false
  return true
}

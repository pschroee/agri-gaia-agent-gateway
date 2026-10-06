// Types following poc/API.md (binding contract).

export type VariantId = "cli" | "mcp" | "api" | "both"

export type Pricing = {
  input: number
  output: number
  cache_read: number
  cache_write: number
  currency: "USD"
  note?: string
  /** URL of the price source */
  source?: string
  /** Retrieval date of the prices, ISO */
  retrieved?: string
}

export type PeakWindow = { days: string; from: string; to: string }

/** Tariff with peak hours (UTC). `pricing` is the peak tariff; outside them `offpeak_factor` applies. */
export type Tariff = {
  peak_windows_utc: PeakWindow[]
  offpeak_factor: number
  note?: string
  /** URL of the tariff source */
  source?: string
  /** Retrieval date, ISO */
  retrieved?: string
}

export type Model = {
  id: string
  provider: string
  model: string
  name: string
  default: boolean
  pricing?: Pricing
  tariff?: Tariff
  peak_now?: boolean
  /** Context window in tokens (0/missing: unknown). */
  context_window?: number
}

export type Variant = { id: VariantId; label: string; tools: string[] }

export type ActivityKind = "idle" | "thinking" | "writing" | "tool" | "waiting_approval" | "starting" | "preparing" | "compacting"

export type Activity = {
  kind: ActivityKind
  tool?: string
  since: string
}

export type SlotState = "starting" | "idle" | "assigned" | "stopping"

export type Slot = {
  id: string
  variant: VariantId
  state: SlotState
  container_id: string
  container_name: string
  image: string
  created_at: string
  assigned_at?: string
  chat_id?: string
  chat_title?: string
  activity?: Activity
  internet?: boolean
}

export type Tokens = { input: number; output: number; cache_read: number; total: number }

export type Pool = {
  slots: Slot[]
  targets: Partial<Record<VariantId, number>>
  totals?: { cost: number; tokens: Tokens; chats_active: number }
}

export type ChatState = "active" | "dormant"

/**
 * Context usage according to pi. tokens/percent are null right after a compaction.
 * percent is between 0 and 100; threshold_tokens: from here on pi compacts automatically.
 */
export type ContextUsage = {
  tokens: number | null
  window: number
  percent: number | null
  threshold_tokens: number
  reserve_tokens: number
  keep_recent_tokens: number
  updated_at: string
}

export type Chat = {
  id: string
  title: string
  model: string
  variant: VariantId
  state: ChatState
  running: boolean
  /** Start of the current turn (ISO), only while the agent is working. */
  running_since?: string
  internet: boolean
  auto_compact?: boolean
  compactions?: number
  context?: ContextUsage
  /** pi's thinking level (/effort); missing until it has been read. */
  thinking_level?: string
  /** Thinking levels the chat's model supports (reported by pi). */
  thinking_levels?: string[]
  /** Model to switch to after the running compaction. */
  pending_model?: string
  /** At most this many subagents at the same time; fixed for the service, the same for every chat. */
  max_subagents?: number
  /** Subagents running right now according to the monitoring (0 while the chat is idle). */
  subagents_running?: number
  /** Delegated rights of the chat; missing: no delegation (reading free, writing with approval). */
  delegation?: Delegation
  /** sub of the owner (login through the platform); missing in token mode. */
  owner?: string
  /** The user's preferred language according to the browser (BCP 47); missing if not given. */
  language?: string
  /** Subagent runs started so far. */
  subagents?: number
  /** Model calls recorded at the LLM proxy. */
  llm_calls?: number
  /** Share of the cost outside the main session's responses (subagents, compaction, direct calls). */
  cost_other?: number
  slot_id?: string
  created_at: string
  updated_at: string
  tokens: Tokens
  /** US dollars by tariff; authoritative are the calls recorded at the proxy (incl. subagents). */
  cost: number
  artifact_count: number
  pending_approvals: number
  /** Last backup of /workspace (survives idling); missing while nothing has been backed up. */
  workspace?: WorkspaceBackup
  /** The chat is currently being resumed in a fresh sandbox. */
  resuming?: boolean
  /** Queued messages not yet handed over. */
  queued?: number
  /** Queued entries are not sent on their own (after an abort, while the chat is idle) but with the next message. */
  queue_held?: boolean
  /** Why queued entries are held back: abort, wake-up limit per hour, too many turns without the user. */
  hold_reason?: HoldReason
  /** Running background tasks (bash with run_in_background). */
  background_running?: number
}

export type HoldReason = "abort" | "wake_limit" | "auto_turns"

/** Trigger of a turn: the user, at the end of a run with a message of the user, without the user (wake-up). */
export type TurnTrigger = "user" | "queue" | "wake"
/** Origin of a request to pi (user message). */
export type MessageOrigin = "user" | "system" | "mixed"
/** Part of a request to pi, in order (Review 3, H1). */
export type MessageSource = {
  kind: "user" | "system"
  /** for system: background (end of a background task), sandbox (ended with the sandbox) or language (preferred language according to the browser) */
  type?: string
  refs?: string[]
  queue_id?: string
  /** Marker of the fence around the data from the sandbox */
  marker?: string
  /** "agent": context for the model only (e.g. the preferred language); the UI does not show this part. Absent: shown. */
  audience?: "agent"
}
/** Origin of a user message (stored or live via SSE "user_meta"). */
export type MessageMeta = {
  turn_id?: number
  trigger?: TurnTrigger
  origin?: MessageOrigin
  sources?: MessageSource[]
}

/** SSE "auto_held": notes stay queued because a limit for turns without the user has been reached. */
export type AutoHeldEvent = { reason: "wake_limit" | "auto_turns"; limit: number; count: number }

/** State of a background task (poc/API.md, "Background tasks"). */
export type BackgroundState = "running" | "exited" | "failed" | "timeout" | "stopped" | "lost" | "suspended" | "closed"

/** Background task: a command the agent started with bash and run_in_background. */
export type BackgroundTask = {
  id: string
  seq: number
  chat_id: string
  /** "main" or the run of the subagent that started it. */
  session: string
  tool_call_id: string
  command: string
  log_path: string
  state: BackgroundState
  exit_code?: number
  error?: string
  /** agent (bg_stop) or user (stop in UI or CLI). */
  stopped_by?: string
  started_at: string
  ended_at?: string
  output_bytes: number
  output_lines: number
  output_sha256?: string
  /** Last output (a few KiB). */
  tail?: string
  /** Note to the agent created (queued or handed over as a wake-up). */
  notified_at?: string
  /** The end started a new turn (wake-up). */
  woke?: boolean
  /** Ended with the sandbox, not yet told to the agent. */
  notice_pending?: boolean
}

/** SSE "background": started, output (throttled), ended, wake_limited (note only queued). */
export type BackgroundEvent = {
  change: "started" | "output" | "ended"
  task: BackgroundTask
}

/** Queued message (the chat's queue). */
export type QueueEntry = {
  id: string
  chat_id: string
  text: string
  attachments: string[]
  created_at: string
  /** user or system (orchestrator note, e.g. the end of a background task). */
  kind?: "user" | "system"
  /** system only: kind of note (background, sandbox) and affected tasks; text: first line header, data below */
  note?: string
  refs?: string[]
}

/** SSE "queue": new state of the queue. For "delivered", text is the request to pi. */
export type QueueEvent = {
  entries: QueueEntry[]
  change: "queued" | "removed" | "delivered" | "restored"
  ids?: string[]
  text?: string
  /** for delivered: origin and parts of the request */
  origin?: MessageOrigin
  sources?: MessageSource[]
}

/**
 * Queued entries handed to pi as one message whose user message pi has not reported yet (steered in while a
 * tool runs, or on their way while the chat resumes); same content as SSE "queue" with change "delivered".
 */
export type QueueDelivery = {
  ids: string[]
  entries: QueueEntry[]
  text: string
  origin?: MessageOrigin
  sources?: MessageSource[]
  delivered_at: string
  /** steered into the running turn: pi reads it after its current step */
  steered: boolean
}

export type ResumePhase = "acquire" | "session" | "settings" | "workspace" | "inputs" | "ready" | "failed"

/** SSE "resume": a step while resuming an idle chat. */
export type ResumeStep = {
  id: string
  phase: ResumePhase
  status: "running" | "done" | "warning" | "error"
  detail?: string
  size?: number
  files?: number
  at: string
  /** Duration of the step; for ready and failed the total duration. */
  ms?: number
}

/** Response to POST /messages, /commands and /queue/send. */
export type SendResult = { ok: boolean; resumed: boolean; queued?: boolean; queue_id?: string; result?: unknown }

/** Backup of the workspace. saved_at missing: never backed up; skipped_*: last one skipped. */
export type WorkspaceBackup = {
  /** Sum of the file sizes (uncompressed). */
  size: number
  archive_size: number
  files: number
  sha256?: string
  saved_at?: string
  skipped_reason?: string
  skipped_size?: number
  skipped_at?: string
}

export type TextContent = { type: "text"; text: string }
export type ThinkingContent = { type: "thinking"; thinking: string }
export type ToolCallContent = { type: "toolCall"; id: string; name: string; arguments: unknown }
export type ImageContent = { type: "image"; data?: string; mimeType?: string }
export type ContentBlock = TextContent | ThinkingContent | ToolCallContent | ImageContent

export type Usage = {
  input?: number
  output?: number
  cacheRead?: number
  cacheWrite?: number
  totalTokens?: number
  cost?: { input?: number; output?: number; cacheRead?: number; cacheWrite?: number; total?: number }
}

export type UserMessage = { role: "user"; content: string | ContentBlock[]; timestamp?: number }
export type AssistantMessage = {
  role: "assistant"
  content: ContentBlock[]
  usage?: Usage
  stopReason?: string
  errorMessage?: string
  model?: string
  timestamp?: number
}
export type ToolResultMessage = {
  role: "toolResult"
  toolCallId: string
  toolName: string
  content: ContentBlock[]
  isError: boolean
  timestamp?: number
}
export type OtherMessage = { role: string; content?: unknown; [key: string]: unknown }
export type PiMessage = UserMessage | AssistantMessage | ToolResultMessage | OtherMessage

export type StoredMessage = {
  seq: number
  role: string
  message: PiMessage
  /** Cost by tariff at the time of the response (authoritative; responses only). */
  cost?: number
  /** Response fell into peak hours. */
  peak?: boolean
  created_at: string
  /** Turn and trigger (all messages of a turn), origin and parts (only the user message). Missing on old rows. */
  turn_id?: number
  trigger?: TurnTrigger
  origin?: MessageOrigin
  sources?: MessageSource[]
}

export type ArtifactKind = "input" | "output"

export type Artifact = {
  chat_id: string
  kind: ArtifactKind
  name: string
  size: number
  sha256: string
  content_type: string
  created_at: string
  via: "cli" | "mcp" | "ui"
  /** Tool call that uploaded the result (outputs only, missing on older ones). */
  tool_call_id?: string
}

export type ApprovalState = "pending" | "approved" | "rejected" | "expired"

export type Approval = {
  id: string
  chat_id: string
  /**
   * artifact_upload: upload a file; internet_access: request for internet access, `name` is the reason;
   * platform_write: writing call to the Agri-Gaia platform, `name` is "METHOD path", `preview` including the body.
   */
  kind: "artifact_upload" | "internet_access" | "platform_write"
  via: "cli" | "mcp"
  name: string
  size: number
  sha256: string
  content_type: string
  state: ApprovalState
  created_at: string
  decided_at?: string
  preview?: string
  /** Who asked: "main" or a subagent run (empty on older entries). */
  session?: string
  tool_call_id?: string
  /** Platform calls: round trip to the platform in ms, without the approval's wait (missing: not measured). */
  duration_ms?: number
}

export type SocketCall = {
  id: number
  chat_id?: string
  slot_id: string
  /** cli/mcp for calls from the sandbox; proxy, orchestrator or pi for monitoring entries. */
  via: "cli" | "mcp" | "proxy" | "orchestrator" | "pi" | (string & {})
  op: string
  detail: string
  result: string
  created_at: string
  /** "main" or a subagent run that made the call (empty: unknown). */
  session?: string
  tool_call_id?: string
}

export type ChatDetail = {
  chat: Chat
  messages: StoredMessage[]
  artifacts: Artifact[]
  approvals: Approval[]
  socket_calls: SocketCall[]
  subagent_entries?: SubagentEntry[]
  subagent_runs?: SubagentRunMeta[]
  queue?: QueueEntry[]
  /** handed to pi but not read yet (issue #21); empty on older gateways */
  queue_delivered?: QueueDelivery[]
  background?: BackgroundTask[]
}

/** Model call recorded at the LLM proxy (measured outside the sandbox, tamper-proof). */
export type LLMCall = {
  id: number
  slot_id: string
  source_ip: string
  model: string
  response_id: string
  status: number
  input: number
  output: number
  cache_read: number
  cache_write: number
  cost: number
  peak: boolean
  /** id: the provider's ID (tool_calls[].id); the reconciliation with the executions relies on it (E9). */
  tool_calls: { id?: string; name: string; arguments: string }[] | null
  started_at: string
  duration_ms: number
  /** Response belongs to the main session; otherwise subagent, compaction or similar. */
  main: boolean
  /** finish_reason of the provider (M1) */
  finish_reason?: string
  /** Response arrived completely (at the proxy with finish_reason); missing on older entries */
  complete?: boolean
}

/**
 * Operation the orchestrator executed for a tool in the execution sandbox (E9).
 * Verified, because the orchestrator executed and recorded it itself.
 */
export type ToolExecutionRecord = {
  id: number
  chat_id: string
  slot_id: string
  /** "main" or ID of the subagent run */
  session: string
  tool_call_id: string
  tool: string
  op: string
  args: Record<string, unknown>
  exit_code?: number
  error?: string
  output_excerpt?: string
  output_sha256?: string
  output_bytes: number
  started_at: string
  duration_ms: number
}

/** Response of GET /api/chats/{id}/tool_executions (reconciliation on the server). */
export type ToolExecutionsResponse = {
  calls: unknown[]
  summary: Record<string, number>
  executions: ToolExecutionRecord[]
  executed_tools: string[]
}

export type SubagentEntryKind = "task" | "tool_call" | "tool_result" | "text"

/**
 * Entry from a subagent's session file. The source is the sandbox, so not
 * tamper-proof; `confirmed`: the corresponding response is verified at the proxy.
 */
export type SubagentEntry = {
  chat_id: string
  run_id: string
  entry_id: string
  agent: string
  kind: SubagentEntryKind
  payload: { text?: string; name?: string; arguments?: string; is_error?: boolean; id?: string; tool_call_id?: string }
  response_id?: string
  confirmed: boolean
  created_at: string
}

/**
 * Name and state of a subagent run from the status files of pi-subagents (sandbox, not
 * tamper-proof). `label` is the name in the workflow (key for runs.run/runs.all).
 */
export type SubagentRunMeta = {
  chat_id: string
  run_id: string
  agent: string
  label?: string
  /** State according to pi-subagents, e.g. running, complete, completed, failed, cancelled. */
  state?: string
  pi_run_id?: string
  parent_run_id?: string
  started_at?: string
  ended_at?: string
  updated_at: string
}

export type CreateChatRequest = {
  model?: string
  variant?: VariantId
  title?: string
  message?: string
  internet?: boolean
  auto_compact?: boolean
  /** Delegated rights (missing: no delegation). */
  delegation?: Delegation
  /** Preferred language according to the browser (BCP 47, navigator.language); the agent uses it only if the message reveals no language. */
  language?: string
}

/** Logged-in user (GET /api/me); in token mode only mode. */
export type Me = { mode: "token" | "oidc"; sub?: string; username?: string; name?: string }

export type Config = {
  internet_default: boolean
  approval_timeout_s: number
  artifact_max_mb: number
  idle_timeout_s: number
  auto_compact_default?: boolean
  compact_reserve_tokens?: number
  compact_keep_recent_tokens?: number
  /** Subagents at the same time per chat, fixed for the service. */
  max_subagents?: number
  /** Tools whose execution is verified at the socket (E9) */
  executed_tools?: string[]
}

/** Slash command; `name` without the leading slash. */
export type Command = {
  name: string
  description?: string
  source: "builtin" | "extension" | "prompt" | "skill"
  args?: string
  /** Possible arguments for completion (/model, /effort). */
  options?: CommandOption[]
}

export type CommandOption = { value: string; label?: string; current?: boolean }

/** A pi RPC event; only the fields the UI evaluates are typed. */
export type PiEvent = { type: string; [key: string]: unknown }

export type ServerEvent =
  | { kind: "pi"; data: PiEvent }
  | { kind: "chat"; data: Chat }
  | { kind: "approval"; data: Approval }
  | { kind: "artifact"; data: Artifact }
  | { kind: "socket_call"; data: SocketCall }
  | { kind: "llm_call"; data: LLMCall }
  | { kind: "subagent"; data: SubagentEntry }
  | { kind: "subagent_run"; data: SubagentRunMeta }
  | { kind: "tool_execution"; data: ToolExecutionRecord }
  | { kind: "error"; data: { message: string } }
  | { kind: "queue"; data: QueueEvent }
  | { kind: "resume"; data: ResumeStep }
  | { kind: "background"; data: BackgroundEvent }
  | { kind: "user_meta"; data: MessageMeta }
  | { kind: "auto_held"; data: AutoHeldEvent }

/** Rule of a delegation: action on a resource, without ids only for calls without an object. */
export type DelegationRule = { action: string; resource: string; ids?: string[] }

/** Delegated rights of a chat (docs/plan-delegation-rest-platform.md, step 1). */
export type Delegation = {
  rules: DelegationRule[]
  expires_at?: string
  /** false: violations are only logged (stage without protection). */
  enforce?: boolean
  confirm?: "writes" | "none"
}

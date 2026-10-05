// Typen nach poc/API.md (verbindlicher Vertrag).

export type VariantId = "cli" | "mcp" | "api" | "beide"

export type Pricing = {
  input: number
  output: number
  cache_read: number
  cache_write: number
  currency: "USD"
  note?: string
  /** URL der Preisquelle */
  source?: string
  /** Abrufdatum der Preise, ISO */
  retrieved?: string
}

export type PeakWindow = { days: string; from: string; to: string }

/** Tarif mit Spitzenzeiten (UTC). `pricing` ist der Spitzentarif; außerhalb gilt `offpeak_factor`. */
export type Tariff = {
  peak_windows_utc: PeakWindow[]
  offpeak_factor: number
  note?: string
  /** URL der Tarifquelle */
  source?: string
  /** Abrufdatum, ISO */
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
  /** Kontextfenster in Tokens (0/fehlt: unbekannt). */
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
 * Kontextauslastung laut pi. tokens/percent sind null direkt nach einer Kompaktierung.
 * percent liegt zwischen 0 und 100; threshold_tokens: ab hier kompaktiert pi automatisch.
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
  /** Beginn des laufenden Durchgangs (ISO), nur solange der Agent arbeitet. */
  running_since?: string
  internet: boolean
  auto_compact?: boolean
  compactions?: number
  context?: ContextUsage
  /** Denkstufe von pi (/effort); fehlt, solange sie noch nicht gelesen ist. */
  thinking_level?: string
  /** Denkstufen, die das Modell des Chats kennt (von pi gemeldet). */
  thinking_levels?: string[]
  /** Modell, zu dem nach der laufenden Kompaktierung gewechselt wird. */
  pending_model?: string
  /** Höchstzahl Subagenten (hart am Proxy und durch Überwachung). */
  max_subagents?: number
  /** Übertragene Rechte des Chats; fehlt: ohne Delegation (lesen frei, schreiben mit Bestätigung). */
  delegation?: Delegation
  /** sub des Besitzers (Anmeldung über die Plattform); fehlt im token-Modus. */
  owner?: string
  /** Bevorzugte Sprache des Nutzers laut Browser (BCP 47); fehlt ohne Angabe. */
  language?: string
  /** Bisher gestartete Subagenten-Läufe. */
  subagents?: number
  /** Am LLM-Proxy erfasste Modellaufrufe. */
  llm_calls?: number
  /** Kostenanteil außerhalb der Antworten der Hauptsitzung (Subagenten, Kompaktierung, direkte Aufrufe). */
  cost_other?: number
  slot_id?: string
  created_at: string
  updated_at: string
  tokens: Tokens
  /** US-Dollar nach Tarif; maßgeblich die am Proxy erfassten Aufrufe (inkl. Subagenten). */
  cost: number
  artifact_count: number
  pending_approvals: number
  /** Letzte Sicherung von /workspace (übersteht das Ruhen); fehlt, solange nichts gesichert ist. */
  workspace?: WorkspaceBackup
  /** Der Chat wird gerade in einer frischen Sandbox fortgesetzt. */
  resuming?: boolean
  /** Eingereihte, noch nicht übergebene Nachrichten. */
  queued?: number
  /** Eingereihtes geht nicht von selbst (nach Abbruch, bei ruhendem Chat), sondern mit der nächsten Nachricht. */
  queue_held?: boolean
  /** Warum Eingereihtes zurückgehalten ist: Abbruch, Weckgrenze je Stunde, zu viele Durchgänge ohne Nutzer. */
  hold_reason?: HoldReason
  /** Laufende Hintergrundaufgaben (bash mit run_in_background). */
  background_running?: number
}

export type HoldReason = "abort" | "wake_limit" | "auto_turns"

/** Auslöser eines Durchgangs: der Nutzer, beim Laufende mit Nachricht des Nutzers, ohne Nutzer (Weckruf). */
export type TurnTrigger = "user" | "queue" | "wake"
/** Herkunft eines Auftrags an pi (Nutzernachricht). */
export type MessageOrigin = "user" | "system" | "mixed"
/** Teil eines Auftrags an pi, in Reihenfolge (Review 3, H1). */
export type MessageSource = {
  kind: "user" | "system"
  /** bei system: background (Ende einer Hintergrundaufgabe), sandbox (mit der Sandbox beendet) oder language (bevorzugte Sprache laut Browser) */
  type?: string
  refs?: string[]
  queue_id?: string
  /** Marke des Zauns um die Daten aus der Sandbox */
  marker?: string
}
/** Herkunft einer Nutzernachricht (gespeichert oder live über SSE „user_meta“). */
export type MessageMeta = {
  turn_id?: number
  trigger?: TurnTrigger
  origin?: MessageOrigin
  sources?: MessageSource[]
}

/** SSE „auto_held“: Meldungen bleiben eingereiht, weil eine Grenze für Durchgänge ohne Nutzer erreicht ist. */
export type AutoHeldEvent = { reason: "wake_limit" | "auto_turns"; limit: number; count: number }

/** Zustand einer Hintergrundaufgabe (poc/API.md, „Hintergrundaufgaben“). */
export type BackgroundState = "running" | "exited" | "failed" | "timeout" | "stopped" | "lost" | "suspended" | "closed"

/** Hintergrundaufgabe: Befehl, den der Agent mit bash und run_in_background gestartet hat. */
export type BackgroundTask = {
  id: string
  seq: number
  chat_id: string
  /** "main" oder Lauf des Subagenten, der sie gestartet hat. */
  session: string
  tool_call_id: string
  command: string
  log_path: string
  state: BackgroundState
  exit_code?: number
  error?: string
  /** agent (bg_stop) oder user (Stopp in UI oder CLI). */
  stopped_by?: string
  started_at: string
  ended_at?: string
  output_bytes: number
  output_lines: number
  output_sha256?: string
  /** Letzte Ausgabe (einige KiB). */
  tail?: string
  /** Meldung an den Agenten erzeugt (eingereiht oder als Weckruf übergeben). */
  notified_at?: string
  /** Das Ende hat einen neuen Durchgang gestartet (Weckruf). */
  woke?: boolean
  /** Mit der Sandbox beendet, dem Agenten noch nicht gesagt. */
  notice_pending?: boolean
}

/** SSE „background“: started, output (gedrosselt), ended, wake_limited (Meldung nur eingereiht). */
export type BackgroundEvent = {
  change: "started" | "output" | "ended"
  task: BackgroundTask
}

/** Eingereihte Nachricht (Warteschlange des Chats). */
export type QueueEntry = {
  id: string
  chat_id: string
  text: string
  attachments: string[]
  created_at: string
  /** user oder system (Meldung des Orchestrators, etwa das Ende einer Hintergrundaufgabe). */
  kind?: "user" | "system"
  /** nur system: Art der Meldung (background, sandbox) und betroffene Aufgaben; text: erste Zeile Kopf, darunter Daten */
  note?: string
  refs?: string[]
}

/** SSE „queue“: neuer Stand der Warteschlange. Bei „delivered“ ist text der Auftrag an pi. */
export type QueueEvent = {
  entries: QueueEntry[]
  change: "queued" | "removed" | "delivered" | "restored"
  ids?: string[]
  text?: string
  /** bei delivered: Herkunft und Teile des Auftrags */
  origin?: MessageOrigin
  sources?: MessageSource[]
}

export type ResumePhase = "acquire" | "session" | "settings" | "workspace" | "inputs" | "ready" | "failed"

/** SSE „resume“: ein Schritt beim Fortsetzen eines ruhenden Chats. */
export type ResumeStep = {
  id: string
  phase: ResumePhase
  status: "running" | "done" | "warning" | "error"
  detail?: string
  size?: number
  files?: number
  at: string
  /** Dauer des Schritts; bei ready und failed die Gesamtdauer. */
  ms?: number
}

/** Antwort auf POST /messages, /commands und /queue/send. */
export type SendResult = { ok: boolean; resumed: boolean; queued?: boolean; queue_id?: string; result?: unknown }

/** Sicherung des Arbeitsbereichs. saved_at fehlt: noch nie gesichert; skipped_*: zuletzt ausgelassen. */
export type WorkspaceBackup = {
  /** Summe der Dateigrößen (unkomprimiert). */
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
  /** Kosten nach Tarif zum Zeitpunkt der Antwort (maßgeblich; nur Antworten). */
  cost?: number
  /** Antwort fiel in die Spitzenzeit. */
  peak?: boolean
  created_at: string
  /** Durchgang und Auslöser (alle Nachrichten eines Durchgangs), Herkunft und Teile (nur die Nutzernachricht). Fehlt bei alten Zeilen. */
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
  /** Werkzeugaufruf, der das Ergebnis hochgeladen hat (nur Ausgaben, fehlt bei älteren). */
  tool_call_id?: string
}

export type ApprovalState = "pending" | "approved" | "rejected" | "expired"

export type Approval = {
  id: string
  chat_id: string
  /**
   * artifact_upload: Datei hochladen; internet_access: Bitte um Internetzugang, `name` ist die Begründung;
   * platform_write: schreibender Aufruf der Agri-Gaia-Plattform, `name` ist „METHODE pfad“, `preview` samt Körper.
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
  /** Wer gefragt hat: „main“ oder ein Subagenten-Lauf (leer bei älteren Einträgen). */
  session?: string
  tool_call_id?: string
}

export type SocketCall = {
  id: number
  chat_id?: string
  slot_id: string
  /** cli/mcp für Aufrufe aus der Sandbox; proxy, orchestrator oder pi für Einträge der Überwachung. */
  via: "cli" | "mcp" | "proxy" | "orchestrator" | "pi" | (string & {})
  op: string
  detail: string
  result: string
  created_at: string
  /** „main“ oder ein Subagenten-Lauf, der den Aufruf machte (leer: unbekannt). */
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
  background?: BackgroundTask[]
}

/** Am LLM-Proxy erfasster Modellaufruf (außerhalb der Sandbox gemessen, fälschungssicher). */
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
  /** id: Kennung des Anbieters (tool_calls[].id); daran hängt der Abgleich mit den Ausführungen (E9). */
  tool_calls: { id?: string; name: string; arguments: string }[] | null
  started_at: string
  duration_ms: number
  /** Antwort gehört zur Hauptsitzung; sonst Subagent, Kompaktierung o. Ä. */
  main: boolean
  /** finish_reason des Anbieters (M1) */
  finish_reason?: string
  /** Antwort kam vollständig an (am Proxy mit finish_reason); fehlt bei älteren Einträgen */
  complete?: boolean
}

/**
 * Operation, die der Orchestrator für ein Werkzeug in der Ausführungs-Sandbox ausgeführt hat (E9).
 * Belegt, weil der Orchestrator sie selbst ausgeführt und eingetragen hat.
 */
export type ToolExecutionRecord = {
  id: number
  chat_id: string
  slot_id: string
  /** "main" oder Kennung des Subagenten-Laufs */
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

/** Antwort von GET /api/chats/{id}/tool_executions (Abgleich auf dem Server). */
export type ToolExecutionsResponse = {
  calls: unknown[]
  summary: Record<string, number>
  executions: ToolExecutionRecord[]
  executed_tools: string[]
}

export type SubagentEntryKind = "task" | "tool_call" | "tool_result" | "text"

/**
 * Eintrag aus der Sitzungsdatei eines Subagenten. Quelle ist die Sandbox, also nicht
 * fälschungssicher; `confirmed`: die zugehörige Antwort ist am Proxy belegt.
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
 * Name und Zustand eines Subagenten-Laufs aus den Statusdateien von pi-subagents (Sandbox, nicht
 * fälschungssicher). `label` ist der Name im Workflow (Schlüssel bei runs.run/runs.all).
 */
export type SubagentRunMeta = {
  chat_id: string
  run_id: string
  agent: string
  label?: string
  /** Zustand laut pi-subagents, etwa running, complete, completed, failed, cancelled. */
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
  max_subagents?: number
  /** Übertragene Rechte (fehlt: ohne Delegation). */
  delegation?: Delegation
  /** Bevorzugte Sprache laut Browser (BCP 47, navigator.language); der Agent nutzt sie nur, wenn die Nachricht keine Sprache erkennen lässt. */
  language?: string
}

/** Angemeldeter Nutzer (GET /api/me); im token-Modus nur mode. */
export type Me = { mode: "token" | "oidc"; sub?: string; username?: string; name?: string }

export type Config = {
  internet_default: boolean
  approval_timeout_s: number
  artifact_max_mb: number
  idle_timeout_s: number
  auto_compact_default?: boolean
  compact_reserve_tokens?: number
  compact_keep_recent_tokens?: number
  max_subagents_default?: number
  max_subagents_limit?: number
  /** Werkzeuge, deren Ausführung am Socket belegt wird (E9) */
  executed_tools?: string[]
}

/** Slash-Befehl; `name` ohne führenden Schrägstrich. */
export type Command = {
  name: string
  description?: string
  source: "builtin" | "extension" | "prompt" | "skill"
  args?: string
  /** Mögliche Argumente zur Vervollständigung (/model, /effort). */
  options?: CommandOption[]
}

export type CommandOption = { value: string; label?: string; current?: boolean }

/** Ein pi-RPC-Ereignis; nur die Felder, die die UI auswertet, sind typisiert. */
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

/** Regel einer Delegation: Aktion auf Ressource, ohne ids nur für Aufrufe ohne Objekt. */
export type DelegationRule = { action: string; resource: string; ids?: string[] }

/** Übertragene Rechte eines Chats (docs/plan-delegation-rest-plattform.md, Schritt 1). */
export type Delegation = {
  rules: DelegationRule[]
  expires_at?: string
  /** false: Übergriffe werden nur protokolliert (Stufe ohne Schutzmaßnahme). */
  enforce?: boolean
  confirm?: "writes" | "none"
}

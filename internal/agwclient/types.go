// Package agwclient is a client for the orchestrator's HTTP API (see poc/API.md).
package agwclient

import "encoding/json"

// Pricing: US dollars per 1M tokens.
type Pricing struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
	Currency   string  `json:"currency"`
	Note       string  `json:"note,omitempty"`
	Source     string  `json:"source,omitempty"`
	Retrieved  string  `json:"retrieved,omitempty"` // date the prices were retrieved
}

// TariffWindow: peak time window in UTC, e.g. days "mon-fri", from "01:00", to "09:00".
type TariffWindow struct {
	Days string `json:"days"`
	From string `json:"from"`
	To   string `json:"to"`
}

// Tariff: the prices in Pricing are the peak tariff; outside the windows OffpeakFactor applies.
type Tariff struct {
	PeakWindowsUTC []TariffWindow `json:"peak_windows_utc"`
	OffpeakFactor  float64        `json:"offpeak_factor"`
	Note           string         `json:"note,omitempty"`
}

type Model struct {
	ID       string   `json:"id"`
	Provider string   `json:"provider"`
	Model    string   `json:"model"`
	Name     string   `json:"name"`
	Default  bool     `json:"default"`
	Pricing  *Pricing `json:"pricing,omitempty"`
	Tariff   *Tariff  `json:"tariff,omitempty"`
	PeakNow  *bool    `json:"peak_now,omitempty"`
}

type Variant struct {
	ID    string   `json:"id"`
	Label string   `json:"label"`
	Tools []string `json:"tools"`
}

type Config struct {
	InternetDefault  bool    `json:"internet_default"`
	ApprovalTimeoutS float64 `json:"approval_timeout_s"`
	ArtifactMaxMB    float64 `json:"artifact_max_mb"`
	IdleTimeoutS     float64 `json:"idle_timeout_s"`

	AutoCompactDefault      bool  `json:"auto_compact_default"`
	CompactReserveTokens    int64 `json:"compact_reserve_tokens"`
	CompactKeepRecentTokens int64 `json:"compact_keep_recent_tokens"`

	MaxSubagentsDefault int `json:"max_subagents_default"`
	MaxSubagentsLimit   int `json:"max_subagents_limit"`
}

type Activity struct {
	Kind  string `json:"kind"`
	Tool  string `json:"tool,omitempty"`
	Since string `json:"since"`
}

type Slot struct {
	ID            string    `json:"id"`
	Variant       string    `json:"variant"`
	State         string    `json:"state"`
	ContainerID   string    `json:"container_id"`
	ContainerName string    `json:"container_name"`
	Image         string    `json:"image"`
	CreatedAt     string    `json:"created_at"`
	AssignedAt    string    `json:"assigned_at,omitempty"`
	ChatID        string    `json:"chat_id,omitempty"`
	ChatTitle     string    `json:"chat_title,omitempty"`
	Activity      *Activity `json:"activity,omitempty"`
	Internet      *bool     `json:"internet,omitempty"`
}

type Totals struct {
	Cost        float64 `json:"cost"`
	Tokens      Tokens  `json:"tokens"`
	ChatsActive int     `json:"chats_active"`
}

type Pool struct {
	Slots   []Slot         `json:"slots"`
	Targets map[string]int `json:"targets"`
	Totals  Totals         `json:"totals"`
}

type Tokens struct {
	Input     int64 `json:"input"`
	Output    int64 `json:"output"`
	CacheRead int64 `json:"cache_read"`
	Total     int64 `json:"total"`
}

// ContextUsage: context usage as reported by pi. Tokens and Percent are nil right after a
// compaction until the next response delivers real values. Percent lies between 0 and 100.
type ContextUsage struct {
	Tokens           *int64   `json:"tokens"`
	Window           int64    `json:"window"`
	Percent          *float64 `json:"percent"`
	ThresholdTokens  int64    `json:"threshold_tokens"`
	ReserveTokens    int64    `json:"reserve_tokens"`
	KeepRecentTokens int64    `json:"keep_recent_tokens"`
	UpdatedAt        string   `json:"updated_at"`
}

type Chat struct {
	Delegation       json.RawMessage `json:"delegation,omitempty"`
	ID               string          `json:"id"`
	Title            string          `json:"title"`
	Model            string          `json:"model"`
	Variant          string          `json:"variant"`
	State            string          `json:"state"`
	Internet         bool            `json:"internet"`
	AutoCompact      bool            `json:"auto_compact"`
	Compactions      int             `json:"compactions"`
	MaxSubagents     int             `json:"max_subagents"` // at most this many subagents
	Subagents        int             `json:"subagents"`     // subagent runs started so far
	LLMCalls         int             `json:"llm_calls"`     // model calls recorded at the LLM proxy
	CostOther        float64         `json:"cost_other"`    // cost outside the main session's responses
	Context          *ContextUsage   `json:"context,omitempty"`
	Running          bool            `json:"running"`
	SlotID           string          `json:"slot_id,omitempty"`
	CreatedAt        string          `json:"created_at"`
	UpdatedAt        string          `json:"updated_at"`
	Tokens           Tokens          `json:"tokens"`
	Cost             float64         `json:"cost"`
	ArtifactCount    int             `json:"artifact_count"`
	PendingApprovals int             `json:"pending_approvals"`
	Workspace        *Workspace      `json:"workspace,omitempty"`
	Resuming         bool            `json:"resuming"`              // is being resumed in a fresh sandbox right now
	Queued           int             `json:"queued"`                // queued messages not yet handed over
	QueueHeld        bool            `json:"queue_held"`            // queued items go out only with the next message
	HoldReason       string          `json:"hold_reason,omitempty"` // abort, wake_limit, auto_turns
	RunningSince     string          `json:"running_since,omitempty"`
	// BackgroundRunning: running background tasks (bash with run_in_background).
	BackgroundRunning int `json:"background_running"`
}

// QueueEntry: queued message (the chat's queue).
type QueueEntry struct {
	ID          string   `json:"id"`
	Text        string   `json:"text"`
	Attachments []string `json:"attachments"`
	CreatedAt   string   `json:"created_at"`
	Kind        string   `json:"kind,omitempty"` // user or system (orchestrator note)
}

// BackgroundTask: background task (bash with run_in_background), see API.md.
type BackgroundTask struct {
	ID           string `json:"id"`
	Session      string `json:"session"`
	ToolCallID   string `json:"tool_call_id"`
	Command      string `json:"command"`
	LogPath      string `json:"log_path"`
	State        string `json:"state"`
	ExitCode     *int   `json:"exit_code,omitempty"`
	Error        string `json:"error,omitempty"`
	StoppedBy    string `json:"stopped_by,omitempty"`
	StartedAt    string `json:"started_at"`
	EndedAt      string `json:"ended_at,omitempty"`
	OutputBytes  int64  `json:"output_bytes"`
	OutputLines  int64  `json:"output_lines"`
	OutputSHA256 string `json:"output_sha256,omitempty"`
	Tail         string `json:"tail,omitempty"`
	Woke         bool   `json:"woke,omitempty"`
}

// Workspace: last backup of /workspace (survives idling). SavedAt empty:
// never backed up; SkippedReason: the last backup was skipped.
type Workspace struct {
	Size          int64  `json:"size"`
	ArchiveSize   int64  `json:"archive_size"`
	Files         int    `json:"files"`
	SavedAt       string `json:"saved_at,omitempty"`
	SkippedReason string `json:"skipped_reason,omitempty"`
	SkippedAt     string `json:"skipped_at,omitempty"`
}

// StoredMessage: Cost and Peak are computed by the orchestrator per tariff (only responses and
// compactions). Role "compaction" marks a compaction.
type StoredMessage struct {
	Seq       int64           `json:"seq"`
	Role      string          `json:"role"`
	Message   json.RawMessage `json:"message"`
	Cost      *float64        `json:"cost,omitempty"`
	Peak      *bool           `json:"peak,omitempty"`
	CreatedAt string          `json:"created_at"`
}

// Command is a slash command (name without "/"). Source: builtin, extension, prompt or skill.
type Command struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Source      string `json:"source"`
	Args        string `json:"args,omitempty"`
}

// CommandResult is the response of POST /api/chats/{id}/commands.
type CommandResult struct {
	OK      bool            `json:"ok"`
	Resumed bool            `json:"resumed"`
	Queued  bool            `json:"queued"`
	QueueID string          `json:"queue_id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
}

type Artifact struct {
	ChatID      string `json:"chat_id"`
	Kind        string `json:"kind"`
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	SHA256      string `json:"sha256"`
	ContentType string `json:"content_type"`
	CreatedAt   string `json:"created_at"`
	Via         string `json:"via"`
}

// Approval: Kind "artifact_upload" (Name, Size, SHA256 describe the file) or
// "internet_access" (Name is the agent's justification, Size 0) or "platform_write" (Name is
// "METHOD path" of a call to the Agri-Gaia platform, Preview including the JSON body).
type Approval struct {
	ID          string `json:"id"`
	ChatID      string `json:"chat_id"`
	Kind        string `json:"kind"`
	Via         string `json:"via"`
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	SHA256      string `json:"sha256"`
	ContentType string `json:"content_type"`
	State       string `json:"state"`
	CreatedAt   string `json:"created_at"`
	DecidedAt   string `json:"decided_at,omitempty"`
	Preview     string `json:"preview,omitempty"`
}

type SocketCall struct {
	ID        int64  `json:"id"`
	ChatID    string `json:"chat_id,omitempty"`
	SlotID    string `json:"slot_id"`
	Via       string `json:"via"`
	Op        string `json:"op"`
	Detail    string `json:"detail"`
	Result    string `json:"result"`
	CreatedAt string `json:"created_at"`
}

// ChatDetail is the response of GET /api/chats/{id}.
type ChatDetail struct {
	Chat        Chat            `json:"chat"`
	Messages    []StoredMessage `json:"messages"`
	Artifacts   []Artifact      `json:"artifacts"`
	Approvals   []Approval      `json:"approvals"`
	SocketCalls []SocketCall    `json:"socket_calls"`

	SubagentEntries []SubagentEntry  `json:"subagent_entries"`
	Queue           []QueueEntry     `json:"queue"`
	Background      []BackgroundTask `json:"background"`
}

// LLMToolCall: tool call requested by the model, as seen by the proxy.
type LLMToolCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// LLMCall is a model call recorded at the LLM proxy (measured outside the sandbox).
// Main: the response belongs to the main session; otherwise subagent, compaction or similar.
type LLMCall struct {
	ID         int64         `json:"id"`
	SlotID     string        `json:"slot_id"`
	SourceIP   string        `json:"source_ip"`
	Model      string        `json:"model"`
	ResponseID string        `json:"response_id"`
	Status     int           `json:"status"`
	Input      int64         `json:"input"`
	Output     int64         `json:"output"`
	CacheRead  int64         `json:"cache_read"`
	CacheWrite int64         `json:"cache_write"`
	Cost       float64       `json:"cost"`
	Peak       bool          `json:"peak"`
	ToolCalls  []LLMToolCall `json:"tool_calls"`
	StartedAt  string        `json:"started_at"`
	DurationMs int64         `json:"duration_ms"`
	Main       bool          `json:"main"`
}

// SubagentPayload: filled depending on Kind (task/text: Text; tool_call: Name, Arguments;
// tool_result: Name, Text, IsError).
type SubagentPayload struct {
	Text      string `json:"text,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
}

// SubagentEntry is an entry from a subagent's session file. The source is the sandbox,
// so it is not tamper-proof; Confirmed means: the corresponding response is attested at the proxy.
// Kind: task, tool_call, tool_result or text.
type SubagentEntry struct {
	ChatID     string          `json:"chat_id"`
	RunID      string          `json:"run_id"`
	EntryID    string          `json:"entry_id"`
	Agent      string          `json:"agent"`
	Kind       string          `json:"kind"`
	Payload    SubagentPayload `json:"payload"`
	ResponseID string          `json:"response_id,omitempty"`
	Confirmed  bool            `json:"confirmed"`
	CreatedAt  string          `json:"created_at"`
}

type SendResult struct {
	OK      bool   `json:"ok"`
	Resumed bool   `json:"resumed"`
	Queued  bool   `json:"queued"`             // queued because the agent is working
	QueueID string `json:"queue_id,omitempty"` // ID of the entry
}

// CreateChatRequest: empty fields are omitted so that the server uses its default.
type CreateChatRequest struct {
	Model    string `json:"model,omitempty"`
	Variant  string `json:"variant,omitempty"`
	Title    string `json:"title,omitempty"`
	Message  string `json:"message,omitempty"`
	Internet *bool  `json:"internet,omitempty"`
	// MaxSubagents: nil = server default; 0 is a valid value.
	MaxSubagents *int `json:"max_subagents,omitempty"`
	// Delegation: delegated rights as JSON (empty: no delegation).
	Delegation json.RawMessage `json:"delegation,omitempty"`
}

// ToolExecution is an operation the orchestrator has executed in the execution sandbox
// (E9).
type ToolExecution struct {
	ID            int64          `json:"id"`
	Session       string         `json:"session"`
	ToolCallID    string         `json:"tool_call_id"`
	Tool          string         `json:"tool"`
	Op            string         `json:"op"`
	Args          map[string]any `json:"args"`
	ExitCode      *int           `json:"exit_code,omitempty"`
	Error         string         `json:"error,omitempty"`
	OutputExcerpt string         `json:"output_excerpt,omitempty"`
	OutputSHA256  string         `json:"output_sha256,omitempty"`
	OutputBytes   int64          `json:"output_bytes"`
	StartedAt     string         `json:"started_at"`
	DurationMs    int64          `json:"duration_ms"`
}

// ReconciledCall is a tool call in the reconciliation: requested (proxy) and/or executed
// (orchestrator). State: confirmed, unrequested, unexecuted, mismatch, internal, aborted (model
// response aborted), rejected (refused according to pi's session; Reason, not tamper-proof).
type ReconciledCall struct {
	ToolCallID   string   `json:"tool_call_id"`
	State        string   `json:"state"`
	Tool         string   `json:"tool"`
	ExecutedTool string   `json:"executed_tool,omitempty"`
	Requested    bool     `json:"requested"`
	Executed     bool     `json:"executed"`
	Main         bool     `json:"main"`
	Session      string   `json:"session,omitempty"`
	Arguments    string   `json:"arguments,omitempty"`
	RequestedAt  string   `json:"requested_at,omitempty"`
	StartedAt    string   `json:"started_at,omitempty"`
	Ops          []string `json:"ops"`
	ExitCode     *int     `json:"exit_code,omitempty"`
	Error        string   `json:"error,omitempty"`
	DurationMs   int64    `json:"duration_ms"`
	ExecutionIDs []int64  `json:"execution_ids"`
	Reason       string   `json:"reason,omitempty"`
}

type Reconciliation struct {
	Calls         []ReconciledCall `json:"calls"`
	Summary       map[string]int   `json:"summary"`
	Executions    []ToolExecution  `json:"executions"`
	ExecutedTools []string         `json:"executed_tools"`
}

// Package store keeps chats, messages, sessions, artifacts, approvals
// and the socket log in Postgres.
package store

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schemaSQL string

// registeredSchemas holds schema files that features register (registerSchema, G1.6). They are
// applied after the built-in schema, in the order they were registered.
var (
	schemaMu          sync.Mutex
	registeredSchemas []string
)

// registerSchema adds a schema file (SQL, written idempotently with IF NOT EXISTS, e.g. a table of the
// feature) that is applied whenever a schema is created. Register from an init() of the feature's own
// file, before the first Open or OpenSchema.
func registerSchema(sql string) {
	schemaMu.Lock()
	defer schemaMu.Unlock()
	registeredSchemas = append(registeredSchemas, sql)
}

// SchemaSQL is the whole schema of this package plus everything features registered.
func SchemaSQL() string {
	schemaMu.Lock()
	defer schemaMu.Unlock()
	parts := make([]string, 0, 2+len(registeredSchemas))
	parts = append(parts, schemaSQL, planSchemaSQL)
	parts = append(parts, registeredSchemas...)
	return strings.Join(parts, "\n")
}

var ErrNotFound = errors.New("not found")

// ErrAlreadySent: an input that went with a message. It belongs to the history and is not removed any more
// (issue L4/#121).
var ErrAlreadySent = errors.New("input already sent")

const (
	StateActive  = "active"
	StateDormant = "dormant"

	KindInput  = "input"
	KindOutput = "output"

	ApprovalPending  = "pending"
	ApprovalApproved = "approved"
	ApprovalRejected = "rejected"
	ApprovalExpired  = "expired"
)

type Tokens struct {
	Input     int64 `json:"input"`
	Output    int64 `json:"output"`
	CacheRead int64 `json:"cache_read"`
	Total     int64 `json:"total"`
}

type Chat struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Model string `json:"model"`
	// ThinkingLevel: pi's thinking level (/effort); empty: pi's default, not read yet.
	ThinkingLevel    string          `json:"thinking_level,omitempty"`
	Variant          string          `json:"variant"`
	State            string          `json:"state"`
	Internet         bool            `json:"internet"`
	AutoCompact      bool            `json:"auto_compact"`
	MaxSubagents     int             `json:"max_subagents"`
	Subagents        int             `json:"subagents"`  // started subagents (runs)
	LLMCalls         int             `json:"llm_calls"`  // model calls captured at the proxy
	CostOther        float64         `json:"cost_other"` // of which outside the main session's replies
	Compactions      int             `json:"compactions"`
	Context          json.RawMessage `json:"context,omitempty"`
	CreatedAt        time.Time       `json:"created_at"`
	UpdatedAt        time.Time       `json:"updated_at"`
	Tokens           Tokens          `json:"tokens"`
	Cost             float64         `json:"cost"`
	ArtifactCount    int             `json:"artifact_count"`
	PendingApprovals int             `json:"pending_approvals"`
	// Queued: enqueued messages not yet delivered (queue).
	Queued int `json:"queued"`
	// BackgroundRunning: running background tasks (bash with run_in_background).
	BackgroundRunning int `json:"background_running"`
	// Workspace: last backup of the workspace (nil: none yet and nothing skipped).
	Workspace *WorkspaceInfo `json:"workspace,omitempty"`
	// Delegation: delegated rights of the chat (nil: without delegation, behaviour as before step 1).
	Delegation json.RawMessage `json:"delegation,omitempty"`
	// Experiment: the profile of an experiment chat (X0), set when the chat is created and unchanged
	// after that. nil: a product chat.
	Experiment *Experiment `json:"experiment,omitempty"`
	// Owner: sub of the user who owns the chat (login through the platform); empty in token mode.
	Owner string `json:"owner,omitempty"`
	// Language: the user's preferred language according to the browser (BCP 47); empty: unknown.
	Language string `json:"language,omitempty"`
	// TrashedAt: moved to the trash at this time (issue #76); nil: not in the trash.
	TrashedAt *time.Time `json:"trashed_at,omitempty"`
}

// Origin of the title: TitleDefault (placeholder title, replaced with the first question),
// TitleAuto (from the first question), TitleUser (set by the user, also with /rename).
const (
	TitleDefault = "default"
	TitleAuto    = "auto"
	TitleUser    = "user"
	TitleModel   = "model" // phrased by the model (once, after the first question)
)

// Experiment is the profile of an experiment chat (X0): the cell it belongs to and its protection
// level, which the gateway maps to the delegation. The model, the provider and the proxy address are
// not part of it: they come from the catalogue (AGW_CATALOG), AGW_DEFAULT_MODEL and the chat's own
// model, so that another provider can be used without changing the profile. Limit is set by the
// gateway alone (X3); a value in a request is discarded.
type Experiment struct {
	Protection string `json:"protection"` // none | instruction | service
	Cell       string `json:"cell,omitempty"`
	Run        string `json:"run,omitempty"`
	Seed       int64  `json:"seed,omitempty"`
	// Trace: syscall trace of this run (X7.6/#152): on | off. Empty means the slot's default,
	// which is on for experiment slots.
	Trace string `json:"trace,omitempty"`
	Limit string `json:"limit,omitempty"`
}

type NewChat struct {
	Title, Model, Variant string
	TitleSource           string // empty: TitleUser
	Internet              bool
	AutoCompact           bool
	MaxSubagents          int
	Delegation            json.RawMessage // nil: without delegation
	Experiment            *Experiment     // nil: a product chat
	Owner                 string          // empty: without owner (token mode)
	Language              string          // empty: unknown; checked by the caller (ValidLanguage)
}

type Message struct {
	Seq       int             `json:"seq"`
	Role      string          `json:"role"`
	Message   json.RawMessage `json:"message"`
	Cost      *float64        `json:"cost,omitempty"` // USD by tariff, only for replies
	Peak      *bool           `json:"peak,omitempty"` // peak tariff applied
	CreatedAt time.Time       `json:"created_at"`
	// Review 3 (H1): turn and trigger (all messages of a turn), origin and parts
	// (only the user message, i.e. the request to pi). Empty for rows from before this change.
	TurnID  *int64   `json:"turn_id,omitempty"`
	Trigger string   `json:"trigger,omitempty"`
	Origin  string   `json:"origin,omitempty"`
	Sources []Source `json:"sources,omitempty"`
	// AbortReason: why the turn this message belongs to was aborted (issue #120, L3), from chat_turns. Empty for
	// turns that were not aborted and for rows from before this change.
	AbortReason string `json:"abort_reason,omitempty"`
}

// Billing is the cost of a reply as computed by the orchestrator.
type Billing struct {
	Cost float64
	Peak bool
}

type Artifact struct {
	ChatID      string    `json:"chat_id"`
	Kind        string    `json:"kind"`
	Name        string    `json:"name"`
	Size        int64     `json:"size"`
	SHA256      string    `json:"sha256"`
	ContentType string    `json:"content_type"`
	Via         string    `json:"via"`
	ObjectKey   string    `json:"-"`
	CreatedAt   time.Time `json:"created_at"`
	// SentAt: when the input went with a message (issue L4/#121); NULL while it is only staged in the field.
	// A sent input is not removed any more, an unsent one is (DeleteUnsentInput).
	SentAt *time.Time `json:"sent_at,omitempty"`
	// ToolCallID: tool call that uploaded the artifact (outputs only; empty for older
	// artifacts). Serves solely to show it at the right place in the history; with the CLI the
	// ID comes from the command's environment, so the agent could change it.
	ToolCallID string `json:"tool_call_id,omitempty"`
	// Preview: how the UI can show the file (image, pdf, table, text, none), decided by the orchestrator when the
	// file is stored (artifacts.PreviewKind). Empty for artifacts from before this field. Only a hint for the UI:
	// the inline download decides again from the bytes.
	Preview string `json:"preview,omitempty"`
}

type toolCallKey struct{}
type sessionKey struct{}

// WithSession attaches the session of a call to the context ("main" or the ID of a
// subagent run, as in tool_executions.session); for display only, changeable by the agent with the CLI.
func WithSession(ctx context.Context, s string) context.Context {
	return context.WithValue(ctx, sessionKey{}, s)
}

// SessionFrom returns the session from WithSession (otherwise empty).
func SessionFrom(ctx context.Context) string {
	s, _ := ctx.Value(sessionKey{}).(string)
	return s
}

// WithToolCall attaches the ID of the tool call to the context of an upload.
func WithToolCall(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, toolCallKey{}, id)
}

type durationKey struct{}

// WithDuration attaches the measured duration of a platform call to the context of its log entry.
func WithDuration(ctx context.Context, d time.Duration) context.Context {
	return context.WithValue(ctx, durationKey{}, d)
}

// DurationMsFrom returns the duration from WithDuration in milliseconds (nil: none attached).
func DurationMsFrom(ctx context.Context) *float64 {
	d, ok := ctx.Value(durationKey{}).(time.Duration)
	if !ok || d < 0 {
		return nil
	}
	ms := math.Round(float64(d)/float64(time.Millisecond)*10) / 10
	return &ms
}

// ToolCallFrom returns the ID from WithToolCall (otherwise empty).
func ToolCallFrom(ctx context.Context) string {
	id, _ := ctx.Value(toolCallKey{}).(string)
	return id
}

type callRefKey struct{}

// CallRef is the reference of a platform call to the tool call of the session (X5.3). Source says how it was
// obtained: "pi" (the extension on pi's socket), "exec_window" (exactly one running execution of the sandbox),
// "exec_ambiguous" (several; Candidates names them), "none" (no running execution). Claimed is the header the agent
// can set himself and therefore never binds.
type CallRef struct {
	Ref        string
	Source     string
	Candidates []string
	Claimed    string
}

func WithCallRef(ctx context.Context, r CallRef) context.Context {
	return context.WithValue(ctx, callRefKey{}, r)
}

// CallRefFrom returns the reference from WithCallRef.
func CallRefFrom(ctx context.Context) (CallRef, bool) {
	r, ok := ctx.Value(callRefKey{}).(CallRef)
	return r, ok
}

type Approval struct {
	ID          string     `json:"id"`
	ChatID      string     `json:"chat_id"`
	Kind        string     `json:"kind"`
	Via         string     `json:"via"`
	Name        string     `json:"name"`
	Size        int64      `json:"size"`
	SHA256      string     `json:"sha256"`
	ContentType string     `json:"content_type"`
	PendingKey  string     `json:"-"`
	Preview     string     `json:"preview,omitempty"`
	State       string     `json:"state"`
	CreatedAt   time.Time  `json:"created_at"`
	DecidedAt   *time.Time `json:"decided_at,omitempty"`
	// DecidedBy: who decided ("user" for the normal path, "experiment" when the gateway approved it in
	// an experiment chat, X0.9); "user" for entries from before the column existed.
	DecidedBy string `json:"decided_by,omitempty"`
	// Session and ToolCallID: who asked (main agent "main" or subagent run) and in which
	// tool call; empty for older entries.
	Session    string `json:"session,omitempty"`
	ToolCallID string `json:"tool_call_id,omitempty"`
	// DurationMs: round trip of a platform call in milliseconds (nil: not measured).
	DurationMs *float64 `json:"duration_ms,omitempty"`
	// Effect: class of a platform call (read, write, compute, irreversible); empty for other kinds and
	// for entries from before issue #63.
	Effect string `json:"effect,omitempty"`
	// Summary: what the gateway says will happen (issue #72), written from the call and platform state the gateway
	// read itself, never from the agent's text; first line the action, then one detail per line. Platform calls and
	// plans only; empty for older entries.
	Summary string `json:"summary,omitempty"`
	// SummaryProposed and SummaryChangedAt (issue L2/#119): the text the user saw when the approval was raised and
	// the time a re-read at the moment of the approval found other values. Both empty/nil while nothing changed.
	SummaryProposed  string     `json:"summary_proposed,omitempty"`
	SummaryChangedAt *time.Time `json:"summary_changed_at,omitempty"`
	// Notebook: the structured data of the approval of a run in the chat's notebook server (own code C-E4), built
	// by the notebook service and written as it is — entry, files with size, SHA-256 and text, datasets and limits.
	// The frontend shows it instead of the text of the preview; empty for every other kind of approval.
	Notebook json.RawMessage `json:"notebook,omitempty"`
	// Uploads: the summary of the files of an upload approval (issue #164) — number, size, folders, file types,
	// annotation file and the SHA-256 over the list of every file — without the list itself. UploadEntries is that
	// list; it is written with the approval, never read with it (the card fetches it when the user opens the list,
	// GetApprovalUploadEntries) and so stays out of every approval event and listing.
	Uploads       json.RawMessage `json:"uploads,omitempty"`
	UploadEntries json.RawMessage `json:"-"`
}

type SocketCall struct {
	ID        int64     `json:"id"`
	ChatID    string    `json:"chat_id,omitempty"`
	SlotID    string    `json:"slot_id"`
	Via       string    `json:"via"`
	Op        string    `json:"op"`
	Detail    string    `json:"detail"`
	Result    string    `json:"result"`
	CreatedAt time.Time `json:"created_at"`
	// Session: main agent ("main") or subagent run that made the call (empty: unknown).
	Session    string `json:"session,omitempty"`
	ToolCallID string `json:"tool_call_id,omitempty"`
	// DurationMs: round trip of a platform call in milliseconds (nil: not measured).
	DurationMs *float64 `json:"duration_ms,omitempty"`
	// Effect: class of the platform call (see Approval.Effect).
	Effect string `json:"effect,omitempty"`
	// X5: the structured record of a platform call.
	Decision          string          `json:"decision,omitempty"`
	Reason            string          `json:"reason,omitempty"`
	Rule              json.RawMessage `json:"rule,omitempty"`
	Body              []byte          `json:"-"`
	BodyTruncated     bool            `json:"body_truncated,omitempty"`
	BodyInSHA256      string          `json:"body_in_sha256,omitempty"`
	CallRef           string          `json:"call_ref,omitempty"`
	CallRefSource     string          `json:"call_ref_source,omitempty"`
	CallRefCandidates []string        `json:"call_ref_candidates,omitempty"`
	CallRefClaimed    string          `json:"call_ref_claimed,omitempty"`
	WithinDelegation  *bool           `json:"within_delegation,omitempty"`
	ApprovalID        string          `json:"approval_id,omitempty"`
	ApprovalDecision  string          `json:"approval_decision,omitempty"`
	PlanStep          *int            `json:"plan_step,omitempty"`
}

// CallRecord is what the orchestrator writes into socket_calls (X5). The old LogCall signatures build one from
// their single values; a platform call adds decision, body and reference.
type CallRecord struct {
	SlotID, ChatID, Via, Op, Detail, Result string
	Session, ToolCallID                     string
	DurationMs                              *float64
	Effect                                  string
	Decision, Reason                        string
	Rule                                    json.RawMessage
	Body                                    []byte
	BodyTruncated                           bool
	BodyInSHA256                            string
	CallRef, CallRefSource                  string
	CallRefCandidates                       []string
	CallRefClaimed                          string
	WithinDelegation                        *bool
	ApprovalID, ApprovalDecision            string
	PlanStep                                *int
}

type Store struct {
	pool   *pgxpool.Pool
	schema string
}

func Open(ctx context.Context, url string) (*Store, error) { return OpenSchema(ctx, url, "") }

var schemaName = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// OpenSchema opens the database and creates the schema. With schema != ""
// the store works in its own Postgres schema (for tests). The schema must not exist yet: joining an
// existing one would mean sharing it with whoever created it, and a later DropSchema would take the
// tables out from under that other user (issue #158).
func OpenSchema(ctx context.Context, url, schema string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	if schema != "" {
		if !schemaName.MatchString(schema) {
			return nil, fmt.Errorf("invalid schema name %q", schema)
		}
		cfg.ConnConfig.RuntimeParams["search_path"] = schema
	}
	var p *pgxpool.Pool
	for i := 0; ; i++ {
		p, err = pgxpool.NewWithConfig(ctx, cfg)
		if err == nil {
			err = p.Ping(ctx)
		}
		if err == nil || i >= 30 {
			break
		}
		if p != nil {
			p.Close()
		}
		time.Sleep(time.Second)
	}
	if err != nil {
		return nil, fmt.Errorf("Postgres: %w", err)
	}
	if schema != "" {
		if _, err := p.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
			p.Close()
			return nil, err
		}
	}
	if _, err := p.Exec(ctx, SchemaSQL()); err != nil {
		p.Close()
		return nil, fmt.Errorf("creating schema: %w", err)
	}
	return &Store{pool: p, schema: schema}, nil
}

func (s *Store) Close() { s.pool.Close() }

// DropSchema removes the schema this store created (tests call it from t.Cleanup). Only this store has
// ever used the schema, so dropping it cannot pull the ground out from under another test.
func (s *Store) DropSchema(ctx context.Context) {
	if s.schema != "" {
		_, _ = s.pool.Exec(ctx, "DROP SCHEMA IF EXISTS "+s.schema+" CASCADE")
	}
}

// chatSelect computes tokens and cost from the stored
// assistant messages (usage per reply, as pi delivers it).
const chatSelect = `
SELECT c.id::text, c.title, c.model, c.thinking_level, c.variant, c.state, c.internet, c.auto_compact, c.max_subagents,
  (SELECT count(*) FROM chat_messages k WHERE k.chat_id = c.id AND k.role = 'compaction'), c.context,
  c.created_at, c.updated_at,
  CASE WHEN l.n > 0 THEN l.input ELSE COALESCE(u.input,0) END,
  CASE WHEN l.n > 0 THEN l.output ELSE COALESCE(u.output,0) END,
  CASE WHEN l.n > 0 THEN l.cache_read ELSE COALESCE(u.cache_read,0) END,
  CASE WHEN l.n > 0 THEN l.input + l.output + l.cache_read ELSE COALESCE(u.total,0) END,
  CASE WHEN l.n > 0 THEN l.cost ELSE COALESCE(u.cost,0) END,
  (SELECT count(*) FROM artifacts a WHERE a.chat_id = c.id AND a.kind = 'output'),
  (SELECT count(*) FROM approvals p WHERE p.chat_id = c.id AND p.state = 'pending'),
  (SELECT count(DISTINCT run_id) FROM subagent_entries s WHERE s.chat_id = c.id),
  l.n, COALESCE(l.other,0),
  (SELECT jsonb_build_object('size', w.size, 'archive_size', w.archive_size, 'files', w.files, 'sha256', w.sha256,
     'saved_at', w.saved_at, 'skipped_reason', w.skipped_reason, 'skipped_size', w.skipped_size, 'skipped_at', w.skipped_at)
   FROM chat_workspaces w WHERE w.chat_id = c.id),
  (SELECT count(*) FROM chat_queue q WHERE q.chat_id = c.id AND q.delivered_at IS NULL),
  (SELECT count(*) FROM background_tasks b WHERE b.chat_id = c.id AND b.state = 'running'),
  c.delegation, COALESCE(c.owner, ''), COALESCE(c.language, ''), c.trashed_at, c.experiment
FROM chats c
LEFT JOIN LATERAL (
  SELECT sum((m.message->'usage'->>'input')::bigint)       AS input,
         sum((m.message->'usage'->>'output')::bigint)      AS output,
         sum((m.message->'usage'->>'cacheRead')::bigint)   AS cache_read,
         sum((m.message->'usage'->>'totalTokens')::bigint) AS total,
         sum(COALESCE(m.cost, (m.message->'usage'->'cost'->>'total')::float8)) AS cost
  FROM chat_messages m WHERE m.chat_id = c.id AND m.role IN ('assistant','compaction')
) u ON true
LEFT JOIN LATERAL (
  SELECT count(*) AS n, COALESCE(sum(x.input),0) AS input, COALESCE(sum(x.output),0) AS output,
         COALESCE(sum(x.cache_read),0) AS cache_read, COALESCE(sum(x.cost),0) AS cost,
         sum(x.cost) FILTER (WHERE x.response_id = '' OR NOT EXISTS (
             SELECT 1 FROM chat_messages mm WHERE mm.chat_id = c.id AND mm.role = 'assistant'
               AND mm.message->>'responseId' = x.response_id)) AS other
  FROM llm_calls x WHERE x.chat_id = c.id
) l ON true`

func scanChat(row pgx.Row) (Chat, error) {
	var c Chat
	var ctxRaw, wsRaw, delRaw, expRaw []byte
	err := row.Scan(&c.ID, &c.Title, &c.Model, &c.ThinkingLevel, &c.Variant, &c.State, &c.Internet, &c.AutoCompact, &c.MaxSubagents, &c.Compactions, &ctxRaw, &c.CreatedAt, &c.UpdatedAt,
		&c.Tokens.Input, &c.Tokens.Output, &c.Tokens.CacheRead, &c.Tokens.Total, &c.Cost, &c.ArtifactCount, &c.PendingApprovals,
		&c.Subagents, &c.LLMCalls, &c.CostOther, &wsRaw, &c.Queued, &c.BackgroundRunning, &delRaw, &c.Owner, &c.Language, &c.TrashedAt, &expRaw)
	if len(delRaw) > 0 {
		c.Delegation = delRaw
	}
	if len(expRaw) > 0 {
		var e Experiment
		if json.Unmarshal(expRaw, &e) == nil {
			c.Experiment = &e
		}
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return c, ErrNotFound
	}
	if len(ctxRaw) > 0 {
		c.Context = ctxRaw
	}
	if len(wsRaw) > 0 {
		var w WorkspaceInfo
		if json.Unmarshal(wsRaw, &w) == nil {
			c.Workspace = &w
		}
	}
	return c, err
}

func (s *Store) CreateChat(ctx context.Context, n NewChat) (Chat, error) {
	var id string
	src := n.TitleSource
	if src == "" {
		src = TitleUser
	}
	var del any // NULL without delegation
	if len(n.Delegation) > 0 {
		del = string(n.Delegation)
	}
	var owner any // NULL without owner
	if n.Owner != "" {
		owner = n.Owner
	}
	var lang any // NULL if not given
	if n.Language != "" {
		lang = n.Language
	}
	var exp any // NULL for a product chat
	if n.Experiment != nil {
		b, _ := json.Marshal(n.Experiment)
		exp = string(b)
	}
	err := s.pool.QueryRow(ctx, `INSERT INTO chats (title, title_source, model, variant, internet, auto_compact, max_subagents, delegation, owner, language, experiment) VALUES ($1,$2,$3,$4,$5,$6,$7,$8::jsonb,$9,$10,$11::jsonb) RETURNING id::text`,
		n.Title, src, n.Model, n.Variant, n.Internet, n.AutoCompact, n.MaxSubagents, del, owner, lang, exp).Scan(&id)
	if err != nil {
		return Chat{}, err
	}
	return s.GetChat(ctx, id)
}

func (s *Store) GetChat(ctx context.Context, id string) (Chat, error) {
	if !isUUID(id) {
		return Chat{}, ErrNotFound
	}
	return scanChat(s.pool.QueryRow(ctx, chatSelect+` WHERE c.id = $1`, id))
}

// ChatOwner returns the owner of a chat (empty: without owner); unknown: ErrNotFound.
func (s *Store) ChatOwner(ctx context.Context, id string) (string, error) {
	if !isUUID(id) {
		return "", ErrNotFound
	}
	var owner string
	err := s.pool.QueryRow(ctx, `SELECT COALESCE(owner, '') FROM chats WHERE id = $1`, id).Scan(&owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return owner, err
}

// ChatGate returns what the LLM proxy needs about a chat (X3): its model, its experiment profile (nil for a
// product chat) and the moment the chat was created (the run's duration counts from there).
func (s *Store) ChatGate(ctx context.Context, id string) (model string, exp *Experiment, created time.Time, err error) {
	if !isUUID(id) {
		return "", nil, time.Time{}, ErrNotFound
	}
	var raw []byte
	err = s.pool.QueryRow(ctx, `SELECT model, experiment, created_at FROM chats WHERE id = $1`, id).Scan(&model, &raw, &created)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil, time.Time{}, ErrNotFound
	}
	if err != nil {
		return "", nil, time.Time{}, err
	}
	if len(raw) > 0 {
		var e Experiment
		if json.Unmarshal(raw, &e) == nil {
			exp = &e
		}
	}
	return model, exp, created, nil
}

// SetExperimentLimit records the limit that ended an experiment run (X3.5) in chats.experiment; false if one was
// already set or the chat is not an experiment chat. The gateway writes this, never a request.
func (s *Store) SetExperimentLimit(ctx context.Context, id, reason string) (bool, error) {
	if !isUUID(id) {
		return false, ErrNotFound
	}
	tag, err := s.pool.Exec(ctx, `
UPDATE chats SET experiment = jsonb_set(experiment, '{limit}', to_jsonb($2::text))
WHERE id = $1 AND experiment IS NOT NULL AND COALESCE(experiment->>'limit', '') = ''`, id, reason)
	return tag.RowsAffected() > 0, err
}

// ListChats returns the chats that are not in the trash, newest first (the trash lists them: ListTrash).
func (s *Store) ListChats(ctx context.Context) ([]Chat, error) {
	rows, err := s.pool.Query(ctx, chatSelect+` WHERE c.trashed_at IS NULL ORDER BY c.updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Chat
	for rows.Next() {
		c, err := scanChat(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ChatsInState returns the chats in a state (for the orchestrator's restart).
func (s *Store) ChatsInState(ctx context.Context, state string) ([]Chat, error) {
	all, err := s.ListChats(ctx)
	if err != nil {
		return nil, err
	}
	var out []Chat
	for _, c := range all {
		if c.State == state {
			out = append(out, c)
		}
	}
	return out, nil
}

func (s *Store) exec1(ctx context.Context, sql string, args ...any) error {
	tag, err := s.pool.Exec(ctx, sql, args...)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) SetState(ctx context.Context, id, state string) error {
	return s.exec1(ctx, `UPDATE chats SET state=$2, updated_at=now() WHERE id=$1`, id, state)
}

func (s *Store) SetInternet(ctx context.Context, id string, on bool) error {
	return s.exec1(ctx, `UPDATE chats SET internet=$2, updated_at=now() WHERE id=$1`, id, on)
}

func (s *Store) SetAutoCompact(ctx context.Context, id string, on bool) error {
	return s.exec1(ctx, `UPDATE chats SET auto_compact=$2, updated_at=now() WHERE id=$1`, id, on)
}

// SetContext records the last known context usage (without changing
// updated_at, so that the chat list is not reordered on every measurement).
func (s *Store) SetContext(ctx context.Context, id string, usage json.RawMessage) error {
	return s.exec1(ctx, `UPDATE chats SET context=$2 WHERE id=$1`, id, []byte(usage))
}

func (s *Store) SetCommands(ctx context.Context, id string, cmds json.RawMessage) error {
	return s.exec1(ctx, `UPDATE chats SET commands=$2 WHERE id=$1`, id, []byte(cmds))
}

func (s *Store) Commands(ctx context.Context, id string) (json.RawMessage, error) {
	var b []byte
	err := s.pool.QueryRow(ctx, `SELECT commands FROM chats WHERE id=$1`, id).Scan(&b)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return b, err
}

// ChatInternet reads only the internet switch (web proxy, web-gate.ts; without GetChat's totals).
func (s *Store) ChatInternet(ctx context.Context, id string) (bool, error) {
	if !isUUID(id) {
		return false, ErrNotFound
	}
	var on bool
	err := s.pool.QueryRow(ctx, `SELECT internet FROM chats WHERE id=$1`, id).Scan(&on)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrNotFound
	}
	return on, err
}

func (s *Store) SetModel(ctx context.Context, id, model string) error {
	return s.exec1(ctx, `UPDATE chats SET model=$2, updated_at=now() WHERE id=$1`, id, model)
}

func (s *Store) SetThinkingLevel(ctx context.Context, id, level string) error {
	return s.exec1(ctx, `UPDATE chats SET thinking_level=$2 WHERE id=$1`, id, level)
}

// SetTitle sets the title as chosen by the user; automatic naming no longer applies afterwards.
// It leaves updated_at alone: the chat list is ordered by it, and a rename is no activity, so the
// chat keeps its place in the history (issue #49 of the thesis repository, as in ChatGPT).
func (s *Store) SetTitle(ctx context.Context, id, title string) error {
	return s.exec1(ctx, `UPDATE chats SET title=$2, title_source='user' WHERE id=$1`, id, title)
}

// ModelTitle replaces the title from the first question with the model's, but not if the
// user has renamed the chat in the meantime; true if it was replaced.
func (s *Store) ModelTitle(ctx context.Context, id, title string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE chats SET title=$2, title_source='model' WHERE id=$1 AND title_source='auto'`, id, title)
	return tag.RowsAffected() > 0, err
}

// AuxCall is a model call of the orchestrator itself (aux_llm_calls).
type AuxCall struct {
	ChatID, Purpose, Model, Error string
	Status                        int
	Input, Output, CacheRead      int64
	Cost                          float64
	Peak                          bool
	StartedAt                     time.Time
	DurationMs                    int64
}

func (s *Store) RecordAuxCall(ctx context.Context, c AuxCall) error {
	_, err := s.pool.Exec(ctx, `
INSERT INTO aux_llm_calls (chat_id, purpose, model, status, input, output, cache_read, cost, peak, error, started_at, duration_ms)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		c.ChatID, c.Purpose, c.Model, c.Status, c.Input, c.Output, c.CacheRead, c.Cost, c.Peak, noNUL(c.Error), c.StartedAt, c.DurationMs)
	return err
}

// WebRequest is a request through the web proxy (web_requests).
type WebRequest struct {
	ID         int64     `json:"id"`
	ChatID     string    `json:"chat_id"`
	SlotID     string    `json:"slot_id"`
	SourceIP   string    `json:"source_ip"`
	Method     string    `json:"method"`
	Host       string    `json:"host"`
	Port       int       `json:"port"`
	Path       string    `json:"path,omitempty"`
	Status     int       `json:"status"`
	BytesUp    int64     `json:"bytes_up"`
	BytesDown  int64     `json:"bytes_down"`
	Denied     string    `json:"denied,omitempty"`
	StartedAt  time.Time `json:"started_at"`
	DurationMs int64     `json:"duration_ms"`
}

func (s *Store) AddWebRequest(ctx context.Context, r WebRequest) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `
INSERT INTO web_requests (chat_id, slot_id, source_ip, method, host, port, path, status, bytes_up, bytes_down, denied, started_at, duration_ms)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13) RETURNING id`,
		r.ChatID, r.SlotID, r.SourceIP, r.Method, noNUL(r.Host), r.Port, noNUL(r.Path), r.Status, r.BytesUp, r.BytesDown, noNUL(r.Denied), r.StartedAt, r.DurationMs).Scan(&id)
	return id, err
}

// FinishWebRequest completes a tunnel when it closes.
func (s *Store) FinishWebRequest(ctx context.Context, id, up, down, durationMs int64, denied string) error {
	return s.exec1(ctx, `UPDATE web_requests SET bytes_up=$2, bytes_down=$3, duration_ms=$4, denied=$5 WHERE id=$1`, id, up, down, durationMs, noNUL(denied))
}

// WebRequests returns a chat's requests through the web proxy.
func (s *Store) WebRequests(ctx context.Context, chatID string) ([]WebRequest, error) {
	rows, err := s.pool.Query(ctx, `
SELECT id, chat_id::text, slot_id, source_ip, method, host, port, path, status, bytes_up, bytes_down, denied, started_at, duration_ms
FROM web_requests WHERE chat_id = $1 ORDER BY id`, chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []WebRequest{}
	for rows.Next() {
		var r WebRequest
		if err := rows.Scan(&r.ID, &r.ChatID, &r.SlotID, &r.SourceIP, &r.Method, &r.Host, &r.Port, &r.Path, &r.Status, &r.BytesUp, &r.BytesDown, &r.Denied, &r.StartedAt, &r.DurationMs); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// AuxCalls returns the orchestrator's model calls for a chat.
func (s *Store) AuxCalls(ctx context.Context, chatID string) ([]AuxCall, error) {
	rows, err := s.pool.Query(ctx, `
SELECT chat_id::text, purpose, model, error, status, input, output, cache_read, cost, peak, started_at, duration_ms
FROM aux_llm_calls WHERE chat_id = $1 ORDER BY id`, chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuxCall
	for rows.Next() {
		var c AuxCall
		if err := rows.Scan(&c.ChatID, &c.Purpose, &c.Model, &c.Error, &c.Status, &c.Input, &c.Output, &c.CacheRead, &c.Cost, &c.Peak, &c.StartedAt, &c.DurationMs); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// AutoTitle replaces the placeholder title, and only that; true if it was replaced.
func (s *Store) AutoTitle(ctx context.Context, id, title string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE chats SET title=$2, title_source='auto' WHERE id=$1 AND title_source='default'`, id, title)
	return tag.RowsAffected() > 0, err
}

func (s *Store) Touch(ctx context.Context, id string) error {
	return s.exec1(ctx, `UPDATE chats SET updated_at=now() WHERE id=$1`, id)
}

func (s *Store) SaveSession(ctx context.Context, id string, data []byte) error {
	return s.exec1(ctx, `UPDATE chats SET session=$2, session_sha=encode(sha256($2),'hex'), updated_at=now() WHERE id=$1`, id, data)
}

// LoadSession returns nil if no session has been saved yet.
func (s *Store) LoadSession(ctx context.Context, id string) ([]byte, error) {
	var b []byte
	err := s.pool.QueryRow(ctx, `SELECT session FROM chats WHERE id=$1`, id).Scan(&b)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return b, err
}

func (s *Store) AppendMessage(ctx context.Context, chatID string, msg json.RawMessage) (int, error) {
	return s.AppendBilledMessage(ctx, chatID, msg, nil)
}

// AppendBilledMessage stores a message along with its computed cost.
func (s *Store) AppendBilledMessage(ctx context.Context, chatID string, msg json.RawMessage, bill *Billing) (int, error) {
	return s.AppendTurnMessage(ctx, chatID, msg, bill, nil)
}

// MessageMeta: turn, trigger and (only for the user message) origin and parts.
type MessageMeta struct {
	TurnID  int64
	Trigger string
	Origin  string
	Sources []Source
}

// AppendTurnMessage stores a message along with cost and turn details (meta may be nil).
func (s *Store) AppendTurnMessage(ctx context.Context, chatID string, msg json.RawMessage, bill *Billing, meta *MessageMeta) (int, error) {
	var cost, peak any
	if bill != nil {
		cost, peak = bill.Cost, bill.Peak
	}
	var turn, trigger, origin, sources any
	if meta != nil {
		if meta.TurnID > 0 {
			turn = meta.TurnID
		}
		if meta.Trigger != "" {
			trigger = meta.Trigger
		}
		if meta.Origin != "" {
			origin = meta.Origin
			b, _ := json.Marshal(meta.Sources)
			sources = b
		}
	}
	var role struct {
		Role string `json:"role"`
	}
	_ = json.Unmarshal(msg, &role)
	var seq int
	err := s.pool.QueryRow(ctx, `
INSERT INTO chat_messages (chat_id, seq, role, message, cost, peak, turn_id, trigger, origin, sources)
VALUES ($1, COALESCE((SELECT max(seq) FROM chat_messages WHERE chat_id=$1),0)+1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING seq`, chatID, role.Role, []byte(msg), cost, peak, turn, trigger, origin, sources).Scan(&seq)
	if err == nil {
		_ = s.Touch(ctx, chatID)
	}
	return seq, err
}

// Messages returns the stored messages of a chat. Every message carries the abort reason of its turn (issue #120,
// L3), so a reloaded chat still shows why a step was stopped.
func (s *Store) Messages(ctx context.Context, chatID string) ([]Message, error) {
	rows, err := s.pool.Query(ctx, `SELECT m.seq, m.role, m.message, m.cost, m.peak, m.created_at, m.turn_id,
       COALESCE(m.trigger,''), COALESCE(m.origin,''), m.sources, COALESCE(t.abort_reason,'')
FROM chat_messages m LEFT JOIN chat_turns t ON t.id = m.turn_id
WHERE m.chat_id=$1 ORDER BY m.seq`, chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Message{}
	for rows.Next() {
		var m Message
		var raw, src []byte
		if err := rows.Scan(&m.Seq, &m.Role, &raw, &m.Cost, &m.Peak, &m.CreatedAt, &m.TurnID, &m.Trigger, &m.Origin, &src, &m.AbortReason); err != nil {
			return nil, err
		}
		m.Message = raw
		if len(src) > 0 {
			_ = json.Unmarshal(src, &m.Sources)
			m.Sources = withAudience(m.Sources)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) StartRun(ctx context.Context, chatID, slotID, containerID string) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `INSERT INTO chat_runs (chat_id, slot_id, container_id) VALUES ($1,$2,$3) RETURNING id`, chatID, slotID, containerID).Scan(&id)
	return id, err
}

func (s *Store) EndRun(ctx context.Context, id int64) error {
	_, err := s.pool.Exec(ctx, `UPDATE chat_runs SET ended_at=now() WHERE id=$1 AND ended_at IS NULL`, id)
	return err
}

func (s *Store) PutArtifact(ctx context.Context, a Artifact) error {
	if a.ContentType == "" {
		a.ContentType = "application/octet-stream"
	}
	// sent_at is not part of the statement: a re-upload of the same name keeps the state of the row that is
	// there (issue L4/#121, safe error: an input that was sent once stays not removable).
	_, err := s.pool.Exec(ctx, `
INSERT INTO artifacts (chat_id, kind, name, size, sha256, content_type, via, object_key, tool_call_id, preview)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
ON CONFLICT (chat_id, kind, name) DO UPDATE SET size=EXCLUDED.size, sha256=EXCLUDED.sha256,
  content_type=EXCLUDED.content_type, via=EXCLUDED.via, object_key=EXCLUDED.object_key,
  tool_call_id=EXCLUDED.tool_call_id, preview=EXCLUDED.preview, created_at=now()`,
		a.ChatID, a.Kind, a.Name, a.Size, a.SHA256, a.ContentType, a.Via, a.ObjectKey, a.ToolCallID, a.Preview)
	return err
}

const artifactCols = `chat_id::text, kind, name, size, sha256, content_type, via, object_key, created_at, tool_call_id, preview, sent_at`

func scanArtifact(row pgx.Row) (Artifact, error) {
	var a Artifact
	err := row.Scan(&a.ChatID, &a.Kind, &a.Name, &a.Size, &a.SHA256, &a.ContentType, &a.Via, &a.ObjectKey, &a.CreatedAt, &a.ToolCallID, &a.Preview, &a.SentAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return a, ErrNotFound
	}
	return a, err
}

func (s *Store) ListArtifacts(ctx context.Context, chatID string) ([]Artifact, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+artifactCols+` FROM artifacts WHERE chat_id=$1 ORDER BY kind, created_at`, chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Artifact{}
	for rows.Next() {
		a, err := scanArtifact(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) GetArtifact(ctx context.Context, chatID, kind, name string) (Artifact, error) {
	return scanArtifact(s.pool.QueryRow(ctx, `SELECT `+artifactCols+` FROM artifacts WHERE chat_id=$1 AND kind=$2 AND name=$3`, chatID, kind, name))
}

// MarkInputsSent notes that these inputs went with a message (issue L4/#121). An input that was sent before keeps
// its earlier time; names that are no input of the chat stay untouched.
func (s *Store) MarkInputsSent(ctx context.Context, chatID string, names []string) error {
	if len(names) == 0 {
		return nil
	}
	_, err := s.pool.Exec(ctx, `UPDATE artifacts SET sent_at=now()
		WHERE chat_id=$1 AND kind=$2 AND name = ANY($3) AND sent_at IS NULL`, chatID, KindInput, names)
	return err
}

// DeleteUnsentInput removes an input that was not sent with a message and returns the row (its ObjectKey is
// needed to remove the file from the object store). ErrNotFound for an unknown name, ErrAlreadySent for a file
// that went with a message (issue L4/#121).
func (s *Store) DeleteUnsentInput(ctx context.Context, chatID, name string) (Artifact, error) {
	a, err := scanArtifact(s.pool.QueryRow(ctx, `DELETE FROM artifacts
		WHERE chat_id=$1 AND kind=$2 AND name=$3 AND sent_at IS NULL RETURNING `+artifactCols, chatID, KindInput, name))
	if err == nil {
		return a, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return Artifact{}, err
	}
	// Nothing deleted: either the input is unknown or it has already been sent.
	if _, gerr := s.GetArtifact(ctx, chatID, KindInput, name); gerr == nil {
		return Artifact{}, ErrAlreadySent
	}
	return Artifact{}, ErrNotFound
}

const approvalCols = `id::text, chat_id::text, kind, via, name, size, sha256, content_type, pending_key, COALESCE(preview,''), state, created_at, decided_at, decided_by, session, tool_call_id, COALESCE(effect,''), COALESCE(summary,''), COALESCE(summary_proposed,''), summary_changed_at, COALESCE(notebook::text,''), COALESCE(uploads::text,'')`

func scanApproval(row pgx.Row) (Approval, error) {
	var a Approval
	var notebook, uploads string
	err := row.Scan(&a.ID, &a.ChatID, &a.Kind, &a.Via, &a.Name, &a.Size, &a.SHA256, &a.ContentType, &a.PendingKey, &a.Preview, &a.State, &a.CreatedAt, &a.DecidedAt, &a.DecidedBy, &a.Session, &a.ToolCallID, &a.Effect, &a.Summary, &a.SummaryProposed, &a.SummaryChangedAt, &notebook, &uploads)
	if errors.Is(err, pgx.ErrNoRows) {
		return a, ErrNotFound
	}
	if err != nil {
		return a, err
	}
	if notebook != "" {
		a.Notebook = json.RawMessage(notebook)
	}
	if uploads != "" {
		a.Uploads = json.RawMessage(uploads)
	}
	return a, nil
}

// CreateApproval inserts an approval. A caller that has to register state under the id before the row becomes
// visible (the manager keeps the recheck of a waiting call there, issue #154) sets a.ID; otherwise the database
// assigns one.
func (s *Store) CreateApproval(ctx context.Context, a Approval) (Approval, error) {
	return scanApproval(s.pool.QueryRow(ctx, `
INSERT INTO approvals (id, chat_id, kind, via, name, size, sha256, content_type, pending_key, preview, session, tool_call_id, effect, summary, notebook, uploads, upload_entries)
VALUES (COALESCE(NULLIF($1,'')::uuid, gen_random_uuid()),$2,$3,$4,$5,$6,$7,$8,$9,NULLIF($10,''),$11,$12,NULLIF($13,''),NULLIF($14,''),NULLIF($15,'')::jsonb,NULLIF($16,'')::jsonb,NULLIF($17,'')::jsonb) RETURNING `+approvalCols,
		a.ID, a.ChatID, a.Kind, a.Via, a.Name, a.Size, a.SHA256, a.ContentType, a.PendingKey, a.Preview, a.Session, a.ToolCallID, a.Effect, a.Summary, string(a.Notebook), string(a.Uploads), string(a.UploadEntries)))
}

func (s *Store) GetApproval(ctx context.Context, id string) (Approval, error) {
	if !isUUID(id) {
		return Approval{}, ErrNotFound
	}
	return scanApproval(s.pool.QueryRow(ctx, `SELECT `+approvalCols+` FROM approvals WHERE id=$1`, id))
}

// Deciders of an approval.
const (
	DecidedByUser       = "user"
	DecidedByExperiment = "experiment" // the gateway approved it in an experiment chat (X0.9)
)

// DecideApproval sets the state only while it is "pending". ok reports
// whether this decision took effect; otherwise the existing state is returned.
func (s *Store) DecideApproval(ctx context.Context, id, state string) (Approval, bool, error) {
	return s.DecideApprovalBy(ctx, id, state, DecidedByUser)
}

// DecideApprovalBy is DecideApproval with the decider recorded ("user" or "experiment"). The
// experiment path uses it, so that the record shows that no user decided (X0.9).
func (s *Store) DecideApprovalBy(ctx context.Context, id, state, by string) (Approval, bool, error) {
	a, err := scanApproval(s.pool.QueryRow(ctx, `UPDATE approvals SET state=$2, decided_at=now(), decided_by=$3 WHERE id=$1 AND state='pending' RETURNING `+approvalCols, id, state, by))
	if err == nil {
		return a, true, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return a, false, err
	}
	a, err = s.GetApproval(ctx, id)
	return a, false, err
}

// ReplaceApprovalSummary overwrites the gateway's summary of a pending approval with what a re-read at the moment of
// the approval found (issue L2/#119). The first proposal stays in summary_proposed (COALESCE), and
// summary_changed_at marks when the change was seen; an empty preview keeps the stored one. ok reports whether it
// took effect (the approval must still be pending); otherwise the existing approval is returned.
func (s *Store) ReplaceApprovalSummary(ctx context.Context, id, summary, preview string) (Approval, bool, error) {
	if !isUUID(id) {
		return Approval{}, false, ErrNotFound
	}
	a, err := scanApproval(s.pool.QueryRow(ctx, `UPDATE approvals
SET summary_proposed = COALESCE(summary_proposed, summary), summary = $2, preview = COALESCE(NULLIF($3,''), preview), summary_changed_at = now()
WHERE id=$1 AND state='pending' RETURNING `+approvalCols, id, summary, preview))
	if err == nil {
		return a, true, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return a, false, err
	}
	a, err = s.GetApproval(ctx, id)
	return a, false, err
}

func (s *Store) ListApprovals(ctx context.Context, state, chatID string) ([]Approval, error) {
	q := `SELECT ` + approvalCols + ` FROM approvals WHERE ($1 = '' OR state = $1) AND ($2 = '' OR chat_id::text = $2) ORDER BY created_at`
	rows, err := s.pool.Query(ctx, q, state, chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Approval{}
	for rows.Next() {
		a, err := scanApproval(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ExpirePendingApprovals sets all open approvals to "expired" at startup:
// the waiting calls no longer exist after a restart.
func (s *Store) ExpirePendingApprovals(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE approvals SET state='expired', decided_at=now() WHERE state='pending'`)
	return tag.RowsAffected(), err
}

// AddSocketCall stores a call; the old signature stays a thin wrapper (X5: CallRecord is the write type).
func (s *Store) AddSocketCall(ctx context.Context, c SocketCall) (SocketCall, error) {
	rec, err := s.AddSocketCallRecord(ctx, CallRecord{SlotID: c.SlotID, ChatID: c.ChatID, Via: c.Via, Op: c.Op, Detail: c.Detail,
		Result: c.Result, Session: c.Session, ToolCallID: c.ToolCallID, DurationMs: c.DurationMs, Effect: c.Effect,
		Decision: c.Decision, Reason: c.Reason, Rule: c.Rule, Body: c.Body, BodyTruncated: c.BodyTruncated,
		BodyInSHA256: c.BodyInSHA256, CallRef: c.CallRef, CallRefSource: c.CallRefSource, CallRefCandidates: c.CallRefCandidates,
		CallRefClaimed: c.CallRefClaimed, WithinDelegation: c.WithinDelegation, ApprovalID: c.ApprovalID,
		ApprovalDecision: c.ApprovalDecision, PlanStep: c.PlanStep})
	if err != nil {
		return c, err
	}
	c.ID, c.CreatedAt = rec.ID, rec.CreatedAt
	return c, nil
}

// AddSocketCallRecord stores one call with its structured fields.
func (s *Store) AddSocketCallRecord(ctx context.Context, c CallRecord) (SocketCall, error) {
	var chat any
	if c.ChatID != "" {
		chat = c.ChatID
	}
	var rule, candidates any
	if len(c.Rule) > 0 {
		rule = []byte(c.Rule)
	}
	if len(c.CallRefCandidates) > 0 {
		b, err := json.Marshal(c.CallRefCandidates)
		if err != nil {
			return SocketCall{}, err
		}
		candidates = b
	}
	var approval, planStep any
	if c.ApprovalID != "" {
		approval = c.ApprovalID
	}
	if c.PlanStep != nil {
		planStep = *c.PlanStep
	}
	out := SocketCall{SlotID: c.SlotID, ChatID: c.ChatID, Via: c.Via, Op: c.Op, Detail: c.Detail, Result: c.Result,
		Session: c.Session, ToolCallID: c.ToolCallID, DurationMs: c.DurationMs, Effect: c.Effect,
		Decision: c.Decision, Reason: c.Reason, Rule: c.Rule, Body: c.Body, BodyTruncated: c.BodyTruncated,
		BodyInSHA256: c.BodyInSHA256, CallRef: c.CallRef, CallRefSource: c.CallRefSource,
		CallRefCandidates: c.CallRefCandidates, CallRefClaimed: c.CallRefClaimed, WithinDelegation: c.WithinDelegation,
		ApprovalID: c.ApprovalID, ApprovalDecision: c.ApprovalDecision, PlanStep: c.PlanStep}
	err := s.pool.QueryRow(ctx, `INSERT INTO socket_calls (chat_id, slot_id, via, op, detail, result, session, tool_call_id,
duration_ms, effect, decision, reason, rule, body, body_truncated, body_in_sha256, call_ref, call_ref_source,
call_ref_candidates, call_ref_claimed, within_delegation, approval_id, approval_decision, plan_step)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,NULLIF($10,''),NULLIF($11,''),NULLIF($12,''),$13,$14,$15,NULLIF($16,''),
NULLIF($17,''),NULLIF($18,''),$19,NULLIF($20,''),$21,$22,NULLIF($23,''),$24) RETURNING id, created_at`,
		chat, c.SlotID, c.Via, c.Op, c.Detail, c.Result, c.Session, c.ToolCallID, c.DurationMs, c.Effect,
		c.Decision, c.Reason, rule, c.Body, c.BodyTruncated, c.BodyInSHA256, c.CallRef, c.CallRefSource,
		candidates, c.CallRefClaimed, c.WithinDelegation, approval, c.ApprovalDecision, planStep).Scan(&out.ID, &out.CreatedAt)
	return out, err
}

func (s *Store) ListSocketCalls(ctx context.Context, chatID string) ([]SocketCall, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, COALESCE(chat_id::text,''), slot_id, via, op, detail, result, created_at, session,
tool_call_id, duration_ms, COALESCE(effect,''), COALESCE(decision,''), COALESCE(reason,''), rule, body, body_truncated,
COALESCE(body_in_sha256,''), COALESCE(call_ref,''), COALESCE(call_ref_source,''), call_ref_candidates,
COALESCE(call_ref_claimed,''), within_delegation, COALESCE(approval_id::text,''), COALESCE(approval_decision,''), plan_step
FROM socket_calls WHERE chat_id=$1 ORDER BY id`, chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SocketCall{}
	for rows.Next() {
		var c SocketCall
		var rule, candidates []byte
		if err := rows.Scan(&c.ID, &c.ChatID, &c.SlotID, &c.Via, &c.Op, &c.Detail, &c.Result, &c.CreatedAt, &c.Session,
			&c.ToolCallID, &c.DurationMs, &c.Effect, &c.Decision, &c.Reason, &rule, &c.Body, &c.BodyTruncated,
			&c.BodyInSHA256, &c.CallRef, &c.CallRefSource, &candidates, &c.CallRefClaimed, &c.WithinDelegation,
			&c.ApprovalID, &c.ApprovalDecision, &c.PlanStep); err != nil {
			return nil, err
		}
		c.Rule = rule
		if len(candidates) > 0 {
			_ = json.Unmarshal(candidates, &c.CallRefCandidates)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Totals sums tokens and cost over all chats (per chat as in chatSelect).
func (s *Store) Totals(ctx context.Context) (Tokens, float64, error) {
	var t Tokens
	var cost float64
	err := s.pool.QueryRow(ctx, `
SELECT COALESCE(sum(v.input),0), COALESCE(sum(v.output),0), COALESCE(sum(v.cache_read),0), COALESCE(sum(v.total),0), COALESCE(sum(v.cost),0)
FROM (`+chatSelectTotals+`) v`).Scan(&t.Input, &t.Output, &t.CacheRead, &t.Total, &cost)
	return t, cost, err
}

const chatSelectTotals = `
SELECT
  CASE WHEN l.n > 0 THEN l.input ELSE COALESCE(u.input,0) END AS input,
  CASE WHEN l.n > 0 THEN l.output ELSE COALESCE(u.output,0) END AS output,
  CASE WHEN l.n > 0 THEN l.cache_read ELSE COALESCE(u.cache_read,0) END AS cache_read,
  CASE WHEN l.n > 0 THEN l.input + l.output + l.cache_read ELSE COALESCE(u.total,0) END AS total,
  CASE WHEN l.n > 0 THEN l.cost ELSE COALESCE(u.cost,0) END AS cost
FROM chats c
LEFT JOIN LATERAL (
  SELECT sum((m.message->'usage'->>'input')::bigint) AS input, sum((m.message->'usage'->>'output')::bigint) AS output,
         sum((m.message->'usage'->>'cacheRead')::bigint) AS cache_read, sum((m.message->'usage'->>'totalTokens')::bigint) AS total,
         sum(COALESCE(m.cost, (m.message->'usage'->'cost'->>'total')::float8)) AS cost
  FROM chat_messages m WHERE m.chat_id = c.id AND m.role IN ('assistant','compaction')
) u ON true
LEFT JOIN LATERAL (
  SELECT count(*) AS n, COALESCE(sum(x.input),0) AS input, COALESCE(sum(x.output),0) AS output,
         COALESCE(sum(x.cache_read),0) AS cache_read, COALESCE(sum(x.cost),0) AS cost
  FROM llm_calls x WHERE x.chat_id = c.id
) l ON true`

// LLMCall is a model call captured at the proxy.
type LLMCall struct {
	ID         int64           `json:"id"`
	ChatID     string          `json:"chat_id,omitempty"`
	SlotID     string          `json:"slot_id"`
	SourceIP   string          `json:"source_ip"`
	Model      string          `json:"model"`
	ResponseID string          `json:"response_id"`
	Status     int             `json:"status"`
	Input      int64           `json:"input"`
	Output     int64           `json:"output"`
	CacheRead  int64           `json:"cache_read"`
	CacheWrite int64           `json:"cache_write"`
	Cost       float64         `json:"cost"`
	Peak       bool            `json:"peak"`
	ToolCalls  json.RawMessage `json:"tool_calls"`
	StartedAt  time.Time       `json:"started_at"`
	DurationMs int64           `json:"duration_ms"`
	Main       bool            `json:"main"` // reply of the main session (responseId in the messages)
	// FinishReason and Complete: see llmproxy.Call (M1).
	FinishReason string `json:"finish_reason"`
	Complete     bool   `json:"complete"`
	// RequestBytes: size of the request body sent to the provider (issue #69); nil for older rows.
	RequestBytes *int64 `json:"request_bytes,omitempty"`
	// Temperature and Seed: the values the proxy actually sent for an experiment run (X3.6); nil = not sent,
	// because the model does not take the field. Older rows and product chats: nil.
	Temperature *float64 `json:"temperature,omitempty"`
	Seed        *int64   `json:"seed,omitempty"`
	// ModelReported: the model the provider reported in the response, with the system fingerprint where it
	// reports one (X3.7). Empty for older rows and for responses without either.
	ModelReported string `json:"model_reported,omitempty"`
	// ToolsSHA256: checksum of the canonical tool list of this call (X5.6); the list itself is in llm_toolsets.
	ToolsSHA256 string `json:"tools_sha256,omitempty"`
}

func (s *Store) AddLLMCall(ctx context.Context, c LLMCall) (LLMCall, error) {
	var chat any
	if c.ChatID != "" {
		chat = c.ChatID
	}
	if len(c.ToolCalls) == 0 {
		c.ToolCalls = json.RawMessage("[]")
	}
	err := s.pool.QueryRow(ctx, `
INSERT INTO llm_calls (chat_id, slot_id, source_ip, model, response_id, status, input, output, cache_read, cache_write, cost, peak, tool_calls, started_at, duration_ms, finish_reason, complete, request_bytes, temperature, seed, model_reported, tools_sha256)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,NULLIF($22,'')) RETURNING id`,
		chat, c.SlotID, c.SourceIP, c.Model, c.ResponseID, c.Status, c.Input, c.Output, c.CacheRead, c.CacheWrite, c.Cost, c.Peak, []byte(c.ToolCalls), c.StartedAt, c.DurationMs, noNUL(c.FinishReason), c.Complete, c.RequestBytes, c.Temperature, c.Seed, noNUL(c.ModelReported), c.ToolsSHA256).Scan(&c.ID)
	return c, err
}

// ChatLLMUsage sums what a chat's model calls cost and how many tokens they used (X3.2, X3.3): llm_calls is the
// authoritative source, subagent runs included. Tokens are input + output without the cache reads.
func (s *Store) ChatLLMUsage(ctx context.Context, chatID string) (cost float64, tokens int64, err error) {
	err = s.pool.QueryRow(ctx, `SELECT COALESCE(SUM(cost),0), COALESCE(SUM(input + output),0) FROM llm_calls WHERE chat_id=$1`, chatID).Scan(&cost, &tokens)
	return cost, tokens, err
}

// PutToolset stores a tool list once, keyed by its checksum (X5.6).
func (s *Store) PutToolset(ctx context.Context, sha string, tools json.RawMessage) error {
	if sha == "" || len(tools) == 0 {
		return nil
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO llm_toolsets (sha256, tools) VALUES ($1,$2) ON CONFLICT (sha256) DO NOTHING`, sha, []byte(tools))
	return err
}

// Toolset reads a stored tool list.
func (s *Store) Toolset(ctx context.Context, sha string) (json.RawMessage, error) {
	var b []byte
	err := s.pool.QueryRow(ctx, `SELECT tools FROM llm_toolsets WHERE sha256=$1`, sha).Scan(&b)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return b, err
}

func (s *Store) ListLLMCalls(ctx context.Context, chatID string) ([]LLMCall, error) {
	rows, err := s.pool.Query(ctx, `
SELECT x.id, x.slot_id, x.source_ip, x.model, x.response_id, x.status, x.input, x.output, x.cache_read, x.cache_write, x.cost, x.peak, x.tool_calls, x.started_at, x.duration_ms,
  (x.response_id <> '' AND EXISTS (SELECT 1 FROM chat_messages m WHERE m.chat_id = x.chat_id AND m.role = 'assistant' AND m.message->>'responseId' = x.response_id)),
  x.finish_reason, x.complete, x.request_bytes, x.temperature, x.seed, x.model_reported, COALESCE(x.tools_sha256,'')
FROM llm_calls x WHERE x.chat_id = $1 ORDER BY x.id`, chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LLMCall{}
	for rows.Next() {
		var c LLMCall
		var tc []byte
		if err := rows.Scan(&c.ID, &c.SlotID, &c.SourceIP, &c.Model, &c.ResponseID, &c.Status, &c.Input, &c.Output,
			&c.CacheRead, &c.CacheWrite, &c.Cost, &c.Peak, &tc, &c.StartedAt, &c.DurationMs, &c.Main, &c.FinishReason,
			&c.Complete, &c.RequestBytes, &c.Temperature, &c.Seed, &c.ModelReported, &c.ToolsSHA256); err != nil {
			return nil, err
		}
		c.ChatID, c.ToolCalls = chatID, tc
		out = append(out, c)
	}
	return out, rows.Err()
}

// ToolRejections returns, per toolCallId, the error message of the tool results with isError from
// the main session and the subagents' sessions (M1). The source is pi's session files,
// hence not tamper-proof: only a hint why a requested call was never executed
// (such as invalid arguments or a hidden tool).
func (s *Store) ToolRejections(ctx context.Context, chatID string) (map[string]string, error) {
	rows, err := s.pool.Query(ctx, `
SELECT m.message->>'toolCallId', COALESCE((SELECT string_agg(p->>'text', ' ') FROM jsonb_array_elements(
         CASE WHEN jsonb_typeof(m.message->'content') = 'array' THEN m.message->'content' ELSE '[]'::jsonb END) p), '')
FROM chat_messages m
WHERE m.chat_id = $1 AND m.role = 'toolResult' AND m.message->>'isError' = 'true' AND COALESCE(m.message->>'toolCallId', '') <> ''
UNION ALL
SELECT e.payload->>'tool_call_id', COALESCE(e.payload->>'text', '')
FROM subagent_entries e
WHERE e.chat_id = $1 AND e.kind = 'tool_result' AND e.payload->>'is_error' = 'true' AND COALESCE(e.payload->>'tool_call_id', '') <> ''`, chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, text string
		if err := rows.Scan(&id, &text); err != nil {
			return nil, err
		}
		if len(text) > 300 {
			text = strings.ToValidUTF8(text[:300], "") + " …"
		}
		out[id] = text
	}
	return out, rows.Err()
}

// SubagentEntry is an entry from a subagent's session file.
type SubagentEntry struct {
	ChatID     string          `json:"chat_id"`
	RunID      string          `json:"run_id"`
	EntryID    string          `json:"entry_id"`
	Agent      string          `json:"agent"`
	Kind       string          `json:"kind"`
	Payload    json.RawMessage `json:"payload"`
	ResponseID string          `json:"response_id,omitempty"`
	Confirmed  bool            `json:"confirmed"` // reply recorded at the proxy
	CreatedAt  time.Time       `json:"created_at"`
}

// AddSubagentEntries stores new entries and returns only the new ones.
func (s *Store) AddSubagentEntries(ctx context.Context, es []SubagentEntry) ([]SubagentEntry, error) {
	var out []SubagentEntry
	for _, e := range es {
		err := s.pool.QueryRow(ctx, `
INSERT INTO subagent_entries (chat_id, run_id, entry_id, agent, kind, payload, response_id) VALUES ($1,$2,$3,$4,$5,$6,$7)
ON CONFLICT DO NOTHING RETURNING created_at,
  ($7 <> '' AND EXISTS (SELECT 1 FROM llm_calls x WHERE x.chat_id = $1 AND x.response_id = $7))`,
			e.ChatID, e.RunID, e.EntryID, e.Agent, e.Kind, []byte(e.Payload), e.ResponseID).Scan(&e.CreatedAt, &e.Confirmed)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return out, err
		}
		out = append(out, e)
	}
	return out, nil
}

func (s *Store) ListSubagentEntries(ctx context.Context, chatID string) ([]SubagentEntry, error) {
	rows, err := s.pool.Query(ctx, `
SELECT e.run_id, e.entry_id, e.agent, e.kind, e.payload, e.response_id, e.created_at,
  (e.response_id <> '' AND EXISTS (SELECT 1 FROM llm_calls x WHERE x.chat_id = e.chat_id AND x.response_id = e.response_id))
FROM subagent_entries e WHERE e.chat_id = $1 ORDER BY e.created_at, e.run_id`, chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SubagentEntry{}
	for rows.Next() {
		e := SubagentEntry{ChatID: chatID}
		var p []byte
		if err := rows.Scan(&e.RunID, &e.EntryID, &e.Agent, &e.Kind, &p, &e.ResponseID, &e.CreatedAt, &e.Confirmed); err != nil {
			return nil, err
		}
		e.Payload = p
		out = append(out, e)
	}
	return out, rows.Err()
}

// SubagentRun: name and state of a run (ID as in subagent_entries.run_id).
type SubagentRun struct {
	ChatID      string `json:"chat_id"`
	RunID       string `json:"run_id"`
	Agent       string `json:"agent"`
	Label       string `json:"label,omitempty"`
	State       string `json:"state,omitempty"`
	PiRunID     string `json:"pi_run_id,omitempty"`
	ParentRunID string `json:"parent_run_id,omitempty"`
	// ContinuesRunID is the run this run continues after a message of the user (issue #81); set by SetRunContinues only.
	ContinuesRunID string     `json:"continues_run_id,omitempty"`
	StartedAt      *time.Time `json:"started_at,omitempty"`
	EndedAt        *time.Time `json:"ended_at,omitempty"`
	UpdatedAt      time.Time  `json:"updated_at"`
	// StoppedBy: who stopped this run (issue #120, L3): "user" or "limit" (subagent limit). Empty when the run was
	// not stopped, or when it ended in a way that records no reason (the chat's abort, the trash). Set by the
	// gateway only; UpsertSubagentRun never changes it.
	StoppedBy string `json:"stopped_by,omitempty"`
	// MessageState: what a message to the run would meet now (issue #82). Set by the chat layer when the run is
	// served or published, never stored.
	MessageState string `json:"message_state,omitempty"`
}

// UpsertSubagentRun stores name and state of a run; empty fields overwrite nothing.
func (s *Store) UpsertSubagentRun(ctx context.Context, r SubagentRun) (SubagentRun, error) {
	err := s.pool.QueryRow(ctx, `
INSERT INTO subagent_runs (chat_id, run_id, agent, label, state, pi_run_id, parent_run_id, started_at, ended_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
ON CONFLICT (chat_id, run_id) DO UPDATE SET
  agent = COALESCE(NULLIF(EXCLUDED.agent,''), subagent_runs.agent),
  label = COALESCE(NULLIF(EXCLUDED.label,''), subagent_runs.label),
  state = COALESCE(NULLIF(EXCLUDED.state,''), subagent_runs.state),
  pi_run_id = COALESCE(NULLIF(EXCLUDED.pi_run_id,''), subagent_runs.pi_run_id),
  parent_run_id = COALESCE(NULLIF(EXCLUDED.parent_run_id,''), subagent_runs.parent_run_id),
  started_at = COALESCE(EXCLUDED.started_at, subagent_runs.started_at),
  ended_at = COALESCE(EXCLUDED.ended_at, subagent_runs.ended_at),
  updated_at = now()
RETURNING agent, label, state, pi_run_id, parent_run_id, started_at, ended_at, updated_at, stopped_by`,
		r.ChatID, r.RunID, r.Agent, r.Label, r.State, r.PiRunID, r.ParentRunID, r.StartedAt, r.EndedAt,
	).Scan(&r.Agent, &r.Label, &r.State, &r.PiRunID, &r.ParentRunID, &r.StartedAt, &r.EndedAt, &r.UpdatedAt, &r.StoppedBy)
	return r, err
}

func (s *Store) ListSubagentRuns(ctx context.Context, chatID string) ([]SubagentRun, error) {
	rows, err := s.pool.Query(ctx, `SELECT run_id, agent, label, state, pi_run_id, parent_run_id, continues_run_id, started_at, ended_at, updated_at, stopped_by
FROM subagent_runs WHERE chat_id=$1 ORDER BY started_at NULLS LAST, run_id`, chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SubagentRun{}
	for rows.Next() {
		r := SubagentRun{ChatID: chatID}
		if err := rows.Scan(&r.RunID, &r.Agent, &r.Label, &r.State, &r.PiRunID, &r.ParentRunID, &r.ContinuesRunID, &r.StartedAt, &r.EndedAt, &r.UpdatedAt, &r.StoppedBy); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SetSubagentStoppedBy records who stopped a run: "user" over the stop route, "limit" for the runs the orchestrator
// ended when too many ran at the same time (issue #120, L3). The first writer wins ("user" before a later "limit",
// and the other way round): a run stopped once says by whom. Only the gateway calls it.
func (s *Store) SetSubagentStoppedBy(ctx context.Context, chatID, runID, by string) error {
	if !isUUID(chatID) {
		return ErrNotFound
	}
	if by == "" {
		return errors.New("stopped_by must not be empty")
	}
	_, err := s.pool.Exec(ctx, `UPDATE subagent_runs SET stopped_by=$3, updated_at=now()
WHERE chat_id=$1 AND run_id=$2 AND stopped_by=''`, chatID, runID, by)
	return err
}

// SetTurnAbortReason records why a turn was aborted (issue #120, L3): user, trash, time_limit, turn_limit or
// subagent_limit. The first reason wins; a later call for the same turn changes nothing. Only the gateway calls it.
func (s *Store) SetTurnAbortReason(ctx context.Context, turnID int64, reason string) error {
	if turnID == 0 || reason == "" {
		return nil
	}
	_, err := s.pool.Exec(ctx, `UPDATE chat_turns SET abort_reason=$2 WHERE id=$1 AND abort_reason IS NULL`, turnID, reason)
	return err
}

// SubagentRunSummary is one line of the short list of a chat's subagent runs (GET /api/chats/{id}/subagent-runs,
// issue #60): enough for a title and a state dot without loading the chat's entries. Runs come from subagent_runs
// and subagent_entries together, so runs of older chats without a status row appear as well.
type SubagentRunSummary struct {
	RunID string `json:"run_id"`
	Agent string `json:"agent"`
	Label string `json:"label,omitempty"`
	State string `json:"state,omitempty"`
	// TaskHead is the beginning of the run's first task (at most TaskHeadLines lines and TaskHeadBytes bytes), so the
	// UI can build the title with the same rule as for an open chat, which skips headings and tags.
	TaskHead  string     `json:"task_head,omitempty"`
	StartedAt *time.Time `json:"started_at,omitempty"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
	// LastAt and LastKind describe the newest entry; the UI estimates the state from them when State is empty.
	LastAt   *time.Time `json:"last_at,omitempty"`
	LastKind string     `json:"last_kind,omitempty"`
	Entries  int        `json:"entries"`
	// ContinuesRunID: the run this one continues after a message of the user (issue #81).
	ContinuesRunID string `json:"continues_run_id,omitempty"`
	// StoppedBy: who stopped the run (issue #120, L3): "user" or "limit". The history needs it to tell "stopped
	// by you" from "ended" without opening the chat.
	StoppedBy string `json:"stopped_by,omitempty"`
	// MessageState: what a message to the run would meet now (ok, starting, foreground, ended, unavailable; issue #82).
	MessageState string `json:"message_state,omitempty"`
}

// Limits of SubagentRunSummary.TaskHead.
const (
	TaskHeadLines = 8
	TaskHeadBytes = 500
)

// taskHead keeps the first TaskHeadLines lines of a task, at most TaskHeadBytes bytes, cut at a character boundary.
func taskHead(task string) string {
	lines := strings.SplitN(task, "\n", TaskHeadLines+1)
	if len(lines) > TaskHeadLines {
		lines = lines[:TaskHeadLines]
	}
	s := strings.Join(lines, "\n")
	if len(s) > TaskHeadBytes {
		s = s[:TaskHeadBytes]
	}
	return strings.TrimRight(strings.ToValidUTF8(s, ""), " \t\r\n")
}

// ListSubagentRunSummaries returns the short list of a chat's runs in the order they started. It reads the database
// only; it never touches the chat's slot.
func (s *Store) ListSubagentRunSummaries(ctx context.Context, chatID string) ([]SubagentRunSummary, error) {
	rows, err := s.pool.Query(ctx, `
WITH e AS (
  SELECT run_id, count(*) AS n, min(created_at) AS first_at, max(created_at) AS last_at,
         (array_agg(agent ORDER BY created_at) FILTER (WHERE agent <> ''))[1] AS agent,
         (array_agg(left(payload->>'text', 2000) ORDER BY created_at) FILTER (WHERE kind = 'task'))[1] AS task,
         (array_agg(kind ORDER BY created_at DESC, entry_id DESC))[1] AS last_kind
  FROM subagent_entries WHERE chat_id = $1 GROUP BY run_id
), r AS (SELECT * FROM subagent_runs WHERE chat_id = $1)
SELECT COALESCE(r.run_id, e.run_id), COALESCE(NULLIF(r.agent, ''), e.agent, ''), COALESCE(r.label, ''),
       COALESCE(r.state, ''), COALESCE(e.task, ''), LEAST(r.started_at, e.first_at), r.ended_at,
       GREATEST(e.last_at, r.ended_at), COALESCE(e.last_kind, ''), COALESCE(e.n, 0), COALESCE(r.continues_run_id, ''),
       COALESCE(r.stopped_by, '')
FROM r FULL OUTER JOIN e ON e.run_id = r.run_id
WHERE e.run_id IS NOT NULL OR r.started_at IS NOT NULL
ORDER BY 6 NULLS LAST, 1`, chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SubagentRunSummary{}
	for rows.Next() {
		var r SubagentRunSummary
		var task string
		if err := rows.Scan(&r.RunID, &r.Agent, &r.Label, &r.State, &task, &r.StartedAt, &r.EndedAt, &r.LastAt, &r.LastKind, &r.Entries, &r.ContinuesRunID, &r.StoppedBy); err != nil {
			return nil, err
		}
		r.TaskHead = taskHead(task)
		out = append(out, r)
	}
	return out, rows.Err()
}

// ChatImage is a display image of a reply (not an artifact), saved in S3.
type ChatImage struct {
	ChatID      string    `json:"chat_id"`
	Msg         string    `json:"msg"`
	Path        string    `json:"path"`
	ObjectKey   string    `json:"-"`
	ContentType string    `json:"content_type"`
	Size        int64     `json:"size"`
	SHA256      string    `json:"sha256"`
	CreatedAt   time.Time `json:"created_at"`
}

// PutChatImage creates the entry; an existing one stays (the first save applies).
func (s *Store) PutChatImage(ctx context.Context, im ChatImage) error {
	_, err := s.pool.Exec(ctx, `
INSERT INTO chat_images (chat_id, msg, path, object_key, content_type, size, sha256)
VALUES ($1,$2,$3,$4,$5,$6,$7) ON CONFLICT (chat_id, msg, path) DO NOTHING`,
		im.ChatID, im.Msg, im.Path, im.ObjectKey, im.ContentType, im.Size, im.SHA256)
	return err
}

func (s *Store) GetChatImage(ctx context.Context, chatID, msg, path string) (ChatImage, error) {
	im := ChatImage{ChatID: chatID, Msg: msg, Path: path}
	if !isUUID(chatID) {
		return im, ErrNotFound
	}
	err := s.pool.QueryRow(ctx, `SELECT object_key, content_type, size, sha256, created_at FROM chat_images
WHERE chat_id=$1 AND msg=$2 AND path=$3`, chatID, msg, path).Scan(&im.ObjectKey, &im.ContentType, &im.Size, &im.SHA256, &im.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return im, ErrNotFound
	}
	return im, err
}

// WorkspaceInfo describes the last backup of a chat's workspace
// (API field workspace). SavedAt nil: never backed up. Skipped*: the last
// backup was skipped (e.g. above the size limit); the previous one remains valid.
type WorkspaceInfo struct {
	Size          int64      `json:"size"`         // sum of the file sizes
	ArchiveSize   int64      `json:"archive_size"` // size of the tar.gz
	Files         int        `json:"files"`
	SHA256        string     `json:"sha256,omitempty"`
	SavedAt       *time.Time `json:"saved_at,omitempty"`
	SkippedReason *string    `json:"skipped_reason,omitempty"`
	SkippedSize   *int64     `json:"skipped_size,omitempty"`
	SkippedAt     *time.Time `json:"skipped_at,omitempty"`
}

// Workspace is the full entry (with storage location and fingerprint).
type Workspace struct {
	WorkspaceInfo
	ChatID      string
	ObjectKey   string // "" = never backed up
	Fingerprint string
}

// GetWorkspace returns the chat's entry or ErrNotFound.
func (s *Store) GetWorkspace(ctx context.Context, chatID string) (Workspace, error) {
	w := Workspace{ChatID: chatID}
	if !isUUID(chatID) {
		return w, ErrNotFound
	}
	var key *string
	err := s.pool.QueryRow(ctx, `SELECT object_key, size, archive_size, files, sha256, fingerprint, saved_at, skipped_reason, skipped_size, skipped_at
FROM chat_workspaces WHERE chat_id=$1`, chatID).Scan(&key, &w.Size, &w.ArchiveSize, &w.Files, &w.SHA256, &w.Fingerprint, &w.SavedAt, &w.SkippedReason, &w.SkippedSize, &w.SkippedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return w, ErrNotFound
	}
	if key != nil {
		w.ObjectKey = *key
	}
	return w, err
}

// PutWorkspace records a successful backup and clears a skip note.
func (s *Store) PutWorkspace(ctx context.Context, w Workspace) error {
	_, err := s.pool.Exec(ctx, `
INSERT INTO chat_workspaces (chat_id, object_key, size, archive_size, files, sha256, fingerprint, saved_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,now())
ON CONFLICT (chat_id) DO UPDATE SET object_key=EXCLUDED.object_key, size=EXCLUDED.size, archive_size=EXCLUDED.archive_size,
  files=EXCLUDED.files, sha256=EXCLUDED.sha256, fingerprint=EXCLUDED.fingerprint, saved_at=now(),
  skipped_reason=NULL, skipped_size=NULL, skipped_at=NULL`,
		w.ChatID, w.ObjectKey, w.Size, w.ArchiveSize, w.Files, w.SHA256, w.Fingerprint)
	return err
}

// SkipWorkspace notes a skipped backup; an existing one remains valid.
func (s *Store) SkipWorkspace(ctx context.Context, chatID, reason string, size int64) error {
	_, err := s.pool.Exec(ctx, `
INSERT INTO chat_workspaces (chat_id, skipped_reason, skipped_size, skipped_at) VALUES ($1,$2,$3,now())
ON CONFLICT (chat_id) DO UPDATE SET skipped_reason=EXCLUDED.skipped_reason, skipped_size=EXCLUDED.skipped_size, skipped_at=now()`,
		chatID, reason, size)
	return err
}

// ClearWorkspaceSkip withdraws the skip note (workspace unchanged since the backup).
func (s *Store) ClearWorkspaceSkip(ctx context.Context, chatID string) error {
	_, err := s.pool.Exec(ctx, `UPDATE chat_workspaces SET skipped_reason=NULL, skipped_size=NULL, skipped_at=NULL WHERE chat_id=$1`, chatID)
	return err
}

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func isUUID(s string) bool { return uuidRe.MatchString(s) }

// ToolExecution is an operation the orchestrator executed for a tool in
// the execution sandbox (E9). Proven, because it executed and recorded it
// itself.
type ToolExecution struct {
	ID            int64           `json:"id"`
	ChatID        string          `json:"chat_id"`
	SlotID        string          `json:"slot_id"`
	Session       string          `json:"session"` // "main" or ID of the subagent run
	ToolCallID    string          `json:"tool_call_id"`
	Tool          string          `json:"tool"`
	Op            string          `json:"op"`
	Args          json.RawMessage `json:"args"`
	ExitCode      *int            `json:"exit_code,omitempty"`
	Error         string          `json:"error,omitempty"`
	OutputExcerpt string          `json:"output_excerpt,omitempty"`
	OutputSHA256  string          `json:"output_sha256,omitempty"`
	OutputBytes   int64           `json:"output_bytes"`
	StartedAt     time.Time       `json:"started_at"`
	DurationMs    int64           `json:"duration_ms"`
	// X5.4: truncated commands say so and keep length and checksum of the untruncated command.
	ArgsTruncated bool   `json:"args_truncated,omitempty"`
	CommandBytes  *int64 `json:"command_bytes,omitempty"`
	CommandSHA256 string `json:"command_sha256,omitempty"`
	// X6: failed syscalls of this operation (strace lines). They are parsed into exec_syscalls
	// and are not part of the API response.
	Trace          []byte `json:"-"`
	TraceTruncated bool   `json:"-"`
}

// AddToolExecution stores an execution. Postgres accepts no NUL in text
// and no \u0000 in jsonb (SQLSTATE 22021/22P05); both occur in
// binary output (printf '\0', read of a PNG file) and are therefore replaced by
// U+2400 (␀). If the insert still fails, a fallback row without excerpt and
// arguments is written: a missing row would wrongly show up in the reconciliation
// as "not executed" (code review K1).
func (s *Store) AddToolExecution(ctx context.Context, e ToolExecution) (ToolExecution, error) {
	if len(e.Args) == 0 {
		e.Args = json.RawMessage("{}")
	}
	if e.Session == "" {
		e.Session = "main"
	}
	e.Session, e.ToolCallID, e.Tool, e.Op = noNUL(e.Session), noNUL(e.ToolCallID), noNUL(e.Tool), noNUL(e.Op)
	e.Error, e.OutputExcerpt, e.Args = noNUL(e.Error), noNUL(e.OutputExcerpt), jsonNoNUL(e.Args)
	id, err := s.insertToolExecution(ctx, e)
	if err == nil {
		e.ID = id
		return e, nil
	}
	fb := e
	fb.Args, fb.OutputExcerpt = json.RawMessage("{}"), ""
	fb.Trace, fb.TraceTruncated = nil, false // if the trace was the cause, the execution still has to be stored
	fb.Error = strings.TrimSpace(fb.Error + " [entry not stored completely: " + noNUL(err.Error()) + "]")
	id, err2 := s.insertToolExecution(ctx, fb)
	if err2 != nil {
		return e, fmt.Errorf("%w (fallback row: %v)", err, err2)
	}
	fb.ID = id
	return fb, nil
}

// insertToolExecution stores the execution and, in the same transaction, the parsed syscalls
// of its trace (X6): a trace without its execution row (or the other way round) would break the
// assignment in the export.
func (s *Store) insertToolExecution(ctx context.Context, e ToolExecution) (int64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var chat any
	if e.ChatID != "" {
		chat = e.ChatID
	}
	var id int64
	if err := tx.QueryRow(ctx, `
INSERT INTO tool_executions (chat_id, slot_id, session, tool_call_id, tool, op, args, exit_code, error, output_excerpt,
output_sha256, output_bytes, started_at, duration_ms, args_truncated, command_bytes, command_sha256)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,NULLIF($17,'')) RETURNING id`,
		chat, e.SlotID, e.Session, e.ToolCallID, e.Tool, e.Op, []byte(e.Args), e.ExitCode, e.Error, e.OutputExcerpt,
		e.OutputSHA256, e.OutputBytes, e.StartedAt, e.DurationMs, e.ArgsTruncated, e.CommandBytes, e.CommandSHA256).Scan(&id); err != nil {
		return 0, err
	}
	if err := insertExecSyscalls(ctx, tx, e, id); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return id, nil
}

// noNUL makes a text storable: NUL becomes U+2400, invalid UTF-8 becomes U+FFFD.
func noNUL(s string) string {
	return strings.ToValidUTF8(strings.ReplaceAll(s, "\x00", "␀"), "�")
}

// jsonNoNUL replaces NUL in all strings of a JSON value. Invalid JSON stays
// unchanged (the insert then fails and becomes the fallback row).
func jsonNoNUL(raw json.RawMessage) json.RawMessage {
	if !strings.Contains(string(raw), `\u0000`) && !strings.ContainsRune(string(raw), 0) {
		return raw
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return raw
	}
	var walk func(any) any
	walk = func(v any) any {
		switch x := v.(type) {
		case string:
			return noNUL(x)
		case map[string]any:
			out := make(map[string]any, len(x))
			for k, val := range x {
				out[noNUL(k)] = walk(val)
			}
			return out
		case []any:
			for i := range x {
				x[i] = walk(x[i])
			}
			return x
		}
		return v
	}
	b, err := json.Marshal(walk(v))
	if err != nil {
		return raw
	}
	return b
}

func (s *Store) ListToolExecutions(ctx context.Context, chatID string) ([]ToolExecution, error) {
	rows, err := s.pool.Query(ctx, `
SELECT id, slot_id, session, tool_call_id, tool, op, args, exit_code, error, output_excerpt, output_sha256, output_bytes,
started_at, duration_ms, args_truncated, command_bytes, COALESCE(command_sha256,'')
FROM tool_executions WHERE chat_id = $1 ORDER BY id`, chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ToolExecution{}
	for rows.Next() {
		e := ToolExecution{ChatID: chatID}
		var args []byte
		if err := rows.Scan(&e.ID, &e.SlotID, &e.Session, &e.ToolCallID, &e.Tool, &e.Op, &args, &e.ExitCode, &e.Error,
			&e.OutputExcerpt, &e.OutputSHA256, &e.OutputBytes, &e.StartedAt, &e.DurationMs, &e.ArgsTruncated,
			&e.CommandBytes, &e.CommandSHA256); err != nil {
			return nil, err
		}
		e.Args = args
		out = append(out, e)
	}
	return out, rows.Err()
}

// AddDelegationObject records that an object was created within this chat's delegation
// (provenance rule). Duplicate entries have no effect.
func (s *Store) AddDelegationObject(ctx context.Context, chatID, resource, id string) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO delegation_objects (chat_id, resource, object_id) VALUES ($1,$2,$3) ON CONFLICT DO NOTHING`, chatID, resource, id)
	return err
}

// IsDelegationObject tells whether an object was created within this chat's delegation.
func (s *Store) IsDelegationObject(ctx context.Context, chatID, resource, id string) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM delegation_objects WHERE chat_id = $1 AND resource = $2 AND object_id = $3)`, chatID, resource, id).Scan(&ok)
	return ok, err
}

// DelegationObject is an entry in the provenance register.
type DelegationObject struct {
	Resource  string    `json:"resource"`
	ObjectID  string    `json:"object_id"`
	CreatedAt time.Time `json:"created_at"`
}

// ListDelegationObjects returns a chat's provenance register.
func (s *Store) ListDelegationObjects(ctx context.Context, chatID string) ([]DelegationObject, error) {
	rows, err := s.pool.Query(ctx, `SELECT resource, object_id, created_at FROM delegation_objects WHERE chat_id = $1 ORDER BY created_at, resource, object_id`, chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DelegationObject{}
	for rows.Next() {
		var o DelegationObject
		if err := rows.Scan(&o.Resource, &o.ObjectID, &o.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

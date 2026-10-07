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
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schemaSQL string

var ErrNotFound = errors.New("not found")

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
	// Owner: sub of the user who owns the chat (login through the platform); empty in token mode.
	Owner string `json:"owner,omitempty"`
	// Language: the user's preferred language according to the browser (BCP 47); empty: unknown.
	Language string `json:"language,omitempty"`
}

// Origin of the title: TitleDefault (placeholder title, replaced with the first question),
// TitleAuto (from the first question), TitleUser (set by the user, also with /rename).
const (
	TitleDefault = "default"
	TitleAuto    = "auto"
	TitleUser    = "user"
	TitleModel   = "model" // phrased by the model (once, after the first question)
)

type NewChat struct {
	Title, Model, Variant string
	TitleSource           string // empty: TitleUser
	Internet              bool
	AutoCompact           bool
	MaxSubagents          int
	Delegation            json.RawMessage // nil: without delegation
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
	// ToolCallID: tool call that uploaded the artifact (outputs only; empty for older
	// artifacts). Serves solely to show it at the right place in the history; with the CLI the
	// ID comes from the command's environment, so the agent could change it.
	ToolCallID string `json:"tool_call_id,omitempty"`
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
	// Session and ToolCallID: who asked (main agent "main" or subagent run) and in which
	// tool call; empty for older entries.
	Session    string `json:"session,omitempty"`
	ToolCallID string `json:"tool_call_id,omitempty"`
	// DurationMs: round trip of a platform call in milliseconds (nil: not measured).
	DurationMs *float64 `json:"duration_ms,omitempty"`
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
}

type Store struct {
	pool   *pgxpool.Pool
	schema string
}

func Open(ctx context.Context, url string) (*Store, error) { return OpenSchema(ctx, url, "") }

var schemaName = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// OpenSchema opens the database and creates the schema. With schema != ""
// the store works in its own Postgres schema (for tests).
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
		if _, err := p.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+schema); err != nil {
			p.Close()
			return nil, err
		}
	}
	if _, err := p.Exec(ctx, schemaSQL); err != nil {
		p.Close()
		return nil, fmt.Errorf("creating schema: %w", err)
	}
	return &Store{pool: p, schema: schema}, nil
}

func (s *Store) Close() { s.pool.Close() }

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
  c.delegation, COALESCE(c.owner, ''), COALESCE(c.language, '')
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
	var ctxRaw, wsRaw, delRaw []byte
	err := row.Scan(&c.ID, &c.Title, &c.Model, &c.ThinkingLevel, &c.Variant, &c.State, &c.Internet, &c.AutoCompact, &c.MaxSubagents, &c.Compactions, &ctxRaw, &c.CreatedAt, &c.UpdatedAt,
		&c.Tokens.Input, &c.Tokens.Output, &c.Tokens.CacheRead, &c.Tokens.Total, &c.Cost, &c.ArtifactCount, &c.PendingApprovals,
		&c.Subagents, &c.LLMCalls, &c.CostOther, &wsRaw, &c.Queued, &c.BackgroundRunning, &delRaw, &c.Owner, &c.Language)
	if len(delRaw) > 0 {
		c.Delegation = delRaw
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
	err := s.pool.QueryRow(ctx, `INSERT INTO chats (title, title_source, model, variant, internet, auto_compact, max_subagents, delegation, owner, language) VALUES ($1,$2,$3,$4,$5,$6,$7,$8::jsonb,$9,$10) RETURNING id::text`,
		n.Title, src, n.Model, n.Variant, n.Internet, n.AutoCompact, n.MaxSubagents, del, owner, lang).Scan(&id)
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

func (s *Store) ListChats(ctx context.Context) ([]Chat, error) {
	rows, err := s.pool.Query(ctx, chatSelect+` ORDER BY c.updated_at DESC`)
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

func (s *Store) Messages(ctx context.Context, chatID string) ([]Message, error) {
	rows, err := s.pool.Query(ctx, `SELECT seq, role, message, cost, peak, created_at, turn_id, COALESCE(trigger,''), COALESCE(origin,''), sources
FROM chat_messages WHERE chat_id=$1 ORDER BY seq`, chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Message{}
	for rows.Next() {
		var m Message
		var raw, src []byte
		if err := rows.Scan(&m.Seq, &m.Role, &raw, &m.Cost, &m.Peak, &m.CreatedAt, &m.TurnID, &m.Trigger, &m.Origin, &src); err != nil {
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
	_, err := s.pool.Exec(ctx, `
INSERT INTO artifacts (chat_id, kind, name, size, sha256, content_type, via, object_key, tool_call_id)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
ON CONFLICT (chat_id, kind, name) DO UPDATE SET size=EXCLUDED.size, sha256=EXCLUDED.sha256,
  content_type=EXCLUDED.content_type, via=EXCLUDED.via, object_key=EXCLUDED.object_key,
  tool_call_id=EXCLUDED.tool_call_id, created_at=now()`,
		a.ChatID, a.Kind, a.Name, a.Size, a.SHA256, a.ContentType, a.Via, a.ObjectKey, a.ToolCallID)
	return err
}

const artifactCols = `chat_id::text, kind, name, size, sha256, content_type, via, object_key, created_at, tool_call_id`

func scanArtifact(row pgx.Row) (Artifact, error) {
	var a Artifact
	err := row.Scan(&a.ChatID, &a.Kind, &a.Name, &a.Size, &a.SHA256, &a.ContentType, &a.Via, &a.ObjectKey, &a.CreatedAt, &a.ToolCallID)
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

const approvalCols = `id::text, chat_id::text, kind, via, name, size, sha256, content_type, pending_key, COALESCE(preview,''), state, created_at, decided_at, session, tool_call_id`

func scanApproval(row pgx.Row) (Approval, error) {
	var a Approval
	err := row.Scan(&a.ID, &a.ChatID, &a.Kind, &a.Via, &a.Name, &a.Size, &a.SHA256, &a.ContentType, &a.PendingKey, &a.Preview, &a.State, &a.CreatedAt, &a.DecidedAt, &a.Session, &a.ToolCallID)
	if errors.Is(err, pgx.ErrNoRows) {
		return a, ErrNotFound
	}
	return a, err
}

func (s *Store) CreateApproval(ctx context.Context, a Approval) (Approval, error) {
	return scanApproval(s.pool.QueryRow(ctx, `
INSERT INTO approvals (chat_id, kind, via, name, size, sha256, content_type, pending_key, preview, session, tool_call_id)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,NULLIF($9,''),$10,$11) RETURNING `+approvalCols,
		a.ChatID, a.Kind, a.Via, a.Name, a.Size, a.SHA256, a.ContentType, a.PendingKey, a.Preview, a.Session, a.ToolCallID))
}

func (s *Store) GetApproval(ctx context.Context, id string) (Approval, error) {
	if !isUUID(id) {
		return Approval{}, ErrNotFound
	}
	return scanApproval(s.pool.QueryRow(ctx, `SELECT `+approvalCols+` FROM approvals WHERE id=$1`, id))
}

// DecideApproval sets the state only while it is "pending". ok reports
// whether this decision took effect; otherwise the existing state is returned.
func (s *Store) DecideApproval(ctx context.Context, id, state string) (Approval, bool, error) {
	a, err := scanApproval(s.pool.QueryRow(ctx, `UPDATE approvals SET state=$2, decided_at=now() WHERE id=$1 AND state='pending' RETURNING `+approvalCols, id, state))
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

func (s *Store) AddSocketCall(ctx context.Context, c SocketCall) (SocketCall, error) {
	var chat any
	if c.ChatID != "" {
		chat = c.ChatID
	}
	err := s.pool.QueryRow(ctx, `INSERT INTO socket_calls (chat_id, slot_id, via, op, detail, result, session, tool_call_id, duration_ms) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING id, created_at`,
		chat, c.SlotID, c.Via, c.Op, c.Detail, c.Result, c.Session, c.ToolCallID, c.DurationMs).Scan(&c.ID, &c.CreatedAt)
	return c, err
}

func (s *Store) ListSocketCalls(ctx context.Context, chatID string) ([]SocketCall, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, COALESCE(chat_id::text,''), slot_id, via, op, detail, result, created_at, session, tool_call_id, duration_ms FROM socket_calls WHERE chat_id=$1 ORDER BY id`, chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SocketCall{}
	for rows.Next() {
		var c SocketCall
		if err := rows.Scan(&c.ID, &c.ChatID, &c.SlotID, &c.Via, &c.Op, &c.Detail, &c.Result, &c.CreatedAt, &c.Session, &c.ToolCallID, &c.DurationMs); err != nil {
			return nil, err
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
INSERT INTO llm_calls (chat_id, slot_id, source_ip, model, response_id, status, input, output, cache_read, cache_write, cost, peak, tool_calls, started_at, duration_ms, finish_reason, complete)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17) RETURNING id`,
		chat, c.SlotID, c.SourceIP, c.Model, c.ResponseID, c.Status, c.Input, c.Output, c.CacheRead, c.CacheWrite, c.Cost, c.Peak, []byte(c.ToolCalls), c.StartedAt, c.DurationMs, noNUL(c.FinishReason), c.Complete).Scan(&c.ID)
	return c, err
}

func (s *Store) ListLLMCalls(ctx context.Context, chatID string) ([]LLMCall, error) {
	rows, err := s.pool.Query(ctx, `
SELECT x.id, x.slot_id, x.source_ip, x.model, x.response_id, x.status, x.input, x.output, x.cache_read, x.cache_write, x.cost, x.peak, x.tool_calls, x.started_at, x.duration_ms,
  (x.response_id <> '' AND EXISTS (SELECT 1 FROM chat_messages m WHERE m.chat_id = x.chat_id AND m.role = 'assistant' AND m.message->>'responseId' = x.response_id)),
  x.finish_reason, x.complete
FROM llm_calls x WHERE x.chat_id = $1 ORDER BY x.id`, chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LLMCall{}
	for rows.Next() {
		var c LLMCall
		var tc []byte
		if err := rows.Scan(&c.ID, &c.SlotID, &c.SourceIP, &c.Model, &c.ResponseID, &c.Status, &c.Input, &c.Output, &c.CacheRead, &c.CacheWrite, &c.Cost, &c.Peak, &tc, &c.StartedAt, &c.DurationMs, &c.Main, &c.FinishReason, &c.Complete); err != nil {
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
	ChatID      string     `json:"chat_id"`
	RunID       string     `json:"run_id"`
	Agent       string     `json:"agent"`
	Label       string     `json:"label,omitempty"`
	State       string     `json:"state,omitempty"`
	PiRunID     string     `json:"pi_run_id,omitempty"`
	ParentRunID string     `json:"parent_run_id,omitempty"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	EndedAt     *time.Time `json:"ended_at,omitempty"`
	UpdatedAt   time.Time  `json:"updated_at"`
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
RETURNING agent, label, state, pi_run_id, parent_run_id, started_at, ended_at, updated_at`,
		r.ChatID, r.RunID, r.Agent, r.Label, r.State, r.PiRunID, r.ParentRunID, r.StartedAt, r.EndedAt,
	).Scan(&r.Agent, &r.Label, &r.State, &r.PiRunID, &r.ParentRunID, &r.StartedAt, &r.EndedAt, &r.UpdatedAt)
	return r, err
}

func (s *Store) ListSubagentRuns(ctx context.Context, chatID string) ([]SubagentRun, error) {
	rows, err := s.pool.Query(ctx, `SELECT run_id, agent, label, state, pi_run_id, parent_run_id, started_at, ended_at, updated_at
FROM subagent_runs WHERE chat_id=$1 ORDER BY started_at NULLS LAST, run_id`, chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SubagentRun{}
	for rows.Next() {
		r := SubagentRun{ChatID: chatID}
		if err := rows.Scan(&r.RunID, &r.Agent, &r.Label, &r.State, &r.PiRunID, &r.ParentRunID, &r.StartedAt, &r.EndedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
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
	fb.Error = strings.TrimSpace(fb.Error + " [entry not stored completely: " + noNUL(err.Error()) + "]")
	id, err2 := s.insertToolExecution(ctx, fb)
	if err2 != nil {
		return e, fmt.Errorf("%w (fallback row: %v)", err, err2)
	}
	fb.ID = id
	return fb, nil
}

func (s *Store) insertToolExecution(ctx context.Context, e ToolExecution) (int64, error) {
	var chat any
	if e.ChatID != "" {
		chat = e.ChatID
	}
	var id int64
	err := s.pool.QueryRow(ctx, `
INSERT INTO tool_executions (chat_id, slot_id, session, tool_call_id, tool, op, args, exit_code, error, output_excerpt, output_sha256, output_bytes, started_at, duration_ms)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14) RETURNING id`,
		chat, e.SlotID, e.Session, e.ToolCallID, e.Tool, e.Op, []byte(e.Args), e.ExitCode, e.Error, e.OutputExcerpt, e.OutputSHA256, e.OutputBytes, e.StartedAt, e.DurationMs).Scan(&id)
	return id, err
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
SELECT id, slot_id, session, tool_call_id, tool, op, args, exit_code, error, output_excerpt, output_sha256, output_bytes, started_at, duration_ms
FROM tool_executions WHERE chat_id = $1 ORDER BY id`, chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ToolExecution{}
	for rows.Next() {
		e := ToolExecution{ChatID: chatID}
		var args []byte
		if err := rows.Scan(&e.ID, &e.SlotID, &e.Session, &e.ToolCallID, &e.Tool, &e.Op, &args, &e.ExitCode, &e.Error, &e.OutputExcerpt, &e.OutputSHA256, &e.OutputBytes, &e.StartedAt, &e.DurationMs); err != nil {
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

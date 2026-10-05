// Package chat manages chats: assigning a slot from the warm pool,
// forwarding pi's events, saving the session, idling and
// resuming in a fresh sandbox (states active / dormant / closed).
package chat

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"agw/internal/artifacts"
	"agw/internal/config"
	"agw/internal/delegation"
	"agw/internal/platform"
	"agw/internal/pool"
	"agw/internal/rpc"
	"agw/internal/sock"
	"agw/internal/store"
	"agw/internal/titler"
)

// Agent is the manager's view of a running sandbox with pi.
type Agent interface {
	Call(ctx context.Context, cmd map[string]any) (rpc.Response, error)
	Events() <-chan rpc.Event
	ContainerID() string
	ContainerName() string
	Image() string
	// Exec runs a command as the agent user in the execution sandbox
	// (workspace, inputs, display images; E9).
	Exec(ctx context.Context, cmd []string, stdin io.Reader) ([]byte, error)
	// ExecPi runs a command in pi's container (no shell, only
	// agw-exec and coreutils): sessions, configuration, subagents.
	ExecPi(ctx context.Context, cmd []string, stdin io.Reader) ([]byte, error)
	SetInternet(ctx context.Context, on bool) error
	// IP is the sandbox's address in the slot network (attribution at the LLM proxy).
	IP() string
	// Notify writes a command to pi without waiting for a response.
	Notify(cmd map[string]any) error
}

// Blobs is the object store (RustFS).
type Blobs interface {
	Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error
	Get(ctx context.Context, key string) (io.ReadCloser, int64, error)
	Move(ctx context.Context, src, dst string) error
	Delete(ctx context.Context, key string) error
}

var (
	ErrInvalid         = errors.New("invalid input")
	ErrTooManyPending  = errors.New("too many pending approvals in this chat; wait for the existing ones first")
	ErrPendingApproval = errors.New("chat has a pending approval")
	ErrRunning         = errors.New("agent is working")
	ErrUnknownModel    = errors.New("unknown model")
	ErrUnknownVariant  = pool.ErrUnknownVariant
	ErrNoSlot          = pool.ErrNoIdleSlot
	ErrNotFound        = store.ErrNotFound
)

// Event is distributed to a chat's SSE subscribers.
type Event struct {
	Kind string `json:"kind"`
	Data any    `json:"data"`
}

type ChatView struct {
	store.Chat
	Running bool   `json:"running"`
	SlotID  string `json:"slot_id,omitempty"`
	// Resuming: the chat is being resumed in a fresh sandbox.
	Resuming bool `json:"resuming"`
	// QueueHeld: there are queued messages that are not handed over on their own
	// (after an abort, while the chat is idle); they go along with the next message.
	QueueHeld bool `json:"queue_held"`
	// RunningSince: start of the current turn (only while the agent is working).
	RunningSince *time.Time `json:"running_since,omitempty"`
	// HoldReason: why queued messages are held back (HoldAbort, HoldWakeLimit, HoldAutoTurns);
	// empty for an idle chat or when nothing is held back.
	HoldReason string `json:"hold_reason,omitempty"`
	// ThinkingLevels: thinking levels the chat's model knows (empty: not known yet).
	ThinkingLevels []string `json:"thinking_levels,omitempty"`
	// PendingModel: model to switch to after the running compaction.
	PendingModel string `json:"pending_model,omitempty"`
}

type live struct {
	slot    *pool.Slot[Agent]
	runID   int64
	running bool
	idle    *time.Timer
	stop    chan struct{}
	stopped sync.Once

	ip              string // address in the slot network
	maxSub          int    // at most this many subagents
	limitEnforcedAt int    // count at which we last intervened

	runningSince time.Time
	toolsRunning int        // running tools (tool_execution_start to _end); moment for steering
	compacting   bool       // manual compaction running; idling blocked
	bg           sync.Mutex // serializes background work (saving, reading the context)

	wsNoSave    bool   // restore failed: do not save (protect the last backup)
	wsSkippedFP string // state last skipped because of the limit (notice only once)

	holdQueue  bool   // after an abort or above a limit: do not hand over the queue on its own
	holdReason string // HoldAbort, HoldWakeLimit, HoldAutoTurns

	activeAt time.Time // last activity (idle timeout)

	// pendingTurns: requests sent to pi whose user message pi has not reported yet;
	// turn: the turn the following responses belong to (Review 3, H1).
	pendingTurns []*turnMeta
	turn         *turnMeta
	agentStartAt time.Time // receipt of the last agent_start
}

// Timeouts for calls to pi. Without them a call hangs when pi stops
// responding, and with it the chat lock (Review H4). The timeout for prompt is in promptTimeout.
const (
	maxPendingPerChat = 3
	callTimeout       = 20 * time.Second
	maxRunTime        = 30 * time.Minute
)

func callT(a Agent, cmd map[string]any, d time.Duration) (rpc.Response, error) {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	return a.Call(ctx, cmd)
}

func execT(a Agent, cmd []string, stdin io.Reader, d time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	return a.Exec(ctx, cmd, stdin)
}

// execPiT: like execT, but in pi's container.
func execPiT(a Agent, cmd []string, stdin io.Reader, d time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	return a.ExecPi(ctx, cmd, stdin)
}

// background runs work outside the pump goroutine, one after another per
// chat. This way the pump keeps reading events while waiting for pi
// (Review H1).
func (m *Manager) background(l *live, f func()) {
	go func() {
		l.bg.Lock()
		defer l.bg.Unlock()
		select {
		case <-l.stop:
			return
		default:
		}
		f()
	}()
}

type Options struct {
	IdleTimeout      time.Duration
	ApprovalTimeout  time.Duration
	ArtifactMaxBytes int64
	AcquireTimeout   time.Duration
	InternetDefault  bool

	AutoCompactDefault   bool
	CompactReserveTokens int
	CompactKeepRecent    int

	MaxSubagentsDefault int
	MaxSubagentsLimit   int

	// ImageMaxBytes limits display images (default DefaultImageMaxBytes).
	ImageMaxBytes int64

	// WorkspaceMaxBytes limits the backup of /workspace (sum of the
	// file sizes; 0 = DefaultWorkspaceMaxBytes, < 0 = no backup).
	WorkspaceMaxBytes int64

	// BgWakesPerHour: how often per chat and hour the end of a background task may start a new
	// turn (0 = DefaultBgWakesPerHour, < 0 = never). BgKeepAlive: for this long after the last
	// activity, running background tasks postpone idling on idle timeout
	// (0 = DefaultBgKeepAlive, < 0 = not at all).
	BgWakesPerHour int
	BgKeepAlive    time.Duration
	// Titler phrases a title after the first question (nil: the title stays the shortened question).
	Titler Titler

	// AutoTurnsMax: at most this many turns without the user (wake-ups) in a row per chat
	// (0 = DefaultAutoTurnsMax, < 0 = none). Above that the manager stops (Review 3, H2).
	AutoTurnsMax int

	// Platform talks to the Agri-Gaia platform (nil: binding off).
	Platform *platform.Client
}

type Manager struct {
	st     *store.Store
	pool   *pool.Pool[Agent]
	cat    *config.Catalog
	blobs  Blobs
	broker *artifacts.Broker
	opt    Options

	mu   sync.Mutex
	live map[string]*live
	subs map[string]map[chan Event]struct{}
	// chatMu serializes state changes per chat (resuming, idling, closing).
	chatMu map[string]*sync.Mutex
	// imgMu serializes saving the display images per chat.
	imgMu map[string]*sync.Mutex
	// wsMu serializes saving and restoring the workspace per chat.
	wsMu map[string]*sync.Mutex
	// qMu serializes the decision "send now or enqueue" per chat.
	qMu map[string]*sync.Mutex
	// sending: a request is on its way (resume or prompt); new messages are enqueued.
	sending map[string]bool
	// resuming: the chat is being resumed (for display).
	resuming map[string]bool
	// pendingModel: model switch waiting for the end of a compaction (SetModel with compactFirst).
	pendingModel map[string]string
	// levels: thinking levels per model, as pi last reported them (get_available_thinking_levels).
	levels map[string][]string
	// userAt: the user's last action per chat (send, abort, remove, stop, resume);
	// decisive for the postponement by background tasks (Review 3, M1).
	userAt map[string]time.Time
	// aborts counts the aborts per chat; a request during which an abort happened does not go to pi.
	aborts map[string]uint64
}

func NewManager(st *store.Store, p *pool.Pool[Agent], cat *config.Catalog, blobs Blobs, broker *artifacts.Broker, opt Options) *Manager {
	if opt.AcquireTimeout == 0 {
		opt.AcquireTimeout = 45 * time.Second
	}
	switch {
	case opt.BgWakesPerHour == 0:
		opt.BgWakesPerHour = DefaultBgWakesPerHour
	case opt.BgWakesPerHour < 0:
		opt.BgWakesPerHour = 0
	}
	switch {
	case opt.BgKeepAlive == 0:
		opt.BgKeepAlive = DefaultBgKeepAlive
	case opt.BgKeepAlive < 0:
		opt.BgKeepAlive = 0
	}
	switch {
	case opt.AutoTurnsMax == 0:
		opt.AutoTurnsMax = DefaultAutoTurnsMax
	case opt.AutoTurnsMax < 0:
		opt.AutoTurnsMax = 0
	}
	return &Manager{st: st, pool: p, cat: cat, blobs: blobs, broker: broker, opt: opt,
		live: map[string]*live{}, subs: map[string]map[chan Event]struct{}{}, chatMu: map[string]*sync.Mutex{},
		imgMu: map[string]*sync.Mutex{}, wsMu: map[string]*sync.Mutex{}, qMu: map[string]*sync.Mutex{},
		sending: map[string]bool{}, resuming: map[string]bool{}, pendingModel: map[string]string{}, levels: map[string][]string{}, userAt: map[string]time.Time{}, aborts: map[string]uint64{}}
}

// userActive records an action of the user (postponement by background tasks, M1).
func (m *Manager) userActive(chatID string) {
	m.mu.Lock()
	m.userAt[chatID] = time.Now()
	m.mu.Unlock()
}

func (m *Manager) lock(chatID string) func() {
	m.mu.Lock()
	l, ok := m.chatMu[chatID]
	if !ok {
		l = &sync.Mutex{}
		m.chatMu[chatID] = l
	}
	m.mu.Unlock()
	l.Lock()
	return l.Unlock
}

func (m *Manager) Options() Options { return m.opt }

// --- Subscriptions ---

func (m *Manager) Subscribe(chatID string) (<-chan Event, func()) {
	ch := make(chan Event, 512)
	m.mu.Lock()
	if m.subs[chatID] == nil {
		m.subs[chatID] = map[chan Event]struct{}{}
	}
	m.subs[chatID][ch] = struct{}{}
	m.mu.Unlock()
	return ch, func() {
		m.mu.Lock()
		delete(m.subs[chatID], ch)
		m.mu.Unlock()
	}
}

func (m *Manager) publish(chatID string, ev Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	important := ev.Kind == "approval" || ev.Kind == "chat" || ev.Kind == "error"
	for ch := range m.subs[chatID] {
		select {
		case ch <- ev:
		default:
			if !important {
				continue // slow reader: drop the stream event instead of holding up the chat
			}
			// Approvals and state changes must not get lost: drop the oldest
			// event to make room.
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- ev:
			default:
			}
		}
	}
}

func (m *Manager) publishChat(ctx context.Context, chatID string) {
	if v, err := m.View(ctx, chatID); err == nil {
		m.publish(chatID, Event{Kind: "chat", Data: v})
	}
}

// --- Queries ---

func (m *Manager) View(ctx context.Context, chatID string) (ChatView, error) {
	c, err := m.st.GetChat(ctx, chatID)
	if err != nil {
		return ChatView{}, err
	}
	return m.view(c), nil
}

func (m *Manager) view(c store.Chat) ChatView {
	v := ChatView{Chat: c}
	m.mu.Lock()
	l, ok := m.live[c.ID]
	if ok {
		v.Running = l.running
		v.SlotID = l.slot.ID
		if l.running && !l.runningSince.IsZero() {
			t := l.runningSince
			v.RunningSince = &t
		}
	}
	v.Resuming = m.resuming[c.ID]
	v.ThinkingLevels = m.levels[c.Model]
	v.PendingModel = m.pendingModel[c.ID]
	v.QueueHeld = c.Queued > 0 && !m.sending[c.ID] && (!ok || l.holdQueue)
	if v.QueueHeld && ok {
		v.HoldReason = l.holdReason
	}
	m.mu.Unlock()
	return v
}

func (m *Manager) List(ctx context.Context) ([]ChatView, error) {
	cs, err := m.st.ListChats(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ChatView, 0, len(cs))
	for _, c := range cs {
		out = append(out, m.view(c))
	}
	return out, nil
}

// SlotChat returns the ID of the chat a slot is assigned to.
func (m *Manager) SlotChat(slotID string) string {
	s, ok := m.pool.Get(slotID)
	if !ok {
		return ""
	}
	return s.ChatID()
}

// slotOf returns the slot of a running chat (empty if it is idle).
func (m *Manager) slotOf(chatID string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if l := m.live[chatID]; l != nil && l.slot != nil {
		return l.slot.ID
	}
	return ""
}

// --- Creating, sending, resuming ---

type NewChat struct {
	Model        string `json:"model"`
	Variant      string `json:"variant"`
	Title        string `json:"title"`
	Message      string `json:"message"`
	Internet     *bool  `json:"internet"`
	AutoCompact  *bool  `json:"auto_compact"`
	MaxSubagents *int   `json:"max_subagents"`
	// Delegation: delegated rights (delegation.Delegation as JSON); without it the chat behaves
	// as before step 1 (reading free, writing with approval).
	Delegation json.RawMessage `json:"delegation,omitempty"`
	// Owner is set by the API from the login (sub), never from the request body.
	Owner string `json:"-"`
	// Language: the user's preferred language according to the browser (BCP 47, e.g. "en-US");
	// optional. Goes to the agent as a note with the first request (language.go).
	Language string `json:"language,omitempty"`
}

func (m *Manager) Create(ctx context.Context, req NewChat) (ChatView, error) {
	if req.Model == "" {
		req.Model = m.cat.Default
	}
	if _, _, ok := m.cat.Lookup(req.Model); !ok {
		return ChatView{}, ErrUnknownModel
	}
	if req.Variant == "" {
		req.Variant = "cli"
	}
	if _, ok := m.pool.Targets()[req.Variant]; !ok {
		return ChatView{}, ErrUnknownVariant
	}
	internet := m.opt.InternetDefault
	if req.Internet != nil {
		internet = *req.Internet
	}
	// Without a title it counts as a placeholder; Send replaces it with the first question
	// (immediately shortened, then by the model), even if the question comes right at creation.
	title, titleSrc := strings.TrimSpace(req.Title), store.TitleUser
	if title == "" {
		title, titleSrc = titleFrom(req.Message), store.TitleDefault
	}
	autoCompact := m.opt.AutoCompactDefault
	if req.AutoCompact != nil {
		autoCompact = *req.AutoCompact
	}
	maxSub := m.opt.MaxSubagentsDefault
	if req.MaxSubagents != nil {
		maxSub = *req.MaxSubagents
	}
	if maxSub < 0 || maxSub > m.opt.MaxSubagentsLimit {
		return ChatView{}, fmt.Errorf("%w: max_subagents must be between 0 and %d", ErrInvalid, m.opt.MaxSubagentsLimit)
	}
	lang, err := NormalizeLanguage(req.Language)
	if err != nil {
		return ChatView{}, err
	}
	var del json.RawMessage
	if t := bytes.TrimSpace(req.Delegation); len(t) > 0 && string(t) != "null" {
		d, err := delegation.Parse(t)
		if err != nil {
			return ChatView{}, fmt.Errorf("%w: %v", ErrInvalid, err)
		}
		del, _ = json.Marshal(d) // the checked, normalized form is stored
	}
	c, err := m.st.CreateChat(ctx, store.NewChat{Title: title, TitleSource: titleSrc, Model: req.Model, Variant: req.Variant, Internet: internet, AutoCompact: autoCompact, MaxSubagents: maxSub, Delegation: del, Owner: req.Owner, Language: lang})
	if err != nil {
		return ChatView{}, err
	}
	unlock := m.lock(c.ID)
	err = m.attach(ctx, c, nil, nil)
	unlock()
	if err != nil {
		// Without a sandbox the chat is idle; the next message tries again.
		_ = m.st.SetState(context.WithoutCancel(ctx), c.ID, store.StateDormant)
		m.publishChat(context.WithoutCancel(ctx), c.ID)
		return ChatView{}, err
	}
	if strings.TrimSpace(req.Message) != "" {
		if _, err := m.Send(ctx, c.ID, req.Message); err != nil {
			return ChatView{}, err
		}
	}
	return m.View(ctx, c.ID)
}

func titleFrom(msg string) string {
	msg = strings.Join(strings.Fields(msg), " ")
	if msg == "" {
		return "New chat " + time.Now().Format("2006-01-02 15:04")
	}
	r := []rune(msg)
	if len(r) > 60 {
		return string(r[:60]) + " …"
	}
	return msg
}

// attach acquires a slot, sets model, internet, inputs and, with
// session != nil, the saved session. The caller holds the chat lock.
// p reports the steps when resuming (nil when creating).
func (m *Manager) attach(ctx context.Context, c store.Chat, session []byte, p *resumeProgress) error {
	p.run(PhaseAcquire)
	slot, err := m.pool.AcquireWait(ctx, c.Variant, c.ID, m.opt.AcquireTimeout)
	if err != nil {
		return err
	}
	p.done(ResumeStep{Detail: slot.ID})
	a := slot.Worker
	fail := func(err error) error {
		m.pool.Release(ctx, slot)
		return err
	}
	p.run(PhaseSession)
	prov, model, _ := m.cat.Lookup(c.Model)
	if _, err := a.Call(ctx, map[string]any{"type": "set_model", "provider": prov.ID, "modelId": model.ID}); err != nil {
		return fail(fmt.Errorf("set model: %w", err))
	}
	m.syncThinking(ctx, c.ID, c.Model, a, c.ThinkingLevel)
	if session != nil {
		sp := "/agent/sessions/" + c.ID + ".jsonl"
		if _, err := a.ExecPi(ctx, []string{"agw-exec", "put", sp}, bytes.NewReader(session)); err != nil {
			return fail(fmt.Errorf("restore session: %w", err))
		}
		resp, err := a.Call(ctx, map[string]any{"type": "switch_session", "sessionPath": sp})
		if err != nil {
			return fail(fmt.Errorf("switch_session: %w", err))
		}
		var d struct {
			Cancelled bool `json:"cancelled"`
		}
		_ = json.Unmarshal(resp.Data, &d)
		if d.Cancelled {
			return fail(errors.New("switch_session was cancelled"))
		}
		p.done(ResumeStep{Size: int64p(int64(len(session)))})
	} else {
		p.done(ResumeStep{Detail: "no session saved"})
	}
	p.run(PhaseSettings)
	if err := a.SetInternet(ctx, c.Internet); err != nil {
		return fail(fmt.Errorf("set internet: %w", err))
	}
	if _, err := a.Call(ctx, map[string]any{"type": "set_auto_compaction", "enabled": c.AutoCompact}); err != nil {
		return fail(fmt.Errorf("set auto-compaction: %w", err))
	}
	p.done(ResumeStep{Detail: "Internet " + onOff(c.Internet)})
	l := &live{slot: slot, stop: make(chan struct{}), ip: a.IP(), maxSub: c.MaxSubagents}
	m.userActive(c.ID) // creating and resuming come from the user
	// Restore the workspace before the inputs and before the first request
	// (the archive contains no inputs/, the two do not get in each other's way).
	p.run(PhaseWorkspace)
	ws := m.restoreWorkspace(ctx, c.ID, l)
	switch {
	case ws.Disabled:
		p.done(ResumeStep{Detail: "backup disabled"})
	case ws.Err != nil:
		p.done(ResumeStep{Status: "warning", Detail: "not restored: " + ws.Err.Error(), Size: int64p(ws.Size), Files: intp(ws.Files)})
	case !ws.Found:
		p.done(ResumeStep{Detail: "no backup"})
	default:
		p.done(ResumeStep{Size: int64p(ws.Size), Files: intp(ws.Files)})
	}
	p.run(PhaseInputs)
	if n, size, err := m.syncInputs(ctx, c.ID, a); err != nil {
		slog.Warn("inputs not mirrored", "chat", c.ID, "error", err)
		p.done(ResumeStep{Status: "warning", Detail: "not all mirrored: " + err.Error(), Size: int64p(size), Files: intp(n)})
	} else {
		p.done(ResumeStep{Size: int64p(size), Files: intp(n)})
	}
	l.runID, _ = m.st.StartRun(ctx, c.ID, slot.ID, a.ContainerID())
	if n, err := m.st.SubagentRunCount(ctx, c.ID); err == nil {
		l.limitEnforcedAt = n // earlier runs do not trigger a new intervention
	}
	m.applySubagentConfig(l, c.MaxSubagents)
	m.mu.Lock()
	m.live[c.ID] = l
	m.mu.Unlock()
	if c.Variant != "mcp" {
		go m.watchSubagents(c.ID, l)
	}
	_ = m.st.SetState(ctx, c.ID, store.StateActive)
	go m.pump(c.ID, l)
	m.armIdle(c.ID, l)
	go m.refreshInfo(context.WithoutCancel(ctx), c.ID, a)
	slog.Info("chat assigned", "chat", c.ID, "slot", slot.ID, "container", a.ContainerName(), "resumed", session != nil)
	m.publishChat(ctx, c.ID)
	return nil
}

// ensureLive returns the chat's active state, resuming an idle
// chat in a fresh sandbox along the way.
func (m *Manager) ensureLive(ctx context.Context, chatID string) (*live, bool, error) {
	unlock := m.lock(chatID)
	defer unlock()
	c, err := m.st.GetChat(ctx, chatID)
	if err != nil {
		return nil, false, err
	}
	m.mu.Lock()
	l := m.live[chatID]
	m.mu.Unlock()
	if l != nil {
		return l, false, nil
	}
	p := m.newResume(chatID)
	m.mu.Lock()
	m.resuming[chatID] = true
	m.mu.Unlock()
	m.publishChat(ctx, chatID)
	defer func() {
		m.mu.Lock()
		delete(m.resuming, chatID)
		m.mu.Unlock()
		m.publishChat(context.WithoutCancel(ctx), chatID)
	}()
	session, err := m.st.LoadSession(ctx, chatID)
	if err == nil {
		err = m.attach(ctx, c, session, p)
	}
	if err != nil {
		p.fail(err)
		return nil, false, err
	}
	p.ready()
	m.mu.Lock()
	l = m.live[chatID]
	m.mu.Unlock()
	return l, true, nil
}

// AttachmentsHeader introduces the block with which attachments are appended
// to a message; the UI recognizes it and shows the attachments as chips.
const AttachmentsHeader = "[Attachments in /workspace/inputs/]"

// AttachmentNote builds the block for the given files.
func AttachmentNote(names []string) string {
	var b strings.Builder
	b.WriteString(AttachmentsHeader)
	for _, n := range names {
		b.WriteString("\n- ")
		b.WriteString(n)
	}
	return b.String()
}

func (m *Manager) Abort(ctx context.Context, chatID string) (ChatView, error) {
	m.mu.Lock()
	m.userAt[chatID] = time.Now()
	m.aborts[chatID]++ // a request currently on its way no longer goes to pi
	l := m.live[chatID]
	if l != nil {
		// Do not hand over queued messages on their own after an abort: the user has stopped;
		// they go with the next message or via "send now".
		l.holdQueue, l.holdReason = true, HoldAbort
	}
	m.mu.Unlock()
	if l != nil {
		// After abort pi continues with what is queued; so reclaim steered messages beforehand
		// (they stay enqueued and held back).
		m.reclaimSteered(ctx, chatID, l)
		if _, err := callT(l.slot.Worker, map[string]any{"type": "abort"}, 10*time.Second); err != nil {
			return ChatView{}, err
		}
	}
	return m.View(ctx, chatID)
}

// Suspend makes the chat idle: save the session, tear down the sandbox.
func (m *Manager) Suspend(ctx context.Context, chatID string) (ChatView, error) {
	unlock := m.lock(chatID)
	defer unlock()
	c, err := m.st.GetChat(ctx, chatID)
	if err != nil {
		return ChatView{}, err
	}
	if c.PendingApprovals > 0 {
		return ChatView{}, ErrPendingApproval
	}
	m.mu.Lock()
	l := m.live[chatID]
	running := l != nil && (l.running || l.compacting)
	m.mu.Unlock()
	if running {
		return ChatView{}, ErrRunning
	}
	if l != nil {
		m.detach(ctx, chatID, l, true, true)
	}
	_ = m.st.SetState(ctx, chatID, store.StateDormant)
	m.publishChat(ctx, chatID)
	return m.View(ctx, chatID)
}

// SetInternet runs under the chat lock: a concurrent resume (attach)
// could otherwise connect a new sandbox with a stale value
// (Review H3).
func (m *Manager) SetInternet(ctx context.Context, chatID string, on bool) (ChatView, error) {
	unlock := m.lock(chatID)
	defer unlock()
	_, err := m.st.GetChat(ctx, chatID)
	if err != nil {
		return ChatView{}, err
	}
	m.mu.Lock()
	l := m.live[chatID]
	m.mu.Unlock()
	if l != nil {
		if err := l.slot.Worker.SetInternet(ctx, on); err != nil {
			return ChatView{}, err
		}
	}
	if err := m.st.SetInternet(ctx, chatID, on); err != nil {
		return ChatView{}, err
	}
	slog.Info("internet toggled", "chat", chatID, "on", on, "immediate", l != nil)
	m.publishChat(ctx, chatID)
	return m.View(ctx, chatID)
}

// detach saves the session (save) and the workspace (workspace) and
// returns the slot (single use).
func (m *Manager) detach(ctx context.Context, chatID string, l *live, save, workspace bool) {
	// The exchanged platform token belongs to the sandbox session; resuming exchanges anew.
	m.opt.Platform.Forget(chatID)
	m.setPendingModel(chatID, "") // a scheduled model switch only applies to this sandbox
	// Background tasks die with the sandbox; mark them beforehand so their end does not trigger
	// a wake-up (Close and agentDied have already marked them with their own state).
	m.endBackground(ctx, chatID, store.BgSuspended, "ended with the sandbox when the chat went idle", true)
	if save {
		if err := m.saveSession(ctx, chatID, l.slot.Worker); err != nil {
			slog.Warn("session not saved", "chat", chatID, "error", err)
		}
		if workspace {
			// Also waits for a running backup after agent_settled (lock per chat).
			if err := m.saveWorkspace(ctx, chatID, l); err != nil {
				slog.Warn("workspace not saved", "chat", chatID, "error", err)
			}
		}
		// Wait for a running backup of display images before the sandbox disappears.
		m.imageLock(chatID)()
	}
	m.mu.Lock()
	if m.live[chatID] == l {
		delete(m.live, chatID)
	}
	if l.idle != nil {
		l.idle.Stop()
	}
	m.mu.Unlock()
	first := false
	l.stopped.Do(func() { close(l.stop); first = true })
	if !first {
		return // already torn down (e.g. idling and simultaneous end of the stream)
	}
	_ = m.st.EndRun(ctx, l.runID)
	m.pool.Release(ctx, l.slot)
	slog.Info("slot returned", "chat", chatID, "slot", l.slot.ID)
}

func (m *Manager) saveSession(ctx context.Context, chatID string, a Agent) error {
	resp, err := callT(a, map[string]any{"type": "get_state"}, callTimeout)
	if err != nil {
		return err
	}
	var st struct {
		SessionFile string `json:"sessionFile"`
	}
	if err := json.Unmarshal(resp.Data, &st); err != nil || st.SessionFile == "" {
		return fmt.Errorf("no session file in get_state: %s", resp.Data)
	}
	data, err := execPiT(a, []string{"cat", st.SessionFile}, nil, callTimeout)
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return nil // pi writes the file only after the first response
	}
	return m.st.SaveSession(context.WithoutCancel(ctx), chatID, data)
}

// --- Idle timeout ---

func (m *Manager) armIdle(chatID string, l *live) {
	m.mu.Lock()
	l.activeAt = time.Now()
	m.mu.Unlock()
	m.armIdleAfter(chatID, l, m.opt.IdleTimeout)
}

// armIdleAfter sets the idle timer to d without moving the last activity.
func (m *Manager) armIdleAfter(chatID string, l *live, d time.Duration) {
	if m.opt.IdleTimeout <= 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if l.idle != nil {
		l.idle.Stop()
	}
	l.idle = time.AfterFunc(d, func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		m.mu.Lock()
		stuck := l.running && !l.runningSince.IsZero() && time.Since(l.runningSince) > maxRunTime
		m.mu.Unlock()
		if stuck { // maximum duration exceeded: abort, then let it go idle
			slog.Warn("turn too long, aborting", "chat", chatID)
			_, _ = callT(l.slot.Worker, map[string]any{"type": "abort"}, 10*time.Second)
			m.setRunning(l, false)
		} else if wait, ok := m.keepAliveForBackground(chatID, l); ok {
			// Running background tasks keep the chat awake until BgKeepAlive after the last
			// activity; then it goes idle, and the tasks end with the sandbox.
			m.armIdleAfter(chatID, l, wait)
			return
		}
		if _, err := m.Suspend(ctx, chatID); err != nil {
			if errors.Is(err, ErrRunning) || errors.Is(err, ErrPendingApproval) {
				m.armIdle(chatID, l)
			}
			return
		}
		slog.Info("chat idle after idle timeout", "chat", chatID)
	})
}

func (m *Manager) touchIdle(chatID string) {
	m.mu.Lock()
	l := m.live[chatID]
	m.mu.Unlock()
	if l != nil {
		m.armIdle(chatID, l)
	}
}

// --- Events from pi ---

// ExecWatcher reports the end of a slot's execution sandbox (E9). If it dies, pi's event
// stream does not end; without this signal all tool calls would go nowhere (H2).
type ExecWatcher interface {
	ExecDone() <-chan struct{}
}

func (m *Manager) pump(chatID string, l *live) {
	a := l.slot.Worker
	var execDone <-chan struct{}
	if w, ok := a.(ExecWatcher); ok {
		execDone = w.ExecDone()
	}
	for {
		select {
		case <-l.stop:
			return
		case <-execDone:
			m.agentDied(chatID, l, "exec")
			return
		case ev, ok := <-a.Events():
			if !ok {
				m.agentDied(chatID, l, "pi")
				return
			}
			m.handle(chatID, l, ev)
		}
	}
}

// agentDied handles the unexpected end of one of a slot's two containers. cause:
// "pi" (event stream ended; the execution sandbox is still alive, the workspace is
// saved) or "exec" (execution sandbox ended; pi is still alive, the session is saved).
func (m *Manager) agentDied(chatID string, l *live, cause string) {
	ctx := context.Background()
	// If the stream ends because the slot is being returned, that is not a crash.
	select {
	case <-l.stop:
		return
	default:
	}
	m.mu.Lock()
	current := m.live[chatID] == l
	m.mu.Unlock()
	if !current {
		return
	}
	msg := "The sandbox ended unexpectedly. The chat is idle; a new message resumes it with the last saved session."
	if cause == "exec" {
		slog.Error("execution sandbox ended", "chat", chatID, "slot", l.slot.ID)
		msg = "The execution sandbox ended unexpectedly. The chat is idle; a new message resumes it with the session and the last saved workspace."
	} else {
		slog.Error("pi ended", "chat", chatID, "slot", l.slot.ID)
	}
	m.publish(chatID, Event{Kind: "error", Data: map[string]string{"message": msg}})
	unlock := m.lock(chatID)
	defer unlock()
	m.mu.Lock()
	current = m.live[chatID] == l
	m.mu.Unlock()
	if !current { // made idle or closed in the meantime
		return
	}
	m.rejectPending(ctx, chatID, "sandbox ended")
	m.endBackground(ctx, chatID, store.BgLost, "sandbox ended unexpectedly", true)
	if cause == "exec" {
		// pi is alive: save the session (the workspace is lost with the sandbox).
		if err := m.saveSession(ctx, chatID, l.slot.Worker); err != nil {
			slog.Warn("session not saved", "chat", chatID, "error", err)
		}
	} else if err := m.saveWorkspace(ctx, chatID, l); err != nil {
		// The execution sandbox is still alive (H1): save the workspace as far as possible.
		slog.Warn("workspace not saved", "chat", chatID, "error", err)
	}
	m.detach(ctx, chatID, l, false, false)
	_ = m.st.SetState(ctx, chatID, store.StateDormant)
	m.publishChat(ctx, chatID)
}

func (m *Manager) handle(chatID string, l *live, ev rpc.Event) {
	ctx := context.Background()
	switch ev.Type {
	case rpc.TypeOverflow:
		slog.Warn("events dropped (flood in the RPC stream)", "chat", chatID, "count", string(ev.Raw))
		m.publish(chatID, Event{Kind: "error", Data: map[string]string{"message": "Events dropped in the RPC stream (" + string(ev.Raw) + "); rechecking state"}})
		m.background(l, func() { m.resync(chatID, l) })
		return
	case rpc.TypeOversized, rpc.TypeInvalid:
		slog.Warn("suspicious line in the RPC stream", "chat", chatID, "kind", ev.Type)
		m.publish(chatID, Event{Kind: "error", Data: map[string]string{"message": "Suspicious line in the RPC stream (" + ev.Type + ")"}})
		return
	}
	if ev.Type == "extension_ui_request" {
		m.answerExtensionUI(chatID, l, ev.Raw)
		return
	}
	var userTurn *turnMeta
	if ev.Type == "message_end" {
		if text, ok := userMessageText(ev.Raw); ok {
			// Distribute the origin before the message: the UI assigns it to the next user message.
			if userTurn = m.takeTurn(l, text); userTurn != nil {
				m.publish(chatID, Event{Kind: "user_meta", Data: UserMeta{TurnID: userTurn.id, Trigger: userTurn.trigger, Origin: userTurn.origin, Sources: userTurn.sources}})
			}
		}
	}
	m.publish(chatID, Event{Kind: "pi", Data: ev.Raw})

	switch ev.Type {
	case "agent_start":
		m.mu.Lock()
		// If nothing is running, pi started the turn itself (dispatch sets running before prompt;
		// retries within a run come before agent_settled): e.g. because a subagent in the
		// background has finished (pi-subagents, triggerTurn).
		byPi := !l.running
		for _, p := range l.pendingTurns {
			if !p.isConsumed() {
				byPi = false // a request from the orchestrator is on its way (e.g. steered in late)
			}
		}
		if !l.running || l.runningSince.IsZero() {
			l.runningSince = time.Now()
		}
		l.agentStartAt = time.Now()
		l.toolsRunning = 0
		m.mu.Unlock()
		m.setRunning(l, true)
		l.slot.SetActivity("thinking", "")
		if byPi {
			m.startPiTurn(ctx, chatID, l)
		}
		m.publishChat(ctx, chatID)
	case "message_update":
		var p struct {
			E struct {
				Type     string `json:"type"`
				ToolName string `json:"toolName"`
			} `json:"assistantMessageEvent"`
		}
		_ = json.Unmarshal(ev.Raw, &p)
		switch p.E.Type {
		case "thinking_start", "thinking_delta":
			l.slot.SetActivity("thinking", "")
		case "text_start", "text_delta":
			l.slot.SetActivity("writing", "")
		case "toolcall_start":
			// The model is writing a tool call (with write and a large file this takes a while).
			l.slot.SetActivity("preparing", p.E.ToolName)
		}
	case "tool_execution_start":
		var p struct {
			ToolName string `json:"toolName"`
		}
		_ = json.Unmarshal(ev.Raw, &p)
		l.slot.SetActivity("tool", p.ToolName)
		m.mu.Lock()
		l.toolsRunning++
		m.mu.Unlock()
		// Steer queued messages in now: pi inserts them after the running tools, before the
		// next model call (like Claude Code), instead of waiting until the end of the turn.
		m.background(l, func() { m.steerQueue(chatID, l) })
	case "tool_execution_end":
		l.slot.SetActivity("thinking", "")
		m.mu.Lock()
		if l.toolsRunning > 0 {
			l.toolsRunning--
		}
		m.mu.Unlock()
	case "message_end":
		var p struct {
			Message json.RawMessage `json:"message"`
		}
		_ = json.Unmarshal(ev.Raw, &p)
		var role struct {
			Role string `json:"role"`
		}
		_ = json.Unmarshal(p.Message, &role)
		switch role.Role {
		case "user", "assistant", "toolResult", "custom":
			var bill *store.Billing
			if role.Role == "assistant" {
				bill = m.bill(ctx, chatID, p.Message)
			}
			var meta *store.MessageMeta
			if role.Role == "custom" {
				// Message from an extension to the agent (e.g. the end of a subagent in the
				// background): belongs to the current turn, not from the user.
				m.mu.Lock()
				if t := l.turn; t != nil {
					meta = &store.MessageMeta{TurnID: t.id, Trigger: t.trigger, Origin: store.OriginSystem, Sources: []store.Source{{Kind: store.QueueSystem, Type: NotePi}}}
				}
				m.mu.Unlock()
			} else if role.Role == "user" {
				if userTurn != nil {
					meta = &store.MessageMeta{TurnID: userTurn.id, Trigger: userTurn.trigger, Origin: userTurn.origin, Sources: userTurn.sources}
				} else {
					m.mu.Lock()
					l.turn = nil // user message without a request from the orchestrator: origin unknown
					m.mu.Unlock()
				}
			} else {
				m.mu.Lock()
				if t := l.turn; t != nil {
					meta = &store.MessageMeta{TurnID: t.id, Trigger: t.trigger}
				}
				m.mu.Unlock()
			}
			if _, err := m.st.AppendTurnMessage(ctx, chatID, p.Message, bill, meta); err != nil {
				slog.Error("message not saved", "chat", chatID, "error", err)
			}
			if role.Role == "assistant" {
				m.publishChat(ctx, chatID)
				m.captureImages(chatID, l.slot.Worker, p.Message)
			}
		}
	case "compaction_start":
		l.slot.SetActivity("compacting", "")
		m.publishChat(ctx, chatID)
	case "compaction_end":
		m.storeCompaction(ctx, chatID, ev.Raw)
		m.mu.Lock()
		running := l.running
		m.mu.Unlock()
		m.mu.Lock()
		l.compacting = false
		m.mu.Unlock()
		if running {
			l.slot.SetActivity("thinking", "")
		} else {
			l.slot.SetActivity("idle", "")
		}
		m.background(l, func() {
			if !running {
				if err := m.saveSession(ctx, chatID, l.slot.Worker); err != nil {
					slog.Warn("session not saved", "chat", chatID, "error", err)
				}
			}
			m.refreshInfo(ctx, chatID, l.slot.Worker)
			m.applyPendingModel(ctx, chatID)
			if !running { // manual compaction over: hand over queued messages now
				m.deliverQueue(chatID, l)
			}
		})
	case "agent_settled":
		m.mu.Lock()
		// Requests from before this run for which pi reported no user message (e.g. commands
		// of an extension) no longer misattribute a later message.
		keep := l.pendingTurns[:0]
		for _, p := range l.pendingTurns {
			if p.at.After(l.agentStartAt) {
				keep = append(keep, p)
			}
		}
		l.pendingTurns = keep
		l.toolsRunning = 0
		m.mu.Unlock()
		m.setRunning(l, false)
		l.slot.SetActivity("idle", "")
		m.background(l, func() {
			// Reclaim steered messages that pi no longer inserted (before deliverQueue).
			m.reclaimSteered(ctx, chatID, l)
			if err := m.saveSession(ctx, chatID, l.slot.Worker); err != nil {
				slog.Warn("session not saved", "chat", chatID, "error", err)
			}
			m.refreshInfo(ctx, chatID, l.slot.Worker)
			if err := m.saveWorkspace(ctx, chatID, l); err != nil {
				slog.Warn("workspace not saved", "chat", chatID, "error", err)
			}
			// Only after saving: the next request starts from a saved state.
			m.deliverQueue(chatID, l)
		})
		m.armIdle(chatID, l)
		m.publishChat(ctx, chatID)
	}
}

// NotePi: source of a message that an extension inside pi posted itself (pi-subagents).
const NotePi = "pi"

// startPiTurn creates a turn that pi started without a request from the orchestrator (wake-up
// without the user). Its responses thus do not count towards the user's previous request, and the
// limit for turns without the user (AGW_AUTO_TURNS_MAX, Review 3, H2) applies here too: once it is
// reached, the orchestrator aborts the turn and holds back queued messages.
func (m *Manager) startPiTurn(ctx context.Context, chatID string, l *live) {
	ev, limited := m.autoLimitTurns(ctx, chatID)
	sources := []store.Source{{Kind: store.QueueSystem, Type: NotePi}}
	tm := &turnMeta{trigger: store.TriggerWake, origin: store.OriginSystem, sources: sources, at: time.Now()}
	if id, err := m.st.CreateTurn(ctx, chatID, store.TriggerWake, store.OriginSystem, sources, nil); err == nil {
		tm.id = id
	} else {
		slog.Warn("turn from pi not created", "chat", chatID, "error", err)
	}
	tm.markConsumed()
	m.mu.Lock()
	l.turn = tm
	m.mu.Unlock()
	slog.Info("pi starts a turn without a request (wake-up)", "chat", chatID, "turn", tm.id)
	if limited {
		m.mu.Lock()
		l.holdQueue, l.holdReason = true, ev.Reason
		m.mu.Unlock()
		slog.Warn("limit for turns without the user reached, turn from pi aborted", "chat", chatID, "limit", ev.Limit)
		m.publish(chatID, Event{Kind: "auto_held", Data: ev})
		go func() { _, _ = callT(l.slot.Worker, map[string]any{"type": "abort"}, 10*time.Second) }()
	}
}

// UserMeta is the SSE event "user_meta": origin of the user message that follows as the next
// "pi" event (message_end, role user).
type UserMeta struct {
	TurnID  int64          `json:"turn_id,omitempty"`
	Trigger string         `json:"trigger"`
	Origin  string         `json:"origin"`
	Sources []store.Source `json:"sources"`
}

// userMessageText returns the text of a user message from message_end (ok only for role user).
func userMessageText(raw json.RawMessage) (string, bool) {
	var p struct {
		Message struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(raw, &p) != nil || p.Message.Role != "user" {
		return "", false
	}
	var s string
	if json.Unmarshal(p.Message.Content, &s) == nil {
		return s, true
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	_ = json.Unmarshal(p.Message.Content, &parts)
	var b strings.Builder
	for _, c := range parts {
		if c.Type == "text" {
			b.WriteString(c.Text)
		}
	}
	return b.String(), true
}

// bill computes the cost of a response according to the provider's tariff at
// the time of the response (peak / off-peak). pi knows only one price.
func (m *Manager) bill(ctx context.Context, chatID string, msg json.RawMessage) *store.Billing {
	var a struct {
		Provider  string       `json:"provider"`
		Model     string       `json:"model"`
		Timestamp int64        `json:"timestamp"`
		Usage     config.Usage `json:"usage"`
	}
	if err := json.Unmarshal(msg, &a); err != nil {
		return nil
	}
	id := a.Provider + "/" + a.Model
	if a.Provider == "" || a.Model == "" {
		c, err := m.st.GetChat(ctx, chatID)
		if err != nil {
			return nil
		}
		id = c.Model
	}
	at := time.Now()
	if a.Timestamp > 0 {
		at = time.UnixMilli(a.Timestamp)
	}
	cost, peak, ok := m.cat.Cost(id, a.Usage, at)
	if !ok {
		return nil
	}
	return &store.Billing{Cost: cost, Peak: peak}
}

// ContextUsage is the context usage as the API returns it.
type ContextUsage struct {
	Tokens           *int64    `json:"tokens"`
	Window           int64     `json:"window"`
	Percent          *float64  `json:"percent"`
	ThresholdTokens  int64     `json:"threshold_tokens"`
	ReserveTokens    int64     `json:"reserve_tokens"`
	KeepRecentTokens int64     `json:"keep_recent_tokens"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// refreshInfo reads context usage and commands from pi and stores them.
func (m *Manager) refreshInfo(ctx context.Context, chatID string, a Agent) {
	if resp, err := callT(a, map[string]any{"type": "get_session_stats"}, callTimeout); err == nil {
		var d struct {
			ContextUsage *struct {
				Tokens        *int64   `json:"tokens"`
				ContextWindow int64    `json:"contextWindow"`
				Percent       *float64 `json:"percent"`
			} `json:"contextUsage"`
		}
		if json.Unmarshal(resp.Data, &d) == nil && d.ContextUsage != nil {
			u := ContextUsage{Tokens: d.ContextUsage.Tokens, Window: d.ContextUsage.ContextWindow, Percent: d.ContextUsage.Percent,
				ReserveTokens: int64(m.opt.CompactReserveTokens), KeepRecentTokens: int64(m.opt.CompactKeepRecent), UpdatedAt: time.Now()}
			u.ThresholdTokens = u.Window - u.ReserveTokens
			if b, err := json.Marshal(u); err == nil {
				_ = m.st.SetContext(ctx, chatID, b)
			}
		}
	}
	if resp, err := callT(a, map[string]any{"type": "get_commands"}, callTimeout); err == nil {
		var d struct {
			Commands json.RawMessage `json:"commands"`
		}
		if json.Unmarshal(resp.Data, &d) == nil && len(d.Commands) > 0 {
			_ = m.st.SetCommands(ctx, chatID, d.Commands)
		}
	}
	m.publishChat(ctx, chatID)
}

// storeCompaction stores a compaction as its own entry and bills
// the summary (it appears in no response).
func (m *Manager) storeCompaction(ctx context.Context, chatID string, raw json.RawMessage) {
	var ev struct {
		Reason       string          `json:"reason"`
		Result       json.RawMessage `json:"result"`
		Aborted      bool            `json:"aborted"`
		ErrorMessage string          `json:"errorMessage"`
	}
	if json.Unmarshal(raw, &ev) != nil {
		return
	}
	if len(ev.Result) == 0 || string(ev.Result) == "null" {
		slog.Warn("compaction without result", "chat", chatID, "aborted", ev.Aborted, "error", ev.ErrorMessage)
		return
	}
	var res map[string]any
	if json.Unmarshal(ev.Result, &res) != nil {
		return
	}
	res["role"] = "compaction"
	res["reason"] = ev.Reason
	res["timestamp"] = time.Now().UnixMilli()
	msg, _ := json.Marshal(res)
	var bill *store.Billing
	if c, err := m.st.GetChat(ctx, chatID); err == nil {
		var u struct {
			Usage config.Usage `json:"usage"`
		}
		_ = json.Unmarshal(ev.Result, &u)
		if cost, peak, ok := m.cat.Cost(c.Model, u.Usage, time.Now()); ok {
			bill = &store.Billing{Cost: cost, Peak: peak}
		}
	}
	if _, err := m.st.AppendBilledMessage(ctx, chatID, msg, bill); err != nil {
		slog.Error("compaction not saved", "chat", chatID, "error", err)
		return
	}
	slog.Info("context compacted", "chat", chatID, "reason", ev.Reason)
}

// Command is a slash command for the UI.
type Command struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Source      string `json:"source"`
	Args        string `json:"args,omitempty"`
	// Options: possible arguments for completion (/model, /effort).
	Options []CommandOption `json:"options,omitempty"`
}

type CommandOption struct {
	Value   string `json:"value"`
	Label   string `json:"label,omitempty"`
	Current bool   `json:"current,omitempty"`
}

// compactLanguageHint precedes the instructions of every manual compaction;
// without it pi writes the summary in English, whatever language the chat is in.
const compactLanguageHint = "Write the summary in the language of the conversation."

var builtinCommands = []Command{
	{Name: "compact", Description: "Summarize the context now; optionally with instructions on what the summary should focus on", Source: "builtin", Args: "[instructions]"},
	{Name: "autocompact", Description: "Turn automatic compaction on or off", Source: "builtin", Args: "on|off"},
	{Name: "rename", Description: "Rename the chat", Source: "builtin", Args: "<name>"},
	{Name: "model", Description: "Switch the model; if the context does not fit, compact first", Source: "builtin", Args: "<provider/model>"},
	{Name: "effort", Description: "Set the model's thinking level", Source: "builtin", Args: "<level>"},
}

// Commands returns the built-in commands and those of pi (last known).
func (m *Manager) Commands(ctx context.Context, chatID string) ([]Command, error) {
	c, err := m.st.GetChat(ctx, chatID)
	if err != nil {
		return nil, err
	}
	out := append([]Command(nil), builtinCommands...)
	for i := range out {
		switch out[i].Name {
		case "model":
			out[i].Options = m.modelOptions(c.Model)
		case "effort":
			out[i].Options = m.effortOptions(c.Model, c.ThinkingLevel)
		}
	}
	raw, err := m.st.Commands(ctx, chatID)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		m.mu.Lock()
		l := m.live[chatID]
		m.mu.Unlock()
		if l != nil {
			m.refreshInfo(ctx, chatID, l.slot.Worker)
			raw, _ = m.st.Commands(ctx, chatID)
		}
	}
	var pi []Command
	_ = json.Unmarshal(raw, &pi)
	return append(out, pi...), nil
}

// RunCommand runs a slash command. Anything that is not a built-in command
// goes to pi as a message (pi expands skills and prompt templates).
func (m *Manager) RunCommand(ctx context.Context, chatID, line string) (SendResult, error) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "/") {
		return m.Send(ctx, chatID, line)
	}
	name, args, _ := strings.Cut(strings.TrimPrefix(line, "/"), " ")
	args = strings.TrimSpace(args)
	switch name {
	case "model":
		_, err := m.SetModel(ctx, chatID, args, false)
		return SendResult{}, err
	case "effort":
		_, err := m.SetThinkingLevel(ctx, chatID, args)
		return SendResult{}, err
	case "rename":
		_, err := m.Rename(ctx, chatID, args)
		return SendResult{}, err
	case "autocompact":
		var on bool
		switch strings.ToLower(args) {
		case "on", "true":
			on = true
		case "off", "false":
		default:
			return SendResult{}, fmt.Errorf("/autocompact expects on or off")
		}
		_, err := m.SetAutoCompact(ctx, chatID, on)
		return SendResult{}, err
	case "compact":
		m.userActive(chatID)
		l, resumed, err := m.ensureLive(ctx, chatID)
		if err != nil {
			return SendResult{}, err
		}
		m.mu.Lock()
		running := l.running || l.compacting
		if !running {
			l.compacting = true
		}
		m.mu.Unlock()
		if running {
			return SendResult{Resumed: resumed}, ErrRunning
		}
		instr := compactLanguageHint
		if args != "" {
			instr += " " + args
		}
		cmd := map[string]any{"type": "compact", "customInstructions": instr}
		l.slot.SetActivity("compacting", "")
		m.touchIdle(chatID)
		// compact responds only after the summary; progress comes via
		// compaction_start/compaction_end.
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			if _, err := l.slot.Worker.Call(ctx, cmd); err != nil {
				m.mu.Lock()
				l.compacting = false
				m.mu.Unlock()
				m.setPendingModel(chatID, "") // a scheduled model switch is dropped
				m.publish(chatID, Event{Kind: "error", Data: map[string]string{"message": "Compaction failed: " + err.Error()}})
				l.slot.SetActivity("idle", "")
				return
			}
			// If pi reports no compaction_end (e.g. because there was nothing to compact), a
			// scheduled model switch would otherwise remain; after compaction_end it is already done.
			m.background(l, func() {
				m.mu.Lock()
				pending := m.pendingModel[chatID] != "" && !l.compacting // otherwise compaction_end handles it
				m.mu.Unlock()
				if pending {
					m.refreshInfo(context.Background(), chatID, l.slot.Worker)
					m.applyPendingModel(context.Background(), chatID)
				}
			})
		}()
		return SendResult{Resumed: resumed}, nil
	}
	return m.Send(ctx, chatID, line)
}

// maxTitle: length of a title in characters (runes).
const maxTitle = 120

// Rename sets the title (/rename); afterwards the orchestrator no longer names the chat itself.
func (m *Manager) Rename(ctx context.Context, chatID, title string) (ChatView, error) {
	title = strings.Join(strings.Fields(title), " ")
	if title == "" {
		return ChatView{}, fmt.Errorf("%w: /rename expects a name", ErrInvalid)
	}
	if r := []rune(title); len(r) > maxTitle {
		title = string(r[:maxTitle])
	}
	if err := m.st.SetTitle(ctx, chatID, title); err != nil {
		return ChatView{}, err
	}
	m.publishChat(ctx, chatID)
	return m.View(ctx, chatID)
}

// Titler phrases a chat title (package titler).
type Titler interface {
	Title(ctx context.Context, chatModel, text string) (titler.Result, error)
}

// autoTitle names a chat with a placeholder title after the user's first question: immediately with
// the shortened question, then (with Titler) once with a title from the model. That call is recorded
// in aux_llm_calls, not in llm_calls, so that cost and call counts per chat show only the agent's
// work.
func (m *Manager) autoTitle(ctx context.Context, chatID, text string) {
	if strings.TrimSpace(text) == "" || strings.HasPrefix(strings.TrimSpace(text), "/") {
		return
	}
	ok, err := m.st.AutoTitle(ctx, chatID, titleFrom(text))
	if err != nil {
		slog.Warn("title not set", "chat", chatID, "error", err)
		return
	}
	if !ok {
		return
	}
	m.publishChat(ctx, chatID)
	if m.opt.Titler == nil {
		return
	}
	c, err := m.st.GetChat(ctx, chatID)
	if err != nil {
		return
	}
	go m.modelTitle(chatID, c.Model, text)
}

func (m *Manager) modelTitle(chatID, model, text string) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	r, err := m.opt.Titler.Title(ctx, model, text)
	call := store.AuxCall{ChatID: chatID, Purpose: "title", Model: r.Model, Status: r.Status,
		Input: r.Usage.Input, Output: r.Usage.Output, CacheRead: r.Usage.CacheRead, Cost: r.Cost, Peak: r.Peak,
		StartedAt: r.Started, DurationMs: r.Duration.Milliseconds()}
	if call.StartedAt.IsZero() {
		call.StartedAt = time.Now()
	}
	if err != nil {
		call.Error = err.Error()
		slog.Warn("title from the model not received", "chat", chatID, "error", err)
	}
	if err := m.st.RecordAuxCall(ctx, call); err != nil {
		slog.Warn("title call not recorded", "chat", chatID, "error", err)
	}
	if r.Title == "" {
		return
	}
	if ok, err := m.st.ModelTitle(ctx, chatID, r.Title); err != nil {
		slog.Warn("title from the model not set", "chat", chatID, "error", err)
	} else if ok {
		m.publishChat(ctx, chatID)
	}
}

func (m *Manager) SetAutoCompact(ctx context.Context, chatID string, on bool) (ChatView, error) {
	unlock := m.lock(chatID)
	defer unlock()
	_, err := m.st.GetChat(ctx, chatID)
	if err != nil {
		return ChatView{}, err
	}
	m.mu.Lock()
	l := m.live[chatID]
	m.mu.Unlock()
	if l != nil {
		if _, err := callT(l.slot.Worker, map[string]any{"type": "set_auto_compaction", "enabled": on}, callTimeout); err != nil {
			return ChatView{}, err
		}
	}
	if err := m.st.SetAutoCompact(ctx, chatID, on); err != nil {
		return ChatView{}, err
	}
	m.publishChat(ctx, chatID)
	return m.View(ctx, chatID)
}

// answerExtensionUI answers queries from extensions (confirm, select,
// input, editor) with a cancellation. Without an answer the extension would wait
// forever; confirmations in this PoC go through the orchestrator, not through pi.
func (m *Manager) answerExtensionUI(chatID string, l *live, raw json.RawMessage) {
	var r struct {
		ID     string `json:"id"`
		Method string `json:"method"`
		Title  string `json:"title"`
	}
	if json.Unmarshal(raw, &r) != nil || r.ID == "" {
		return
	}
	switch r.Method {
	case "confirm", "select", "input", "editor":
		_ = l.slot.Worker.Notify(map[string]any{"type": "extension_ui_response", "id": r.ID, "cancelled": true})
		m.LogCall(l.slot.ID, chatID, "pi", "extension_ui", r.Method+": "+r.Title, "rejected")
	}
}

// resync checks after dropped events whether pi is still working.
func (m *Manager) resync(chatID string, l *live) {
	resp, err := callT(l.slot.Worker, map[string]any{"type": "get_state"}, callTimeout)
	if err != nil {
		return
	}
	var st struct {
		IsStreaming  bool `json:"isStreaming"`
		IsCompacting bool `json:"isCompacting"`
	}
	if json.Unmarshal(resp.Data, &st) == nil && !st.IsStreaming && !st.IsCompacting {
		m.setRunning(l, false)
		l.slot.SetActivity("idle", "")
		_ = m.saveSession(context.Background(), chatID, l.slot.Worker)
		m.publishChat(context.Background(), chatID)
	}
}

func (m *Manager) setRunning(l *live, v bool) {
	m.mu.Lock()
	if !v {
		l.runningSince = time.Time{}
	}
	l.running = v
	m.mu.Unlock()
}

// --- Artifacts and approval (backend of the socket) ---

func (m *Manager) ChatForSlot(slotID string) string { return m.SlotChat(slotID) }

func (m *Manager) LogCall(slotID, chatID, via, op, detail, result string) {
	m.LogCallBy(context.Background(), slotID, chatID, via, op, detail, result)
}

// LogCallBy logs a socket call together with session and tool call from the context
// (sock.CallerLogger): this way the log shows whether the main agent or a subagent asked.
func (m *Manager) LogCallBy(ctx context.Context, slotID, chatID, via, op, detail, result string) {
	sc, err := m.st.AddSocketCall(context.WithoutCancel(ctx), store.SocketCall{ChatID: chatID, SlotID: slotID, Via: via, Op: op, Detail: detail, Result: result,
		Session: store.SessionFrom(ctx), ToolCallID: store.ToolCallFrom(ctx)})
	if err != nil {
		slog.Error("socket call not logged", "error", err)
		return
	}
	slog.Info("socket call", "slot", slotID, "chat", chatID, "via", via, "op", op, "detail", detail, "result", result)
	if chatID != "" {
		m.publish(chatID, Event{Kind: "socket_call", Data: sc})
	}
}

func contentType(name string, head []byte) string {
	if ct := mime.TypeByExtension(strings.ToLower(filepath.Ext(name))); ct != "" {
		return ct
	}
	if artifacts.IsText(head) {
		return "text/plain; charset=utf-8"
	}
	return "application/octet-stream"
}

func objectKey(chatID, kind, name string) string { return path.Join(chatID, kind, name) }

// Upload stores the file as pending and waits for the user's decision.
// Timeout and cancellation count as rejection.
func (m *Manager) Upload(ctx context.Context, chatID, slotID, via, name string, size int64, sha string, body io.Reader) (sock.UploadResult, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return sock.UploadResult{}, err
	}
	if err := m.checkPendingLimit(ctx, chatID); err != nil {
		return sock.UploadResult{}, err
	}
	sum := sha256.Sum256(data)
	got := hex.EncodeToString(sum[:])
	if sha != "" && !strings.EqualFold(sha, got) {
		return sock.UploadResult{Status: "rejected", Name: name, Size: int64(len(data)), SHA256: got, Message: "checksum does not match"}, nil
	}
	ct := contentType(name, data)
	preview := ""
	if artifacts.IsText(data) {
		p := data
		if len(p) > 4096 {
			p = p[:4096]
		}
		preview = strings.ToValidUTF8(string(p), "")
	}
	pendingKey := path.Join("pending", chatID, fmt.Sprintf("%d-%s", time.Now().UnixNano(), name))
	if err := m.blobs.Put(ctx, pendingKey, bytes.NewReader(data), int64(len(data)), ct); err != nil {
		return sock.UploadResult{}, fmt.Errorf("store: %w", err)
	}
	ap, err := m.st.CreateApproval(ctx, store.Approval{ChatID: chatID, Kind: "artifact_upload", Via: via, Name: name, Size: int64(len(data)), SHA256: got, ContentType: ct, PendingKey: pendingKey, Preview: preview,
		Session: store.SessionFrom(ctx), ToolCallID: store.ToolCallFrom(ctx)})
	if err != nil {
		_ = m.blobs.Delete(ctx, pendingKey)
		return sock.UploadResult{}, err
	}
	w := m.broker.Register(ap.ID)
	var prevAct *pool.Activity
	if s, ok := m.pool.Get(slotID); ok {
		prevAct = s.Info().Activity
		s.SetActivity("waiting_approval", "")
	}
	m.publish(chatID, Event{Kind: "approval", Data: ap})
	m.publishChat(ctx, chatID)
	slog.Info("approval requested", "chat", chatID, "approval", ap.ID, "name", name, "bytes", len(data), "via", via)

	approved, werr := w.Wait(ctx, m.opt.ApprovalTimeout)
	bg := context.WithoutCancel(ctx)
	final := m.settle(bg, chatID, ap.ID, approved, werr)
	msg := "rejected by the user"
	if final == store.ApprovalExpired {
		msg = "no decision within the waiting time"
	}
	if s, ok := m.pool.Get(slotID); ok {
		if prevAct != nil && prevAct.Kind == "tool" {
			s.SetActivity("tool", prevAct.Tool)
		} else {
			s.SetActivity("thinking", "")
		}
	}
	res := sock.UploadResult{Name: name, Size: int64(len(data)), SHA256: got}
	if final != store.ApprovalApproved {
		_ = m.blobs.Delete(bg, pendingKey)
		res.Status, res.Message = "rejected", msg
		m.publishChat(bg, chatID)
		return res, nil
	}
	key := objectKey(chatID, store.KindOutput, name)
	if err := m.blobs.Move(bg, pendingKey, key); err != nil {
		return res, fmt.Errorf("store after approval: %w", err)
	}
	art := store.Artifact{ChatID: chatID, Kind: store.KindOutput, Name: name, Size: int64(len(data)), SHA256: got, ContentType: ct, Via: via, ObjectKey: key,
		ToolCallID: store.ToolCallFrom(ctx)}
	if err := m.st.PutArtifact(bg, art); err != nil {
		return res, err
	}
	art.CreatedAt = time.Now()
	m.publish(chatID, Event{Kind: "artifact", Data: art})
	m.publishChat(bg, chatID)
	res.Status = "approved"
	return res, nil
}

// RequestInternet: the agent asks for internet access. The call waits for
// the user's decision; only on approval is the sandbox connected to the
// egress network.
func (m *Manager) RequestInternet(ctx context.Context, chatID, slotID, via, reason string) (sock.UploadResult, error) {
	c, err := m.st.GetChat(ctx, chatID)
	if err != nil {
		return sock.UploadResult{}, err
	}
	if c.Internet {
		return sock.UploadResult{Status: "approved", Name: "internet", Message: "internet access is already granted"}, nil
	}
	if err := m.checkPendingLimit(ctx, chatID); err != nil {
		return sock.UploadResult{}, err
	}
	reason = strings.TrimSpace(strings.ToValidUTF8(reason, ""))
	if r := []rune(reason); len(r) > 500 {
		reason = string(r[:500]) + " …"
	}
	if reason == "" {
		reason = "(no reason given)"
	}
	ap, err := m.st.CreateApproval(ctx, store.Approval{ChatID: chatID, Kind: "internet_access", Via: via, Name: reason, ContentType: "text/plain", PendingKey: "-", Preview: reason,
		Session: store.SessionFrom(ctx), ToolCallID: store.ToolCallFrom(ctx)})
	if err != nil {
		return sock.UploadResult{}, err
	}
	w := m.broker.Register(ap.ID)
	if s, ok := m.pool.Get(slotID); ok {
		s.SetActivity("waiting_approval", "")
	}
	m.publish(chatID, Event{Kind: "approval", Data: ap})
	m.publishChat(ctx, chatID)
	slog.Info("internet access requested", "chat", chatID, "approval", ap.ID, "via", via)
	approved, werr := w.Wait(ctx, m.opt.ApprovalTimeout)
	bg := context.WithoutCancel(ctx)
	final := m.settle(bg, chatID, ap.ID, approved, werr)
	if s, ok := m.pool.Get(slotID); ok {
		s.SetActivity("tool", "")
	}
	if final == store.ApprovalExpired {
		m.publishChat(bg, chatID)
		return sock.UploadResult{Status: "rejected", Name: "internet", Message: "no decision within the waiting time"}, nil
	}
	if final != store.ApprovalApproved {
		m.publishChat(bg, chatID)
		return sock.UploadResult{Status: "rejected", Name: "internet", Message: "rejected by the user"}, nil
	}
	if _, err := m.SetInternet(bg, chatID, true); err != nil {
		return sock.UploadResult{}, err
	}
	return sock.UploadResult{Status: "approved", Name: "internet", Message: "internet access granted"}, nil
}

// visibleControls writes control and bidi characters as \u sequences so that the preview of an
// approval does not visually reorder or hide anything (Review W1). Newline and tab stay.
func visibleControls(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f || (r >= 0x200b && r <= 0x200f) || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) || r == 0xfeff:
			fmt.Fprintf(&b, "\\u%04x", r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// maxPlatformPreview limits method, path and indented body of a writing platform call.
const maxPlatformPreview = 16000

// PlatformExchanged logs a token exchange for a chat (platform.Client.OnExchange):
// who is in the token (sub, azp, aud), until when it is valid and that no act claim comes.
func (m *Manager) PlatformExchanged(chatID string, cl platform.Claims) {
	m.LogCall(m.slotOf(chatID), chatID, "orchestrator", "token_exchange", cl.String(), "ok")
}

// PlatformCall performs a call to the Agri-Gaia platform. Reading calls (GET)
// go straight through; everything else waits for the user's approval and is
// only executed on approval. The approval shows method, path and body.
func (m *Manager) PlatformCall(ctx context.Context, chatID, slotID, via string, req platform.Request) (platform.Result, error) {
	if m.opt.Platform == nil {
		return platform.Result{Status: "error", Message: platform.ErrNotConfigured.Error()}, nil
	}
	// Check here too, not only at the caller: Writes() assumes a normalized method (Review W2).
	req, err := platform.Normalize(req)
	if err != nil {
		return platform.Result{Status: "error", Message: err.Error()}, nil
	}
	del, err := m.delegationOf(ctx, chatID)
	if err != nil {
		// An unreadable delegation grants nothing.
		return platform.Result{Status: "denied", Message: err.Error()}, nil
	}
	if req.Path == platform.RightsPath {
		return platform.Result{Status: "ok", HTTPStatus: 200, Body: m.rightsText(ctx, chatID, del)}, nil
	}
	// Delegation (step 1): classification from method and path alone, check before any confirmation.
	access := delegation.Classify(req)
	violation := ""
	if del != nil {
		own := func(res, id string) bool {
			ok, _ := m.st.IsDelegationObject(ctx, chatID, res, id)
			return ok
		}
		if dec := del.Check(access, time.Now(), own); !dec.Allowed {
			if del.Enforcing() {
				return platform.Result{Status: "denied", Message: dec.Reason}, nil
			}
			violation = dec.Reason // stage without protective measure: only log
		}
	}
	if req.Writes() && (del == nil || del.ConfirmWrites()) {
		if err := m.checkPendingLimit(ctx, chatID); err != nil {
			return platform.Result{}, err
		}
		preview := req.String() + "\nClassification: " + access.String()
		if del != nil && violation == "" {
			preview += " (within the delegated rights)"
		}
		if d := req.Describe(); d != "" {
			preview += "\n\n" + d
		}
		if len(req.Body) > 0 {
			var buf bytes.Buffer
			if json.Indent(&buf, req.Body, "", "  ") == nil {
				preview += "\n\n" + buf.String()
			} else {
				preview += "\n\n" + string(req.Body)
			}
		}
		preview = visibleControls(preview)
		// The user only approves what they see in full: better refuse than truncate (Review W1).
		if n := len([]rune(preview)); n > maxPlatformPreview {
			return platform.Result{Status: "error", Message: fmt.Sprintf("call too large for an approval (%d characters, at most %d); shrink the body", n, maxPlatformPreview)}, nil
		}
		size := int64(len(req.Body))
		for _, u := range req.Uploads {
			size += int64(len(u.Data))
		}
		ap, err := m.st.CreateApproval(ctx, store.Approval{ChatID: chatID, Kind: "platform_write", Via: via, Name: req.String(), Size: size,
			ContentType: "application/json", PendingKey: "-", Preview: preview, Session: store.SessionFrom(ctx), ToolCallID: store.ToolCallFrom(ctx)})
		if err != nil {
			return platform.Result{}, err
		}
		w := m.broker.Register(ap.ID)
		if s, ok := m.pool.Get(slotID); ok {
			s.SetActivity("waiting_approval", "")
		}
		m.publish(chatID, Event{Kind: "approval", Data: ap})
		m.publishChat(ctx, chatID)
		slog.Info("platform call requested", "chat", chatID, "approval", ap.ID, "call", req.String(), "via", via)
		approved, werr := w.Wait(ctx, m.opt.ApprovalTimeout)
		bg := context.WithoutCancel(ctx)
		final := m.settle(bg, chatID, ap.ID, approved, werr)
		if s, ok := m.pool.Get(slotID); ok {
			s.SetActivity("tool", "")
		}
		m.publishChat(bg, chatID)
		if final == store.ApprovalExpired {
			return platform.Result{Status: "rejected", Message: "no decision within the waiting time; nothing executed"}, nil
		}
		if final != store.ApprovalApproved {
			return platform.Result{Status: "rejected", Message: "rejected by the user; nothing executed"}, nil
		}
		ctx = bg
		// The delegation may have expired during the waiting time (Review 5, W3): reload and check again.
		if del != nil {
			fresh, err := m.delegationOf(ctx, chatID)
			if err != nil || fresh == nil {
				return platform.Result{Status: "denied", Message: "delegation no longer readable after the approval"}, nil
			}
			own := func(res, id string) bool {
				ok, _ := m.st.IsDelegationObject(ctx, chatID, res, id)
				return ok
			}
			if dec := fresh.Check(access, time.Now(), own); !dec.Allowed && fresh.Enforcing() {
				return platform.Result{Status: "denied", Message: dec.Reason + " (checked again after the approval)"}, nil
			}
		}
	}
	res, err := m.opt.Platform.Do(ctx, chatID, req)
	if err != nil {
		return platform.Result{Status: "error", Message: err.Error(), Violation: violation}, nil
	}
	res.Violation = violation
	if del != nil {
		if r, id, ok := delegation.Created(access, res); ok {
			if err := m.st.AddDelegationObject(context.WithoutCancel(ctx), chatID, r, id); err != nil {
				slog.Error("provenance not recorded", "chat", chatID, "object", r+" "+id, "error", err)
			}
		}
	}
	return res, nil
}

// PlatformPrecheck checks a call against the delegation without executing it. The socket calls
// it before reading the files of an upload so that a violation does not read 512 MB from the
// sandbox (Review 5, M7). The check in PlatformCall remains authoritative.
func (m *Manager) PlatformPrecheck(ctx context.Context, chatID string, req platform.Request) (platform.Result, bool) {
	del, err := m.delegationOf(ctx, chatID)
	if err != nil {
		return platform.Result{Status: "denied", Message: err.Error()}, false
	}
	if del == nil || !del.Enforcing() {
		return platform.Result{}, true
	}
	own := func(res, id string) bool {
		ok, _ := m.st.IsDelegationObject(ctx, chatID, res, id)
		return ok
	}
	if dec := del.Check(delegation.Classify(req), time.Now(), own); !dec.Allowed {
		return platform.Result{Status: "denied", Message: dec.Reason}, false
	}
	return platform.Result{}, true
}

// delegationOf reads a chat's delegation (nil: without delegation).
func (m *Manager) delegationOf(ctx context.Context, chatID string) (*delegation.Delegation, error) {
	c, err := m.st.GetChat(ctx, chatID)
	if err != nil {
		return nil, err
	}
	if len(c.Delegation) == 0 {
		return nil, nil
	}
	d, err := delegation.Parse(c.Delegation)
	if err != nil {
		return nil, fmt.Errorf("chat's delegation unreadable: %w", err)
	}
	return d, nil
}

// rightsText describes to the agent its rights together with the objects it has created.
func (m *Manager) rightsText(ctx context.Context, chatID string, del *delegation.Delegation) string {
	if del == nil {
		return "No delegation is set for this chat: reading works without confirmation, every writing call needs the user's approval."
	}
	s := del.Summary()
	if objs, err := m.st.ListDelegationObjects(ctx, chatID); err == nil && len(objs) > 0 {
		s += "\nCreated in this chat:"
		for _, o := range objs {
			s += "\n  " + o.Resource + " " + o.ObjectID
		}
	}
	return s
}

// checkPendingLimit limits pending approvals per chat (Review H2).
func (m *Manager) checkPendingLimit(ctx context.Context, chatID string) error {
	aps, err := m.st.ListApprovals(ctx, store.ApprovalPending, chatID)
	if err != nil {
		return err
	}
	if len(aps) >= maxPendingPerChat {
		return ErrTooManyPending
	}
	return nil
}

// settle returns the final state of an approval. If the waiting time
// expires while the user is deciding, the decision from the database
// applies, not the expiry (Review M7).
func (m *Manager) settle(ctx context.Context, chatID, id string, approved bool, werr error) string {
	if werr == nil {
		if approved {
			return store.ApprovalApproved
		}
		return store.ApprovalRejected
	}
	a, ok, err := m.st.DecideApproval(ctx, id, store.ApprovalExpired)
	if err != nil {
		return store.ApprovalExpired
	}
	if ok {
		m.publish(chatID, Event{Kind: "approval", Data: a})
	}
	return a.State
}

// Decide is the user's decision from the API.
func (m *Manager) Decide(ctx context.Context, approvalID string, approve bool) (store.Approval, error) {
	state := store.ApprovalRejected
	if approve {
		state = store.ApprovalApproved
	}
	ap, ok, err := m.st.DecideApproval(ctx, approvalID, state)
	if err != nil {
		return ap, err
	}
	if ok {
		m.broker.Resolve(approvalID, approve)
		m.publish(ap.ChatID, Event{Kind: "approval", Data: ap})
		m.publishChat(ctx, ap.ChatID)
		slog.Info("approval decided", "approval", approvalID, "approved", approve)
	}
	return ap, nil
}

func (m *Manager) rejectPending(ctx context.Context, chatID, why string) {
	aps, err := m.st.ListApprovals(ctx, store.ApprovalPending, chatID)
	if err != nil {
		return
	}
	for _, a := range aps {
		if d, ok, _ := m.st.DecideApproval(ctx, a.ID, store.ApprovalRejected); ok {
			m.broker.Resolve(a.ID, false)
			m.publish(chatID, Event{Kind: "approval", Data: d})
			slog.Info("pending approval rejected", "approval", a.ID, "reason", why)
		}
	}
}

func (m *Manager) ListArtifacts(ctx context.Context, chatID string) ([]store.Artifact, error) {
	return m.st.ListArtifacts(ctx, chatID)
}

func (m *Manager) OpenArtifact(ctx context.Context, chatID, kind, name string) (io.ReadCloser, int64, error) {
	a, err := m.st.GetArtifact(ctx, chatID, kind, name)
	if err != nil {
		return nil, 0, err
	}
	return m.blobs.Get(ctx, a.ObjectKey)
}

// --- The user's inputs ---

const inputsDir = "/workspace/inputs"

// AddInput stores a file uploaded by the user and, for an active chat,
// mirrors it into the sandbox immediately.
func (m *Manager) AddInput(ctx context.Context, chatID, name string, data []byte) (store.Artifact, error) {
	_, err := m.st.GetChat(ctx, chatID)
	if err != nil {
		return store.Artifact{}, err
	}
	name = artifacts.SanitizeName(name)
	if name == "" {
		return store.Artifact{}, errors.New("invalid file name")
	}
	if int64(len(data)) > m.opt.ArtifactMaxBytes {
		return store.Artifact{}, fmt.Errorf("file larger than %d MB", m.opt.ArtifactMaxBytes>>20)
	}
	sum := sha256.Sum256(data)
	ct := contentType(name, data)
	key := objectKey(chatID, store.KindInput, name)
	if err := m.blobs.Put(ctx, key, bytes.NewReader(data), int64(len(data)), ct); err != nil {
		return store.Artifact{}, err
	}
	art := store.Artifact{ChatID: chatID, Kind: store.KindInput, Name: name, Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:]), ContentType: ct, Via: "ui", ObjectKey: key}
	if err := m.st.PutArtifact(ctx, art); err != nil {
		return store.Artifact{}, err
	}
	art.CreatedAt = time.Now()
	m.mu.Lock()
	l := m.live[chatID]
	m.mu.Unlock()
	if l != nil {
		if err := m.pushInput(ctx, l.slot.Worker, name, data); err != nil {
			slog.Warn("input not mirrored into the sandbox", "chat", chatID, "name", name, "error", err)
		}
	}
	m.publish(chatID, Event{Kind: "artifact", Data: art})
	m.publishChat(ctx, chatID)
	return art, nil
}

func (m *Manager) pushInput(ctx context.Context, a Agent, name string, data []byte) error {
	target := inputsDir + "/" + name
	_, err := a.Exec(ctx, []string{"sh", "-c", `mkdir -p "$1" && cat > "$2"`, "sh", inputsDir, target}, bytes.NewReader(data))
	return err
}

// syncInputs mirrors all of the chat's inputs into the sandbox; returns count and total size.
func (m *Manager) syncInputs(ctx context.Context, chatID string, a Agent) (files int, size int64, err error) {
	arts, err := m.st.ListArtifacts(ctx, chatID)
	if err != nil {
		return 0, 0, err
	}
	for _, art := range arts {
		if art.Kind != store.KindInput {
			continue
		}
		rc, _, err := m.blobs.Get(ctx, art.ObjectKey)
		if err != nil {
			return files, size, err
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return files, size, err
		}
		if err := m.pushInput(ctx, a, art.Name, data); err != nil {
			return files, size, err
		}
		files++
		size += int64(len(data))
	}
	return files, size, nil
}

// --- Start and end of the orchestrator ---

// Recover sets active chats to dormant after a restart (their sandboxes
// no longer exist) and pending approvals to expired.
func (m *Manager) Recover(ctx context.Context) error {
	n, err := m.st.ExpirePendingApprovals(ctx)
	if err != nil {
		return err
	}
	active, err := m.st.ChatsInState(ctx, store.StateActive)
	if err != nil {
		return err
	}
	for _, c := range active {
		_ = m.st.SetState(ctx, c.ID, store.StateDormant)
	}
	// Background tasks have disappeared with the sandboxes; tell the agent with the next request.
	if nbg, err := m.st.EndAllRunningBackground(ctx, "orchestrator restarted"); err != nil {
		return err
	} else if nbg > 0 {
		slog.Info("background tasks ended after restart", "count", nbg)
	}
	if n > 0 || len(active) > 0 {
		slog.Info("cleaned up after restart", "expired_approvals", n, "set_dormant", len(active))
	}
	return nil
}

// Shutdown saves the sessions of all active chats and makes them idle.
func (m *Manager) Shutdown(ctx context.Context) {
	m.mu.Lock()
	ids := make([]string, 0, len(m.live))
	for id := range m.live {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	for _, id := range ids {
		m.rejectPending(ctx, id, "orchestrator stopped")
		m.mu.Lock()
		l := m.live[id]
		m.mu.Unlock()
		if l == nil {
			continue
		}
		m.detach(ctx, id, l, true, true)
		_ = m.st.SetState(ctx, id, store.StateDormant)
	}
}

// Messages, Approvals and SocketCalls for the chat view.
func (m *Manager) Messages(ctx context.Context, chatID string) ([]store.Message, error) {
	return m.st.Messages(ctx, chatID)
}

// ChatOwner returns a chat's owner (empty: without owner, token mode).
func (m *Manager) ChatOwner(ctx context.Context, chatID string) (string, error) {
	return m.st.ChatOwner(ctx, chatID)
}

// ApprovalChat returns the chat of an approval.
func (m *Manager) ApprovalChat(ctx context.Context, approvalID string) (string, error) {
	ap, err := m.st.GetApproval(ctx, approvalID)
	if err != nil {
		return "", err
	}
	return ap.ChatID, nil
}

func (m *Manager) Approvals(ctx context.Context, state, chatID string) ([]store.Approval, error) {
	return m.st.ListApprovals(ctx, state, chatID)
}
func (m *Manager) SocketCalls(ctx context.Context, chatID string) ([]store.SocketCall, error) {
	return m.st.ListSocketCalls(ctx, chatID)
}
func (m *Manager) Session(ctx context.Context, chatID string) ([]byte, error) {
	m.mu.Lock()
	l := m.live[chatID]
	m.mu.Unlock()
	if l != nil {
		_ = m.saveSession(ctx, chatID, l.slot.Worker)
	}
	return m.st.LoadSession(ctx, chatID)
}
func (m *Manager) Totals(ctx context.Context) (store.Tokens, float64, error) { return m.st.Totals(ctx) }

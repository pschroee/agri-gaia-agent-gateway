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
	"net/http"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/singleflight"

	"agw/internal/artifacts"
	"agw/internal/config"
	"agw/internal/delegation"
	"agw/internal/netgrant"
	"agw/internal/plan"
	"agw/internal/platform"
	"agw/internal/pool"
	"agw/internal/rpc"
	"agw/internal/sock"
	"agw/internal/store"
	"agw/internal/titler"
	"agw/internal/toolset"
	"agw/internal/watch"
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
	// NetBytes is the sum of the bytes the execution sandbox's network interfaces have sent and received (Docker's
	// counters; the byte count of a full internet grant).
	NetBytes(ctx context.Context) (int64, error)
	// SetExecTrace switches the syscall trace of the execution sandbox for this run (X7.6/#152).
	// A running supervisor is restarted; the slot's default stays the fallback.
	SetExecTrace(ctx context.Context, on bool) error
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
	// ErrVariantFixed: a client asked for another binding than AGW_TOOLSETS gives new chats (issue #29).
	ErrVariantFixed = errors.New("the bindings of new chats are fixed by the gateway (AGW_TOOLSETS)")
	// ErrExperimentNotAllowed: the owner is not in AGW_EXPERIMENT_USERS (X0.2).
	ErrExperimentNotAllowed = errors.New("experiment chats are not allowed for this account (AGW_EXPERIMENT_USERS)")
	// ErrExperimentNeedsDelegation: an experiment chat without delegated rights has no protection level.
	ErrExperimentNeedsDelegation = errors.New("an experiment chat needs a delegation")
	// ErrExperimentProtection: the protection level is not none, instruction or service.
	ErrExperimentProtection = errors.New("unknown protection level; allowed are none, instruction and service")
	// ErrExperimentTrace: the per-run syscall trace setting is not on or off (X7.6/#152).
	ErrExperimentTrace = errors.New(`unknown syscall trace setting; allowed are "on" and "off"`)
	ErrNoSlot          = pool.ErrNoIdleSlot
	ErrNotFound        = store.ErrNotFound
	// ErrAlreadySent: the input went with a message and is not removed any more (issue L4/#121, RemoveInput).
	ErrAlreadySent = store.ErrAlreadySent
)

// Event is distributed to a chat's SSE subscribers.
type Event struct {
	Kind string `json:"kind"`
	Data any    `json:"data"`
}

// ChatEvent is an event of one chat as seen by the subscribers of all chats (SubscribeAll).
type ChatEvent struct {
	ChatID string
	Event
}

// crossChatKinds: event kinds that also go to the subscribers of all chats. "plan_alert" (A-E4, D4.3) is
// carried by the same stream as an approval and filtered by the same owner check (GET /api/events).
var crossChatKinds = map[string]bool{"approval": true, "plan_alert": true, KindPlatformChanged: true}

type ChatView struct {
	store.Chat
	Running bool   `json:"running"`
	SlotID  string `json:"slot_id,omitempty"`
	// Resuming: the chat is being resumed in a fresh sandbox.
	Resuming bool `json:"resuming"`
	// Starting: a new chat created with Async waits for its first sandbox (Resuming is set as well, so
	// clients that only know resuming keep working).
	Starting bool `json:"starting,omitempty"`
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
	// SubagentsRunning: subagents running right now according to the monitoring (0 while idle).
	SubagentsRunning int `json:"subagents_running"`
	// InternetGrants: the chat's active internet grants with their counters (issue #175); Internet stays true while any is active.
	InternetGrants []store.Grant `json:"internet_grants"`
	// InternetBlocked: refused connections of the chat since the oldest active grant began (15 minutes back when
	// none is active), with the latest three; nil when there is none (issue #175).
	InternetBlocked *BlockedView `json:"internet_blocked,omitempty"`
}

type live struct {
	slot    *pool.Slot[Agent]
	runID   int64
	running bool
	idle    *time.Timer
	stop    chan struct{}
	stopped sync.Once

	ip string // address in the slot network
	// egress: the execution sandbox is attached to the egress network (an active full grant; under m.mu).
	egress bool
	maxSub int // at most this many subagents at the same time (Options.MaxSubagents)
	// subRuns: what the monitoring knows about the subagent runs of this sandbox (under pollMu);
	// subRunning: how many of them run right now (under m.mu, for the view).
	pollMu     sync.Mutex
	subRuns    map[string]*subTrack
	subRunning int

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
	// InternetLimits: defaults and bounds of the time and data of an internet grant (AGW_INTERNET_*, issue #175);
	// zero values take netgrant.DefaultLimits. InternetLogRows: single rows per grant in web_requests (0: 500).
	InternetLimits  netgrant.Limits
	InternetLogRows int

	AutoCompactDefault   bool
	CompactReserveTokens int
	CompactKeepRecent    int

	// MaxSubagents: at most this many subagents run at the same time per chat (AGW_MAX_SUBAGENTS).
	// Fixed for the service: there is no setting per chat, and stored values of older chats are ignored.
	MaxSubagents int
	// ComputeMaxPerChat: compute calls per chat the platform may accept (AGW_COMPUTE_MAX_PER_CHAT, below 1: default).
	ComputeMaxPerChat int
	// A-E1: upper bounds of an autonomous plan's budget (AGW_PLAN_MAX_DURATION, AGW_PLAN_MAX_LLM_COST);
	// zero takes the config defaults.
	PlanMaxDuration time.Duration
	PlanMaxLLMCost  float64
	// X3: limits of an experiment run at the model pass-through (AGW_EXP_*, placeholders until XE5). A value
	// of 0 turns the limit off; the proxy sets temperature only where the catalog says the model takes it.
	ExpMaxCostUSD  float64
	ExpMaxTokens   int64
	ExpMaxDuration time.Duration
	ExpTemperature float64
	// TrashRetentionDays: chats in the trash are deleted for good after this many days (AGW_TRASH_RETENTION_DAYS,
	// 0 = never; issue #76).
	TrashRetentionDays int

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

	// SearchMaxHours: a training search ends after this many hours at the latest (0: config.DefaultSearchMaxHours).
	SearchMaxHours int

	// Platform talks to the Agri-Gaia platform (nil: binding off).
	Platform *platform.Client

	// Sessions is the login side of the gateway (A-E3): while an autonomous plan of a user is approved, the
	// endurance loop renews that user's session, and plans a lost login had paused continue with the next
	// login. nil in the token mode: there is no session to keep fresh and none to continue a plan with.
	Sessions SessionKeeper

	// Notebook runs the chat's own code in its notebook server in JupyterHub (own code C-E3); nil
	// when AGW_JUPYTERHUB_API_URL is not set. main.go registers it as the doer of the notebook paths.
	Notebook Notebooker

	// Checks answers the capability "checks" (R0.23): can this setup check a result right now, and how
	// many checks are left in this chat. nil: the entry says that the gateway does not check results.
	Checks Checker

	// Toolsets: the bindings every new chat gets (AGW_TOOLSETS, issue #29); empty = toolset.Default.
	// Stored chats keep the combination they were created with.
	Toolsets toolset.Set

	// ExperimentUsers: accounts (preferred_username) that may create experiment chats
	// (AGW_EXPERIMENT_USERS, X0.2). Empty: no experiment chats, also not through the API.
	ExperimentUsers []string
}

type Manager struct {
	st     *store.Store
	pool   *pool.Pool[Agent]
	cat    *config.Catalog
	blobs  Blobs
	broker *artifacts.Broker
	opt    Options
	// relayWait: replies awaited for relay commands to subagents, by ref (under mu; issue #81).
	relayWait map[string]chan relayReply
	// stopRefs: refs of stops of subagents on their way, mapped to the chat they belong to (under mu; issue #120).
	// A stop has no stored message that could vouch for its ref (DL3.3); the socket call subagent_stop is its record.
	stopRefs map[string]string
	// jobPolls: one poll per observed job at a time; the watcher and the plan wait guard share it (issue #180).
	jobPolls singleflight.Group

	mu   sync.Mutex
	live map[string]*live
	subs map[string]map[chan Event]struct{}
	// allSubs: subscribers of approval events of every chat (GET /api/events); the API filters by owner.
	allSubs map[chan ChatEvent]struct{}
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
	// delivering: messages in deliver (claimed, "delivered" published, prompt not answered yet,
	// e.g. while resuming); with pendingTurns the source of DeliveredQueue.
	delivering map[string][]*turnMeta
	// resuming: the chat is being resumed (for display).
	resuming map[string]bool
	// starting: a new chat waits for its first sandbox (Create with Async, for display).
	starting map[string]bool
	// pendingModel: model switch waiting for the end of a compaction (SetModel with compactFirst).
	pendingModel map[string]string
	// levels: thinking levels per model, as pi last reported them (get_available_thinking_levels).
	levels map[string][]string
	// endSeen: background tasks (chat -> seq) whose end the main agent read with bg_output before the
	// note about it was enqueued; the note is then dropped (issue #197, under m.mu).
	endSeen map[string]map[int]bool
	// userAt: the user's last action per chat (send, abort, remove, stop, resume);
	// decisive for the postponement by background tasks (Review 3, M1).
	userAt map[string]time.Time
	// aborts counts the aborts per chat; a request during which an abort happened does not go to pi.
	aborts map[string]uint64
	// rechecks: how to re-read the summary of a pending approval at the moment of the decision (issue L2/#119).
	rechecks map[string]recheck
	// planDecisions: how to record the outcome of a pending plan approval, before the waiting call wakes (issue #166).
	planDecisions map[string]*planDecision
	// apMu serializes the decisions on one approval (two clicks at the same time).
	apMu map[string]*sync.Mutex
	// virtuals: doers for /_agw/ paths that the gateway answers itself (G1.5), by path prefix.
	virtuals map[string]VirtualDoer
	// capCache: capabilities that read platform state, held in memory for capabilityCacheTTL
	// (G1.11), by chat and key.
	capCache map[string]capabilityCacheEntry
	// planCache: the active autonomous plan of a chat, held in memory for planCacheTTL so that the
	// guard and the LLM proxy's budget do not put a query behind every call (A-E1).
	planCache map[string]planCacheEntry
	// expCache: the model and experiment profile of a chat, held in memory for planCacheTTL so that the
	// LLM proxy does not put a query behind every call (X3).
	expCache map[string]expGateEntry
	// planAlertMarks: the state each autonomous plan was last published in, so that its alert goes out
	// once per state change (A-E4, D4.3); by plan id.
	planAlertMarks map[string]string
	// grants: the active internet grants of all chats with their counters (issue #175).
	grants *grantBook
	// inst: installed checks of packages grants per chat (issue #175).
	instMu sync.Mutex
	inst   map[string]*instState
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
	if opt.Toolsets.Empty() {
		opt.Toolsets, _ = toolset.Parse(toolset.Default)
	}
	// A-E1: the budget limits of autonomous plans fall back to the config defaults.
	if opt.PlanMaxDuration <= 0 {
		opt.PlanMaxDuration = config.DefaultPlanMaxDuration
	}
	if opt.PlanMaxLLMCost <= 0 {
		opt.PlanMaxLLMCost = config.DefaultPlanMaxLLMCost
	}
	return &Manager{st: st, pool: p, cat: cat, blobs: blobs, broker: broker, opt: opt,
		live: map[string]*live{}, subs: map[string]map[chan Event]struct{}{}, allSubs: map[chan ChatEvent]struct{}{}, chatMu: map[string]*sync.Mutex{},
		imgMu: map[string]*sync.Mutex{}, wsMu: map[string]*sync.Mutex{}, qMu: map[string]*sync.Mutex{},
		sending: map[string]bool{}, delivering: map[string][]*turnMeta{}, resuming: map[string]bool{}, starting: map[string]bool{}, pendingModel: map[string]string{}, levels: map[string][]string{}, userAt: map[string]time.Time{}, endSeen: map[string]map[int]bool{}, aborts: map[string]uint64{},
		rechecks: map[string]recheck{}, apMu: map[string]*sync.Mutex{}, planCache: map[string]planCacheEntry{},
		expCache: map[string]expGateEntry{}, grants: newGrantBook()}
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

// SubscribeAll delivers the approval events (new and decided) of every chat, with the chat's ID, for the
// stream across chats (GET /api/events). It carries no ownership check: the caller filters by owner.
func (m *Manager) SubscribeAll() (<-chan ChatEvent, func()) {
	ch := make(chan ChatEvent, 256)
	m.mu.Lock()
	m.allSubs[ch] = struct{}{}
	m.mu.Unlock()
	return ch, func() {
		m.mu.Lock()
		delete(m.allSubs, ch)
		m.mu.Unlock()
	}
}

func (m *Manager) publish(chatID string, ev Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	important := ev.Kind == "approval" || ev.Kind == "chat" || ev.Kind == "error" || ev.Kind == "plan" || ev.Kind == "search"
	for ch := range m.subs[chatID] {
		sendDropOldest(ch, ev, important)
	}
	if crossChatKinds[ev.Kind] {
		for ch := range m.allSubs {
			sendDropOldest(ch, ChatEvent{ChatID: chatID, Event: ev}, true)
		}
	}
}

// sendDropOldest hands ev to a subscriber without blocking. With a full buffer an unimportant event is
// dropped (slow reader: do not hold up the chat); approvals, plans and state changes must not get lost, so the
// oldest buffered event makes room for them.
func sendDropOldest[T any](ch chan T, ev T, important bool) {
	select {
	case ch <- ev:
		return
	default:
	}
	if !important {
		return
	}
	select {
	case <-ch:
	default:
	}
	select {
	case ch <- ev:
	default:
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
	v := m.view(c)
	v.InternetBlocked = m.blockedView(ctx, chatID)
	return v, nil
}

func (m *Manager) view(c store.Chat) ChatView {
	v := ChatView{Chat: c}
	// The limit is the service's, also for chats that stored another value before issue #24.
	v.MaxSubagents = m.opt.MaxSubagents
	v.InternetGrants = m.ActiveGrantViews(c.ID)
	m.mu.Lock()
	l, ok := m.live[c.ID]
	if ok {
		v.SubagentsRunning = l.subRunning
		v.Running = l.running
		v.SlotID = l.slot.ID
		if l.running && !l.runningSince.IsZero() {
			t := l.runningSince
			v.RunningSince = &t
		}
	}
	v.Resuming = m.resuming[c.ID]
	v.Starting = m.starting[c.ID]
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
	Model       string `json:"model"`
	Variant     string `json:"variant"`
	Title       string `json:"title"`
	Message     string `json:"message"`
	Internet    *bool  `json:"internet"`
	AutoCompact *bool  `json:"auto_compact"`
	// MaxSubagents is accepted for older clients and ignored: the limit is Options.MaxSubagents.
	MaxSubagents *int `json:"max_subagents"`
	// Delegation: delegated rights (delegation.Delegation as JSON); without it the chat behaves
	// as before step 1 (reading free, writing with approval). In an experiment chat it carries the
	// protection level: Enforce and Confirm are set from Experiment.Protection, not from the request.
	Delegation json.RawMessage `json:"delegation,omitempty"`
	// Experiment: the profile of an experiment chat (X0): binding and protection level per chat. Only
	// with Delegation and only for an account in AGW_EXPERIMENT_USERS; the binding may differ from
	// AGW_TOOLSETS. The model, the provider and the proxy address stay as configured (catalogue,
	// AGW_DEFAULT_MODEL, AGW_PROXY_BASE_URL) — the profile changes only the action space and the
	// protection level.
	Experiment *store.Experiment `json:"experiment,omitempty"`
	// Owner is set by the API from the login (sub), never from the request body.
	Owner string `json:"-"`
	// OwnerName is set by the API from the login (preferred_username), never from the request body.
	// AGW_EXPERIMENT_USERS names accounts this way.
	OwnerName string `json:"-"`
	// Language: the user's preferred language according to the browser (BCP 47, e.g. "en-US");
	// optional. Goes to the agent as a note with the first request (language.go).
	Language string `json:"language,omitempty"`
	// Async: do not wait for a sandbox when the pool has no free slot. The chat comes back at once
	// with starting (and resuming) set, the SSE event "resume" (start: true) reports the steps, and a
	// message sent meanwhile waits until the sandbox is assigned. With a free slot nothing changes.
	// Only without Message: a first message in the request keeps the synchronous behaviour.
	Async bool `json:"async,omitempty"`
}

// experimentAllowed reports whether one of the names (the login's preferred_username and, for older
// clients, the owner) may create experiment chats (X0.2). An empty list allows nobody.
func (m *Manager) experimentAllowed(names ...string) bool {
	for _, name := range names {
		if name == "" {
			continue
		}
		for _, allowed := range m.opt.ExperimentUsers {
			if strings.TrimSpace(allowed) == name {
				return true
			}
		}
	}
	return false
}

// experimentOf returns the experiment profile of a chat; nil for a product chat or an unknown chat.
func (m *Manager) experimentOf(ctx context.Context, chatID string) *store.Experiment {
	c, err := m.st.GetChat(ctx, chatID)
	if err != nil {
		return nil
	}
	return c.Experiment
}

func (m *Manager) Create(ctx context.Context, req NewChat) (ChatView, error) {
	if req.Model == "" {
		req.Model = m.cat.Default
	}
	if _, _, ok := m.cat.Lookup(req.Model); !ok {
		return ChatView{}, ErrUnknownModel
	}
	// The bindings come from AGW_TOOLSETS. A variant in the request is only accepted when it names
	// the same combination (in any order, "both" = "cli,mcp"); another one is refused instead of
	// quietly giving the chat other tools than the client asked for. An experiment chat may name
	// another binding (X0.1); it is stored as "exp:<key>".
	experiment := req.Experiment
	if experiment != nil {
		if !m.experimentAllowed(req.OwnerName, req.Owner) {
			return ChatView{}, ErrExperimentNotAllowed
		}
		if t := bytes.TrimSpace(req.Delegation); len(t) == 0 || string(t) == "null" {
			return ChatView{}, ErrExperimentNeedsDelegation
		}
		switch experiment.Protection {
		case "none", "instruction", "service":
		case "":
			return ChatView{}, fmt.Errorf("%w: missing", ErrExperimentProtection)
		default:
			return ChatView{}, fmt.Errorf("%w: %q", ErrExperimentProtection, experiment.Protection)
		}
		switch experiment.Trace {
		case "", "on", "off":
		default:
			return ChatView{}, fmt.Errorf("%w: %q", ErrExperimentTrace, experiment.Trace)
		}
		base := m.opt.Toolsets
		if req.Variant != "" {
			ts, err := toolset.FromVariant(req.Variant)
			if err != nil {
				return ChatView{}, ErrUnknownVariant
			}
			base = ts
		}
		req.Variant = toolset.ExperimentPrefix + base.Key()
	} else {
		if req.Variant != "" {
			ts, err := toolset.FromVariant(req.Variant)
			if err != nil {
				return ChatView{}, ErrUnknownVariant
			}
			if ts != m.opt.Toolsets {
				return ChatView{}, fmt.Errorf("%w: requested %q, new chats get %q", ErrVariantFixed, ts.Key(), m.opt.Toolsets.Key())
			}
		}
		req.Variant = m.opt.Toolsets.Key()
	}
	internet := m.opt.InternetDefault
	if req.Internet != nil {
		internet = *req.Internet
	}
	if experiment != nil {
		// X0/X1: the experiment runs without internet, and without a way to request it (the tools
		// for that are not in its slots either).
		internet = false
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
	if experiment != nil {
		// X2.5: no compaction in experiment chats. The instruction of the treatment must stay in the
		// context for the whole run; otherwise it could vanish with a summary and the levels would
		// differ by run length. A run that exceeds the context window ends with an error.
		autoCompact = false
	}
	lang, err := NormalizeLanguage(req.Language)
	if err != nil {
		return ChatView{}, err
	}
	var del json.RawMessage
	var exp *store.Experiment
	if t := bytes.TrimSpace(req.Delegation); len(t) > 0 && string(t) != "null" {
		d, err := delegation.Parse(t)
		if err != nil {
			return ChatView{}, fmt.Errorf("%w: %v", ErrInvalid, err)
		}
		if experiment != nil {
			// X0.7: the protection level alone decides enforcement and confirmation, not the request.
			enforce := experiment.Protection == "service"
			d.Enforce = &enforce
			d.Confirm = "none"
			stored := *experiment
			stored.Limit = "" // X3: the gateway sets the limit when it runs the chat
			if l := stored.Limits; l != nil {
				if l.MaxCostUSD < 0 || l.MaxTokens < 0 {
					return ChatView{}, fmt.Errorf("%w: experiment limits: negative value", ErrInvalid)
				}
				if l.MaxDuration != "" {
					if d, err := time.ParseDuration(l.MaxDuration); err != nil || d < 0 {
						return ChatView{}, fmt.Errorf("%w: experiment limits: unreadable duration %q", ErrInvalid, l.MaxDuration)
					}
				}
			}
			exp = &stored
		}
		del, _ = json.Marshal(d) // the checked, normalized form is stored
	}
	c, err := m.st.CreateChat(ctx, store.NewChat{Title: title, TitleSource: titleSrc, Model: req.Model, Variant: req.Variant, Internet: internet, AutoCompact: autoCompact, MaxSubagents: m.opt.MaxSubagents, Delegation: del, Experiment: exp, Owner: req.Owner, Language: lang})
	if err != nil {
		return ChatView{}, err
	}
	unlock := m.lock(c.ID)
	if internet {
		// The old switch at creation is a full grant now (issue #175), with the default limits.
		origin := store.GrantByUser
		if req.Internet == nil {
			origin = store.GrantByDefault
		}
		if err := m.startUserFull(ctx, c.ID, origin, ViaUser); err != nil {
			unlock()
			return ChatView{}, err
		}
	}
	if req.Async && strings.TrimSpace(req.Message) == "" {
		slot, err := m.pool.Acquire(c.Variant, c.ID)
		switch {
		case errors.Is(err, pool.ErrNoIdleSlot):
			// Unlocked by startLater once the sandbox is assigned or the start failed.
			m.startLater(ctx, c, unlock)
			return m.View(ctx, c.ID)
		case err != nil:
			unlock()
			_ = m.st.SetState(context.WithoutCancel(ctx), c.ID, store.StateDormant)
			return ChatView{}, err
		}
		err = m.attachSlot(ctx, c, nil, nil, slot)
		unlock()
		if err != nil {
			_ = m.st.SetState(context.WithoutCancel(ctx), c.ID, store.StateDormant)
			m.publishChat(context.WithoutCancel(ctx), c.ID)
			return ChatView{}, err
		}
		return m.View(ctx, c.ID)
	}
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

// startLater assigns the first sandbox of a new chat in the background (Create with Async and an
// empty pool). The caller holds the chat lock; startLater releases it when done, so a message sent
// meanwhile waits in ensureLive and then finds the chat active instead of starting a second sandbox.
func (m *Manager) startLater(ctx context.Context, c store.Chat, unlock func()) {
	ctx = context.WithoutCancel(ctx)
	p := m.newResume(c.ID)
	p.first = true
	m.mu.Lock()
	m.resuming[c.ID] = true
	m.starting[c.ID] = true
	m.mu.Unlock()
	m.publishChat(ctx, c.ID)
	go func() {
		defer func() {
			m.mu.Lock()
			delete(m.resuming, c.ID)
			delete(m.starting, c.ID)
			m.mu.Unlock()
			unlock()
			m.publishChat(ctx, c.ID)
		}()
		if err := m.attach(ctx, c, nil, p); err != nil {
			// Without a sandbox the chat rests; the next message tries again like a resume.
			p.fail(err)
			_ = m.st.SetState(ctx, c.ID, store.StateDormant)
			return
		}
		p.ready()
	}()
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
	return m.attachSlot(ctx, c, session, p, nil)
}

// attachSlot is attach with a slot the caller already acquired for the chat (nil: acquire one here).
func (m *Manager) attachSlot(ctx context.Context, c store.Chat, session []byte, p *resumeProgress, slot *pool.Slot[Agent]) error {
	start := time.Now()
	p.run(PhaseAcquire)
	// The stored variant may be an older id ("both") or another combination than the configured one;
	// the pool is keyed by the canonical key and starts such slots on demand. An experiment chat keeps
	// its "exp:" prefix: its slots have the experiment profile (X0.3).
	base, experiment := toolset.SplitExperiment(c.Variant)
	ts, err := toolset.FromVariant(base)
	if err != nil {
		if slot != nil {
			m.pool.Release(ctx, slot, pool.ReasonAttachFailed)
		}
		return ErrUnknownVariant
	}
	key := ts.Key()
	if experiment {
		key = toolset.ExperimentPrefix + key
	}
	if slot == nil {
		if slot, err = m.pool.AcquireWait(ctx, key, c.ID, m.opt.AcquireTimeout); err != nil {
			return err
		}
	}
	waited := time.Since(start)
	p.done(ResumeStep{Detail: slot.ID})
	a := slot.Worker
	fail := func(err error) error {
		m.pool.Release(ctx, slot, pool.ReasonAttachFailed)
		return err
	}
	if experiment {
		// X7.6/#152: the plan decides the trace per run; empty keeps the slot's default (on for
		// experiment slots, set from the variant in the worker). Setting it explicitly also
		// resets a warm slot that a previous run left with the other value.
		traceOn := c.Experiment == nil || c.Experiment.Trace != "off"
		if err := a.SetExecTrace(ctx, traceOn); err != nil {
			return fail(fmt.Errorf("set syscall trace: %w", err))
		}
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
	// A slot is assigned only once and starts without the egress network (worker.Create), so only
	// switching internet on does anything here; off would cost several Docker calls for nothing.
	egress := m.egressWanted(c.ID)
	if egress {
		if err := a.SetInternet(ctx, true); err != nil {
			return fail(fmt.Errorf("set internet: %w", err))
		}
	}
	if _, err := a.Call(ctx, map[string]any{"type": "set_auto_compaction", "enabled": c.AutoCompact}); err != nil {
		return fail(fmt.Errorf("set auto-compaction: %w", err))
	}
	p.done(ResumeStep{Detail: "Internet " + onOff(len(m.grants.active(c.ID)) > 0)})
	l := &live{slot: slot, stop: make(chan struct{}), ip: a.IP(), maxSub: m.opt.MaxSubagents, subRuns: map[string]*subTrack{}, egress: egress}
	if egress {
		for _, g := range m.grants.active(c.ID) {
			g.resetSample()
		}
	}
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
	m.mu.Lock()
	m.live[c.ID] = l
	m.mu.Unlock()
	if ts.Has(toolset.CLI) { // subagents only come with the command line
		go m.watchSubagents(c.ID, l)
	}
	_ = m.st.SetState(ctx, c.ID, store.StateActive)
	go m.pump(c.ID, l)
	m.armIdle(c.ID, l)
	go m.refreshInfo(context.WithoutCancel(ctx), c.ID, a)
	slog.Info("chat assigned", "chat", c.ID, "slot", slot.ID, "container", a.ContainerName(), "resumed", session != nil,
		"wait_ms", waited.Milliseconds(), "ms", time.Since(start).Milliseconds())
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
	// A chat in the trash takes no sandbox (issue #76); checked under the chat lock, which Trash holds too.
	if c.TrashedAt != nil {
		return nil, false, ErrTrashed
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

// Resume resumes an idle chat in the background, without a message (issue #31: the UI wakes a chat as
// soon as it is opened, so the user never sees it idle). It returns at once; SSE "resume" reports
// the steps as for a resume through a message. Idempotent: an active chat, a chat already being
// resumed or started, and a second call during a wake change nothing. A message sent meanwhile
// waits in ensureLive for the wake and never takes a second slot. After a failed wake the chat stays
// idle; calling Resume again (or sending) tries again. Not to be confused with the wake-ups of
// background tasks (BgWakesPerHour), which start a turn of the agent.
func (m *Manager) Resume(ctx context.Context, chatID string) (ChatView, error) {
	c, err := m.st.GetChat(ctx, chatID)
	if err != nil {
		return ChatView{}, err
	}
	if c.TrashedAt != nil {
		return ChatView{}, ErrTrashed
	}
	m.mu.Lock()
	busy := m.live[chatID] != nil || m.resuming[chatID] || m.starting[chatID]
	if !busy {
		// set before the response, so the chat comes back as resuming and a second call is a no-op
		m.resuming[chatID] = true
	}
	m.mu.Unlock()
	if busy {
		return m.view(c), nil
	}
	m.userActive(chatID)
	go func() {
		ctx := context.WithoutCancel(ctx)
		_, _, err := m.ensureLive(ctx, chatID)
		// ensureLive clears the flag itself when it resumed; when the chat was live meanwhile (a message
		// resumed it first) or loading the chat failed, it is cleared here
		m.mu.Lock()
		delete(m.resuming, chatID)
		m.mu.Unlock()
		if err != nil {
			slog.Info("resume on open failed", "chat", chatID, "error", err)
		}
		m.publishChat(ctx, chatID)
	}()
	return m.view(c), nil
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

// Abort is the user's stop (POST /api/chats/{id}/abort). It calls abortWith with AbortUser; every other abort of the
// gateway (the trash, a limit) does the same with its own reason, so the turn records who or what cut it short
// (issue #120, L3).
func (m *Manager) Abort(ctx context.Context, chatID string) (ChatView, error) {
	return m.abortWith(ctx, chatID, AbortUser)
}

// abortWith stops the running turn of a chat. reason names what did it (AbortUser and the constants next to it) and
// is recorded on the turn before pi is asked to stop.
func (m *Manager) abortWith(ctx context.Context, chatID, reason string) (ChatView, error) {
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
	m.markTurnAborted(chatID, l, reason)
	// Stop means "halt", not "throw away": a running autonomous plan is paused, not cancelled (A-E1, D1.10).
	// Platform jobs (a training) keep running; stopping one is a separate action.
	if ap, err := m.st.ActiveAutonomousPlan(ctx, chatID); err == nil && ap.State == plan.StateApproved {
		if _, perr := m.pausePlan(ctx, chatID, ap.ID, "abort"); perr != nil {
			slog.Error("plan not paused on abort", "chat", chatID, "plan", ap.ID, "error", perr)
		}
	}
	if l != nil {
		// After abort pi continues with what is queued; so reclaim steered messages beforehand
		// (they stay enqueued and held back).
		m.reclaimSteered(ctx, chatID, l)
		if _, err := callT(l.slot.Worker, map[string]any{"type": "abort"}, 10*time.Second); err != nil {
			return ChatView{}, err
		}
		// If no run is going on in pi (a command of an extension left the flag set), the abort
		// has nothing to end; take the flag back.
		go func() {
			time.Sleep(time.Duration(settleDelay.Load()))
			m.settleIfIdle(chatID, l)
		}()
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
		m.detach(ctx, chatID, l, true, true, pool.ReasonSuspended)
	}
	_ = m.st.SetState(ctx, chatID, store.StateDormant)
	m.publishChat(ctx, chatID)
	return m.View(ctx, chatID)
}

// detach saves the session (save) and the workspace (workspace) and
// returns the slot (single use).
func (m *Manager) detach(ctx context.Context, chatID string, l *live, save, workspace bool, reason string) {
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
		delete(m.endSeen, chatID)
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
	m.pool.Release(ctx, l.slot, reason)
	slog.Info("slot returned", "chat", chatID, "slot", l.slot.ID, "reason", reason)
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
			m.markTurnAborted(chatID, l, AbortTimeLimit)
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
	m.detach(ctx, chatID, l, false, false, pool.ReasonAgentDied+":"+cause)
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
		m.markTurnAborted(chatID, l, AbortTurnLimit)
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
	for _, c := range pi {
		// the gateway's own extension commands (agw-…, issue #81) are not for the user
		if !strings.HasPrefix(c.Name, "agw-") {
			out = append(out, c)
		}
	}
	return out, nil
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
	if c.Experiment != nil {
		// X3.1: no title from the model in an experiment run; the runner sets the title, and a second
		// model would distort the run's tokens and cost.
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

// SlotOf names the slot of a running chat (empty when the chat is idle). Features that act for a
// chat outside a socket call log their calls this way (the notebook service logs the dataset
// archives it streams, own code C-E3).
func (m *Manager) SlotOf(chatID string) string { return m.slotOf(chatID) }

// Store gives read access to the store for the experiment export (X5.7); the API uses it for runs and tables.
func (m *Manager) Store() *store.Store { return m.st }

func (m *Manager) LogCall(slotID, chatID, via, op, detail, result string) {
	m.LogCallBy(context.Background(), slotID, chatID, via, op, detail, result)
}

// LogCallBy logs a socket call together with session and tool call from the context
// (sock.CallerLogger): this way the log shows whether the main agent or a subagent asked.
func (m *Manager) LogCallBy(ctx context.Context, slotID, chatID, via, op, detail, result string) {
	rec := store.CallRecord{SlotID: slotID, ChatID: chatID, Via: via, Op: op, Detail: detail, Result: result,
		Session: store.SessionFrom(ctx), ToolCallID: store.ToolCallFrom(ctx), DurationMs: store.DurationMsFrom(ctx),
		Effect: store.EffectFrom(ctx)}
	if ref, ok := store.CallRefFrom(ctx); ok {
		rec.CallRef, rec.CallRefSource, rec.CallRefCandidates, rec.CallRefClaimed = ref.Ref, ref.Source, ref.Candidates, ref.Claimed
	}
	m.insertCall(ctx, rec)
}

// LogPlatformCall logs a platform call with its structured record (X5): decision, reason, rule, body and the facts
// FF4 needs. sock.PlatformLogger reaches it through the socket backend; the rendering in result stays for display.
func (m *Manager) LogPlatformCall(ctx context.Context, slotID, chatID, via, detail, result string, res platform.Result) {
	rec := store.CallRecord{SlotID: slotID, ChatID: chatID, Via: via, Op: "platform", Detail: detail, Result: result,
		Session: store.SessionFrom(ctx), ToolCallID: store.ToolCallFrom(ctx), DurationMs: store.DurationMsFrom(ctx),
		Effect: store.EffectFrom(ctx), Decision: res.Decision, Reason: res.Reason, Rule: res.Rule, Body: res.SentBody,
		BodyTruncated: res.BodyTruncated, BodyInSHA256: res.BodyInSHA256, WithinDelegation: res.WithinDelegation,
		ApprovalID: res.ApprovalID, ApprovalDecision: res.ApprovalDecision}
	if ref, ok := store.CallRefFrom(ctx); ok {
		rec.CallRef, rec.CallRefSource, rec.CallRefCandidates, rec.CallRefClaimed = ref.Ref, ref.Source, ref.Candidates, ref.Claimed
	}
	m.insertCall(ctx, rec)
}

func (m *Manager) insertCall(ctx context.Context, rec store.CallRecord) {
	sc, err := m.st.AddSocketCallRecord(context.WithoutCancel(ctx), rec)
	if err != nil {
		slog.Error("socket call not logged", "error", err)
		return
	}
	slog.Info("socket call", "slot", rec.SlotID, "chat", rec.ChatID, "via", rec.Via, "op", rec.Op, "detail", rec.Detail,
		"result", rec.Result, "decision", rec.Decision)
	if rec.ChatID != "" {
		m.publish(rec.ChatID, Event{Kind: "socket_call", Data: sc})
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

// previewOf is the preview kind stored with an artifact; it is decided from the first bytes of the file.
func previewOf(name string, data []byte) string {
	head := data
	if len(head) > artifacts.PreviewHeadSize {
		head = head[:artifacts.PreviewHeadSize]
	}
	return artifacts.PreviewKind(name, head)
}

// Upload stores a file the agent sends to the user (issue #62): no approval, the file is stored as an output
// artifact of the chat right away and announced with the event "artifact", which the UI shows as a file message of
// the agent. The socket has already checked size and name; the helpers in the sandbox check the path. A checksum
// sent along (agw-artifact) must match, otherwise nothing is stored.
func (m *Manager) Upload(ctx context.Context, chatID, slotID, via, name string, size int64, sha string, body io.Reader) (sock.UploadResult, error) {
	if m.inTrash(ctx, chatID) {
		return sock.UploadResult{Status: sock.UploadRejected, Name: name, Message: platform.RefusedPrefix + trashRefusal}, nil
	}
	data, err := io.ReadAll(body)
	if err != nil {
		return sock.UploadResult{}, err
	}
	sum := sha256.Sum256(data)
	got := hex.EncodeToString(sum[:])
	res := sock.UploadResult{Name: name, Size: int64(len(data)), SHA256: got}
	if sha != "" && !strings.EqualFold(sha, got) {
		res.Status, res.Message = sock.UploadRejected, "checksum does not match"
		return res, nil
	}
	ct := contentType(name, data)
	key := objectKey(chatID, store.KindOutput, name)
	if err := m.blobs.Put(ctx, key, bytes.NewReader(data), int64(len(data)), ct); err != nil {
		return sock.UploadResult{}, fmt.Errorf("store: %w", err)
	}
	art := store.Artifact{ChatID: chatID, Kind: store.KindOutput, Name: name, Size: int64(len(data)), SHA256: got, ContentType: ct, Via: via, ObjectKey: key,
		ToolCallID: store.ToolCallFrom(ctx), Preview: previewOf(name, data)}
	if err := m.st.PutArtifact(context.WithoutCancel(ctx), art); err != nil {
		return sock.UploadResult{}, err
	}
	art.CreatedAt = time.Now()
	m.publish(chatID, Event{Kind: "artifact", Data: art})
	m.publishChat(ctx, chatID)
	slog.Info("artifact sent to the user", "chat", chatID, "name", name, "bytes", len(data), "via", via)
	res.Status = sock.UploadStored
	return res, nil
}

// maxPlatformPreview limits method, path and indented body of a writing platform call.
const maxPlatformPreview = 16000

// maxNotebookPreview limits the approval of a run in the chat's notebook server (own code C-E3). The
// user must see the code that runs, so a run gets more room than other calls; code that does not fit
// is refused instead of cut (Review W1).
const maxNotebookPreview = 48000

// notebookRun reports whether a call starts a run in the chat's notebook server. Its approval shows
// the code of the run in full instead of the form fields.
func notebookRun(req platform.Request) bool {
	return req.Method == http.MethodPost && req.Path == platform.NotebookPrefix+"/runs"
}

// PlatformExchanged logs a token exchange for a chat (platform.Client.OnExchange):
// who is in the token (sub, azp, aud), until when it is valid and that no act claim comes.
func (m *Manager) PlatformExchanged(chatID string, cl platform.Claims) {
	m.LogCall(m.slotOf(chatID), chatID, "orchestrator", "token_exchange", cl.String(), "ok")
}

// PlatformCall performs a call to the Agri-Gaia platform. Every call has an effect class (delegation.Classify,
// issue #63): read calls go straight through; write and compute calls wait for the user's approval unless the
// delegation says confirm none; irreversible calls always wait; compute calls are limited per chat. The
// approval shows method, path, the class and the body. A call whose class cannot be determined is refused.
func (m *Manager) PlatformCall(ctx context.Context, chatID, slotID, via string, req platform.Request) (res platform.Result, err error) {
	if m.opt.Platform == nil {
		return platform.Result{Status: "error", Message: platform.ErrNotConfigured.Error(), Decision: platform.DecisionError, Reason: "not_configured"}, nil
	}
	if m.inTrash(ctx, chatID) {
		return platform.Result{Status: "error", Message: platform.RefusedPrefix + trashRefusal, Decision: platform.DecisionError, Reason: "trashed"}, nil
	}
	// Check here too, not only at the caller: Writes() assumes a normalized method (Review W2).
	req, err = platform.Normalize(req)
	if err != nil {
		decision, reason := platform.DecisionDeniedFixed, "invalid_call"
		if errors.Is(err, platform.ErrBlocked) {
			reason = "blocked_path"
		}
		return platform.Result{Status: "error", Message: err.Error(), Decision: decision, Reason: reason}, nil
	}
	// X5.2: the body as the gateway sends it, and the checksum of what the agent sent.
	sentBody, sentTruncated := platform.LogBody(req)
	defer func() {
		res.SentBody, res.BodyTruncated, res.BodyInSHA256 = sentBody, sentTruncated, req.BodyInSHA256
	}()
	del, err := m.delegationOf(ctx, chatID)
	if err != nil {
		// An unreadable delegation grants nothing.
		return platform.Result{Status: "denied", Message: err.Error(), Decision: platform.DecisionDeniedDelegation, Reason: "delegation_unreadable"}, nil
	}
	// withinDelegation is computed independently of the protection level and of the decision below (X5.5).
	// Without a delegation there are no transferred rights to compare with; nil means "not computable".
	var within *bool
	own := func(res, id string) bool {
		ok, _ := m.st.IsDelegationObject(ctx, chatID, res, id)
		return ok
	}
	if req.Path == platform.RightsPath {
		return platform.Result{Status: "ok", HTTPStatus: 200, Body: m.rightsText(ctx, chatID, del), Decision: platform.DecisionAllowed, Reason: "virtual"}, nil
	}
	if req.Path == platform.PlanPath {
		// A plan proposal is answered by the gateway; it is no call to the platform.
		pres, perr := m.ProposePlan(ctx, chatID, slotID, via, req)
		pres.Decision, pres.Reason = platform.DecisionAllowed, "virtual"
		return pres, perr
	}
	if req.Path == platform.SearchPrefix && req.Method == http.MethodPost {
		// A proposal of a training search is answered by the gateway too (issue #168); the paths below the
		// prefix go to the search service registered as a virtual doer.
		sres, serr := m.ProposeSearch(ctx, chatID, slotID, via, req)
		sres.Decision, sres.Reason = platform.DecisionAllowed, "virtual"
		return sres, serr
	}
	if req.Path == platform.CapabilitiesPath {
		// The overview of this chat's setup (G1.10): reading, without approval, like the rights.
		res, _ := m.capabilitiesResult(ctx, chatID)
		res.Decision, res.Reason = platform.DecisionAllowed, "virtual"
		return res, nil
	}
	// A /_agw/ path that no doer answers belongs to the gateway and is answered here (G1.5): no
	// delegation check, no approval, no compute call — there is no platform route behind it (G1.13).
	if res, unavailable := m.virtualUnavailable(req.Path); unavailable {
		res.Decision, res.Reason = platform.DecisionError, "unavailable"
		return res, nil
	}
	// Delegation (step 1): classification from method and path alone, check before any confirmation.
	access := delegation.Classify(req)
	// Field allowlists hold without a delegation too (issue #65): a body with a field the platform would take
	// over is refused before the delegation check and before any approval.
	if access.Problem != "" {
		rule, _ := json.Marshal(access)
		return platform.Result{Status: "denied", Message: "refused: " + access.Problem, Decision: platform.DecisionDeniedFixed,
			Reason: delegation.CodeFieldRule, Rule: rule}, nil
	}
	if refusal, refused := effectRefusal(access.Effect); refused {
		refusal.Decision, refusal.Reason = platform.DecisionDeniedFixed, "unclassified"
		return refusal, nil
	}
	// The delegation is checked even in a stage without protective measure, so that within_delegation is
	// known for every call (X5.5).
	violation := ""
	delDecision := delegation.Decision{Allowed: del == nil}
	if del != nil {
		delDecision = del.Check(access, time.Now(), own)
		allowed := delDecision.Allowed
		within = &allowed
		if !delDecision.Allowed {
			if del.Enforcing() {
				return platform.Result{Status: "denied", Message: delDecision.Reason, Decision: platform.DecisionDeniedDelegation,
					Reason: delDecision.Code, Rule: delDecision.Rule, WithinDelegation: within}, nil
			}
			violation = delDecision.Reason // stage without protective measure: only log
		}
	}
	rule := delDecision.Rule
	// Guards of features (G1.4): after the delegation, before the compute limit, the plan match and
	// the approval. A guard's error fails closed.
	if res, refused, err := m.guardRefusal(ctx, chatID, req, access); err != nil {
		return platform.Result{Decision: platform.DecisionError, Reason: "guard_error"}, err
	} else if refused {
		ruleJSON, _ := json.Marshal(access)
		res.Decision, res.Reason, res.Rule, res.WithinDelegation = platform.DecisionDeniedFixed, "guard", ruleJSON, within
		return res, nil
	}
	// The search's own paths (declare a trial, read the search, end it) are the gateway's bookkeeping: delegation and
	// guards are checked, nothing goes to the platform, and nobody is asked (issue #168).
	if isSearchControl(req) {
		t0 := time.Now()
		sres, serr := m.platformDo(ctx, chatID, req)
		if serr != nil {
			return platform.Result{Status: "error", Message: serr.Error(), Decision: platform.DecisionError, Reason: "call_failed", Rule: rule, WithinDelegation: within}, nil
		}
		sres.Duration, sres.Violation = time.Since(t0), violation
		sres.Decision, sres.Reason, sres.Rule, sres.WithinDelegation = platform.DecisionAllowed, "", rule, within
		return sres, nil
	}
	if access.Effect == delegation.EffectCompute {
		res, allowed, err := m.computeAllowed(ctx, chatID)
		if err != nil {
			return platform.Result{Decision: platform.DecisionError, Reason: "compute_check_failed"}, err
		}
		if !allowed {
			res.Decision, res.Reason, res.Rule, res.WithinDelegation = platform.DecisionDeniedCompute, "compute_limit", rule, within
			return res, nil
		}
	}
	// A call that is exactly the next step of an approved plan (issue #70) needs no new approval when the
	// step is covered by the plan; an irreversible step is returned unclaimed and still asks the user.
	var step *store.PlanStep
	if access.Effect != delegation.EffectRead || req.Writes() {
		var nextStep int
		var differs string
		step, nextStep, differs = m.planNext(ctx, chatID, req) // nil: no plan step applies
		if step == nil && differs == "files" && len(req.Uploads) > 0 {
			// The call is the next step but for its files: the user approved other files (issue #172). Not a
			// fall-back to a single approval, so that the agent notices it left the plan; the step stays open.
			return platform.Result{Status: "error", Message: platform.RefusedPrefix + fmt.Sprintf(
				"the files of this upload are not the ones approved for plan step %d (a file was changed, added or removed "+
					"since the plan was approved); nothing was sent. Restore the files, or propose a new plan", nextStep),
				Decision: platform.DecisionDeniedFixed, Reason: "files_changed", Rule: rule, WithinDelegation: within}, nil
		}
	}
	// A call that is the next call of a trial of the running training search (issue #168) is covered by the
	// approval of the search, as far as the search says so; only a call no plan step claims is asked.
	var claim *trialClaim
	if step == nil && (access.Effect != delegation.EffectRead || req.Writes()) {
		claim = m.searchMatch(ctx, chatID, req) // nil: no search applies
	}
	planStep := func(s *store.PlanStep) *int {
		if s == nil {
			return nil
		}
		idx := s.Idx
		return &idx
	}
	// A claimed step must end up finished: every return before the call to the platform fails it and the plan.
	stepFinished := false
	if step != nil && step.Covered {
		defer func() {
			if !stepFinished {
				m.planFinish(ctx, chatID, step, access, req, platform.Result{Status: "error", Message: "not executed"}, errors.New("not executed"))
			}
		}()
	}
	// The gateway repeating a parked step (A-E2, D2.8): the user decided, and the marker names the
	// approval the step was parked with. Only then does the repeated call not ask again.
	approvedHere := m.parkedHere(ctx, chatID, req)
	approvalID, approvalBy, executedDecision := "", "none", platform.DecisionAllowed
	approvedManifest := "" // SHA-256 over the list of the uploaded files the user approved (issue #164)
	if needsApproval(access, req.Writes(), del) && (step == nil || !step.Covered) && !(claim != nil && claim.Covered) && !approvedHere {
		// A step that already waits for the user's decision is not asked a second time (A-E2, D2.8): its
		// approval stays open until the plan expires, and a second one would run the same irreversible call
		// twice. The same answer as at the first call, so that the agent ends its turn either way.
		if res, waiting := m.parkedAgain(ctx, step); waiting {
			res.Decision, res.Reason, res.Rule, res.WithinDelegation, res.PlanStep = platform.DecisionPendingUser, "user_approval_pending", rule, within, planStep(step)
			return res, nil
		}
		if err := m.checkPendingLimit(ctx, chatID); err != nil {
			return platform.Result{Decision: platform.DecisionError, Reason: "pending_limit"}, err
		}
		preview := req.String() + "\n" + effectLine(access.Effect, m.computeMax()) + "\nClassification: " + access.String()
		if del != nil && violation == "" {
			preview += " (within the delegated rights)"
		}
		limit := maxPlatformPreview
		if notebookRun(req) {
			// A run in the notebook server shows its code, not its form fields: the user approves
			// what will run (own code C-E3).
			code, err := platform.NotebookPreview(req, m.notebookShape())
			if err != nil {
				return platform.Result{Status: "error", Message: platform.RefusedPrefix + err.Error(), Decision: platform.DecisionDeniedFixed,
					Reason: "invalid_run", Rule: rule, WithinDelegation: within, PlanStep: planStep(step)}, nil
			}
			preview += "\n\n" + code
			limit = maxNotebookPreview
		} else if d := req.Describe(); d != "" {
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
		preview += irreversibleNote(access.Effect, req.Body)
		preview = platform.VisibleControls(preview)
		// The user only approves what they see in full: better refuse than truncate (Review W1).
		if n := len([]rune(preview)); n > limit {
			if limit == maxNotebookPreview {
				return platform.Result{Status: "error", Message: fmt.Sprintf(
					"the code is too long for one approval (%d characters, at most %d); shorten it or split it into runs", n, limit),
					Decision: platform.DecisionDeniedFixed, Reason: "preview_too_large", Rule: rule, WithinDelegation: within, PlanStep: planStep(step)}, nil
			}
			return platform.Result{Status: "error", Message: fmt.Sprintf("call too large for an approval (%d characters, at most %d); shrink the body", n, limit),
				Decision: platform.DecisionDeniedFixed, Reason: "preview_too_large", Rule: rule, WithinDelegation: within, PlanStep: planStep(step)}, nil
		}
		// What will happen, written by the gateway from this request and platform state it reads now (issue #72).
		tool := ""
		if inv, ok := platform.InvocationFrom(ctx); ok {
			tool = inv.Tool
		}
		summaryCall := platform.SummaryCallOf(tool, req)
		rl := &readLog{}
		read := m.loggedSummaryReader(chatID, rl)
		if notebookRun(req) {
			// One read per path for the summary and the structured data of a run (own code C-E4): the
			// activity entry of the summary reads names every path once (issue L2/#119).
			read = cachedReads(read)
		}
		summary := platform.VisibleControls(platform.Summarize(ctx, read, summaryCall))
		// The structured data of the run (own code C-E4): the card shows the files instead of the text
		// preview. Without data there is no approval — the user must see the code of the run.
		var notebookData json.RawMessage
		if notebookRun(req) {
			if m.opt.Notebook == nil {
				return platform.Result{Status: "error", Message: platform.RefusedPrefix + "the notebook service of this gateway is not available",
					Decision: platform.DecisionError, Reason: "notebook_unavailable", Rule: rule, WithinDelegation: within, PlanStep: planStep(step)}, nil
			}
			data, err := m.opt.Notebook.ApprovalData(ctx, req, read)
			if err != nil {
				return platform.Result{Status: "error", Message: platform.RefusedPrefix + err.Error(),
					Decision: platform.DecisionError, Reason: "notebook_error", Rule: rule, WithinDelegation: within, PlanStep: planStep(step)}, nil
			}
			notebookData = data
		}
		size := int64(len(req.Body))
		for _, u := range req.Uploads {
			size += int64(len(u.Data))
		}
		// The files of an upload (issue #164): the card shows the summary and fetches the list of every file when
		// the user opens it; the hash over that list is what the user approves and what is checked before sending.
		var uploadsData, uploadEntries json.RawMessage
		if len(req.Uploads) > 0 {
			man, merr := platform.ManifestOf(req)
			if merr != nil {
				return platform.Result{Status: "error", Message: platform.RefusedPrefix + merr.Error(),
					Decision: platform.DecisionDeniedFixed, Reason: "invalid_upload", Rule: rule, WithinDelegation: within, PlanStep: planStep(step)}, nil
			}
			if uploadsData, err = json.Marshal(man.Summary()); err == nil {
				uploadEntries, err = json.Marshal(man.Entries)
			}
			if err != nil {
				return platform.Result{Decision: platform.DecisionError, Reason: "approval_failed"}, err
			}
			approvedManifest = man.SHA256
		}
		// The id is assigned here and the recheck is registered before the insert: the pending approval is
		// visible to the API (and to the user) as soon as the row exists, and a click that arrives before the
		// registration would skip the re-read and approve the summary as proposed (issue #154).
		inExperiment := m.experimentOf(ctx, chatID) != nil
		apID := uuid.NewString()
		if !inExperiment {
			defer m.registerRecheck(apID, m.platformRecheck(chatID, apID, tool, req))()
		}
		// The reads of the summary are one activity entry of the system (issue L2/#119), and while the call waits,
		// approving re-reads them first (DL2.1). It is written before the approval row exists: an approval that the
		// API shows must never be visible without the entry that explains its summary (issue #157).
		m.logSummaryReads(ctx, chatID, apID, "proposed", rl)
		ap, err := m.st.CreateApproval(ctx, store.Approval{ID: apID, ChatID: chatID, Kind: "platform_write", Via: via, Name: req.String(), Size: size,
			ContentType: "application/json", PendingKey: "-", Preview: preview, Session: store.SessionFrom(ctx), ToolCallID: store.ToolCallFrom(ctx),
			Effect: string(access.Effect), Summary: summary, Notebook: notebookData, Uploads: uploadsData, UploadEntries: uploadEntries,
			Search: claim.ApprovalData()})
		if err != nil {
			return platform.Result{Decision: platform.DecisionError, Reason: "approval_failed"}, err
		}
		approvalID, approvalBy = ap.ID, "user"
		bg := context.WithoutCancel(ctx)
		var final string
		if inExperiment {
			// X0.9: in an experiment chat the gateway decides instead of the user. The approval stays in
			// the record with decided_by "experiment", and only the decided event goes out (no pending
			// one, nobody waits for it).
			decided, _, derr := m.st.DecideApprovalBy(ctx, ap.ID, store.ApprovalApproved, store.DecidedByExperiment)
			if derr != nil {
				return platform.Result{}, derr
			}
			ap = decided
			final = store.ApprovalApproved
			approvalBy = store.DecidedByExperiment
			executedDecision = platform.DecisionApprovedAuto
			m.publish(chatID, Event{Kind: "approval", Data: ap})
			slog.Info("platform call approved by the experiment profile", "chat", chatID, "approval", ap.ID, "call", req.String(), "via", via)
		} else {
			w := m.broker.Register(ap.ID)
			if s, ok := m.pool.Get(slotID); ok {
				s.SetActivity("waiting_approval", "")
			}
			m.publish(chatID, Event{Kind: "approval", Data: ap})
			m.publishChat(ctx, chatID)
			slog.Info("platform call requested", "chat", chatID, "approval", ap.ID, "call", req.String(), "via", via)
			// A step of an autonomous plan that the plan does not cover is parked instead of waited for
			// (A-E2, D2.8): the user may decide hours later, the gateway repeats the stored call then. A step
			// that carries files of the sandbox is asked synchronously — the gateway cannot repeat it without
			// the running sandbox, and the approval has to show the bytes that were read.
			if m.parkedStep(ctx, step, req) {
				// The step is marked as parked (D2.8): the approval stays open until the plan expires, and the
				// gateway repeats the stored call when the user decides. The mark has to be written before the
				// answer, because that is what finds the step again at the decision.
				if ok, perr := m.st.ParkApproval(ctx, step.PlanID, step.Idx, ap.ID); perr != nil || !ok {
					// The step is not open any more (finished or skipped in between): ask as usual instead of
					// parking it, so that no decision is handed to a step that no longer waits.
					slog.Warn("step not parked, asking as usual", "chat", chatID, "plan", step.PlanID, "step", step.Idx, "error", perr)
				} else {
					if s, ok := m.pool.Get(slotID); ok {
						s.SetActivity("tool", "")
					}
					m.publishChat(ctx, chatID)
					m.publishPlan(ctx, step.PlanID)
					m.parkApproval(ctx, chatID, slotID, via, req, step, ap, w)
					return platform.Result{Status: "pending_user", Message: fmt.Sprintf(
						"waiting for the user's approval of step %d; end your turn, you will be woken when the user decides", step.Idx),
						Decision: platform.DecisionPendingUser, Reason: "user_approval_parked", Rule: rule, WithinDelegation: within,
						ApprovalID: approvalID, ApprovalDecision: "none", PlanStep: planStep(step)}, nil
				}
			}
			approved, werr := w.Wait(ctx, m.opt.ApprovalTimeout)
			final = m.settle(bg, chatID, ap.ID, approved, werr)
		}
		if s, ok := m.pool.Get(slotID); ok {
			s.SetActivity("tool", "")
		}
		m.publishChat(bg, chatID)
		if final == store.ApprovalExpired {
			return platform.Result{Status: "rejected", Message: "no decision within the waiting time; nothing executed",
				Decision: platform.DecisionRejectedUser, Reason: "approval_expired", Rule: rule, WithinDelegation: within,
				ApprovalID: approvalID, ApprovalDecision: approvalBy, PlanStep: planStep(step)}, nil
		}
		if final != store.ApprovalApproved {
			return platform.Result{Status: "rejected", Message: "rejected by the user; nothing executed",
				Decision: platform.DecisionRejectedUser, Reason: "approval_rejected", Rule: rule, WithinDelegation: within,
				ApprovalID: approvalID, ApprovalDecision: approvalBy, PlanStep: planStep(step)}, nil
		}
		ctx = bg
		// The delegation may have expired during the waiting time (Review 5, W3): reload and check again.
		if del != nil {
			fresh, err := m.delegationOf(ctx, chatID)
			if err != nil || fresh == nil {
				return platform.Result{Status: "denied", Message: "delegation no longer readable after the approval",
					Decision: platform.DecisionDeniedDelegation, Reason: "delegation_unreadable", Rule: rule, WithinDelegation: within,
					ApprovalID: approvalID, ApprovalDecision: approvalBy, PlanStep: planStep(step)}, nil
			}
			own := func(res, id string) bool {
				ok, _ := m.st.IsDelegationObject(ctx, chatID, res, id)
				return ok
			}
			if dec := fresh.Check(access, time.Now(), own); !dec.Allowed && fresh.Enforcing() {
				allowed := false
				return platform.Result{Status: "denied", Message: dec.Reason + " (checked again after the approval)",
					Decision: platform.DecisionDeniedDelegation, Reason: dec.Code, Rule: dec.Rule, WithinDelegation: &allowed,
					ApprovalID: approvalID, ApprovalDecision: approvalBy, PlanStep: planStep(step)}, nil
			}
		}
	}
	// A step covered by an approved plan: the plan's manifest, pinned when the plan was proposed (issue #172).
	if approvedManifest == "" && step != nil && step.Covered && len(req.Uploads) > 0 {
		if man, ok := plan.StepManifest(*step); ok {
			approvedManifest = man.SHA256
		}
	}
	// The bytes about to be sent must be the ones whose list the user approved (issue #164). They were read before
	// the approval and are never read again, so this holds by construction; the check makes it a step that fails.
	if approvedManifest != "" {
		if cerr := platform.CheckManifest(req, approvedManifest); cerr != nil {
			return platform.Result{Status: "error", Message: platform.RefusedPrefix + cerr.Error(),
				Decision: platform.DecisionDeniedFixed, Reason: "files_changed", Rule: rule, WithinDelegation: within,
				ApprovalID: approvalID, ApprovalDecision: approvalBy, PlanStep: planStep(step)}, nil
		}
	}
	// A compute call counts against the chat's limit once the platform takes it (not when refused or not
	// accepted; see ReleaseCompute).
	reserved := false
	if access.Effect == delegation.EffectCompute {
		ok, rerr := m.st.ReserveCompute(context.WithoutCancel(ctx), chatID, m.computeMax())
		if rerr != nil {
			return platform.Result{Decision: platform.DecisionError, Reason: "compute_reservation_failed"}, rerr
		}
		if !ok {
			res := computeLimitResult(m.computeMax())
			res.Decision, res.Reason, res.Rule, res.WithinDelegation, res.PlanStep = platform.DecisionDeniedCompute, "compute_limit", rule, within, planStep(step)
			res.ApprovalID, res.ApprovalDecision = approvalID, approvalBy
			return res, nil
		}
		reserved = true
	}
	// A covered claim of a search takes its trial now; one that returned earlier took nothing (issue #168).
	if !m.reserveClaim(context.WithoutCancel(ctx), claim) {
		if reserved {
			if rerr := m.st.ReleaseCompute(context.WithoutCancel(ctx), chatID); rerr != nil {
				slog.Error("compute call not given back", "chat", chatID, "error", rerr)
			}
		}
		return platform.Result{Status: "error", Message: platform.RefusedPrefix + "this trial of the training search was already used or is no longer declared; declare a new one with {{tool:search_next}}",
			Decision: platform.DecisionDeniedFixed, Reason: "trial_unavailable", Rule: rule, WithinDelegation: within,
			ApprovalID: approvalID, ApprovalDecision: approvalBy, PlanStep: planStep(step)}, nil
	}
	// Duration of the round trip to the platform (token exchange included), without the approval's waiting time.
	t0 := time.Now()
	res, err = m.platformDo(ctx, chatID, req)
	took := time.Since(t0)
	stepFinished = true
	// nil step: nothing to record. A step that waits for an observed platform job from here on says so in
	// the answer (A-E2, D2.1): the gateway watches the job and wakes the agent, so the agent ends its turn
	// instead of holding it open and polling the platform itself (seen live on 16.10.2026).
	if jobID := m.planFinish(ctx, chatID, step, access, req, res, err); jobID != "" && step != nil {
		res.Note = m.watchNote(ctx, step, jobID)
	}
	jobID := ""
	if step == nil && err == nil {
		// No plan step: a started job is only observed for the cards (A-E2.2, no note, no wake-up).
		jobID = m.recordJobOutsidePlan(ctx, chatID, access, req, res)
	}
	// The outcome of a call of a search's trial: after the job of a started run is observed, so that the trial can
	// name it (issue #168). A nil claim does nothing.
	m.searchFinish(ctx, chatID, claim, access, req, res, err, jobID)
	// A call that stops a job (own code C-E4): the job records the agent as the one who stopped it, and the
	// user's own route does the same in StopJob.
	m.stopJobByTheAgent(ctx, chatID, req, res)
	if reserved && (err != nil || res.Status != "ok") {
		if rerr := m.st.ReleaseCompute(context.WithoutCancel(ctx), chatID); rerr != nil {
			slog.Error("compute call not given back", "chat", chatID, "error", rerr)
		}
	}
	if err != nil {
		// D3.5: the user's login was gone when the call went out. That is no failure of the step (the call
		// never reached the platform) and no reason to let the rest of an autonomous plan fail as well: the
		// step keeps its claim back (planFinish above) and the plan pauses until the login is back (D3.6).
		if loginGone(err) {
			if ap, aerr := m.activeAutonomousPlan(ctx, chatID); aerr == nil && ap.State == plan.StateApproved {
				m.pausePlanForLogin(ctx, ap.ID)
			}
		}
		return platform.Result{Status: "error", Message: err.Error(), Violation: violation, Duration: took,
			Decision: platform.DecisionError, Reason: "call_failed", Rule: rule, WithinDelegation: within,
			ApprovalID: approvalID, ApprovalDecision: approvalBy, PlanStep: planStep(step)}, nil
	}
	res.Violation = violation
	res.Duration = took
	// X5.1: the decision of the executed call. A violation that was only logged is its own value.
	// A handler that refuses the call itself before doing anything (a refusal with its own reason code, e.g. the
	// final check of #184) keeps its error decision and reason, so that platform_calls can tell it apart.
	handlerRefused := res.Status == "error" && res.Decision == platform.DecisionError && res.Reason != ""
	handlerReason := res.Reason
	res.Decision, res.Reason, res.Rule, res.WithinDelegation = executedDecision, "", rule, within
	if handlerRefused {
		res.Decision, res.Reason = platform.DecisionError, handlerReason
	}
	if violation != "" {
		res.Decision, res.Reason, res.Rule = platform.DecisionAllowedViolation, delDecision.Code, delDecision.Rule
	}
	res.ApprovalID, res.ApprovalDecision, res.PlanStep = approvalID, approvalBy, planStep(step)
	// Platform pages reload after a successful writing call (#167): reads, refusals and failures send nothing.
	if res.Status == "ok" && res.HTTPStatus >= 200 && res.HTTPStatus < 300 && access.Effect != delegation.EffectRead {
		m.PublishPlatformChanged(chatID, PlatformChangedEvent{Source: ChangeSourceCall, Path: req.Path,
			Changes: withTasks(delegation.Changes(access, req, res), watch.TaskIDs(access, req, res))})
	}
	// Provenance (edge D1.7, D1.9): images, container deployments and the tasks of a deployment are
	// recorded in every chat, also without a delegation, because the edge guard needs them live.
	// Every other object keeps the old rule and is recorded only with a delegation.
	if r, id, ok := delegation.Created(access, res); ok && (del != nil || alwaysAttributed(access)) {
		m.recordDelegationObject(ctx, chatID, r, id)
	}
	if ref, ok := delegation.CreatedImage(access, req, res); ok {
		m.recordDelegationObject(ctx, chatID, delegation.ContainerImage, ref)
	}
	// A model uploaded from a file of the sandbox keeps its origin when the gateway had handed that file out
	// of a notebook run of this chat (R2.6): the hook records it, so that a later check of the model names
	// the check of its file. Only here do the uploads of the call and the created model come together; the
	// hook itself lives in the platform package (platform.SetOnModelUploaded).
	if req.Method == http.MethodPost && req.Path == "/models" && len(req.Uploads) > 0 {
		platform.NotifyModelUploaded(ctx, chatID, res, req.Uploads)
	}
	// A read of an image this chat knows answers with the number the platform gave it; the number is
	// what POST /container-deployments carries, while the build only knows repo:tag (D1.9).
	if ref, ids, ok := delegation.ImageRead(access, res); ok {
		if own, err := m.st.IsDelegationObject(ctx, chatID, delegation.ContainerImage, ref); err == nil && own {
			for _, id := range ids {
				m.recordDelegationObject(ctx, chatID, delegation.ContainerImage, id)
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
		return platform.Result{Status: "denied", Message: err.Error(), Decision: platform.DecisionDeniedDelegation, Reason: "delegation_unreadable"}, false
	}
	access := delegation.Classify(req)
	if access.Problem != "" { // field allowlists hold without a delegation (issue #65)
		rule, _ := json.Marshal(access)
		return platform.Result{Status: "denied", Message: "refused: " + access.Problem, Decision: platform.DecisionDeniedFixed,
			Reason: delegation.CodeFieldRule, Rule: rule}, false
	}
	// The guards run here too (G1.4), also without an enforcing delegation: they can refuse a call
	// that the delegation alone would allow.
	if res, refused, err := m.guardRefusal(ctx, chatID, req, access); err != nil {
		return platform.Result{Status: "error", Message: err.Error(), Decision: platform.DecisionError, Reason: "guard_error"}, false
	} else if refused {
		rule, _ := json.Marshal(access)
		res.Decision, res.Reason, res.Rule = platform.DecisionDeniedFixed, "guard", rule
		return res, false
	}
	if del == nil || !del.Enforcing() {
		return platform.Result{}, true
	}
	own := func(res, id string) bool {
		ok, _ := m.st.IsDelegationObject(ctx, chatID, res, id)
		return ok
	}
	if dec := del.Check(access, time.Now(), own); !dec.Allowed {
		allowed := false
		return platform.Result{Status: "denied", Message: dec.Reason, Decision: platform.DecisionDeniedDelegation, Reason: dec.Code,
			Rule: dec.Rule, WithinDelegation: &allowed}, false
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

// rightsText describes to the agent its rights together with the objects it has created. In an
// experiment chat the protection level stays unreadable (X0.10): the summary names no enforcement,
// and in level "none" the answer is the one of a chat without delegated rights.
func (m *Manager) rightsText(ctx context.Context, chatID string, del *delegation.Delegation) string {
	experiment := m.experimentOf(ctx, chatID)
	if experiment != nil && experiment.Protection == "none" {
		del = nil
	}
	if del == nil {
		return "No delegation is set for this chat.\n" + effectRules(m.computeMax())
	}
	s := del.Summary()
	if experiment != nil {
		s = del.SummaryNeutral()
	}
	if objs, err := m.st.ListDelegationObjects(ctx, chatID); err == nil && len(objs) > 0 {
		s += "\nCreated in this chat:"
		for _, o := range objs {
			s += "\n  " + o.Resource + " " + o.ObjectID
		}
	}
	return s + "\n" + effectRules(m.computeMax())
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

// Decide is the user's decision from the API. Approving re-reads the platform first when the approval belongs to a
// waiting call (issue L2/#119): the user only approves what they saw, see decideChecked.
func (m *Manager) Decide(ctx context.Context, approvalID string, approve bool) (store.Approval, error) {
	if approve {
		if f := m.recheckFor(approvalID); f != nil {
			return m.decideChecked(ctx, approvalID, f)
		}
	}
	return m.decide(ctx, approvalID, approve)
}

// decide writes the decision and wakes the waiting call.
func (m *Manager) decide(ctx context.Context, approvalID string, approve bool) (store.Approval, error) {
	state := store.ApprovalRejected
	if approve {
		state = store.ApprovalApproved
	}
	ap, ok, err := m.st.DecideApproval(ctx, approvalID, state)
	if err != nil {
		return ap, err
	}
	if ok {
		// A plan is recorded and announced before the waiting call wakes and before the approval goes out (issue #166).
		if d := m.planDecisionFor(approvalID); d != nil {
			if _, err := d.decide(ctx, state); err != nil {
				slog.Error("plan decision not recorded", "approval", approvalID, "error", err)
			}
		}
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
	art := store.Artifact{ChatID: chatID, Kind: store.KindInput, Name: name, Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:]), ContentType: ct, Via: "ui", ObjectKey: key,
		Preview: previewOf(name, data)}
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
	if err := m.st.RecoverPlans(ctx); err != nil {
		return err
	}
	// Result checks of the last run have no container any more; they end as failed (R0.22).
	if n, err := m.st.RecoverAssessments(ctx); err != nil {
		return err
	} else if n > 0 {
		slog.Info("result checks ended after restart", "count", n)
	}
	// The background jobs of pre-annotation (V3) ran in a goroutine of the last process: a job that was
	// queued or running can never end by itself, and the agent would wait for it forever.
	if n, err := m.st.RecoverAnnotationJobs(ctx); err != nil {
		return err
	} else if n > 0 {
		slog.Info("annotation jobs ended after restart", "count", n)
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
		m.detach(ctx, id, l, true, true, pool.ReasonShutdown)
		_ = m.st.SetState(ctx, id, store.StateDormant)
	}
}

// Messages, Approvals and SocketCalls for the chat view.
func (m *Manager) Messages(ctx context.Context, chatID string) ([]store.Message, error) {
	return m.st.Messages(ctx, chatID)
}

// ChatBindings are the bindings the chat has (its variant, without the experiment prefix); empty if the chat
// or its variant cannot be read. The references to tools in the texts for the agent are rendered with them (#200).
func (m *Manager) ChatBindings(ctx context.Context, chatID string) toolset.Set {
	c, err := m.st.GetChat(ctx, chatID)
	if err != nil {
		return toolset.Set{}
	}
	base, _ := toolset.SplitExperiment(c.Variant)
	ts, err := toolset.FromVariant(base)
	if err != nil {
		return toolset.Set{}
	}
	return ts
}

// RenderNote renders the tool references in a note the gateway wrote itself, for all bindings of the chat
// (a note answers no call, so it names the forms of every binding with "or"). Text from the sandbox is not
// rendered.
func (m *Manager) RenderNote(ctx context.Context, chatID, s string) string {
	if !strings.Contains(s, "{{tool:") {
		return s
	}
	set := m.ChatBindings(ctx, chatID)
	if set.Empty() {
		set = toolset.Of(toolset.Order...)
	}
	return platform.Render(s, "", set)
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

// Activity returns platform calls across chats (GET /api/activity); the caller sets the owner.
func (m *Manager) Activity(ctx context.Context, f store.ActivityFilter) (store.ActivityPage, error) {
	return m.st.ListActivity(ctx, f)
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

// notebookShape is the shape of the notebook servers the approval names; without a notebook service
// the default.
func (m *Manager) notebookShape() platform.NotebookShape {
	if m.opt.Notebook == nil {
		return platform.DefaultNotebookShape
	}
	return m.opt.Notebook.Shape()
}

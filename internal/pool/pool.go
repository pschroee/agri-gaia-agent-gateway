// Package pool keeps pre-started sandboxes ready per binding variant
// (warm pool, E7). A variant is the canonical key of a combination of bindings (package toolset);
// the targets name the keys kept warm, further keys accepted by SetKnown get a slot on demand. Every slot is assigned exactly once and destroyed
// afterwards, never put back into the pool.
package pool

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"sort"
	"sync"
	"time"
)

type State string

const (
	StateStarting State = "starting"
	StateIdle     State = "idle"
	StateAssigned State = "assigned"
	StateStopping State = "stopping"
)

var (
	ErrNoIdleSlot     = errors.New("no free slot in the pool")
	ErrUnknownVariant = errors.New("unknown binding variant")
)

type CreateFunc[W any] func(ctx context.Context, slotID, variant string) (W, error)

// DestroyFunc tears a worker down. The context carries the reason (Reason), which the
// destroy function logs with its teardown (issue #55).
type DestroyFunc[W any] func(ctx context.Context, w W)

// Reasons for tearing a slot down, logged as reason= (issue #55).
const (
	ReasonStartFailed  = "start_failed"   // the start failed; the slot never became free
	ReasonShutdown     = "shutdown"       // the orchestrator stops
	ReasonSuspended    = "chat_suspended" // the chat went idle (timer, user, close)
	ReasonAgentDied    = "agent_died"     // pi or the execution sandbox of the chat ended
	ReasonAttachFailed = "attach_failed"  // the chat could not take the slot over
	ReasonUnspecified  = "unspecified"    // no reason in the context (tests, direct calls)
)

type reasonKey struct{}

// WithReason returns ctx carrying the reason for a teardown.
func WithReason(ctx context.Context, reason string) context.Context {
	return context.WithValue(ctx, reasonKey{}, reason)
}

// Reason is the teardown reason carried by ctx, ReasonUnspecified if there is none.
func Reason(ctx context.Context) string {
	if r, ok := ctx.Value(reasonKey{}).(string); ok && r != "" {
		return r
	}
	return ReasonUnspecified
}

// Backoff of the warm start after failed starts (issue #55): retryDelay doubles with every failure
// in a row up to maxRetryDelay; from failStreakAlarm failures in a row on the pool reports it once
// as an error, and once more when a start succeeds again.
const (
	defaultRetryDelay = 3 * time.Second
	defaultMaxRetry   = 5 * time.Minute
	failStreakAlarm   = 3
)

// backoffDelay is the wait before the next warm start after n failures in a row (n >= 1).
func backoffDelay(n int, base, max time.Duration) time.Duration {
	d := base
	for i := 1; i < n && d < max; i++ {
		d *= 2
	}
	if d > max {
		d = max
	}
	return d
}

// failState counts failed starts in a row per variant.
type failState struct {
	streak  int
	until   time.Time // no warm start before this time
	alarmed bool      // the streak has been reported as an error
}

type Activity struct {
	Kind  string    `json:"kind"`
	Tool  string    `json:"tool,omitempty"`
	Since time.Time `json:"since"`
}

// Info is the status API's view of a slot.
type Info struct {
	ID         string     `json:"id"`
	Variant    string     `json:"variant"`
	State      State      `json:"state"`
	CreatedAt  time.Time  `json:"created_at"`
	AssignedAt *time.Time `json:"assigned_at,omitempty"`
	ChatID     string     `json:"chat_id,omitempty"`
	Activity   *Activity  `json:"activity,omitempty"`
}

type Slot[W any] struct {
	ID      string
	Variant string
	Worker  W

	mu         sync.Mutex
	state      State
	createdAt  time.Time
	assignedAt time.Time
	chatID     string
	activity   *Activity
}

func (s *Slot[W]) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// ChatID is the slot's binding. The slot's socket derives from it
// which chat a request is for.
func (s *Slot[W]) ChatID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != StateAssigned {
		return ""
	}
	return s.chatID
}

func (s *Slot[W]) SetActivity(kind, tool string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.activity != nil && s.activity.Kind == kind && s.activity.Tool == tool {
		return
	}
	s.activity = &Activity{Kind: kind, Tool: tool, Since: time.Now()}
}

func (s *Slot[W]) Info() Info {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := Info{ID: s.ID, Variant: s.Variant, State: s.state, CreatedAt: s.createdAt, ChatID: s.chatID}
	if !s.assignedAt.IsZero() {
		t := s.assignedAt
		i.AssignedAt = &t
	}
	if s.activity != nil && s.state == StateAssigned {
		a := *s.activity
		i.Activity = &a
	}
	return i
}

// Lock order: Pool.mu before Slot.mu.
type Pool[W any] struct {
	create  CreateFunc[W]
	destroy DestroyFunc[W]
	targets map[string]int
	known   func(variant string) bool // further variants started on demand (nil: none)

	mu      sync.Mutex
	slots   map[string]*Slot[W]
	lastErr map[string]string
	fails   map[string]*failState
	wake    chan struct{}
	idleCh  chan struct{} // closed and replaced on every new free slot
	ctx     context.Context
	wg      sync.WaitGroup

	retryDelay    time.Duration
	maxRetryDelay time.Duration
	log           *slog.Logger
}

func New[W any](create CreateFunc[W], destroy DestroyFunc[W], targets map[string]int) *Pool[W] {
	t := map[string]int{}
	for k, v := range targets {
		t[k] = v
	}
	return &Pool[W]{
		create: create, destroy: destroy, targets: t,
		slots: map[string]*Slot[W]{}, lastErr: map[string]string{}, fails: map[string]*failState{},
		wake: make(chan struct{}, 1), idleCh: make(chan struct{}),
		retryDelay: defaultRetryDelay, maxRetryDelay: defaultMaxRetry,
		log: slog.Default(),
	}
}

// SetKnown accepts variants beyond the targets: they have no warm slots, AcquireWait starts one on
// demand. Used for chats stored with another combination than the configured one, so that they
// resume (issue #29). Call before Start.
func (p *Pool[W]) SetKnown(known func(variant string) bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.known = known
}

func (p *Pool[W]) Targets() map[string]int {
	out := map[string]int{}
	for k, v := range p.targets {
		out[k] = v
	}
	return out
}

func (p *Pool[W]) Start(ctx context.Context) {
	p.ctx = ctx
	p.wg.Add(1)
	go p.loop(ctx)
	p.kick()
}

func (p *Pool[W]) kick() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *Pool[W]) loop(ctx context.Context) {
	defer p.wg.Done()
	for {
		p.fill(ctx)
		select {
		case <-ctx.Done():
			return
		case <-p.wake:
		}
	}
}

// fill starts the missing slots and returns at once. Each start runs on its own, so a slot taken
// while others are still starting is replaced right away instead of after the slowest start of the
// running batch (issue #30). After a failed start the variant waits (backoff, issue #55); the
// failure kicks the loop again when the wait is over.
func (p *Pool[W]) fill(ctx context.Context) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	for variant, target := range p.targets {
		if f := p.fails[variant]; f != nil && now.Before(f.until) {
			continue
		}
		have := 0
		for _, s := range p.slots {
			if st := s.State(); s.Variant == variant && (st == StateIdle || st == StateStarting) {
				have++
			}
		}
		for ; have < target; have++ {
			s := &Slot[W]{ID: newID(), Variant: variant, state: StateStarting, createdAt: time.Now()}
			p.slots[s.ID] = s
			p.wg.Add(1)
			go func() {
				defer p.wg.Done()
				if d, failed := p.startWarm(ctx, s); failed {
					p.retryLater(ctx, d)
				}
			}()
		}
	}
}

// startWarm starts a slot of the warm pool and keeps the backoff of its variant: after a failure
// it returns the wait before the next warm start and true.
func (p *Pool[W]) startWarm(ctx context.Context, s *Slot[W]) (time.Duration, bool) {
	ok := p.start(ctx, s)
	p.mu.Lock()
	defer p.mu.Unlock()
	f := p.fails[s.Variant]
	if ok {
		if f != nil && f.alarmed {
			p.log.Info("pool slots start again", "variant", s.Variant, "failed_before", f.streak)
		}
		delete(p.fails, s.Variant)
		return 0, false
	}
	if ctx.Err() != nil {
		return 0, true // shutting down; retryLater returns at once
	}
	if f == nil {
		f = &failState{}
		p.fails[s.Variant] = f
	}
	f.streak++
	d := backoffDelay(f.streak, p.retryDelay, p.maxRetryDelay)
	f.until = time.Now().Add(d)
	if f.streak >= failStreakAlarm && !f.alarmed {
		f.alarmed = true
		p.log.Error("pool slots keep failing to start; starting new ones with growing delay until one succeeds",
			"variant", s.Variant, "failures", f.streak, "next_try_in", d.String(), "max_delay", p.maxRetryDelay.String(),
			"error", p.lastErr[s.Variant])
	}
	return d, true
}

// start creates the worker of a slot registered as starting and makes it idle. False if the start failed.
// A failed start is logged as a WARN with its error: the slot is discarded before its first use.
func (p *Pool[W]) start(ctx context.Context, s *Slot[W]) bool {
	w, err := p.create(ctx, s.ID, s.Variant)
	p.mu.Lock()
	if err != nil {
		delete(p.slots, s.ID)
		p.lastErr[s.Variant] = err.Error()
		p.mu.Unlock()
		if ctx.Err() == nil {
			p.log.Warn("pool slot discarded before first use", "slot", s.ID, "variant", s.Variant,
				"reason", ReasonStartFailed, "after_ms", time.Since(s.createdAt).Milliseconds(), "error", err.Error())
		}
		return false
	}
	if ctx.Err() != nil {
		// Finished only after shutdown began: tear down here, not in a separate
		// goroutine, otherwise Shutdown would return before the container is gone.
		delete(p.slots, s.ID)
		p.mu.Unlock()
		p.destroy(WithReason(context.WithoutCancel(ctx), ReasonShutdown), w)
		return true
	}
	defer p.mu.Unlock()
	s.mu.Lock()
	s.Worker = w
	s.state = StateIdle
	s.mu.Unlock()
	close(p.idleCh)
	p.idleCh = make(chan struct{})
	return true
}

// retryLater kicks the fill loop after d (a failed start), unless the pool shuts down.
func (p *Pool[W]) retryLater(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
		p.kick()
	}
}

func (p *Pool[W]) LastError(variant string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastErr[variant]
}

// Acquire assigns a free slot of the variant to a chat.
func (p *Pool[W]) Acquire(variant, chatID string) (*Slot[W], error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.targets[variant]; !ok && (p.known == nil || !p.known(variant)) {
		return nil, ErrUnknownVariant
	}
	var best *Slot[W]
	for _, s := range p.slots {
		if s.Variant == variant && s.State() == StateIdle && (best == nil || s.createdAt.Before(best.createdAt)) {
			best = s
		}
	}
	if best == nil {
		p.kick()
		return nil, ErrNoIdleSlot
	}
	best.mu.Lock()
	best.state = StateAssigned
	best.chatID = chatID
	best.assignedAt = time.Now()
	best.activity = &Activity{Kind: "idle", Since: best.assignedAt}
	best.mu.Unlock()
	p.kick()
	return best, nil
}

// AcquireWait waits up to timeout for a free slot. Variants
// with target size 0 are also started once on demand.
func (p *Pool[W]) AcquireWait(ctx context.Context, variant, chatID string, timeout time.Duration) (*Slot[W], error) {
	deadline := time.After(timeout)
	for {
		s, err := p.Acquire(variant, chatID)
		if !errors.Is(err, ErrNoIdleSlot) {
			return s, err
		}
		p.mu.Lock()
		if p.targets[variant] == 0 && !p.startingLocked(variant) {
			p.spawnOnDemandLocked(variant)
		}
		ch := p.idleCh
		p.mu.Unlock()
		select {
		case <-ch:
		case <-deadline:
			return nil, ErrNoIdleSlot
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (p *Pool[W]) startingLocked(variant string) bool {
	for _, s := range p.slots {
		if st := s.State(); s.Variant == variant && (st == StateStarting || st == StateIdle) {
			return true
		}
	}
	return false
}

func (p *Pool[W]) spawnOnDemandLocked(variant string) {
	s := &Slot[W]{ID: newID(), Variant: variant, state: StateStarting, createdAt: time.Now()}
	p.slots[s.ID] = s
	ctx := p.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.start(ctx, s)
	}()
}

// Release destroys an assigned slot (single assignment); reason is logged with the teardown.
func (p *Pool[W]) Release(ctx context.Context, s *Slot[W], reason string) {
	s.mu.Lock()
	if s.state == StateStopping {
		s.mu.Unlock()
		return
	}
	s.state = StateStopping
	s.mu.Unlock()
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.destroy(WithReason(context.WithoutCancel(ctx), reason), s.Worker)
		p.mu.Lock()
		delete(p.slots, s.ID)
		p.mu.Unlock()
		p.kick()
	}()
}

func (p *Pool[W]) Get(id string) (*Slot[W], bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.slots[id]
	return s, ok
}

func (p *Pool[W]) Snapshot() []Info {
	p.mu.Lock()
	slots := make([]*Slot[W], 0, len(p.slots))
	for _, s := range p.slots {
		slots = append(slots, s)
	}
	p.mu.Unlock()
	out := make([]Info, 0, len(slots))
	for _, s := range slots {
		out = append(out, s.Info())
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Variant != out[j].Variant {
			return out[i].Variant < out[j].Variant
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out
}

// Shutdown waits for running starts and destroys all slots.
func (p *Pool[W]) Shutdown(ctx context.Context) {
	p.wg.Wait()
	p.mu.Lock()
	var all []*Slot[W]
	for _, s := range p.slots {
		all = append(all, s)
	}
	p.slots = map[string]*Slot[W]{}
	p.mu.Unlock()
	var wg sync.WaitGroup
	for _, s := range all {
		if s.State() == StateStarting {
			continue
		}
		wg.Add(1)
		go func(s *Slot[W]) { defer wg.Done(); p.destroy(WithReason(ctx, ReasonShutdown), s.Worker) }(s)
	}
	wg.Wait()
}

func newID() string {
	b := make([]byte, 6) // 48 bits: collisions practically ruled out
	_, _ = rand.Read(b)
	return "p-" + hex.EncodeToString(b)
}

// Package pool hält je Anbindungsvariante vorgestartete Sandboxen bereit
// (Warm-Pool, E7). Jeder Platz wird genau einmal vergeben und danach
// zerstört, nie in den Pool zurückgelegt.
package pool

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
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
	ErrNoIdleSlot     = errors.New("kein freier Platz im Pool")
	ErrUnknownVariant = errors.New("unbekannte Anbindungsvariante")
)

type CreateFunc[W any] func(ctx context.Context, slotID, variant string) (W, error)
type DestroyFunc[W any] func(ctx context.Context, w W)

type Activity struct {
	Kind  string    `json:"kind"`
	Tool  string    `json:"tool,omitempty"`
	Since time.Time `json:"since"`
}

// Info ist die Sicht der Status-API auf einen Platz.
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

// ChatID ist die Bindung des Platzes. Der Socket des Platzes leitet daraus ab,
// für welchen Chat eine Anfrage gilt.
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

// Sperrreihenfolge: Pool.mu vor Slot.mu.
type Pool[W any] struct {
	create  CreateFunc[W]
	destroy DestroyFunc[W]
	targets map[string]int

	mu      sync.Mutex
	slots   map[string]*Slot[W]
	lastErr map[string]string
	wake    chan struct{}
	idleCh  chan struct{} // wird bei jedem neuen freien Platz geschlossen und ersetzt
	ctx     context.Context
	wg      sync.WaitGroup

	retryDelay time.Duration
}

func New[W any](create CreateFunc[W], destroy DestroyFunc[W], targets map[string]int) *Pool[W] {
	t := map[string]int{}
	for k, v := range targets {
		t[k] = v
	}
	return &Pool[W]{
		create: create, destroy: destroy, targets: t,
		slots: map[string]*Slot[W]{}, lastErr: map[string]string{},
		wake: make(chan struct{}, 1), idleCh: make(chan struct{}),
		retryDelay: 3 * time.Second,
	}
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
		failed := p.fill(ctx)
		var retry <-chan time.Time
		if failed {
			retry = time.After(p.retryDelay)
		}
		select {
		case <-ctx.Done():
			return
		case <-p.wake:
		case <-retry:
		}
	}
}

// fill startet fehlende Plätze parallel und wartet auf alle. Liefert true,
// wenn ein Start fehlschlug.
func (p *Pool[W]) fill(ctx context.Context) bool {
	type job struct{ slot *Slot[W] }
	var jobs []job
	p.mu.Lock()
	for variant, target := range p.targets {
		have := 0
		for _, s := range p.slots {
			if st := s.State(); s.Variant == variant && (st == StateIdle || st == StateStarting) {
				have++
			}
		}
		for ; have < target; have++ {
			s := &Slot[W]{ID: newID(), Variant: variant, state: StateStarting, createdAt: time.Now()}
			p.slots[s.ID] = s
			jobs = append(jobs, job{s})
		}
	}
	p.mu.Unlock()

	var wg sync.WaitGroup
	var failed sync.Once
	anyFailed := false
	for _, j := range jobs {
		wg.Add(1)
		go func(s *Slot[W]) {
			defer wg.Done()
			w, err := p.create(ctx, s.ID, s.Variant)
			p.mu.Lock()
			if err != nil {
				delete(p.slots, s.ID)
				p.lastErr[s.Variant] = err.Error()
				p.mu.Unlock()
				failed.Do(func() { anyFailed = true })
				return
			}
			if ctx.Err() != nil {
				// Erst nach Beginn des Herunterfahrens fertig: hier abbauen, nicht in einer eigenen
				// Goroutine, sonst kehrte Shutdown zurück, bevor der Container weg ist.
				delete(p.slots, s.ID)
				p.mu.Unlock()
				p.destroy(context.WithoutCancel(ctx), w)
				return
			}
			defer p.mu.Unlock()
			s.mu.Lock()
			s.Worker = w
			s.state = StateIdle
			s.mu.Unlock()
			close(p.idleCh)
			p.idleCh = make(chan struct{})
		}(j.slot)
	}
	wg.Wait()
	return anyFailed
}

func (p *Pool[W]) LastError(variant string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastErr[variant]
}

// Acquire vergibt einen freien Platz der Variante an einen Chat.
func (p *Pool[W]) Acquire(variant, chatID string) (*Slot[W], error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.targets[variant]; !ok {
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

// AcquireWait wartet bis zu timeout auf einen freien Platz. Auch Varianten
// mit Zielgröße 0 werden bei Bedarf einmalig gestartet.
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
	go func() {
		w, err := p.create(ctx, s.ID, variant)
		p.mu.Lock()
		defer p.mu.Unlock()
		if err != nil {
			delete(p.slots, s.ID)
			p.lastErr[variant] = err.Error()
			return
		}
		s.mu.Lock()
		s.Worker, s.state = w, StateIdle
		s.mu.Unlock()
		close(p.idleCh)
		p.idleCh = make(chan struct{})
	}()
}

// Release zerstört einen vergebenen Platz (Einmalvergabe).
func (p *Pool[W]) Release(ctx context.Context, s *Slot[W]) {
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
		p.destroy(context.WithoutCancel(ctx), s.Worker)
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

// Shutdown wartet auf laufende Starts und zerstört alle Plätze.
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
		go func(s *Slot[W]) { defer wg.Done(); p.destroy(ctx, s.Worker) }(s)
	}
	wg.Wait()
}

func newID() string {
	b := make([]byte, 6) // 48 Bit: Kollisionen praktisch ausgeschlossen
	_, _ = rand.Read(b)
	return "p-" + hex.EncodeToString(b)
}

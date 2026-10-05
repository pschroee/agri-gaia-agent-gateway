package pool

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeWorker struct {
	variant string
	id      string
}

type fakeFactory struct {
	mu        sync.Mutex
	created   []string
	destroyed []string
	fail      atomic.Int32 // this many Create calls fail
	block     chan struct{}
}

func (f *fakeFactory) Create(ctx context.Context, slotID, variant string) (*fakeWorker, error) {
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if f.fail.Load() > 0 {
		f.fail.Add(-1)
		return nil, errors.New("start failed")
	}
	f.mu.Lock()
	f.created = append(f.created, slotID)
	f.mu.Unlock()
	return &fakeWorker{variant: variant, id: slotID}, nil
}

func (f *fakeFactory) Destroy(_ context.Context, w *fakeWorker) {
	f.mu.Lock()
	f.destroyed = append(f.destroyed, w.id)
	f.mu.Unlock()
}

func (f *fakeFactory) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.created), len(f.destroyed)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timeout: %s", what)
}

func idle(p *Pool[*fakeWorker], variant string) int {
	n := 0
	for _, s := range p.Snapshot() {
		if s.Variant == variant && s.State == StateIdle {
			n++
		}
	}
	return n
}

func newTestPool(t *testing.T, f *fakeFactory, targets map[string]int) *Pool[*fakeWorker] {
	t.Helper()
	p := New[*fakeWorker](f.Create, f.Destroy, targets)
	p.retryDelay = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	p.Start(ctx)
	t.Cleanup(func() { cancel(); p.Shutdown(context.Background()) })
	return p
}

func TestFillsToTargetPerVariant(t *testing.T) {
	f := &fakeFactory{}
	p := newTestPool(t, f, map[string]int{"cli": 2, "mcp": 1, "beide": 0})
	waitFor(t, "pool filled", func() bool { return idle(p, "cli") == 2 && idle(p, "mcp") == 1 })
	if n := idle(p, "beide"); n != 0 {
		t.Fatalf("beide: %d", n)
	}
}

func TestAcquireAssignsAndRefills(t *testing.T) {
	f := &fakeFactory{}
	p := newTestPool(t, f, map[string]int{"cli": 1})
	waitFor(t, "pool filled", func() bool { return idle(p, "cli") == 1 })

	s, err := p.Acquire("cli", "chat-1")
	if err != nil {
		t.Fatal(err)
	}
	if s.State() != StateAssigned || s.ChatID() != "chat-1" || s.Worker == nil {
		t.Fatalf("slot not assigned: %+v", s.Info())
	}
	// Refill: a new free slot appears, the assigned one stays.
	waitFor(t, "refilled", func() bool { return idle(p, "cli") == 1 })
	if len(p.Snapshot()) != 2 {
		t.Fatalf("expected 2 slots, got %d", len(p.Snapshot()))
	}
}

func TestAcquireEmpty(t *testing.T) {
	f := &fakeFactory{block: make(chan struct{})}
	p := newTestPool(t, f, map[string]int{"cli": 1})
	if _, err := p.Acquire("cli", "c"); !errors.Is(err, ErrNoIdleSlot) {
		t.Fatalf("expected ErrNoIdleSlot, got %v", err)
	}
	if _, err := p.Acquire("doesnotexist", "c"); !errors.Is(err, ErrUnknownVariant) {
		t.Fatalf("expected ErrUnknownVariant, got %v", err)
	}
	close(f.block)
}

// Single assignment: a returned slot is destroyed, never reused.
func TestReleaseDestroysNeverReuses(t *testing.T) {
	f := &fakeFactory{}
	p := newTestPool(t, f, map[string]int{"cli": 1})
	waitFor(t, "filled", func() bool { return idle(p, "cli") == 1 })
	s, _ := p.Acquire("cli", "chat-1")
	first := s.ID
	p.Release(context.Background(), s)
	waitFor(t, "destroyed", func() bool { _, d := f.counts(); return d == 1 })
	for _, info := range p.Snapshot() {
		if info.ID == first {
			t.Fatalf("destroyed slot still in the pool: %+v", info)
		}
	}
	s2, err := p.AcquireWait(context.Background(), "cli", "chat-2", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if s2.ID == first {
		t.Fatal("slot reused")
	}
}

func TestCreateFailureIsRetried(t *testing.T) {
	f := &fakeFactory{}
	f.fail.Store(2)
	p := newTestPool(t, f, map[string]int{"cli": 1})
	waitFor(t, "filled after errors", func() bool { return idle(p, "cli") == 1 })
	if p.LastError("cli") == "" {
		t.Fatal("last error not recorded")
	}
}

func TestShutdownDestroysAll(t *testing.T) {
	f := &fakeFactory{}
	p := New[*fakeWorker](f.Create, f.Destroy, map[string]int{"cli": 2})
	ctx, cancel := context.WithCancel(context.Background())
	p.Start(ctx)
	waitFor(t, "filled", func() bool { return idle(p, "cli") == 2 })
	_, _ = p.Acquire("cli", "c")
	cancel()
	p.Shutdown(context.Background())
	c, d := f.counts()
	if c != d {
		t.Fatalf("created %d, destroyed %d", c, d)
	}
}

func TestActivityOnSlot(t *testing.T) {
	f := &fakeFactory{}
	p := newTestPool(t, f, map[string]int{"cli": 1})
	waitFor(t, "filled", func() bool { return idle(p, "cli") == 1 })
	s, _ := p.Acquire("cli", "c")
	s.SetActivity("tool", "bash")
	info := s.Info()
	if info.Activity == nil || info.Activity.Kind != "tool" || info.Activity.Tool != "bash" {
		t.Fatalf("activity: %+v", info.Activity)
	}
}

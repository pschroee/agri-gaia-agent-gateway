package pool

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agw/internal/toolset"
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
	p := newTestPool(t, f, map[string]int{"cli": 2, "mcp": 1, "both": 0})
	waitFor(t, "pool filled", func() bool { return idle(p, "cli") == 2 && idle(p, "mcp") == 1 })
	if n := idle(p, "both"); n != 0 {
		t.Fatalf("both: %d", n)
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
	p.Release(context.Background(), s, "test")
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

// Issue #29: the pool keeps warm slots only for the configured combination; chats of another
// stored combination get a slot on demand, unknown keys are refused.
func TestKnownVariantsOnDemand(t *testing.T) {
	f := &fakeFactory{}
	p := newTestPool(t, f, map[string]int{"cli,api": 2})
	p.SetKnown(toolset.Valid)
	waitFor(t, "pool filled", func() bool { return idle(p, "cli,api") == 2 })
	if _, err := p.Acquire("nonsense", "c"); !errors.Is(err, ErrUnknownVariant) {
		t.Fatalf("unknown key: %v", err)
	}
	ctx := context.Background()
	for _, v := range []string{"cli", "mcp", "api", "cli,mcp"} {
		s, err := p.AcquireWait(ctx, v, "chat-"+v, 2*time.Second)
		if err != nil {
			t.Fatalf("%s: %v", v, err)
		}
		if s.Variant != v || s.Worker.variant != v {
			t.Fatalf("%s: slot of %q (worker %q)", v, s.Variant, s.Worker.variant)
		}
	}
	// No warm slots for keys outside the targets, and the configured ones stay untouched.
	time.Sleep(50 * time.Millisecond)
	for _, v := range []string{"cli", "mcp", "api", "cli,mcp"} {
		if n := idle(p, v); n != 0 {
			t.Fatalf("%s kept %d warm slots", v, n)
		}
	}
	if n := idle(p, "cli,api"); n != 2 {
		t.Fatalf("configured combination: %d idle", n)
	}
	if got := p.Targets(); len(got) != 1 || got["cli,api"] != 2 {
		t.Fatalf("targets: %v", got)
	}
}

// gatedFactory holds back chosen starts (counted from 1) until their gate is closed.
type gatedFactory struct {
	fakeFactory
	gmu     sync.Mutex
	started int
	gates   map[int]chan struct{}
}

func (f *gatedFactory) hold(n int) chan struct{} {
	f.gmu.Lock()
	defer f.gmu.Unlock()
	if f.gates == nil {
		f.gates = map[int]chan struct{}{}
	}
	g := make(chan struct{})
	f.gates[n] = g
	return g
}

func (f *gatedFactory) count() int {
	f.gmu.Lock()
	defer f.gmu.Unlock()
	return f.started
}

func (f *gatedFactory) Create(ctx context.Context, slotID, variant string) (*fakeWorker, error) {
	f.gmu.Lock()
	f.started++
	g := f.gates[f.started]
	f.gmu.Unlock()
	if g != nil {
		select {
		case <-g:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return f.fakeFactory.Create(ctx, slotID, variant)
}

// Issue #30: a slot taken while another replacement is still starting is replaced at once, not after
// the running start has finished (the pool used to wait for the whole batch before filling again).
func TestRefillDoesNotWaitForRunningStarts(t *testing.T) {
	f := &gatedFactory{}
	p := New[*fakeWorker](f.Create, f.Destroy, map[string]int{"cli": 2})
	p.retryDelay = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	p.Start(ctx)
	t.Cleanup(func() { cancel(); p.Shutdown(context.Background()) })
	waitFor(t, "pool filled", func() bool { return idle(p, "cli") == 2 })

	slow := f.hold(3) // the first replacement hangs
	if _, err := p.Acquire("cli", "chat-1"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "first replacement starting", func() bool { return f.count() == 3 })
	if _, err := p.Acquire("cli", "chat-2"); err != nil {
		t.Fatal(err)
	}
	// the second replacement becomes free while the first still hangs
	waitFor(t, "second replacement ready", func() bool { return idle(p, "cli") == 1 })
	close(slow)
	waitFor(t, "pool full again", func() bool { return idle(p, "cli") == 2 })
}

// Issue #55: the wait before the next warm start doubles with every failure in a row and is capped.
func TestBackoffDelay(t *testing.T) {
	base, max := 3*time.Second, 5*time.Minute
	for n, want := range map[int]time.Duration{
		1: 3 * time.Second, 2: 6 * time.Second, 3: 12 * time.Second, 4: 24 * time.Second,
		7: 192 * time.Second, 8: 5 * time.Minute, 30: 5 * time.Minute,
	} {
		if got := backoffDelay(n, base, max); got != want {
			t.Errorf("backoffDelay(%d) = %v, want %v", n, got, want)
		}
	}
}

func TestReasonFromContext(t *testing.T) {
	if r := Reason(context.Background()); r != ReasonUnspecified {
		t.Fatalf("without reason: %q", r)
	}
	if r := Reason(WithReason(context.Background(), ReasonShutdown)); r != ReasonShutdown {
		t.Fatalf("with reason: %q", r)
	}
}

// reasonFactory records the teardown reason of every destroyed worker.
type reasonFactory struct {
	fakeFactory
	rmu     sync.Mutex
	reasons map[string]string
}

func (f *reasonFactory) Destroy(ctx context.Context, w *fakeWorker) {
	f.rmu.Lock()
	if f.reasons == nil {
		f.reasons = map[string]string{}
	}
	f.reasons[w.id] = Reason(ctx)
	f.rmu.Unlock()
	f.fakeFactory.Destroy(ctx, w)
}

func (f *reasonFactory) reason(id string) string {
	f.rmu.Lock()
	defer f.rmu.Unlock()
	return f.reasons[id]
}

// Every teardown through the pool carries a reason: the caller's on Release, shutdown on Shutdown.
func TestTeardownCarriesReason(t *testing.T) {
	f := &reasonFactory{}
	p := New[*fakeWorker](f.Create, f.Destroy, map[string]int{"cli": 2})
	p.retryDelay = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	p.Start(ctx)
	waitFor(t, "pool filled", func() bool { return idle(p, "cli") == 2 })
	s, err := p.Acquire("cli", "chat-1")
	if err != nil {
		t.Fatal(err)
	}
	p.Release(context.Background(), s, ReasonSuspended)
	waitFor(t, "released slot destroyed", func() bool { return f.reason(s.ID) != "" })
	if r := f.reason(s.ID); r != ReasonSuspended {
		t.Fatalf("release reason: %q", r)
	}
	waitFor(t, "pool refilled", func() bool { return idle(p, "cli") == 2 })
	var rest []string
	for _, i := range p.Snapshot() {
		rest = append(rest, i.ID)
	}
	cancel()
	p.Shutdown(context.Background())
	for _, id := range rest {
		if r := f.reason(id); r != ReasonShutdown {
			t.Fatalf("slot %s: shutdown reason %q", id, r)
		}
	}
}

// logRecorder is a slog handler that keeps the records.
type logRecorder struct {
	mu   sync.Mutex
	recs []slog.Record
}

func (h *logRecorder) Enabled(context.Context, slog.Level) bool { return true }
func (h *logRecorder) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	h.recs = append(h.recs, r.Clone())
	h.mu.Unlock()
	return nil
}
func (h *logRecorder) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *logRecorder) WithGroup(string) slog.Handler      { return h }

// count returns how many records have the level and message; attrs of the last one by key.
func (h *logRecorder) count(level slog.Level, msg string) (int, map[string]string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	n, attrs := 0, map[string]string{}
	for _, r := range h.recs {
		if r.Level == level && r.Message == msg {
			n++
			r.Attrs(func(a slog.Attr) bool { attrs[a.Key] = a.Value.String(); return true })
		}
	}
	return n, attrs
}

// Issue #55: slots that fail to start are logged with their error as discarded before first use, the
// warm start backs off instead of retrying in a fixed rhythm, the streak is reported once as an error,
// and the recovery once as info.
func TestFailedStartsBackOffAndAreReportedOnce(t *testing.T) {
	f := &fakeFactory{}
	f.fail.Store(5)
	rec := &logRecorder{}
	p := New[*fakeWorker](f.Create, f.Destroy, map[string]int{"cli": 1})
	p.retryDelay, p.maxRetryDelay, p.log = 20*time.Millisecond, 80*time.Millisecond, slog.New(rec)
	ctx, cancel := context.WithCancel(context.Background())
	began := time.Now()
	p.Start(ctx)
	t.Cleanup(func() { cancel(); p.Shutdown(context.Background()) })
	waitFor(t, "filled after five failures", func() bool { return idle(p, "cli") == 1 })
	// Waits 20+40+80+80+80 ms between the six starts, at least 300 ms in all; a fixed 20 ms
	// rhythm would have taken 100 ms.
	if d := time.Since(began); d < 300*time.Millisecond {
		t.Fatalf("no backoff: filled after %v", d)
	}
	n, attrs := rec.count(slog.LevelWarn, "pool slot discarded before first use")
	if n != 5 || attrs["reason"] != ReasonStartFailed || attrs["error"] != "start failed" || attrs["variant"] != "cli" {
		t.Fatalf("discard warnings: %d %v", n, attrs)
	}
	if n, attrs := rec.count(slog.LevelError, "pool slots keep failing to start; starting new ones with growing delay until one succeeds"); n != 1 || attrs["failures"] != "3" || attrs["error"] != "start failed" {
		t.Fatalf("streak reported %d times: %v", n, attrs)
	}
	if n, attrs := rec.count(slog.LevelInfo, "pool slots start again"); n != 1 || attrs["failed_before"] != "5" {
		t.Fatalf("recovery reported %d times: %v", n, attrs)
	}
}

// A single failure is a warning, not yet a streak: no error, and the backoff resets after a success.
func TestSingleFailureNoAlarm(t *testing.T) {
	f := &fakeFactory{}
	f.fail.Store(1)
	rec := &logRecorder{}
	p := New[*fakeWorker](f.Create, f.Destroy, map[string]int{"cli": 1})
	p.retryDelay, p.log = 10*time.Millisecond, slog.New(rec)
	ctx, cancel := context.WithCancel(context.Background())
	p.Start(ctx)
	t.Cleanup(func() { cancel(); p.Shutdown(context.Background()) })
	waitFor(t, "filled", func() bool { return idle(p, "cli") == 1 })
	if n, _ := rec.count(slog.LevelError, "pool slots keep failing to start; starting new ones with growing delay until one succeeds"); n != 0 {
		t.Fatalf("error after one failure")
	}
	if n, _ := rec.count(slog.LevelInfo, "pool slots start again"); n != 0 {
		t.Fatalf("recovery reported without an alarm")
	}
	p.mu.Lock()
	_, left := p.fails["cli"]
	p.mu.Unlock()
	if left {
		t.Fatal("failure streak not reset after a successful start")
	}
}

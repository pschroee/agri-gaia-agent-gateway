package bgtask

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"agw/internal/execbox"
	"agw/internal/execproto"
	"agw/internal/store"
)

// fakeRunner simulates a background task: start, output, then end on demand or
// abort. The command determines the behaviour.
type fakeRunner struct {
	mu      sync.Mutex
	release map[string]chan execproto.Frame
	reqs    []execproto.Request
}

func (f *fakeRunner) RunBackground(ctx context.Context, req execproto.Request, onStart func(int), onData func([]byte)) (execproto.Frame, error) {
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	ch := make(chan execproto.Frame, 1)
	if f.release == nil {
		f.release = map[string]chan execproto.Frame{}
	}
	f.release[req.Command] = ch
	f.mu.Unlock()
	if strings.HasPrefix(req.Command, "nostart") {
		return execproto.Frame{Done: true, Error: "Working directory does not exist: /nope", Code: "ENOENT"}, nil
	}
	onStart(4242)
	if rest, ok := strings.CutPrefix(req.Command, "echo "); ok {
		onData([]byte(rest + "\n"))
	}
	if strings.HasPrefix(req.Command, "big ") { // 120 KiB of output in chunks as from the supervisor
		chunk := []byte(strings.Repeat("x", 32<<10-1) + "\n")
		for i := 0; i < 4; i++ {
			onData(chunk[:30<<10])
		}
	}
	select {
	case fr := <-ch:
		return fr, nil
	case <-ctx.Done():
		return execproto.Frame{Done: true, Error: "aborted", Code: "aborted"}, ctx.Err()
	}
}

func (f *fakeRunner) end(cmd string, fr execproto.Frame) {
	f.mu.Lock()
	ch := f.release[cmd]
	f.mu.Unlock()
	ch <- fr
}

type fakeNotifier struct {
	mu       sync.Mutex
	delay    time.Duration // BackgroundCreate takes this long (database)
	seq      int
	progress []store.BackgroundTask
	ended    []store.BackgroundTask
	notify   []bool
	endedCh  chan store.BackgroundTask
}

func (n *fakeNotifier) BackgroundCreate(_ context.Context, t store.BackgroundTask) (store.BackgroundTask, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.delay > 0 {
		n.mu.Unlock()
		time.Sleep(n.delay)
		n.mu.Lock()
	}
	n.seq++
	t.Seq, t.ID, t.State, t.StartedAt, t.LogPath = n.seq, store.BgID(n.seq), store.BgRunning, time.Now(), execproto.BgLogPath(n.seq)
	return t, nil
}
func (n *fakeNotifier) BackgroundProgress(t store.BackgroundTask) {
	n.mu.Lock()
	n.progress = append(n.progress, t)
	n.mu.Unlock()
}
func (n *fakeNotifier) BackgroundEnded(t store.BackgroundTask, notify bool) {
	n.mu.Lock()
	n.ended = append(n.ended, t)
	n.notify = append(n.notify, notify)
	n.mu.Unlock()
	if n.endedCh != nil {
		n.endedCh <- t
	}
}
func (n *fakeNotifier) BackgroundLookup(_ context.Context, chatID string, seq int) (store.BackgroundTask, error) {
	n.mu.Lock()
	for _, e := range n.ended {
		if e.Seq == seq && e.ChatID == chatID {
			n.mu.Unlock()
			return e, nil
		}
	}
	n.mu.Unlock()
	if seq == 99 {
		return store.BackgroundTask{ID: "bg-99", Seq: 99, ChatID: chatID, State: store.BgSuspended, Tail: "old\n"}, nil
	}
	return store.BackgroundTask{}, store.ErrNotFound
}

func newReg(t *testing.T, max int) (*Registry, *fakeRunner, *fakeNotifier) {
	run := &fakeRunner{}
	n := &fakeNotifier{endedCh: make(chan store.BackgroundTask, 16)}
	r := New("p-1", run, n, max)
	t.Cleanup(r.Close)
	return r, run, n
}

func waitEnded(t *testing.T, n *fakeNotifier) store.BackgroundTask {
	t.Helper()
	select {
	case e := <-n.endedCh:
		return e
	case <-time.After(5 * time.Second):
		t.Fatal("no end reported")
	}
	return store.BackgroundTask{}
}

func TestStartOutputEnd(t *testing.T) {
	r, run, n := newReg(t, 2)
	ctx := context.Background()
	bt, err := r.Start(ctx, StartParams{ChatID: "c1", Session: "main", ToolCallID: "call_1", Command: "echo finish-bg", Cwd: "/workspace", Timeout: 5})
	if err != nil || bt.ID != "bg-1" || bt.State != store.BgRunning || bt.LogPath != "/tmp/agw-bg/bg-1.log" {
		t.Fatalf("Start: %+v %v", bt, err)
	}
	if req := run.reqs[0]; req.Op != execproto.OpBg || req.Spill != "/tmp/agw-bg/bg-1.log" || req.Timeout != 5 {
		t.Fatalf("request: %+v", req)
	}
	time.Sleep(20 * time.Millisecond)
	s, full, err := r.Output(ctx, "c1", "bg-1")
	if err != nil || full != "finish-bg\n" || s.State != store.BgRunning || s.OutputBytes != 10 {
		t.Fatalf("Output: %+v %q %v", s, full, err)
	}
	if _, _, err := r.Output(ctx, "other", "bg-1"); !errors.Is(err, ErrUnknown) {
		t.Fatalf("other chat: %v", err)
	}
	code := 0
	run.end("echo finish-bg", execproto.Frame{Done: true, Exit: &code})
	e := waitEnded(t, n)
	if e.State != store.BgExited || *e.ExitCode != 0 || e.OutputSHA256 == "" || e.OutputLines != 1 || e.Tail != "finish-bg\n" || e.EndedAt == nil {
		t.Fatalf("end: %+v", e)
	}
	if !n.notify[0] {
		t.Fatal("end without notification")
	}
	if r.Running() != 0 {
		t.Fatal("still running")
	}
	// unknown and old tasks
	if _, _, err := r.Output(ctx, "c1", "bg-7"); !errors.Is(err, ErrUnknown) {
		t.Fatalf("unknown: %v", err)
	}
	if s, full, err := r.Output(ctx, "c1", "bg-99"); err != nil || s.State != store.BgSuspended || full != "old\n" {
		t.Fatalf("from the database: %+v %v", s, err)
	}
	if _, _, err := r.Output(ctx, "c1", "x"); err == nil {
		t.Fatal("invalid ID accepted")
	}
}

func TestLimitStopAndNotStarted(t *testing.T) {
	r, _, n := newReg(t, 2)
	ctx := context.Background()
	for _, c := range []string{"sleep a", "sleep b"} {
		if _, err := r.Start(ctx, StartParams{ChatID: "c1", Command: c, Cwd: "/workspace"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.Start(ctx, StartParams{ChatID: "c1", Command: "sleep c", Cwd: "/workspace"}); !errors.Is(err, ErrLimit) || !strings.Contains(err.Error(), "limit 2") {
		t.Fatalf("limit: %v", err)
	}
	s, err := r.Stop(ctx, "c1", "bg-1", "user")
	if err != nil || s.State != store.BgStopped || s.StoppedBy != "user" {
		t.Fatalf("Stop: %+v %v", s, err)
	}
	if e := waitEnded(t, n); e.State != store.BgStopped {
		t.Fatalf("end after Stop: %+v", e)
	}
	// Stop of an ended task changes nothing
	if s, _ := r.Stop(ctx, "c1", "bg-1", "agent"); s.StoppedBy != "user" {
		t.Fatalf("second Stop: %+v", s)
	}
	// command does not start (e.g. missing working directory): error, no notification
	_, err = r.Start(ctx, StartParams{ChatID: "c1", Command: "nostart", Cwd: "/nope"})
	if err == nil || !strings.Contains(err.Error(), "Working directory does not exist") {
		t.Fatalf("not started: %v", err)
	}
	e := waitEnded(t, n)
	if e.State != store.BgFailed || n.notify[len(n.notify)-1] {
		t.Fatalf("not started: %+v notify=%v", e, n.notify)
	}
	// teardown of the slot: running task ends as lost
	r.Close()
	if e := waitEnded(t, n); e.State != store.BgLost {
		t.Fatalf("after Close: %+v", e)
	}
	if _, err := r.Start(ctx, StartParams{ChatID: "c1", Command: "sleep d", Cwd: "/workspace"}); err == nil {
		t.Fatal("Start after Close")
	}
}

func TestProgressThrottled(t *testing.T) {
	old := ProgressEvery
	ProgressEvery = 150 * time.Millisecond
	defer func() { ProgressEvery = old }()
	n := &fakeNotifier{}
	tk := &task{t: store.BackgroundTask{ChatID: "c", Seq: 1}, done: make(chan struct{}), sum: newHash()}
	w := tk.write(n)
	for i := 0; i < 50; i++ {
		w([]byte("line\n"))
	}
	time.Sleep(400 * time.Millisecond)
	n.mu.Lock()
	got := len(n.progress)
	last := n.progress[len(n.progress)-1]
	n.mu.Unlock()
	if got < 2 || got > 3 {
		t.Fatalf("progress reported %d times", got)
	}
	if last.OutputLines != 50 {
		t.Fatalf("last state without all lines: %d", last.OutputLines)
	}
}

func TestFinishStates(t *testing.T) {
	code := 3
	cases := []struct {
		f       execproto.Frame
		err     error
		started bool
		stop    string
		want    string
	}{
		{execproto.Frame{Done: true, Exit: &code}, nil, true, "", store.BgExited},
		{execproto.Frame{Done: true, Code: "timeout", Error: "timeout"}, nil, true, "", store.BgTimeout},
		{execproto.Frame{Done: true, Code: "aborted", Error: "aborted"}, context.Canceled, true, "agent", store.BgStopped},
		{execproto.Frame{Done: true, Code: "aborted", Error: "aborted"}, context.Canceled, true, "", store.BgLost},
		{execproto.Frame{}, execbox.ErrLost, true, "", store.BgLost},
		{execproto.Frame{Done: true, Code: "EIO", Error: "execution helper exited without result"}, nil, true, "", store.BgFailed},
		{execproto.Frame{Done: true, Code: "ELIMIT", Error: "too many background tasks (limit 5)"}, nil, false, "", store.BgFailed},
	}
	for _, c := range cases {
		tk := &task{t: store.BackgroundTask{Seq: 1}, done: make(chan struct{}), sum: newHash(), stoppedBy: c.stop}
		if got := tk.finish(c.f, c.err, c.started); got.State != c.want {
			t.Errorf("%+v %v: %s instead of %s (%s)", c.f, c.err, got.State, c.want, got.Error)
		}
	}
}

func TestExcerptAndTail(t *testing.T) {
	tk := &task{t: store.BackgroundTask{Seq: 1}, done: make(chan struct{}), sum: newHash()}
	w := tk.write(&fakeNotifier{})
	w([]byte(strings.Repeat("a", 3000)))
	w([]byte(strings.Repeat("b", 3000) + "ä"))
	f := tk.finish(execproto.Frame{Done: true, Exit: new(int)}, nil, true)
	if !strings.HasPrefix(f.OutputExcerpt, strings.Repeat("a", 2048)) || !strings.Contains(f.OutputExcerpt, "bytes omitted") || !strings.HasSuffix(f.OutputExcerpt, "bä") {
		t.Fatalf("excerpt: %q…", f.OutputExcerpt[:80])
	}
	if len(f.Tail) > ShortTailBytes || !strings.HasSuffix(f.Tail, "ä") {
		t.Fatalf("tail: %d", len(f.Tail))
	}
	small := &task{t: store.BackgroundTask{Seq: 2}, done: make(chan struct{}), sum: newHash()}
	small.write(&fakeNotifier{})([]byte("short"))
	if f := small.finish(execproto.Frame{Done: true, Exit: new(int)}, nil, true); f.OutputExcerpt != "short" || f.OutputLines != 1 {
		t.Fatalf("short excerpt: %+v", f)
	}
}

// A converted foreground command counts towards the limit, knows the output so far, ends via
// Finish and can be stopped with bg_stop (cancel of the operation).
func TestAdopt(t *testing.T) {
	n := &fakeNotifier{endedCh: make(chan store.BackgroundTask, 4)}
	r := New("p-1", &fakeRunner{}, n, 1)
	cancelled := make(chan struct{})
	a, err := r.Adopt(context.Background(), StartParams{ChatID: "c", Command: "long"}, "/tmp/pi-bash-x.log", []byte("before\n"), func() { close(cancelled) })
	if err != nil {
		t.Fatal(err)
	}
	if r.Running() != 1 {
		t.Fatalf("running: %d", r.Running())
	}
	if _, err := r.Adopt(context.Background(), StartParams{ChatID: "c"}, "", nil, func() {}); !errors.Is(err, ErrLimit) {
		t.Fatalf("limit: %v", err)
	}
	a.Write([]byte("after\n"))
	_, out, _ := r.Output(context.Background(), "c", a.Task.ID)
	if out != "before\nafter\n" {
		t.Fatalf("output: %q", out)
	}
	go func() {
		<-cancelled
		a.Finish(execproto.Frame{Done: true, Error: "aborted", Code: "aborted"}, nil)
	}()
	st, err := r.Stop(context.Background(), "c", a.Task.ID, "agent")
	if err != nil || st.State != store.BgStopped || st.StoppedBy != "agent" {
		t.Fatalf("stop: %+v %v", st, err)
	}
	if e := <-n.endedCh; e.State != store.BgStopped {
		t.Fatalf("end: %+v", e)
	}
	a.Finish(execproto.Frame{}, nil) // second Finish has no effect
	r.Close()
}

func TestTail(t *testing.T) {
	tl := NewTail(8)
	if tl.Bytes() != nil {
		t.Fatal("expected empty")
	}
	tl.Write([]byte("abcdef"))
	tl.Write([]byte("ghijk"))
	if got := string(tl.Bytes()); got != "defghijk" {
		t.Fatalf("tail: %q", got)
	}
}

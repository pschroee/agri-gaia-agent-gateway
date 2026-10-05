// Package bgtask manages the background tasks of a slot: commands the agent starts with
// bash and run_in_background. The orchestrator runs them like any command in the
// execution sandbox (operation bg of agw-exec), reads their output to the end and
// reports start, progress (throttled) and end to the manager. The connection to the supervisor
// carries the end by itself (push): no polling, and the end comes from the process that started
// the command, not from a file the agent could modify.
package bgtask

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"agw/internal/execbox"
	"agw/internal/execproto"
	"agw/internal/store"
)

// Runner runs a background task in the execution sandbox (execbox.Client).
type Runner interface {
	RunBackground(ctx context.Context, req execproto.Request, onStart func(pgid int), onData func([]byte)) (execproto.Frame, error)
}

// Notifier is the manager: it creates the row (number within the chat), distributes progress and end
// and notifies the agent.
type Notifier interface {
	// BackgroundCreate creates the task and assigns number and output file.
	BackgroundCreate(ctx context.Context, t store.BackgroundTask) (store.BackgroundTask, error)
	// BackgroundProgress: running task with new output (at most every ProgressEvery).
	BackgroundProgress(t store.BackgroundTask)
	// BackgroundEnded: the task has ended. notify=false: it never started, the
	// agent learns that from the tool result.
	BackgroundEnded(t store.BackgroundTask, notify bool)
	// BackgroundLookup: a task this registry does not know (e.g. from before idling).
	BackgroundLookup(ctx context.Context, chatID string, seq int) (store.BackgroundTask, error)
}

// Limits and intervals.
var (
	// TailBytes: this much of the end of the output is kept by the registry (like pi's rolling tail, 2 × 50 KiB).
	TailBytes = 2 * execproto.PiMaxBytes
	// ShortTailBytes: this much of the end is stored in the database and in the events.
	ShortTailBytes = 4 << 10
	// ProgressEvery: at most this often a progress update per task.
	ProgressEvery = 2 * time.Second
	// StartWait: this long Start waits for the command to be running.
	StartWait = 15 * time.Second
	// StopWait: this long Stop waits for the end.
	StopWait = 15 * time.Second
	// KeepEnded: this many ended tasks are kept by the registry (full tail for bg_output); older ones
	// are released, their state is in background_tasks (Review 3, M2).
	KeepEnded = 4
)

// Errors that reach the model as a tool result are in English (L4).
var (
	ErrUnknown = errors.New("unknown background task")
	ErrLimit   = errors.New("too many background tasks")
)

// StartParams describes a start.
type StartParams struct {
	ChatID     string
	Session    string
	ToolCallID string
	Command    string
	Cwd        string
	Env        map[string]string
	Timeout    float64
}

type task struct {
	mu        sync.Mutex
	t         store.BackgroundTask
	cancel    context.CancelFunc
	done      chan struct{}
	tail      ring // rolling tail (TailBytes), ring buffer instead of append and copy (Review 3, N3)
	lastByte  byte
	sum       hash.Hash
	head      []byte
	excerpt   ring // tail for the excerpt (2 KiB)
	stoppedBy string
	lastProg  time.Time
	progTimer *time.Timer
	dirty     bool
}

// Registry: background tasks of a slot. A slot belongs to exactly one chat (single assignment);
// the chat ID is checked on every access nonetheless.
type Registry struct {
	slot string
	run  Runner
	n    Notifier
	max  int

	ctx    context.Context
	cancel context.CancelFunc

	mu       sync.Mutex
	tasks    map[int]*task
	reserved int   // reserved places whose start is in progress (atomic limit, Review 3, N2)
	ended    []int // ended tasks in the registry, oldest first (at most KeepEnded)
	wg       sync.WaitGroup
}

func New(slot string, run Runner, n Notifier, max int) *Registry {
	if max <= 0 {
		max = execproto.DefaultBgMax
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Registry{slot: slot, run: run, n: n, max: max, ctx: ctx, cancel: cancel, tasks: map[int]*task{}}
}

// Max is the limit of concurrent tasks.
func (r *Registry) Max() int { return r.max }

// Running counts running tasks.
func (r *Registry) Running() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.runningLocked()
}

// runningLocked: running tasks; r.mu is locked.
func (r *Registry) runningLocked() int {
	n := 0
	for _, t := range r.tasks {
		select {
		case <-t.done:
		default:
			n++
		}
	}
	return n
}

// Start starts a task and returns as soon as the command is running (or does not start at all).
func (r *Registry) Start(ctx context.Context, p StartParams) (store.BackgroundTask, error) {
	if r.ctx.Err() != nil {
		return store.BackgroundTask{}, errors.New("execution sandbox closed")
	}
	// Reserve a place under the lock: checking and taking are one step, even if
	// creating the row in the database takes time in between.
	r.mu.Lock()
	if r.runningLocked()+r.reserved >= r.max {
		r.mu.Unlock()
		return store.BackgroundTask{}, fmt.Errorf("%w (limit %d); stop one with bg_stop or wait for one to finish", ErrLimit, r.max)
	}
	r.reserved++
	r.mu.Unlock()
	row, err := r.n.BackgroundCreate(ctx, store.BackgroundTask{ChatID: p.ChatID, SlotID: r.slot, Session: p.Session, ToolCallID: p.ToolCallID, Command: p.Command, Cwd: p.Cwd})
	if err != nil {
		r.mu.Lock()
		r.reserved--
		r.mu.Unlock()
		return store.BackgroundTask{}, err
	}
	tctx, cancel := context.WithCancel(r.ctx)
	t := &task{t: row, cancel: cancel, done: make(chan struct{}), sum: newHash()}
	req := execproto.Request{Op: execproto.OpBg, Command: p.Command, Cwd: p.Cwd, Env: p.Env, Timeout: p.Timeout, Spill: row.LogPath}
	r.mu.Lock()
	r.tasks[row.Seq] = t
	r.reserved--
	r.mu.Unlock()
	started := make(chan struct{})
	var startOnce sync.Once
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		f, err := r.run.RunBackground(tctx, req, func(int) { startOnce.Do(func() { close(started) }) }, t.write(r.n))
		wasStarted := false
		select {
		case <-started:
			wasStarted = true
		default:
		}
		final := t.finish(f, err, wasStarted)
		close(t.done)
		r.n.BackgroundEnded(final, wasStarted)
		r.release(row.Seq)
	}()
	timer := time.NewTimer(StartWait)
	defer timer.Stop()
	select {
	case <-started:
		return t.snapshot(), nil
	case <-t.done:
		s := t.snapshot()
		return s, errors.New(strings.TrimSpace("background task not started: " + s.Error))
	case <-timer.C:
		// If the helper does not start (e.g. with an exhausted process limit), do not wait forever.
		cancel()
		<-t.done
		s := t.snapshot()
		return s, errors.New("background task not started in time")
	}
}

// Adopted is a foreground command the user has turned into a background task: the
// command keeps running in its operation (bash), whose output and end the caller forwards here.
type Adopted struct {
	Task store.BackgroundTask
	// Write takes further output; Finish the end of the operation (exactly once).
	Write  func([]byte)
	Finish func(f execproto.Frame, err error)
}

// Adopt takes over a running command as a background task. cancel aborts its operation
// (bg_stop, stop in the UI, teardown of the slot); soFar is the output so far.
func (r *Registry) Adopt(ctx context.Context, p StartParams, logPath string, soFar []byte, cancel context.CancelFunc) (*Adopted, error) {
	if r.ctx.Err() != nil {
		return nil, errors.New("execution sandbox closed")
	}
	r.mu.Lock()
	if r.runningLocked()+r.reserved >= r.max {
		r.mu.Unlock()
		return nil, fmt.Errorf("%w (limit %d)", ErrLimit, r.max)
	}
	r.reserved++
	r.mu.Unlock()
	row, err := r.n.BackgroundCreate(ctx, store.BackgroundTask{ChatID: p.ChatID, SlotID: r.slot, Session: p.Session, ToolCallID: p.ToolCallID, Command: p.Command, Cwd: p.Cwd, LogPath: logPath})
	if err != nil {
		r.mu.Lock()
		r.reserved--
		r.mu.Unlock()
		return nil, err
	}
	t := &task{t: row, cancel: cancel, done: make(chan struct{}), sum: newHash()}
	r.mu.Lock()
	r.tasks[row.Seq] = t
	r.reserved--
	r.mu.Unlock()
	r.wg.Add(1)
	stopWithSlot := context.AfterFunc(r.ctx, cancel) // teardown of the slot also ends this task
	write := t.write(r.n)
	write(soFar)
	var once sync.Once
	return &Adopted{Task: t.snapshot(), Write: write, Finish: func(f execproto.Frame, err error) {
		once.Do(func() {
			defer r.wg.Done()
			stopWithSlot()
			final := t.finish(f, err, true)
			close(t.done)
			r.n.BackgroundEnded(final, true)
			r.release(row.Seq)
		})
	}}, nil
}

// release frees an ended task as soon as more than KeepEnded ended tasks are in the registry
// (its state is then in background_tasks; Output and Stop ask the manager).
func (r *Registry) release(seq int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ended = append(r.ended, seq)
	for len(r.ended) > KeepEnded {
		delete(r.tasks, r.ended[0])
		r.ended = r.ended[1:]
	}
}

// write collects the output: checksum, head, rolling tail, throttled progress. In
// steady state a chunk allocates nothing new (ring buffer); throughput is limited by agw-exec, which
// reads only throttled after execproto.BgThrottleAfter (Review 3, N3).
func (t *task) write(n Notifier) func([]byte) {
	return func(b []byte) {
		if len(b) == 0 {
			return
		}
		t.mu.Lock()
		t.sum.Write(b)
		t.t.OutputBytes += int64(len(b))
		t.t.OutputLines += int64(bytes.Count(b, nl))
		t.lastByte = b[len(b)-1]
		if half := 2048; len(t.head) < half {
			k := min(half-len(t.head), len(b))
			t.head = append(t.head, b[:k]...)
		}
		t.excerpt.init(2048)
		t.excerpt.write(b)
		t.tail.init(TailBytes)
		t.tail.write(b)
		t.dirty = true
		now := time.Now()
		due := now.Sub(t.lastProg) >= ProgressEvery
		if due {
			t.lastProg, t.dirty = now, false
		} else if t.progTimer == nil {
			// Straggler: the last output of a quiet phase arrives after ProgressEvery at the latest.
			t.progTimer = time.AfterFunc(ProgressEvery-now.Sub(t.lastProg), func() {
				t.mu.Lock()
				t.progTimer = nil
				send := t.dirty
				t.dirty = false
				t.lastProg = time.Now()
				t.mu.Unlock()
				select {
				case <-t.done:
					return
				default:
				}
				if send {
					n.BackgroundProgress(t.snapshot())
				}
			})
		}
		t.mu.Unlock()
		if due {
			n.BackgroundProgress(t.snapshot())
		}
	}
}

var nl = []byte{'\n'}

// Tail keeps the last bytes of an output (ring buffer, no new allocation in steady state);
// used for the output so far of a foreground command that can be converted.
type Tail struct {
	r    ring
	size int
}

func NewTail(size int) *Tail { return &Tail{size: size} }

func (t *Tail) Write(b []byte) {
	t.r.init(t.size)
	t.r.write(b)
}

// Bytes returns a copy of the kept tail.
func (t *Tail) Bytes() []byte {
	if t.r.buf == nil {
		return nil
	}
	return t.r.bytes()
}

// ring keeps the last n bytes; write copies only the new chunk.
type ring struct {
	buf   []byte
	start int // start of the data
	n     int // length of the data
}

// init allocates the buffer on first use.
func (r *ring) init(size int) {
	if r.buf == nil {
		r.buf = make([]byte, size)
	}
}

func (r *ring) write(b []byte) {
	c := len(r.buf)
	if len(b) >= c {
		copy(r.buf, b[len(b)-c:])
		r.start, r.n = 0, c
		return
	}
	end := (r.start + r.n) % c
	k := copy(r.buf[end:], b)
	copy(r.buf, b[k:])
	r.n += len(b)
	if r.n > c {
		r.start = (r.start + r.n - c) % c
		r.n = c
	}
}

// last returns the last n bytes in order (copy).
func (r *ring) last(n int) []byte {
	n = min(n, r.n)
	out := make([]byte, n)
	if n == 0 {
		return out
	}
	c := len(r.buf)
	from := (r.start + r.n - n) % c
	k := copy(out, r.buf[from:min(from+n, c)])
	copy(out[k:], r.buf[:n-k])
	return out
}

func (r *ring) bytes() []byte { return r.last(r.n) }

// finish determines the final state from the last frame.
func (t *task) finish(f execproto.Frame, err error, started bool) store.BackgroundTask {
	t.mu.Lock()
	if t.progTimer != nil {
		t.progTimer.Stop()
		t.progTimer = nil
	}
	now := time.Now()
	t.t.EndedAt = &now
	t.t.OutputSHA256 = hex.EncodeToString(t.sum.Sum(nil))
	t.t.OutputExcerpt = t.excerptText()
	if t.t.OutputBytes > 0 && t.lastByte != '\n' {
		t.t.OutputLines++
	}
	switch {
	case f.Code == "aborted" && t.stoppedBy != "":
		t.t.State, t.t.StoppedBy = store.BgStopped, t.stoppedBy
	case f.Code == "timeout":
		t.t.State, t.t.Error = store.BgTimeout, "timeout"
	case f.Error == "" && err == nil && f.Exit != nil:
		t.t.State, t.t.ExitCode = store.BgExited, f.Exit
	case errors.Is(err, execbox.ErrLost) || errors.Is(err, execbox.ErrClosed) || errors.Is(err, context.Canceled) || f.Code == "aborted":
		t.t.State = store.BgLost
		t.t.Error = "execution sandbox gone"
		if err != nil && !errors.Is(err, context.Canceled) {
			t.t.Error = err.Error()
		}
	default:
		t.t.State = store.BgFailed
		t.t.Error = strings.TrimSpace(strings.TrimPrefix(f.Code+": "+f.Error, ": "))
		if t.t.Error == "" && err != nil {
			t.t.Error = err.Error()
		}
		if f.Code == "ELIMIT" || f.Code == "EINVAL" || f.Code == "ENOENT" {
			t.t.Error = f.Error
		}
	}
	if !started && t.t.State != store.BgFailed {
		t.t.State = store.BgFailed
		if t.t.Error == "" {
			t.t.Error = "not started"
		}
	}
	t.mu.Unlock()
	return t.snapshot()
}

func (t *task) excerptText() string {
	s := string(t.head)
	ex := t.excerpt.bytes()
	omitted := t.t.OutputBytes - int64(len(t.head)) - int64(len(ex))
	if omitted > 0 {
		s += fmt.Sprintf("\n… [%d bytes omitted] …\n", omitted)
		s += string(ex)
	} else if t.t.OutputBytes > int64(len(t.head)) {
		// Head and tail overlap: append only the part after the head.
		rest := t.t.OutputBytes - int64(len(t.head))
		s += string(ex[int64(len(ex))-rest:])
	}
	return strings.ToValidUTF8(s, "�")
}

// snapshot: state of the task; Tail is the short tail (ShortTailBytes).
func (t *task) snapshot() store.BackgroundTask {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.t
	s.Tail = cleanTail(t.tail.last(ShortTailBytes+4), ShortTailBytes)
	return s
}

// cleanTail: at most n bytes from the end, starting at a character boundary, valid UTF-8.
func cleanTail(b []byte, n int) string {
	if len(b) > n {
		b = b[len(b)-n:]
		for i := 0; i < 4 && len(b) > 0 && !utf8.RuneStart(b[0]); i++ {
			b = b[1:]
		}
	}
	return strings.ToValidUTF8(string(b), "�")
}

func (r *Registry) get(chatID, id string) (*task, int, error) {
	seq := store.ParseBgID(id)
	if seq == 0 {
		return nil, 0, fmt.Errorf("%w %q (expected an id like bg-1)", ErrUnknown, id)
	}
	r.mu.Lock()
	t := r.tasks[seq]
	r.mu.Unlock()
	if t != nil && t.t.ChatID != chatID {
		return nil, seq, fmt.Errorf("%w %s", ErrUnknown, id)
	}
	return t, seq, nil
}

// Output returns the state and the tail of the output (up to TailBytes) of a task. If the registry
// does not know it (e.g. from an earlier sandbox of the chat), it asks the manager.
func (r *Registry) Output(ctx context.Context, chatID, id string) (store.BackgroundTask, string, error) {
	t, seq, err := r.get(chatID, id)
	if err != nil {
		return store.BackgroundTask{}, "", err
	}
	if t == nil {
		bt, err := r.n.BackgroundLookup(ctx, chatID, seq)
		if err != nil {
			return store.BackgroundTask{}, "", fmt.Errorf("%w %s", ErrUnknown, id)
		}
		return bt, bt.Tail, nil
	}
	s := t.snapshot()
	t.mu.Lock()
	full := cleanTail(t.tail.bytes(), TailBytes)
	t.mu.Unlock()
	return s, full, nil
}

// Stop stops a task (by: agent or user) and waits briefly for its end. If it has already
// ended, Stop only returns its state.
func (r *Registry) Stop(ctx context.Context, chatID, id, by string) (store.BackgroundTask, error) {
	t, seq, err := r.get(chatID, id)
	if err != nil {
		return store.BackgroundTask{}, err
	}
	if t == nil {
		bt, err := r.n.BackgroundLookup(ctx, chatID, seq)
		if err != nil {
			return store.BackgroundTask{}, fmt.Errorf("%w %s", ErrUnknown, id)
		}
		return bt, nil
	}
	t.mu.Lock()
	running := t.t.EndedAt == nil
	if running && t.stoppedBy == "" {
		t.stoppedBy = by
	}
	t.mu.Unlock()
	if running {
		t.cancel()
	}
	timer := time.NewTimer(StopWait)
	defer timer.Stop()
	select {
	case <-t.done:
	case <-timer.C:
	case <-ctx.Done():
	}
	return t.snapshot(), nil
}

// List returns the state of all tasks of the chat in this registry.
func (r *Registry) List(chatID string) []store.BackgroundTask {
	r.mu.Lock()
	ts := make([]*task, 0, len(r.tasks))
	for _, t := range r.tasks {
		ts = append(ts, t)
	}
	r.mu.Unlock()
	out := []store.BackgroundTask{}
	for _, t := range ts {
		s := t.snapshot()
		if s.ChatID == chatID {
			out = append(out, s)
		}
	}
	return out
}

// Close aborts all tasks (the slot is being torn down) and waits briefly for their end.
func (r *Registry) Close() {
	r.cancel()
	done := make(chan struct{})
	go func() { r.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
	}
}

func newHash() hash.Hash { return sha256.New() }

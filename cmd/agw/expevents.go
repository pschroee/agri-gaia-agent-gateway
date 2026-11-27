// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package main

// Events of agw exp run (issue #280): one JSON object per line (--events) and an optional hook program that gets the
// same object on stdin (--hook). The runner never waits for a hook: a slow or broken hook must not delay or stop a run.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// expEventVersion is the version of the event format (field v).
const expEventVersion = 1

// Event names (field event).
const (
	evPlanStart  = "plan_start"
	evResumed    = "resumed"
	evCellStart  = "cell_start"
	evRunPending = "run_pending"
	evRunStart   = "run_start"
	evRunEnd     = "run_end"
	evProgress   = "progress"
	evCellEnd    = "cell_end"
	evPaused     = "paused"
	evAbort      = "abort"
	evError      = "error"
	evPlanEnd    = "plan_end"
)

// Kinds of an error event.
const (
	kindPlanLimit   = "plan_limit"
	kindEnvironment = "environment"
	kindFixtures    = "fixtures"
	kindBuild       = "build"
	kindAPI         = "api"
	kindOther       = "other"
)

// expEventTokens are the tokens of one run's chat.
type expEventTokens struct {
	Input     int64 `json:"input"`
	Output    int64 `json:"output"`
	CacheRead int64 `json:"cache_read"`
}

// expEvent is one line of the event stream. Everything except v, ts, seq, session, event and plan is optional. The events
// carry no prompt, no answer of the agent and no secret.
type expEvent struct {
	V       int    `json:"v"`
	TS      string `json:"ts"`
	Seq     int    `json:"seq"`
	Session string `json:"session"`
	Event   string `json:"event"`
	Plan    string `json:"plan"`
	// Material is the short SHA-256 of the material the plan runs with.
	Material string `json:"material,omitempty"`

	// plan_start
	Cells       int    `json:"cells,omitempty"`
	K           int    `json:"k,omitempty"`
	Seed        *int64 `json:"seed,omitempty"`
	Runs        *int   `json:"runs,omitempty"`
	Lock        *bool  `json:"lock,omitempty"`
	EndedBefore *int   `json:"ended_before,omitempty"`
	// resumed: the attempts (ids) of earlier starts that stayed "started" and are replaced.
	Superseded []string `json:"superseded,omitempty"`

	// run events
	Cell    string `json:"cell,omitempty"`
	Task    string `json:"task,omitempty"`
	Rep     int    `json:"rep,omitempty"`
	Key     string `json:"key,omitempty"`
	Attempt string `json:"attempt,omitempty"`
	Chat    string `json:"chat,omitempty"`

	// run_end
	Status    string          `json:"status,omitempty"`
	Pass      json.RawMessage `json:"pass,omitempty"` // true, false or null
	Reason    string          `json:"reason,omitempty"`
	DurationS *float64        `json:"duration_s,omitempty"`
	Tokens    *expEventTokens `json:"tokens,omitempty"`

	// progress, cell_end, plan_end and the events that stop the plan
	Progress *expProgressView `json:"progress,omitempty"`
	Counts   *expCounts       `json:"counts,omitempty"`
	Complete *bool            `json:"complete,omitempty"` // cell_end: every planned run of the cell has ended
	Skipped  *int             `json:"skipped,omitempty"`  // plan_end

	// error
	Kind    string `json:"kind,omitempty"`
	Message string `json:"message,omitempty"`
}

// passJSON is the value of the pass field: true, false, or null for a run that was not judged online.
func passJSON(p *bool) json.RawMessage {
	switch {
	case p == nil:
		return json.RawMessage("null")
	case *p:
		return json.RawMessage("true")
	}
	return json.RawMessage("false")
}

// expEmitter writes the events and hands them to the hook. A nil emitter does nothing.
type expEmitter struct {
	mu       sync.Mutex
	w        io.Writer // nil: no event file
	hook     *expHook
	now      func() time.Time
	errw     io.Writer
	seq      int
	session  string
	plan     string
	material string
	warned   bool
}

func newExpEmitter(w io.Writer, hook *expHook, now func() time.Time, errw io.Writer, plan, material string) *expEmitter {
	if w == nil && hook == nil {
		return nil
	}
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return &expEmitter{w: w, hook: hook, now: now, errw: errw, session: hex.EncodeToString(b), plan: plan, material: material}
}

// Emit completes the event (version, time, sequence, session, plan), appends it as one line and queues it for the hook.
func (e *expEmitter) Emit(ev expEvent) {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.seq++
	ev.V, ev.Seq, ev.Session, ev.Plan, ev.Material = expEventVersion, e.seq, e.session, e.plan, e.material
	ev.TS = e.now().UTC().Format("2006-01-02T15:04:05.000Z")
	line, err := json.Marshal(ev)
	if err != nil {
		fmt.Fprintf(e.errw, "events: %v\n", err)
		return
	}
	line = append(line, '\n')
	if e.w != nil {
		// One Write per line, so that lines of several writers to the same file do not interleave.
		if _, err := e.w.Write(line); err != nil && !e.warned {
			e.warned = true
			fmt.Fprintf(e.errw, "events: writing failed, the run goes on without them: %v\n", err)
		}
	}
	if e.hook != nil {
		e.hook.enqueue(ev.Event, line)
	}
}

// Close waits for the hook to deliver what is queued, at most twice its time limit.
func (e *expEmitter) Close() {
	if e == nil || e.hook == nil {
		return
	}
	e.hook.close()
}

// Hook defaults.
const (
	defaultHookTimeout = 10 * time.Second
	hookQueueSize      = 256
	hookStderrLimit    = 500
)

type hookItem struct {
	event string
	line  []byte
}

// expHook runs the hook program once per event, one after the other, in a worker of its own.
type expHook struct {
	argv    []string
	timeout time.Duration
	logf    func(format string, args ...any)

	mu      sync.Mutex
	queue   []hookItem
	closed  bool
	abandon bool
	wake    chan struct{}
	done    chan struct{}
}

func newExpHook(argv []string, timeout time.Duration, errw io.Writer) *expHook {
	if len(argv) == 0 {
		return nil
	}
	if timeout <= 0 {
		timeout = defaultHookTimeout
	}
	var logMu sync.Mutex
	h := &expHook{argv: argv, timeout: timeout, wake: make(chan struct{}, 1), done: make(chan struct{}),
		logf: func(format string, args ...any) {
			logMu.Lock()
			defer logMu.Unlock()
			fmt.Fprintf(errw, "hook: "+format+"\n", args...)
		}}
	go h.worker()
	return h
}

// enqueue appends an event. A full queue drops the oldest progress event, else the oldest event.
func (h *expHook) enqueue(event string, line []byte) {
	h.mu.Lock()
	if len(h.queue) >= hookQueueSize {
		drop := 0
		for i, it := range h.queue {
			if it.event == evProgress {
				drop = i
				break
			}
		}
		h.queue = append(h.queue[:drop], h.queue[drop+1:]...)
	}
	h.queue = append(h.queue, hookItem{event: event, line: line})
	h.mu.Unlock()
	select {
	case h.wake <- struct{}{}:
	default:
	}
}

func (h *expHook) close() {
	h.mu.Lock()
	h.closed = true
	h.mu.Unlock()
	select {
	case h.wake <- struct{}{}:
	default:
	}
	select {
	case <-h.done:
	case <-time.After(2 * h.timeout):
		h.mu.Lock()
		h.abandon = true
		n := len(h.queue)
		h.mu.Unlock()
		h.logf("%d events not delivered when the run ended", n)
	}
}

func (h *expHook) worker() {
	defer close(h.done)
	for {
		h.mu.Lock()
		if h.abandon {
			h.mu.Unlock()
			return
		}
		if len(h.queue) == 0 {
			closed := h.closed
			h.mu.Unlock()
			if closed {
				return
			}
			<-h.wake
			continue
		}
		it := h.queue[0]
		h.queue = h.queue[1:]
		h.mu.Unlock()
		h.run(it)
	}
}

// limitedBuffer keeps the first n bytes written to it.
type limitedBuffer struct {
	buf bytes.Buffer
	n   int
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if room := l.n - l.buf.Len(); room > 0 {
		l.buf.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

// run starts the hook program for one event: no shell, the event on stdin, the event name in AGW_EXP_EVENT. After the
// time limit the whole process group is killed. A failure is only logged.
func (h *expHook) run(it hookItem) {
	ctx, cancel := context.WithTimeout(context.Background(), h.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, h.argv[0], h.argv[1:]...)
	cmd.Stdin = bytes.NewReader(it.line)
	cmd.Env = append(os.Environ(), "AGW_EXP_EVENT="+it.event)
	cmd.SysProcAttr = hookSysProcAttr()
	cmd.Cancel = func() error { return killGroup(cmd.Process.Pid) }
	cmd.WaitDelay = 2 * time.Second
	stderr := &limitedBuffer{n: hookStderrLimit}
	cmd.Stderr = stderr
	err := cmd.Run()
	if err == nil {
		return
	}
	text := strings.Join(strings.Fields(stderr.buf.String()), " ")
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		h.logf("%s: timed out after %s, killed", it.event, h.timeout)
	case text != "":
		h.logf("%s: %v: %s", it.event, err, text)
	default:
		h.logf("%s: %v", it.event, err)
	}
}

// syncWriter serialises the writes of the runner's goroutine and the hook worker to the same stream.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

// timestampWriter puts the UTC time at the start of every line (--timestamps). A partial line waits for its end.
type timestampWriter struct {
	mu      sync.Mutex
	w       io.Writer
	now     func() time.Time
	pending bool // the last write ended inside a line
}

func (t *timestampWriter) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out bytes.Buffer
	rest := p
	for len(rest) > 0 {
		if !t.pending {
			out.WriteString(t.now().UTC().Format("2006-01-02T15:04:05Z") + " ")
			t.pending = true
		}
		i := bytes.IndexByte(rest, '\n')
		if i < 0 {
			out.Write(rest)
			break
		}
		out.Write(rest[:i+1])
		rest = rest[i+1:]
		t.pending = false
	}
	if _, err := t.w.Write(out.Bytes()); err != nil {
		return 0, err
	}
	return len(p), nil
}

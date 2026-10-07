package chat

import (
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"time"
)

// Steps when resuming an idle chat (SSE event "resume"). They are exactly the steps that attach
// actually performs, in this order; the UI shows them live.
const (
	PhaseAcquire   = "acquire"   // take a slot from the pool (waits up to AcquireTimeout)
	PhaseSession   = "session"   // set the model, restore the session file, switch_session
	PhaseSettings  = "settings"  // set internet and auto-compaction
	PhaseWorkspace = "workspace" // restore the workspace from the backup
	PhaseInputs    = "inputs"    // mirror the user's inputs to /workspace/inputs/
	PhaseReady     = "ready"     // done, the message now goes to pi
	PhaseFailed    = "failed"    // aborted, reason in detail
)

// ResumeStep is an event while resuming. Per step, status "running" comes first, then "done",
// "warning" (carrying on despite a problem) or "error" (resuming failed).
type ResumeStep struct {
	ID     string    `json:"id"`    // ID of this resume
	Phase  string    `json:"phase"` // see Phase*
	Status string    `json:"status"`
	Detail string    `json:"detail,omitempty"`
	Size   *int64    `json:"size,omitempty"`  // workspace: sum of the file sizes; inputs: sum; session: bytes
	Files  *int      `json:"files,omitempty"` // workspace, inputs: number of files
	At     time.Time `json:"at"`
	Ms     int64     `json:"ms,omitempty"` // duration of the step; for ready and failed the total duration
	// Start: the first sandbox of a new chat (Create with Async), not a resume of a resting chat.
	Start bool `json:"start,omitempty"`
}

var resumeSeq struct {
	sync.Mutex
	n int64
}

// resumeProgress reports the steps of a resume. All methods tolerate nil (creating a new chat
// reports nothing).
type resumeProgress struct {
	m      *Manager
	chatID string
	id     string
	start  time.Time
	phase  string
	since  time.Time
	first  bool // the first sandbox of a new chat (ResumeStep.Start)
}

func (m *Manager) newResume(chatID string) *resumeProgress {
	resumeSeq.Lock()
	resumeSeq.n++
	n := resumeSeq.n
	resumeSeq.Unlock()
	now := time.Now()
	return &resumeProgress{m: m, chatID: chatID, id: "r" + strconv.FormatInt(now.UnixMilli(), 36) + "-" + strconv.FormatInt(n, 36), start: now}
}

func (p *resumeProgress) emit(s ResumeStep) {
	s.ID = p.id
	s.At = time.Now()
	s.Start = p.first
	p.m.publish(p.chatID, Event{Kind: "resume", Data: s})
}

// run starts a step.
func (p *resumeProgress) run(phase string) {
	if p == nil {
		return
	}
	p.phase, p.since = phase, time.Now()
	p.emit(ResumeStep{Phase: phase, Status: "running"})
}

// done completes the running step; s adds detail, size and file count.
func (p *resumeProgress) done(s ResumeStep) {
	if p == nil || p.phase == "" {
		return
	}
	s.Phase, s.Ms = p.phase, time.Since(p.since).Milliseconds()
	if s.Status == "" {
		s.Status = "done"
	}
	p.emit(s)
	p.phase = ""
}

func (p *resumeProgress) ready() {
	if p == nil {
		return
	}
	p.emit(ResumeStep{Phase: PhaseReady, Status: "done", Ms: time.Since(p.start).Milliseconds()})
}

// fail marks the running step as failed and reports failed with the reason.
func (p *resumeProgress) fail(err error) {
	if p == nil {
		return
	}
	reason := resumeFailReason(err)
	if p.phase != "" {
		p.done(ResumeStep{Status: "error", Detail: reason})
	}
	p.emit(ResumeStep{Phase: PhaseFailed, Status: "error", Detail: reason, Ms: time.Since(p.start).Milliseconds()})
	slog.Warn("resume failed", "chat", p.chatID, "err", err)
}

func resumeFailReason(err error) string {
	if errors.Is(err, ErrNoSlot) {
		return "No free slot in the pool" // matched by the CLI and web UI tests; stays German for now
	}
	return err.Error()
}

func intp(n int) *int       { return &n }
func int64p(n int64) *int64 { return &n }

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

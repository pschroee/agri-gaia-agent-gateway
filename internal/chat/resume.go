package chat

import (
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"time"
)

// Schritte beim Fortsetzen eines ruhenden Chats (SSE-Ereignis „resume“). Es sind genau die
// Schritte, die attach tatsächlich ausführt, in dieser Reihenfolge; die UI zeigt sie live.
const (
	PhaseAcquire   = "acquire"   // Platz aus dem Pool holen (wartet bis AcquireTimeout)
	PhaseSession   = "session"   // Modell setzen, Sitzungsdatei einspielen, switch_session
	PhaseSettings  = "settings"  // Internet und Auto-Kompaktierung setzen
	PhaseWorkspace = "workspace" // Arbeitsbereich aus der Sicherung einspielen
	PhaseInputs    = "inputs"    // Eingaben des Nutzers nach /workspace/inputs/ spiegeln
	PhaseReady     = "ready"     // fertig, der Auftrag geht jetzt an pi
	PhaseFailed    = "failed"    // abgebrochen, Grund in detail
)

// ResumeStep ist ein Ereignis beim Fortsetzen. Je Schritt kommt zuerst status "running",
// dann "done", "warning" (weiter trotz Problem) oder "error" (Fortsetzen gescheitert).
type ResumeStep struct {
	ID     string    `json:"id"`    // Kennung dieses Fortsetzens
	Phase  string    `json:"phase"` // siehe Phase*
	Status string    `json:"status"`
	Detail string    `json:"detail,omitempty"`
	Size   *int64    `json:"size,omitempty"`  // workspace: Summe der Dateigrößen; inputs: Summe; session: Bytes
	Files  *int      `json:"files,omitempty"` // workspace, inputs: Zahl der Dateien
	At     time.Time `json:"at"`
	Ms     int64     `json:"ms,omitempty"` // Dauer des Schritts; bei ready und failed die Gesamtdauer
}

var resumeSeq struct {
	sync.Mutex
	n int64
}

// resumeProgress meldet die Schritte eines Fortsetzens. Alle Methoden vertragen nil
// (Anlegen eines neuen Chats meldet nichts).
type resumeProgress struct {
	m      *Manager
	chatID string
	id     string
	start  time.Time
	phase  string
	since  time.Time
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
	p.m.publish(p.chatID, Event{Kind: "resume", Data: s})
}

// run beginnt einen Schritt.
func (p *resumeProgress) run(phase string) {
	if p == nil {
		return
	}
	p.phase, p.since = phase, time.Now()
	p.emit(ResumeStep{Phase: phase, Status: "running"})
}

// done schließt den laufenden Schritt ab; s ergänzt Detail, Größe und Dateizahl.
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

// fail markiert den laufenden Schritt als gescheitert und meldet failed mit dem Grund.
func (p *resumeProgress) fail(err error) {
	if p == nil {
		return
	}
	reason := resumeFailReason(err)
	if p.phase != "" {
		p.done(ResumeStep{Status: "error", Detail: reason})
	}
	p.emit(ResumeStep{Phase: PhaseFailed, Status: "error", Detail: reason, Ms: time.Since(p.start).Milliseconds()})
	slog.Warn("Fortsetzen gescheitert", "chat", p.chatID, "fehler", err)
}

func resumeFailReason(err error) string {
	if errors.Is(err, ErrNoSlot) {
		return "Kein freier Platz im Pool"
	}
	return err.Error()
}

func intp(n int) *int       { return &n }
func int64p(n int64) *int64 { return &n }

func onOff(b bool) string {
	if b {
		return "an"
	}
	return "aus"
}

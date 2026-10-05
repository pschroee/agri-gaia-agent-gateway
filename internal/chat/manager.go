// Package chat verwaltet Chats: Zuweisung eines Platzes aus dem Warm-Pool,
// Weiterleitung der pi-Ereignisse, Sicherung der Sitzung, Ruhen und
// Fortsetzen in einer frischen Sandbox (Zustände aktiv / ruhend / beendet).
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
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"agw/internal/artifacts"
	"agw/internal/config"
	"agw/internal/delegation"
	"agw/internal/platform"
	"agw/internal/pool"
	"agw/internal/rpc"
	"agw/internal/sock"
	"agw/internal/store"
	"agw/internal/titler"
)

// Agent ist die Sicht des Managers auf eine laufende Sandbox mit pi.
type Agent interface {
	Call(ctx context.Context, cmd map[string]any) (rpc.Response, error)
	Events() <-chan rpc.Event
	ContainerID() string
	ContainerName() string
	Image() string
	// Exec führt ein Kommando als Agent-Nutzer in der Ausführungs-Sandbox aus
	// (Arbeitsbereich, Eingaben, Anzeige-Bilder; E9).
	Exec(ctx context.Context, cmd []string, stdin io.Reader) ([]byte, error)
	// ExecPi führt ein Kommando im Container von pi aus (ohne Shell, nur
	// agw-exec und coreutils): Sitzungen, Konfiguration, Subagenten.
	ExecPi(ctx context.Context, cmd []string, stdin io.Reader) ([]byte, error)
	SetInternet(ctx context.Context, on bool) error
	// IP ist die Adresse der Sandbox im Platz-Netz (Zuordnung am LLM-Proxy).
	IP() string
	// Notify schreibt einen Befehl an pi, ohne auf eine Antwort zu warten.
	Notify(cmd map[string]any) error
}

// Blobs ist die Objektablage (RustFS).
type Blobs interface {
	Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error
	Get(ctx context.Context, key string) (io.ReadCloser, int64, error)
	Move(ctx context.Context, src, dst string) error
	Delete(ctx context.Context, key string) error
}

var (
	ErrInvalid         = errors.New("ungültige Angabe")
	ErrTooManyPending  = errors.New("zu viele offene Bestätigungen in diesem Chat; erst die bestehenden abwarten")
	ErrPendingApproval = errors.New("Chat hat eine offene Bestätigung")
	ErrRunning         = errors.New("Agent arbeitet gerade")
	ErrUnknownModel    = errors.New("unbekanntes Modell")
	ErrUnknownVariant  = pool.ErrUnknownVariant
	ErrNoSlot          = pool.ErrNoIdleSlot
	ErrNotFound        = store.ErrNotFound
)

// Event wird an die SSE-Abonnenten eines Chats verteilt.
type Event struct {
	Kind string `json:"kind"`
	Data any    `json:"data"`
}

type ChatView struct {
	store.Chat
	Running bool   `json:"running"`
	SlotID  string `json:"slot_id,omitempty"`
	// Resuming: Der Chat wird gerade in einer frischen Sandbox fortgesetzt.
	Resuming bool `json:"resuming"`
	// QueueHeld: Es gibt eingereihte Nachrichten, die nicht von selbst übergeben werden
	// (nach einem Abbruch, bei ruhendem Chat); sie gehen mit der nächsten Nachricht mit.
	QueueHeld bool `json:"queue_held"`
	// RunningSince: Beginn des laufenden Durchgangs (nur, solange der Agent arbeitet).
	RunningSince *time.Time `json:"running_since,omitempty"`
	// HoldReason: warum Eingereihtes zurückgehalten ist (HoldAbort, HoldWakeLimit, HoldAutoTurns);
	// leer bei ruhendem Chat oder ohne Zurückhalten.
	HoldReason string `json:"hold_reason,omitempty"`
	// ThinkingLevels: Denkstufen, die das Modell des Chats kennt (leer: noch nicht bekannt).
	ThinkingLevels []string `json:"thinking_levels,omitempty"`
	// PendingModel: Modell, zu dem nach der laufenden Kompaktierung gewechselt wird.
	PendingModel string `json:"pending_model,omitempty"`
}

type live struct {
	slot    *pool.Slot[Agent]
	runID   int64
	running bool
	idle    *time.Timer
	stop    chan struct{}
	stopped sync.Once

	ip              string // Adresse im Platz-Netz
	maxSub          int    // höchstens so viele Subagenten
	limitEnforcedAt int    // bei dieser Zahl zuletzt eingegriffen

	runningSince time.Time
	toolsRunning int        // laufende Werkzeuge (tool_execution_start bis _end); Zeitpunkt zum Einschleusen
	compacting   bool       // manuelle Kompaktierung läuft; Ruhen gesperrt
	bg           sync.Mutex // serialisiert Hintergrundarbeit (Sichern, Kontext lesen)

	wsNoSave    bool   // Einspielen gescheitert: nicht sichern (letzte Sicherung schützen)
	wsSkippedFP string // zuletzt wegen der Grenze ausgelassener Stand (Hinweis nur einmal)

	holdQueue  bool   // nach Abbruch oder über einer Grenze: Warteschlange nicht von selbst übergeben
	holdReason string // HoldAbort, HoldWakeLimit, HoldAutoTurns

	activeAt time.Time // letzte Aktivität (Leerlauf)

	// pendingTurns: an pi geschickte Aufträge, deren Nutzernachricht pi noch nicht gemeldet hat;
	// turn: der Durchgang, zu dem die folgenden Antworten gehören (Review 3, H1).
	pendingTurns []*turnMeta
	turn         *turnMeta
	agentStartAt time.Time // Empfang des letzten agent_start
}

// Fristen für Aufrufe an pi. Ohne sie hängt ein Aufruf, wenn pi nicht mehr
// antwortet, und mit ihm die Chat-Sperre (Review H4). Die Frist für prompt steht in promptTimeout.
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

// execPiT: wie execT, aber im Container von pi.
func execPiT(a Agent, cmd []string, stdin io.Reader, d time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	return a.ExecPi(ctx, cmd, stdin)
}

// background führt Arbeit außerhalb der Pump-Goroutine aus, je Chat der
// Reihe nach. So liest die Pumpe weiter Ereignisse, während auf pi gewartet
// wird (Review H1).
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

	AutoCompactDefault   bool
	CompactReserveTokens int
	CompactKeepRecent    int

	MaxSubagentsDefault int
	MaxSubagentsLimit   int

	// ImageMaxBytes begrenzt Anzeige-Bilder (Standard DefaultImageMaxBytes).
	ImageMaxBytes int64

	// WorkspaceMaxBytes begrenzt die Sicherung von /workspace (Summe der
	// Dateigrößen; 0 = DefaultWorkspaceMaxBytes, < 0 = keine Sicherung).
	WorkspaceMaxBytes int64

	// BgWakesPerHour: So oft darf das Ende einer Hintergrundaufgabe je Chat und Stunde einen neuen
	// Durchgang starten (0 = DefaultBgWakesPerHour, < 0 = nie). BgKeepAlive: So lange nach der
	// letzten Aktivität verschieben laufende Hintergrundaufgaben das Ruhen im Leerlauf
	// (0 = DefaultBgKeepAlive, < 0 = gar nicht).
	BgWakesPerHour int
	BgKeepAlive    time.Duration
	// Titler formuliert nach der ersten Frage einen Titel (nil: der Titel bleibt die gekürzte Frage).
	Titler Titler

	// AutoTurnsMax: höchstens so viele Durchgänge ohne Nutzer (Weckrufe) hintereinander je Chat
	// (0 = DefaultAutoTurnsMax, < 0 = keiner). Darüber hält der Manager an (Review 3, H2).
	AutoTurnsMax int

	// Platform spricht mit der Agri-Gaia-Plattform (nil: Anbindung aus).
	Platform *platform.Client
}

type Manager struct {
	st     *store.Store
	pool   *pool.Pool[Agent]
	cat    *config.Catalog
	blobs  Blobs
	broker *artifacts.Broker
	opt    Options

	mu   sync.Mutex
	live map[string]*live
	subs map[string]map[chan Event]struct{}
	// chatMu serialisiert Zustandswechsel je Chat (Fortsetzen, Ruhen, Beenden).
	chatMu map[string]*sync.Mutex
	// imgMu serialisiert das Sichern der Anzeige-Bilder je Chat.
	imgMu map[string]*sync.Mutex
	// wsMu serialisiert Sichern und Einspielen des Arbeitsbereichs je Chat.
	wsMu map[string]*sync.Mutex
	// qMu serialisiert die Entscheidung „sofort senden oder einreihen“ je Chat.
	qMu map[string]*sync.Mutex
	// sending: Ein Auftrag ist unterwegs (Fortsetzen oder prompt), neue Nachrichten werden eingereiht.
	sending map[string]bool
	// resuming: Der Chat wird gerade fortgesetzt (für die Anzeige).
	resuming map[string]bool
	// pendingModel: Modellwechsel, der auf das Ende einer Kompaktierung wartet (SetModel mit compactFirst).
	pendingModel map[string]string
	// levels: Denkstufen je Modell, wie pi sie zuletzt gemeldet hat (get_available_thinking_levels).
	levels map[string][]string
	// userAt: letzte Aktion des Nutzers je Chat (Senden, Abbrechen, Entfernen, Stoppen, Fortsetzen);
	// maßgeblich für den Aufschub durch Hintergrundaufgaben (Review 3, M1).
	userAt map[string]time.Time
	// aborts zählt die Abbrüche je Chat; ein Auftrag, währenddessen abgebrochen wurde, geht nicht an pi.
	aborts map[string]uint64
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
	return &Manager{st: st, pool: p, cat: cat, blobs: blobs, broker: broker, opt: opt,
		live: map[string]*live{}, subs: map[string]map[chan Event]struct{}{}, chatMu: map[string]*sync.Mutex{},
		imgMu: map[string]*sync.Mutex{}, wsMu: map[string]*sync.Mutex{}, qMu: map[string]*sync.Mutex{},
		sending: map[string]bool{}, resuming: map[string]bool{}, pendingModel: map[string]string{}, levels: map[string][]string{}, userAt: map[string]time.Time{}, aborts: map[string]uint64{}}
}

// userActive hält eine Aktion des Nutzers fest (Aufschub durch Hintergrundaufgaben, M1).
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

// --- Abonnements ---

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

func (m *Manager) publish(chatID string, ev Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	important := ev.Kind == "approval" || ev.Kind == "chat" || ev.Kind == "error"
	for ch := range m.subs[chatID] {
		select {
		case ch <- ev:
		default:
			if !important {
				continue // langsamer Leser: Strom-Ereignis verwerfen statt den Chat aufzuhalten
			}
			// Bestätigungen und Zustandswechsel dürfen nicht verloren gehen: ältestes
			// Ereignis verwerfen, um Platz zu schaffen.
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- ev:
			default:
			}
		}
	}
}

func (m *Manager) publishChat(ctx context.Context, chatID string) {
	if v, err := m.View(ctx, chatID); err == nil {
		m.publish(chatID, Event{Kind: "chat", Data: v})
	}
}

// --- Abfragen ---

func (m *Manager) View(ctx context.Context, chatID string) (ChatView, error) {
	c, err := m.st.GetChat(ctx, chatID)
	if err != nil {
		return ChatView{}, err
	}
	return m.view(c), nil
}

func (m *Manager) view(c store.Chat) ChatView {
	v := ChatView{Chat: c}
	m.mu.Lock()
	l, ok := m.live[c.ID]
	if ok {
		v.Running = l.running
		v.SlotID = l.slot.ID
		if l.running && !l.runningSince.IsZero() {
			t := l.runningSince
			v.RunningSince = &t
		}
	}
	v.Resuming = m.resuming[c.ID]
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

// SlotChat liefert die Kennung des Chats, dem ein Platz zugewiesen ist.
func (m *Manager) SlotChat(slotID string) string {
	s, ok := m.pool.Get(slotID)
	if !ok {
		return ""
	}
	return s.ChatID()
}

// slotOf liefert den Platz eines laufenden Chats (leer, wenn er ruht).
func (m *Manager) slotOf(chatID string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if l := m.live[chatID]; l != nil && l.slot != nil {
		return l.slot.ID
	}
	return ""
}

// --- Anlegen, Senden, Fortsetzen ---

type NewChat struct {
	Model        string `json:"model"`
	Variant      string `json:"variant"`
	Title        string `json:"title"`
	Message      string `json:"message"`
	Internet     *bool  `json:"internet"`
	AutoCompact  *bool  `json:"auto_compact"`
	MaxSubagents *int   `json:"max_subagents"`
	// Delegation: übertragene Rechte (delegation.Delegation als JSON); fehlt sie, verhält sich der
	// Chat wie vor Schritt 1 (lesen frei, schreiben mit Bestätigung).
	Delegation json.RawMessage `json:"delegation,omitempty"`
}

func (m *Manager) Create(ctx context.Context, req NewChat) (ChatView, error) {
	if req.Model == "" {
		req.Model = m.cat.Default
	}
	if _, _, ok := m.cat.Lookup(req.Model); !ok {
		return ChatView{}, ErrUnknownModel
	}
	if req.Variant == "" {
		req.Variant = "cli"
	}
	if _, ok := m.pool.Targets()[req.Variant]; !ok {
		return ChatView{}, ErrUnknownVariant
	}
	internet := m.opt.InternetDefault
	if req.Internet != nil {
		internet = *req.Internet
	}
	// Ohne Titel gilt er als Platzhalter; Send ersetzt ihn mit der ersten Frage (sofort gekürzt,
	// dann vom Modell), auch wenn die Frage gleich beim Anlegen kommt.
	title, titleSrc := strings.TrimSpace(req.Title), store.TitleUser
	if title == "" {
		title, titleSrc = titleFrom(req.Message), store.TitleDefault
	}
	autoCompact := m.opt.AutoCompactDefault
	if req.AutoCompact != nil {
		autoCompact = *req.AutoCompact
	}
	maxSub := m.opt.MaxSubagentsDefault
	if req.MaxSubagents != nil {
		maxSub = *req.MaxSubagents
	}
	if maxSub < 0 || maxSub > m.opt.MaxSubagentsLimit {
		return ChatView{}, fmt.Errorf("%w: max_subagents muss zwischen 0 und %d liegen", ErrInvalid, m.opt.MaxSubagentsLimit)
	}
	var del json.RawMessage
	if t := bytes.TrimSpace(req.Delegation); len(t) > 0 && string(t) != "null" {
		d, err := delegation.Parse(t)
		if err != nil {
			return ChatView{}, fmt.Errorf("%w: %v", ErrInvalid, err)
		}
		del, _ = json.Marshal(d) // gespeichert wird die geprüfte, einheitliche Form
	}
	c, err := m.st.CreateChat(ctx, store.NewChat{Title: title, TitleSource: titleSrc, Model: req.Model, Variant: req.Variant, Internet: internet, AutoCompact: autoCompact, MaxSubagents: maxSub, Delegation: del})
	if err != nil {
		return ChatView{}, err
	}
	unlock := m.lock(c.ID)
	err = m.attach(ctx, c, nil, nil)
	unlock()
	if err != nil {
		// Ohne Sandbox ruht der Chat; die nächste Nachricht versucht es erneut.
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

func titleFrom(msg string) string {
	msg = strings.Join(strings.Fields(msg), " ")
	if msg == "" {
		return "Neuer Chat " + time.Now().Format("02.01. 15:04")
	}
	r := []rune(msg)
	if len(r) > 60 {
		return string(r[:60]) + " …"
	}
	return msg
}

// attach holt einen Platz, setzt Modell, Internet, Eingaben und bei
// session != nil die gesicherte Sitzung. Aufrufer hält die Chat-Sperre.
// p meldet die Schritte beim Fortsetzen (nil beim Anlegen).
func (m *Manager) attach(ctx context.Context, c store.Chat, session []byte, p *resumeProgress) error {
	p.run(PhaseAcquire)
	slot, err := m.pool.AcquireWait(ctx, c.Variant, c.ID, m.opt.AcquireTimeout)
	if err != nil {
		return err
	}
	p.done(ResumeStep{Detail: slot.ID})
	a := slot.Worker
	fail := func(err error) error {
		m.pool.Release(ctx, slot)
		return err
	}
	p.run(PhaseSession)
	prov, model, _ := m.cat.Lookup(c.Model)
	if _, err := a.Call(ctx, map[string]any{"type": "set_model", "provider": prov.ID, "modelId": model.ID}); err != nil {
		return fail(fmt.Errorf("Modell setzen: %w", err))
	}
	m.syncThinking(ctx, c.ID, c.Model, a, c.ThinkingLevel)
	if session != nil {
		sp := "/agent/sessions/" + c.ID + ".jsonl"
		if _, err := a.ExecPi(ctx, []string{"agw-exec", "put", sp}, bytes.NewReader(session)); err != nil {
			return fail(fmt.Errorf("Sitzung einspielen: %w", err))
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
			return fail(errors.New("switch_session wurde abgebrochen"))
		}
		p.done(ResumeStep{Size: int64p(int64(len(session)))})
	} else {
		p.done(ResumeStep{Detail: "keine Sitzung gesichert"})
	}
	p.run(PhaseSettings)
	if err := a.SetInternet(ctx, c.Internet); err != nil {
		return fail(fmt.Errorf("Internet setzen: %w", err))
	}
	if _, err := a.Call(ctx, map[string]any{"type": "set_auto_compaction", "enabled": c.AutoCompact}); err != nil {
		return fail(fmt.Errorf("Auto-Kompaktierung setzen: %w", err))
	}
	p.done(ResumeStep{Detail: "Internet " + onOff(c.Internet)})
	l := &live{slot: slot, stop: make(chan struct{}), ip: a.IP(), maxSub: c.MaxSubagents}
	m.userActive(c.ID) // Anlegen und Fortsetzen gehen vom Nutzer aus
	// Arbeitsbereich vor den Eingaben und vor dem ersten Auftrag einspielen
	// (das Archiv enthält kein inputs/, beides kommt sich nicht in die Quere).
	p.run(PhaseWorkspace)
	ws := m.restoreWorkspace(ctx, c.ID, l)
	switch {
	case ws.Disabled:
		p.done(ResumeStep{Detail: "Sicherung abgeschaltet"})
	case ws.Err != nil:
		p.done(ResumeStep{Status: "warning", Detail: "nicht wiederhergestellt: " + ws.Err.Error(), Size: int64p(ws.Size), Files: intp(ws.Files)})
	case !ws.Found:
		p.done(ResumeStep{Detail: "keine Sicherung"})
	default:
		p.done(ResumeStep{Size: int64p(ws.Size), Files: intp(ws.Files)})
	}
	p.run(PhaseInputs)
	if n, size, err := m.syncInputs(ctx, c.ID, a); err != nil {
		slog.Warn("Eingaben nicht gespiegelt", "chat", c.ID, "fehler", err)
		p.done(ResumeStep{Status: "warning", Detail: "nicht alle gespiegelt: " + err.Error(), Size: int64p(size), Files: intp(n)})
	} else {
		p.done(ResumeStep{Size: int64p(size), Files: intp(n)})
	}
	l.runID, _ = m.st.StartRun(ctx, c.ID, slot.ID, a.ContainerID())
	if n, err := m.st.SubagentRunCount(ctx, c.ID); err == nil {
		l.limitEnforcedAt = n // frühere Läufe lösen keinen neuen Eingriff aus
	}
	m.applySubagentConfig(l, c.MaxSubagents)
	m.mu.Lock()
	m.live[c.ID] = l
	m.mu.Unlock()
	if c.Variant != "mcp" {
		go m.watchSubagents(c.ID, l)
	}
	_ = m.st.SetState(ctx, c.ID, store.StateActive)
	go m.pump(c.ID, l)
	m.armIdle(c.ID, l)
	go m.refreshInfo(context.WithoutCancel(ctx), c.ID, a)
	slog.Info("Chat zugewiesen", "chat", c.ID, "platz", slot.ID, "container", a.ContainerName(), "fortgesetzt", session != nil)
	m.publishChat(ctx, c.ID)
	return nil
}

// ensureLive liefert den aktiven Zustand des Chats und setzt einen ruhenden
// Chat dabei in einer frischen Sandbox fort.
func (m *Manager) ensureLive(ctx context.Context, chatID string) (*live, bool, error) {
	unlock := m.lock(chatID)
	defer unlock()
	c, err := m.st.GetChat(ctx, chatID)
	if err != nil {
		return nil, false, err
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

// AttachmentsHeader leitet den Block ein, mit dem Anhänge an eine Nachricht
// gehängt werden; die UI erkennt ihn und zeigt die Anhänge als Chips.
const AttachmentsHeader = "[Anhänge unter /workspace/inputs/]"

// AttachmentNote baut den Block für die genannten Dateien.
func AttachmentNote(names []string) string {
	var b strings.Builder
	b.WriteString(AttachmentsHeader)
	for _, n := range names {
		b.WriteString("\n- ")
		b.WriteString(n)
	}
	return b.String()
}

func (m *Manager) Abort(ctx context.Context, chatID string) (ChatView, error) {
	m.mu.Lock()
	m.userAt[chatID] = time.Now()
	m.aborts[chatID]++ // ein Auftrag, der gerade unterwegs ist, geht nicht mehr an pi
	l := m.live[chatID]
	if l != nil {
		// Eingereihte Nachrichten nach einem Abbruch nicht von selbst übergeben: Der Nutzer hat
		// angehalten; sie gehen mit der nächsten Nachricht oder über „jetzt senden“.
		l.holdQueue, l.holdReason = true, HoldAbort
	}
	m.mu.Unlock()
	if l != nil {
		// pi setzt nach abort fort, was eingereiht ist; Eingeschleustes deshalb vorher zurückholen
		// (es bleibt zurückgehalten eingereiht).
		m.reclaimSteered(ctx, chatID, l)
		if _, err := callT(l.slot.Worker, map[string]any{"type": "abort"}, 10*time.Second); err != nil {
			return ChatView{}, err
		}
	}
	return m.View(ctx, chatID)
}

// Suspend lässt den Chat ruhen: Sitzung sichern, Sandbox abbauen.
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
		m.detach(ctx, chatID, l, true, true)
	}
	_ = m.st.SetState(ctx, chatID, store.StateDormant)
	m.publishChat(ctx, chatID)
	return m.View(ctx, chatID)
}

// SetInternet läuft unter der Chat-Sperre: Ein gleichzeitiges Fortsetzen
// (attach) könnte sonst eine neue Sandbox mit einem veralteten Wert verbinden
// (Review H3).
func (m *Manager) SetInternet(ctx context.Context, chatID string, on bool) (ChatView, error) {
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
		if err := l.slot.Worker.SetInternet(ctx, on); err != nil {
			return ChatView{}, err
		}
	}
	if err := m.st.SetInternet(ctx, chatID, on); err != nil {
		return ChatView{}, err
	}
	slog.Info("Internet umgeschaltet", "chat", chatID, "an", on, "sofort", l != nil)
	m.publishChat(ctx, chatID)
	return m.View(ctx, chatID)
}

// detach sichert die Sitzung (save) und den Arbeitsbereich (workspace) und
// gibt den Platz zurück (Einmalvergabe).
func (m *Manager) detach(ctx context.Context, chatID string, l *live, save, workspace bool) {
	// Das getauschte Plattform-Token gehört zur Sandbox-Sitzung; das Fortsetzen tauscht neu.
	m.opt.Platform.Forget(chatID)
	m.setPendingModel(chatID, "") // ein vorgemerkter Modellwechsel gilt nur für diese Sandbox
	// Hintergrundaufgaben sterben mit der Sandbox; vorher markieren, damit ihr Ende keinen
	// Weckruf auslöst (Close und agentDied haben schon mit eigenem Zustand markiert).
	m.endBackground(ctx, chatID, store.BgSuspended, "beim Ruhen des Chats mit der Sandbox beendet", true)
	if save {
		if err := m.saveSession(ctx, chatID, l.slot.Worker); err != nil {
			slog.Warn("Sitzung nicht gesichert", "chat", chatID, "fehler", err)
		}
		if workspace {
			// Wartet auch eine laufende Sicherung nach agent_settled ab (Sperre je Chat).
			if err := m.saveWorkspace(ctx, chatID, l); err != nil {
				slog.Warn("Arbeitsbereich nicht gesichert", "chat", chatID, "fehler", err)
			}
		}
		// Eine laufende Sicherung von Anzeige-Bildern abwarten, bevor die Sandbox verschwindet.
		m.imageLock(chatID)()
	}
	m.mu.Lock()
	if m.live[chatID] == l {
		delete(m.live, chatID)
	}
	if l.idle != nil {
		l.idle.Stop()
	}
	m.mu.Unlock()
	first := false
	l.stopped.Do(func() { close(l.stop); first = true })
	if !first {
		return // schon abgebaut (etwa Ruhen und gleichzeitiges Ende des Stroms)
	}
	_ = m.st.EndRun(ctx, l.runID)
	m.pool.Release(ctx, l.slot)
	slog.Info("Platz zurückgegeben", "chat", chatID, "platz", l.slot.ID)
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
		return fmt.Errorf("keine Sitzungsdatei in get_state: %s", resp.Data)
	}
	data, err := execPiT(a, []string{"cat", st.SessionFile}, nil, callTimeout)
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return nil // pi schreibt die Datei erst nach der ersten Antwort
	}
	return m.st.SaveSession(context.WithoutCancel(ctx), chatID, data)
}

// --- Leerlauf ---

func (m *Manager) armIdle(chatID string, l *live) {
	m.mu.Lock()
	l.activeAt = time.Now()
	m.mu.Unlock()
	m.armIdleAfter(chatID, l, m.opt.IdleTimeout)
}

// armIdleAfter stellt den Leerlauf-Timer auf d, ohne die letzte Aktivität zu verschieben.
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
		if stuck { // Höchstdauer überschritten: abbrechen, dann ruhen lassen
			slog.Warn("Durchgang zu lang, wird abgebrochen", "chat", chatID)
			_, _ = callT(l.slot.Worker, map[string]any{"type": "abort"}, 10*time.Second)
			m.setRunning(l, false)
		} else if wait, ok := m.keepAliveForBackground(chatID, l); ok {
			// Laufende Hintergrundaufgaben halten den Chat wach, bis BgKeepAlive nach der letzten
			// Aktivität; danach ruht er, und die Aufgaben enden mit der Sandbox.
			m.armIdleAfter(chatID, l, wait)
			return
		}
		if _, err := m.Suspend(ctx, chatID); err != nil {
			if errors.Is(err, ErrRunning) || errors.Is(err, ErrPendingApproval) {
				m.armIdle(chatID, l)
			}
			return
		}
		slog.Info("Chat ruht nach Leerlauf", "chat", chatID)
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

// --- Ereignisse von pi ---

// ExecWatcher meldet das Ende der Ausführungs-Sandbox eines Platzes (E9). Stirbt sie, endet
// pis Ereignisstrom nicht; ohne diese Meldung liefen alle Werkzeugaufrufe ins Leere (H2).
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

// agentDied behandelt das unerwartete Ende eines der beiden Container eines Platzes. cause:
// "pi" (Ereignisstrom endete; die Ausführungs-Sandbox lebt noch, der Arbeitsbereich wird
// gesichert) oder "exec" (Ausführungs-Sandbox beendet; pi lebt noch, die Sitzung wird gesichert).
func (m *Manager) agentDied(chatID string, l *live, cause string) {
	ctx := context.Background()
	// Endet der Strom, weil der Platz gerade zurückgegeben wird, ist das kein Absturz.
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
	msg := "Die Sandbox ist unerwartet beendet worden. Der Chat ruht; eine neue Nachricht setzt ihn mit der zuletzt gesicherten Sitzung fort."
	if cause == "exec" {
		slog.Error("Ausführungs-Sandbox beendet", "chat", chatID, "platz", l.slot.ID)
		msg = "Die Ausführungs-Sandbox ist unerwartet beendet worden. Der Chat ruht; eine neue Nachricht setzt ihn mit der Sitzung und dem zuletzt gesicherten Arbeitsbereich fort."
	} else {
		slog.Error("pi beendet", "chat", chatID, "platz", l.slot.ID)
	}
	m.publish(chatID, Event{Kind: "error", Data: map[string]string{"message": msg}})
	unlock := m.lock(chatID)
	defer unlock()
	m.mu.Lock()
	current = m.live[chatID] == l
	m.mu.Unlock()
	if !current { // inzwischen ruhen gelassen oder beendet
		return
	}
	m.rejectPending(ctx, chatID, "Sandbox beendet")
	m.endBackground(ctx, chatID, store.BgLost, "Sandbox unerwartet beendet", true)
	if cause == "exec" {
		// pi lebt: Sitzung sichern (der Arbeitsbereich ist mit der Sandbox verloren).
		if err := m.saveSession(ctx, chatID, l.slot.Worker); err != nil {
			slog.Warn("Sitzung nicht gesichert", "chat", chatID, "fehler", err)
		}
	} else if err := m.saveWorkspace(ctx, chatID, l); err != nil {
		// Die Ausführungs-Sandbox lebt noch (H1): Arbeitsbereich sichern, soweit möglich.
		slog.Warn("Arbeitsbereich nicht gesichert", "chat", chatID, "fehler", err)
	}
	m.detach(ctx, chatID, l, false, false)
	_ = m.st.SetState(ctx, chatID, store.StateDormant)
	m.publishChat(ctx, chatID)
}

func (m *Manager) handle(chatID string, l *live, ev rpc.Event) {
	ctx := context.Background()
	switch ev.Type {
	case rpc.TypeOverflow:
		slog.Warn("Ereignisse verworfen (Flut im RPC-Strom)", "chat", chatID, "anzahl", string(ev.Raw))
		m.publish(chatID, Event{Kind: "error", Data: map[string]string{"message": "Ereignisse im RPC-Strom verworfen (" + string(ev.Raw) + "); Zustand wird nachgeprüft"}})
		m.background(l, func() { m.resync(chatID, l) })
		return
	case rpc.TypeOversized, rpc.TypeInvalid:
		slog.Warn("auffällige Zeile im RPC-Strom", "chat", chatID, "art", ev.Type)
		m.publish(chatID, Event{Kind: "error", Data: map[string]string{"message": "Auffällige Zeile im RPC-Strom (" + ev.Type + ")"}})
		return
	}
	if ev.Type == "extension_ui_request" {
		m.answerExtensionUI(chatID, l, ev.Raw)
		return
	}
	var userTurn *turnMeta
	if ev.Type == "message_end" {
		if text, ok := userMessageText(ev.Raw); ok {
			// Herkunft vor der Nachricht verteilen: Die UI ordnet sie der nächsten Nutzernachricht zu.
			if userTurn = m.takeTurn(l, text); userTurn != nil {
				m.publish(chatID, Event{Kind: "user_meta", Data: UserMeta{TurnID: userTurn.id, Trigger: userTurn.trigger, Origin: userTurn.origin, Sources: userTurn.sources}})
			}
		}
	}
	m.publish(chatID, Event{Kind: "pi", Data: ev.Raw})

	switch ev.Type {
	case "agent_start":
		m.mu.Lock()
		// Läuft nichts, hat pi den Durchgang selbst begonnen (dispatch setzt running vor prompt;
		// Wiederholungen innerhalb eines Laufs kommen vor agent_settled): etwa, weil ein Subagent im
		// Hintergrund fertig ist (pi-subagents, triggerTurn).
		byPi := !l.running
		for _, p := range l.pendingTurns {
			if !p.isConsumed() {
				byPi = false // ein Auftrag des Orchestrators ist unterwegs (etwa spät eingeschleust)
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
			// Das Modell schreibt einen Werkzeugaufruf (bei write mit großer Datei dauert das).
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
		// Eingereihtes jetzt einschleusen: pi fügt es nach den laufenden Werkzeugen ein, vor dem
		// nächsten Modellaufruf (wie Claude Code), statt bis zum Ende des Durchgangs zu warten.
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
				// Nachricht einer Erweiterung an den Agenten (etwa das Ende eines Subagenten im
				// Hintergrund): gehört zum laufenden Durchgang, nicht vom Nutzer.
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
					l.turn = nil // Nutzernachricht ohne Auftrag des Orchestrators: Herkunft unbekannt
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
				slog.Error("Nachricht nicht gespeichert", "chat", chatID, "fehler", err)
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
					slog.Warn("Sitzung nicht gesichert", "chat", chatID, "fehler", err)
				}
			}
			m.refreshInfo(ctx, chatID, l.slot.Worker)
			m.applyPendingModel(ctx, chatID)
			if !running { // manuelle Kompaktierung vorbei: Eingereihtes jetzt übergeben
				m.deliverQueue(chatID, l)
			}
		})
	case "agent_settled":
		m.mu.Lock()
		// Aufträge von vor diesem Lauf, zu denen pi keine Nutzernachricht gemeldet hat (etwa
		// Befehle einer Extension), ordnen keine spätere Nachricht mehr falsch zu.
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
			// Eingeschleustes, das pi nicht mehr eingefügt hat, zurückholen (vor deliverQueue).
			m.reclaimSteered(ctx, chatID, l)
			if err := m.saveSession(ctx, chatID, l.slot.Worker); err != nil {
				slog.Warn("Sitzung nicht gesichert", "chat", chatID, "fehler", err)
			}
			m.refreshInfo(ctx, chatID, l.slot.Worker)
			if err := m.saveWorkspace(ctx, chatID, l); err != nil {
				slog.Warn("Arbeitsbereich nicht gesichert", "chat", chatID, "fehler", err)
			}
			// Erst nach dem Sichern: Der nächste Auftrag beginnt auf einem gesicherten Stand.
			m.deliverQueue(chatID, l)
		})
		m.armIdle(chatID, l)
		m.publishChat(ctx, chatID)
	}
}

// NotePi: Quelle einer Nachricht, die eine Erweiterung in pi selbst eingestellt hat (pi-subagents).
const NotePi = "pi"

// startPiTurn legt einen Durchgang an, den pi ohne Auftrag des Orchestrators begonnen hat (Weckruf ohne
// Nutzer). Seine Antworten zählen so nicht zum vorigen Auftrag des Nutzers, und die Grenze für
// Durchgänge ohne Nutzer (AGW_AUTO_TURNS_MAX, Review 3, H2) gilt auch hier: Ist sie erreicht, bricht
// der Orchestrator den Durchgang ab und hält Eingereihtes zurück.
func (m *Manager) startPiTurn(ctx context.Context, chatID string, l *live) {
	ev, limited := m.autoLimitTurns(ctx, chatID)
	sources := []store.Source{{Kind: store.QueueSystem, Type: NotePi}}
	tm := &turnMeta{trigger: store.TriggerWake, origin: store.OriginSystem, sources: sources, at: time.Now()}
	if id, err := m.st.CreateTurn(ctx, chatID, store.TriggerWake, store.OriginSystem, sources, nil); err == nil {
		tm.id = id
	} else {
		slog.Warn("Durchgang von pi nicht angelegt", "chat", chatID, "fehler", err)
	}
	tm.markConsumed()
	m.mu.Lock()
	l.turn = tm
	m.mu.Unlock()
	slog.Info("pi beginnt einen Durchgang ohne Auftrag (Weckruf)", "chat", chatID, "durchgang", tm.id)
	if limited {
		m.mu.Lock()
		l.holdQueue, l.holdReason = true, ev.Reason
		m.mu.Unlock()
		slog.Warn("Grenze für Durchgänge ohne Nutzer erreicht, Durchgang von pi abgebrochen", "chat", chatID, "grenze", ev.Limit)
		m.publish(chatID, Event{Kind: "auto_held", Data: ev})
		go func() { _, _ = callT(l.slot.Worker, map[string]any{"type": "abort"}, 10*time.Second) }()
	}
}

// UserMeta ist das SSE-Ereignis „user_meta“: Herkunft der Nutzernachricht, die als nächstes
// Ereignis „pi“ (message_end, role user) folgt.
type UserMeta struct {
	TurnID  int64          `json:"turn_id,omitempty"`
	Trigger string         `json:"trigger"`
	Origin  string         `json:"origin"`
	Sources []store.Source `json:"sources"`
}

// userMessageText liefert den Text einer Nutzernachricht aus message_end (ok nur bei role user).
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

// bill rechnet die Kosten einer Antwort nach dem Tarif des Anbieters zum
// Zeitpunkt der Antwort (Spitzen-/Nebenzeit). pi kennt nur einen Preis.
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

// ContextUsage ist die Kontextauslastung, wie die API sie liefert.
type ContextUsage struct {
	Tokens           *int64    `json:"tokens"`
	Window           int64     `json:"window"`
	Percent          *float64  `json:"percent"`
	ThresholdTokens  int64     `json:"threshold_tokens"`
	ReserveTokens    int64     `json:"reserve_tokens"`
	KeepRecentTokens int64     `json:"keep_recent_tokens"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// refreshInfo liest Kontextauslastung und Befehle von pi und speichert sie.
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

// storeCompaction legt eine Kompaktierung als eigenen Eintrag ab und rechnet
// die Zusammenfassung ab (sie erscheint in keiner Antwort).
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
		slog.Warn("Kompaktierung ohne Ergebnis", "chat", chatID, "abgebrochen", ev.Aborted, "fehler", ev.ErrorMessage)
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
		slog.Error("Kompaktierung nicht gespeichert", "chat", chatID, "fehler", err)
		return
	}
	slog.Info("Kontext kompaktiert", "chat", chatID, "grund", ev.Reason)
}

// Command ist ein Slash-Befehl für die UI.
type Command struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Source      string `json:"source"`
	Args        string `json:"args,omitempty"`
	// Options: mögliche Argumente zur Vervollständigung (/model, /effort).
	Options []CommandOption `json:"options,omitempty"`
}

type CommandOption struct {
	Value   string `json:"value"`
	Label   string `json:"label,omitempty"`
	Current bool   `json:"current,omitempty"`
}

// compactLanguageHint steht vor den Anweisungen jeder manuellen Kompaktierung;
// ohne ihn schreibt pi die Zusammenfassung auf Englisch.
const compactLanguageHint = "Schreibe die Zusammenfassung auf Deutsch."

var builtinCommands = []Command{
	{Name: "compact", Description: "Kontext jetzt zusammenfassen; optional mit Anweisungen, worauf die Zusammenfassung achten soll", Source: "builtin", Args: "[Anweisungen]"},
	{Name: "autocompact", Description: "Automatische Kompaktierung ein- oder ausschalten", Source: "builtin", Args: "on|off"},
	{Name: "rename", Description: "Chat umbenennen", Source: "builtin", Args: "<Name>"},
	{Name: "model", Description: "Modell wechseln; passt der Kontext nicht, erst kompaktieren", Source: "builtin", Args: "<anbieter/modell>"},
	{Name: "effort", Description: "Denkstufe des Modells einstellen", Source: "builtin", Args: "<Stufe>"},
}

// Commands liefert die eingebauten Befehle und die von pi (zuletzt bekannt).
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
	return append(out, pi...), nil
}

// RunCommand führt einen Slash-Befehl aus. Was kein eingebauter Befehl ist,
// geht als Nachricht an pi (pi expandiert Skills und Prompt-Vorlagen).
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
		case "on", "an", "ein", "true":
			on = true
		case "off", "aus", "false":
		default:
			return SendResult{}, fmt.Errorf("/autocompact erwartet on oder off")
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
		// compact antwortet erst nach der Zusammenfassung; der Verlauf kommt über
		// compaction_start/compaction_end.
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			if _, err := l.slot.Worker.Call(ctx, cmd); err != nil {
				m.mu.Lock()
				l.compacting = false
				m.mu.Unlock()
				m.setPendingModel(chatID, "") // ein vorgemerkter Modellwechsel entfällt
				m.publish(chatID, Event{Kind: "error", Data: map[string]string{"message": "Kompaktierung fehlgeschlagen: " + err.Error()}})
				l.slot.SetActivity("idle", "")
				return
			}
			// Meldet pi kein compaction_end (etwa, weil nichts zu kompaktieren war), bliebe ein
			// vorgemerkter Modellwechsel sonst stehen; nach compaction_end ist er schon erledigt.
			m.background(l, func() {
				m.mu.Lock()
				pending := m.pendingModel[chatID] != "" && !l.compacting // sonst erledigt es compaction_end
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

// maxTitle: Länge eines Titels in Zeichen (Runen).
const maxTitle = 120

// Rename setzt den Titel (/rename); danach benennt der Orchestrator den Chat nicht mehr selbst.
func (m *Manager) Rename(ctx context.Context, chatID, title string) (ChatView, error) {
	title = strings.Join(strings.Fields(title), " ")
	if title == "" {
		return ChatView{}, fmt.Errorf("%w: /rename erwartet einen Namen", ErrInvalid)
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

// Titler formuliert einen Chattitel (Paket titler).
type Titler interface {
	Title(ctx context.Context, chatModel, text string) (titler.Result, error)
}

// autoTitle benennt einen Chat mit Platzhaltertitel nach der ersten Frage des Nutzers: sofort mit
// der gekürzten Frage, dann (mit Titler) einmal mit einem Titel des Modells. Dessen Aufruf steht in
// aux_llm_calls, nicht in llm_calls, damit Kosten und Aufrufzahlen je Chat nur die Arbeit des
// Agenten zeigen.
func (m *Manager) autoTitle(ctx context.Context, chatID, text string) {
	if strings.TrimSpace(text) == "" || strings.HasPrefix(strings.TrimSpace(text), "/") {
		return
	}
	ok, err := m.st.AutoTitle(ctx, chatID, titleFrom(text))
	if err != nil {
		slog.Warn("Titel nicht gesetzt", "chat", chatID, "fehler", err)
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
		slog.Warn("Titel des Modells nicht erhalten", "chat", chatID, "fehler", err)
	}
	if err := m.st.RecordAuxCall(ctx, call); err != nil {
		slog.Warn("Titel-Aufruf nicht erfasst", "chat", chatID, "fehler", err)
	}
	if r.Title == "" {
		return
	}
	if ok, err := m.st.ModelTitle(ctx, chatID, r.Title); err != nil {
		slog.Warn("Titel des Modells nicht gesetzt", "chat", chatID, "fehler", err)
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

// answerExtensionUI beantwortet Rückfragen von Extensions (confirm, select,
// input, editor) mit Abbruch. Ohne Antwort würde die Extension ewig warten;
// Freigaben laufen in diesem PoC über den Orchestrator, nicht über pi.
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
		m.LogCall(l.slot.ID, chatID, "pi", "extension_ui", r.Method+": "+r.Title, "abgelehnt")
	}
}

// resync prüft nach verworfenen Ereignissen, ob pi noch arbeitet.
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

// --- Artefakte und Bestätigung (Backend des Sockets) ---

func (m *Manager) ChatForSlot(slotID string) string { return m.SlotChat(slotID) }

func (m *Manager) LogCall(slotID, chatID, via, op, detail, result string) {
	m.LogCallBy(context.Background(), slotID, chatID, via, op, detail, result)
}

// LogCallBy protokolliert einen Socket-Aufruf samt Sitzung und Werkzeugaufruf aus dem Kontext
// (sock.CallerLogger): So zeigt das Protokoll, ob der Hauptagent oder ein Subagent fragte.
func (m *Manager) LogCallBy(ctx context.Context, slotID, chatID, via, op, detail, result string) {
	sc, err := m.st.AddSocketCall(context.WithoutCancel(ctx), store.SocketCall{ChatID: chatID, SlotID: slotID, Via: via, Op: op, Detail: detail, Result: result,
		Session: store.SessionFrom(ctx), ToolCallID: store.ToolCallFrom(ctx)})
	if err != nil {
		slog.Error("Socket-Aufruf nicht protokolliert", "fehler", err)
		return
	}
	slog.Info("Socket-Aufruf", "platz", slotID, "chat", chatID, "weg", via, "op", op, "detail", detail, "ergebnis", result)
	if chatID != "" {
		m.publish(chatID, Event{Kind: "socket_call", Data: sc})
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

// Upload legt die Datei als ausstehend ab und wartet auf die Entscheidung des
// Nutzers. Zeitüberschreitung und Abbruch gelten als Ablehnung.
func (m *Manager) Upload(ctx context.Context, chatID, slotID, via, name string, size int64, sha string, body io.Reader) (sock.UploadResult, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return sock.UploadResult{}, err
	}
	if err := m.checkPendingLimit(ctx, chatID); err != nil {
		return sock.UploadResult{}, err
	}
	sum := sha256.Sum256(data)
	got := hex.EncodeToString(sum[:])
	if sha != "" && !strings.EqualFold(sha, got) {
		return sock.UploadResult{Status: "rejected", Name: name, Size: int64(len(data)), SHA256: got, Message: "Prüfsumme stimmt nicht"}, nil
	}
	ct := contentType(name, data)
	preview := ""
	if artifacts.IsText(data) {
		p := data
		if len(p) > 4096 {
			p = p[:4096]
		}
		preview = strings.ToValidUTF8(string(p), "")
	}
	pendingKey := path.Join("pending", chatID, fmt.Sprintf("%d-%s", time.Now().UnixNano(), name))
	if err := m.blobs.Put(ctx, pendingKey, bytes.NewReader(data), int64(len(data)), ct); err != nil {
		return sock.UploadResult{}, fmt.Errorf("Ablage: %w", err)
	}
	ap, err := m.st.CreateApproval(ctx, store.Approval{ChatID: chatID, Kind: "artifact_upload", Via: via, Name: name, Size: int64(len(data)), SHA256: got, ContentType: ct, PendingKey: pendingKey, Preview: preview,
		Session: store.SessionFrom(ctx), ToolCallID: store.ToolCallFrom(ctx)})
	if err != nil {
		_ = m.blobs.Delete(ctx, pendingKey)
		return sock.UploadResult{}, err
	}
	w := m.broker.Register(ap.ID)
	var prevAct *pool.Activity
	if s, ok := m.pool.Get(slotID); ok {
		prevAct = s.Info().Activity
		s.SetActivity("waiting_approval", "")
	}
	m.publish(chatID, Event{Kind: "approval", Data: ap})
	m.publishChat(ctx, chatID)
	slog.Info("Bestätigung angefragt", "chat", chatID, "freigabe", ap.ID, "name", name, "bytes", len(data), "weg", via)

	approved, werr := w.Wait(ctx, m.opt.ApprovalTimeout)
	bg := context.WithoutCancel(ctx)
	final := m.settle(bg, chatID, ap.ID, approved, werr)
	msg := "vom Nutzer abgelehnt"
	if final == store.ApprovalExpired {
		msg = "keine Entscheidung innerhalb der Wartezeit"
	}
	if s, ok := m.pool.Get(slotID); ok {
		if prevAct != nil && prevAct.Kind == "tool" {
			s.SetActivity("tool", prevAct.Tool)
		} else {
			s.SetActivity("thinking", "")
		}
	}
	res := sock.UploadResult{Name: name, Size: int64(len(data)), SHA256: got}
	if final != store.ApprovalApproved {
		_ = m.blobs.Delete(bg, pendingKey)
		res.Status, res.Message = "rejected", msg
		m.publishChat(bg, chatID)
		return res, nil
	}
	key := objectKey(chatID, store.KindOutput, name)
	if err := m.blobs.Move(bg, pendingKey, key); err != nil {
		return res, fmt.Errorf("Ablage nach Bestätigung: %w", err)
	}
	art := store.Artifact{ChatID: chatID, Kind: store.KindOutput, Name: name, Size: int64(len(data)), SHA256: got, ContentType: ct, Via: via, ObjectKey: key,
		ToolCallID: store.ToolCallFrom(ctx)}
	if err := m.st.PutArtifact(bg, art); err != nil {
		return res, err
	}
	art.CreatedAt = time.Now()
	m.publish(chatID, Event{Kind: "artifact", Data: art})
	m.publishChat(bg, chatID)
	res.Status = "approved"
	return res, nil
}

// RequestInternet: Der Agent bittet um Internetzugang. Der Aufruf wartet auf
// die Entscheidung des Nutzers; erst bei Zustimmung wird die Sandbox mit dem
// Egress-Netz verbunden.
func (m *Manager) RequestInternet(ctx context.Context, chatID, slotID, via, reason string) (sock.UploadResult, error) {
	c, err := m.st.GetChat(ctx, chatID)
	if err != nil {
		return sock.UploadResult{}, err
	}
	if c.Internet {
		return sock.UploadResult{Status: "approved", Name: "internet", Message: "Internetzugang ist bereits freigegeben"}, nil
	}
	if err := m.checkPendingLimit(ctx, chatID); err != nil {
		return sock.UploadResult{}, err
	}
	reason = strings.TrimSpace(strings.ToValidUTF8(reason, ""))
	if r := []rune(reason); len(r) > 500 {
		reason = string(r[:500]) + " …"
	}
	if reason == "" {
		reason = "(keine Begründung angegeben)"
	}
	ap, err := m.st.CreateApproval(ctx, store.Approval{ChatID: chatID, Kind: "internet_access", Via: via, Name: reason, ContentType: "text/plain", PendingKey: "-", Preview: reason,
		Session: store.SessionFrom(ctx), ToolCallID: store.ToolCallFrom(ctx)})
	if err != nil {
		return sock.UploadResult{}, err
	}
	w := m.broker.Register(ap.ID)
	if s, ok := m.pool.Get(slotID); ok {
		s.SetActivity("waiting_approval", "")
	}
	m.publish(chatID, Event{Kind: "approval", Data: ap})
	m.publishChat(ctx, chatID)
	slog.Info("Internetzugang angefragt", "chat", chatID, "freigabe", ap.ID, "weg", via)
	approved, werr := w.Wait(ctx, m.opt.ApprovalTimeout)
	bg := context.WithoutCancel(ctx)
	final := m.settle(bg, chatID, ap.ID, approved, werr)
	if s, ok := m.pool.Get(slotID); ok {
		s.SetActivity("tool", "")
	}
	if final == store.ApprovalExpired {
		m.publishChat(bg, chatID)
		return sock.UploadResult{Status: "rejected", Name: "internet", Message: "keine Entscheidung innerhalb der Wartezeit"}, nil
	}
	if final != store.ApprovalApproved {
		m.publishChat(bg, chatID)
		return sock.UploadResult{Status: "rejected", Name: "internet", Message: "vom Nutzer abgelehnt"}, nil
	}
	if _, err := m.SetInternet(bg, chatID, true); err != nil {
		return sock.UploadResult{}, err
	}
	return sock.UploadResult{Status: "approved", Name: "internet", Message: "Internetzugang freigegeben"}, nil
}

// visibleControls schreibt Steuer- und Bidi-Zeichen als \u-Folge, damit die Vorschau einer
// Bestätigung nichts optisch umstellt oder verbirgt (Review W1). Zeilenumbruch und Tab bleiben.
func visibleControls(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f || (r >= 0x200b && r <= 0x200f) || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) || r == 0xfeff:
			fmt.Fprintf(&b, "\\u%04x", r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// maxPlatformPreview begrenzt Methode, Pfad und eingerückten Körper eines schreibenden Plattform-Aufrufs.
const maxPlatformPreview = 16000

// PlatformExchanged protokolliert einen Token-Austausch für einen Chat (platform.Client.OnExchange):
// wer im Token steht (sub, azp, aud), bis wann es gilt und dass kein act-Claim kommt.
func (m *Manager) PlatformExchanged(chatID string, cl platform.Claims) {
	m.LogCall(m.slotOf(chatID), chatID, "orchestrator", "token_exchange", cl.String(), "ok")
}

// PlatformCall führt einen Aufruf der Agri-Gaia-Plattform aus. Lesende Aufrufe (GET)
// gehen direkt durch; alles andere wartet auf die Bestätigung des Nutzers und wird
// nur bei Zustimmung ausgeführt. Die Bestätigung zeigt Methode, Pfad und Körper.
func (m *Manager) PlatformCall(ctx context.Context, chatID, slotID, via string, req platform.Request) (platform.Result, error) {
	if m.opt.Platform == nil {
		return platform.Result{Status: "error", Message: platform.ErrNotConfigured.Error()}, nil
	}
	// Prüfung auch hier, nicht nur beim Aufrufer: Writes() setzt eine normalisierte Methode voraus (Review W2).
	req, err := platform.Normalize(req)
	if err != nil {
		return platform.Result{Status: "error", Message: err.Error()}, nil
	}
	del, err := m.delegationOf(ctx, chatID)
	if err != nil {
		// Eine unlesbare Delegation gewährt nichts.
		return platform.Result{Status: "denied", Message: err.Error()}, nil
	}
	if req.Path == platform.RightsPath {
		return platform.Result{Status: "ok", HTTPStatus: 200, Body: m.rightsText(ctx, chatID, del)}, nil
	}
	// Delegation (Schritt 1): Einordnung allein aus Methode und Pfad, Prüfung vor jeder Rückfrage.
	access := delegation.Classify(req)
	violation := ""
	if del != nil {
		own := func(res, id string) bool {
			ok, _ := m.st.IsDelegationObject(ctx, chatID, res, id)
			return ok
		}
		if dec := del.Check(access, time.Now(), own); !dec.Allowed {
			if del.Enforcing() {
				return platform.Result{Status: "denied", Message: dec.Reason}, nil
			}
			violation = dec.Reason // Stufe ohne Schutzmaßnahme: nur protokollieren
		}
	}
	if req.Writes() && (del == nil || del.ConfirmWrites()) {
		if err := m.checkPendingLimit(ctx, chatID); err != nil {
			return platform.Result{}, err
		}
		preview := req.String() + "\nEinordnung: " + access.String()
		if del != nil && violation == "" {
			preview += " (in den übertragenen Rechten)"
		}
		if d := req.Describe(); d != "" {
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
		preview = visibleControls(preview)
		// Der Nutzer bestätigt nur, was er vollständig sieht: lieber abweisen als kürzen (Review W1).
		if n := len([]rune(preview)); n > maxPlatformPreview {
			return platform.Result{Status: "error", Message: fmt.Sprintf("Aufruf zu groß für eine Bestätigung (%d Zeichen, höchstens %d); Körper verkleinern", n, maxPlatformPreview)}, nil
		}
		size := int64(len(req.Body))
		for _, u := range req.Uploads {
			size += int64(len(u.Data))
		}
		ap, err := m.st.CreateApproval(ctx, store.Approval{ChatID: chatID, Kind: "platform_write", Via: via, Name: req.String(), Size: size,
			ContentType: "application/json", PendingKey: "-", Preview: preview, Session: store.SessionFrom(ctx), ToolCallID: store.ToolCallFrom(ctx)})
		if err != nil {
			return platform.Result{}, err
		}
		w := m.broker.Register(ap.ID)
		if s, ok := m.pool.Get(slotID); ok {
			s.SetActivity("waiting_approval", "")
		}
		m.publish(chatID, Event{Kind: "approval", Data: ap})
		m.publishChat(ctx, chatID)
		slog.Info("Plattform-Aufruf angefragt", "chat", chatID, "freigabe", ap.ID, "aufruf", req.String(), "weg", via)
		approved, werr := w.Wait(ctx, m.opt.ApprovalTimeout)
		bg := context.WithoutCancel(ctx)
		final := m.settle(bg, chatID, ap.ID, approved, werr)
		if s, ok := m.pool.Get(slotID); ok {
			s.SetActivity("tool", "")
		}
		m.publishChat(bg, chatID)
		if final == store.ApprovalExpired {
			return platform.Result{Status: "rejected", Message: "keine Entscheidung innerhalb der Wartezeit; nichts ausgeführt"}, nil
		}
		if final != store.ApprovalApproved {
			return platform.Result{Status: "rejected", Message: "vom Nutzer abgelehnt; nichts ausgeführt"}, nil
		}
		ctx = bg
		// Während der Wartezeit kann die Delegation abgelaufen sein (Review 5, W3): frisch laden und erneut prüfen.
		if del != nil {
			fresh, err := m.delegationOf(ctx, chatID)
			if err != nil || fresh == nil {
				return platform.Result{Status: "denied", Message: "Delegation nach der Bestätigung nicht mehr lesbar"}, nil
			}
			own := func(res, id string) bool {
				ok, _ := m.st.IsDelegationObject(ctx, chatID, res, id)
				return ok
			}
			if dec := fresh.Check(access, time.Now(), own); !dec.Allowed && fresh.Enforcing() {
				return platform.Result{Status: "denied", Message: dec.Reason + " (nach der Bestätigung erneut geprüft)"}, nil
			}
		}
	}
	res, err := m.opt.Platform.Do(ctx, chatID, req)
	if err != nil {
		return platform.Result{Status: "error", Message: err.Error(), Violation: violation}, nil
	}
	res.Violation = violation
	if del != nil {
		if r, id, ok := delegation.Created(access, res); ok {
			if err := m.st.AddDelegationObject(context.WithoutCancel(ctx), chatID, r, id); err != nil {
				slog.Error("Herkunft nicht festgehalten", "chat", chatID, "objekt", r+" "+id, "fehler", err)
			}
		}
	}
	return res, nil
}

// PlatformPrecheck prüft einen Aufruf gegen die Delegation, ohne ihn auszuführen. Der Socket ruft
// sie vor dem Lesen der Dateien eines Uploads auf, damit ein Übergriff keine 512 MB aus der Sandbox
// liest (Review 5, M7). Maßgeblich bleibt die Prüfung in PlatformCall.
func (m *Manager) PlatformPrecheck(ctx context.Context, chatID string, req platform.Request) (platform.Result, bool) {
	del, err := m.delegationOf(ctx, chatID)
	if err != nil {
		return platform.Result{Status: "denied", Message: err.Error()}, false
	}
	if del == nil || !del.Enforcing() {
		return platform.Result{}, true
	}
	own := func(res, id string) bool {
		ok, _ := m.st.IsDelegationObject(ctx, chatID, res, id)
		return ok
	}
	if dec := del.Check(delegation.Classify(req), time.Now(), own); !dec.Allowed {
		return platform.Result{Status: "denied", Message: dec.Reason}, false
	}
	return platform.Result{}, true
}

// delegationOf liest die Delegation eines Chats (nil: ohne Delegation).
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
		return nil, fmt.Errorf("Delegation des Chats unlesbar: %w", err)
	}
	return d, nil
}

// rightsText beschreibt dem Agenten seine Rechte samt den Objekten, die er angelegt hat.
func (m *Manager) rightsText(ctx context.Context, chatID string, del *delegation.Delegation) string {
	if del == nil {
		return "Für diesen Chat ist keine Delegation festgelegt: Lesen geht ohne Rückfrage, jeder schreibende Aufruf braucht die Bestätigung des Nutzers."
	}
	s := del.Summary()
	if objs, err := m.st.ListDelegationObjects(ctx, chatID); err == nil && len(objs) > 0 {
		s += "\nIn diesem Chat angelegt:"
		for _, o := range objs {
			s += "\n  " + o.Resource + " " + o.ObjectID
		}
	}
	return s
}

// checkPendingLimit begrenzt offene Bestätigungen je Chat (Review H2).
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

// settle liefert den endgültigen Zustand einer Bestätigung. Läuft die
// Wartezeit ab, während der Nutzer gerade entscheidet, gilt die Entscheidung
// aus der Datenbank, nicht der Ablauf (Review M7).
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

// Decide ist die Entscheidung des Nutzers aus der API.
func (m *Manager) Decide(ctx context.Context, approvalID string, approve bool) (store.Approval, error) {
	state := store.ApprovalRejected
	if approve {
		state = store.ApprovalApproved
	}
	ap, ok, err := m.st.DecideApproval(ctx, approvalID, state)
	if err != nil {
		return ap, err
	}
	if ok {
		m.broker.Resolve(approvalID, approve)
		m.publish(ap.ChatID, Event{Kind: "approval", Data: ap})
		m.publishChat(ctx, ap.ChatID)
		slog.Info("Bestätigung entschieden", "freigabe", approvalID, "bestätigt", approve)
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
			slog.Info("offene Bestätigung abgelehnt", "freigabe", a.ID, "grund", why)
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

// --- Eingaben des Nutzers ---

const inputsDir = "/workspace/inputs"

// AddInput legt eine vom Nutzer hochgeladene Datei ab und spiegelt sie bei
// aktivem Chat sofort in die Sandbox.
func (m *Manager) AddInput(ctx context.Context, chatID, name string, data []byte) (store.Artifact, error) {
	_, err := m.st.GetChat(ctx, chatID)
	if err != nil {
		return store.Artifact{}, err
	}
	name = artifacts.SanitizeName(name)
	if name == "" {
		return store.Artifact{}, errors.New("ungültiger Dateiname")
	}
	if int64(len(data)) > m.opt.ArtifactMaxBytes {
		return store.Artifact{}, fmt.Errorf("Datei größer als %d MB", m.opt.ArtifactMaxBytes>>20)
	}
	sum := sha256.Sum256(data)
	ct := contentType(name, data)
	key := objectKey(chatID, store.KindInput, name)
	if err := m.blobs.Put(ctx, key, bytes.NewReader(data), int64(len(data)), ct); err != nil {
		return store.Artifact{}, err
	}
	art := store.Artifact{ChatID: chatID, Kind: store.KindInput, Name: name, Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:]), ContentType: ct, Via: "ui", ObjectKey: key}
	if err := m.st.PutArtifact(ctx, art); err != nil {
		return store.Artifact{}, err
	}
	art.CreatedAt = time.Now()
	m.mu.Lock()
	l := m.live[chatID]
	m.mu.Unlock()
	if l != nil {
		if err := m.pushInput(ctx, l.slot.Worker, name, data); err != nil {
			slog.Warn("Eingabe nicht in Sandbox gespiegelt", "chat", chatID, "name", name, "fehler", err)
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

// syncInputs spiegelt alle Eingaben des Chats in die Sandbox; liefert Zahl und Summe der Größen.
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

// --- Start und Ende des Orchestrators ---

// Recover setzt nach einem Neustart aktive Chats auf ruhend (ihre Sandboxen
// existieren nicht mehr) und offene Bestätigungen auf abgelaufen.
func (m *Manager) Recover(ctx context.Context) error {
	n, err := m.st.ExpirePendingApprovals(ctx)
	if err != nil {
		return err
	}
	active, err := m.st.ChatsInState(ctx, store.StateActive)
	if err != nil {
		return err
	}
	for _, c := range active {
		_ = m.st.SetState(ctx, c.ID, store.StateDormant)
	}
	// Hintergrundaufgaben sind mit den Sandboxen verschwunden; dem Agenten beim nächsten Auftrag sagen.
	if nbg, err := m.st.EndAllRunningBackground(ctx, "Orchestrator neu gestartet"); err != nil {
		return err
	} else if nbg > 0 {
		slog.Info("Hintergrundaufgaben nach Neustart beendet", "anzahl", nbg)
	}
	if n > 0 || len(active) > 0 {
		slog.Info("Neustart aufgeräumt", "abgelaufene_bestaetigungen", n, "ruhend_gesetzt", len(active))
	}
	return nil
}

// Shutdown sichert die Sitzungen aller aktiven Chats und lässt sie ruhen.
func (m *Manager) Shutdown(ctx context.Context) {
	m.mu.Lock()
	ids := make([]string, 0, len(m.live))
	for id := range m.live {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	for _, id := range ids {
		m.rejectPending(ctx, id, "Orchestrator beendet")
		m.mu.Lock()
		l := m.live[id]
		m.mu.Unlock()
		if l == nil {
			continue
		}
		m.detach(ctx, id, l, true, true)
		_ = m.st.SetState(ctx, id, store.StateDormant)
	}
}

// Messages, Approvals und SocketCalls für die Chat-Ansicht.
func (m *Manager) Messages(ctx context.Context, chatID string) ([]store.Message, error) {
	return m.st.Messages(ctx, chatID)
}
func (m *Manager) Approvals(ctx context.Context, state, chatID string) ([]store.Approval, error) {
	return m.st.ListApprovals(ctx, state, chatID)
}
func (m *Manager) SocketCalls(ctx context.Context, chatID string) ([]store.SocketCall, error) {
	return m.st.ListSocketCalls(ctx, chatID)
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

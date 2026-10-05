// Package bgtask verwaltet die Hintergrundaufgaben eines Platzes: Befehle, die der Agent mit
// bash und run_in_background startet. Der Orchestrator führt sie wie jeden Befehl in der
// Ausführungs-Sandbox aus (Operation bg von agw-exec), liest ihre Ausgabe bis zum Ende mit und
// meldet Start, Fortschritt (gedrosselt) und Ende an den Manager. Die Verbindung zum Überwacher
// trägt das Ende von selbst (Push): Kein Abfragen, und das Ende kommt von dem Prozess, der den
// Befehl gestartet hat, nicht aus einer Datei, die der Agent verändern könnte.
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

// Runner führt eine Hintergrundaufgabe in der Ausführungs-Sandbox aus (execbox.Client).
type Runner interface {
	RunBackground(ctx context.Context, req execproto.Request, onStart func(pgid int), onData func([]byte)) (execproto.Frame, error)
}

// Notifier ist der Manager: Er legt die Zeile an (Nummer im Chat), verteilt Fortschritt und Ende
// und benachrichtigt den Agenten.
type Notifier interface {
	// BackgroundCreate legt die Aufgabe an und vergibt Nummer und Ausgabedatei.
	BackgroundCreate(ctx context.Context, t store.BackgroundTask) (store.BackgroundTask, error)
	// BackgroundProgress: laufende Aufgabe mit neuer Ausgabe (höchstens alle ProgressEvery).
	BackgroundProgress(t store.BackgroundTask)
	// BackgroundEnded: Die Aufgabe ist geendet. notify=false: Sie ist gar nicht gestartet, der
	// Agent erfährt das aus dem Werkzeugergebnis.
	BackgroundEnded(t store.BackgroundTask, notify bool)
	// BackgroundLookup: eine Aufgabe, die dieses Register nicht kennt (etwa vor dem Ruhen).
	BackgroundLookup(ctx context.Context, chatID string, seq int) (store.BackgroundTask, error)
}

// Grenzen und Takte.
var (
	// TailBytes: So viel vom Ende der Ausgabe hält das Register (wie pis rollendes Ende, 2 × 50 KiB).
	TailBytes = 2 * execproto.PiMaxBytes
	// ShortTailBytes: so viel vom Ende steht in der Datenbank und in den Ereignissen.
	ShortTailBytes = 4 << 10
	// ProgressEvery: höchstens so oft ein Fortschritt je Aufgabe.
	ProgressEvery = 2 * time.Second
	// StartWait: So lange wartet Start darauf, dass der Befehl läuft.
	StartWait = 15 * time.Second
	// StopWait: So lange wartet Stop auf das Ende.
	StopWait = 15 * time.Second
	// KeepEnded: So viele beendete Aufgaben behält das Register (volles Ende für bg_output); ältere
	// gibt es frei, ihr Stand steht in background_tasks (Review 3, M2).
	KeepEnded = 4
)

// Fehler, die als Werkzeugergebnis beim Modell ankommen, sind englisch (L4).
var (
	ErrUnknown = errors.New("unknown background task")
	ErrLimit   = errors.New("too many background tasks")
)

// StartParams beschreibt einen Start.
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
	tail      ring // rollendes Ende (TailBytes), Ringpuffer statt append und Kopie (Review 3, N3)
	lastByte  byte
	sum       hash.Hash
	head      []byte
	excerpt   ring // Ende für den Auszug (2 KiB)
	stoppedBy string
	lastProg  time.Time
	progTimer *time.Timer
	dirty     bool
}

// Registry: Hintergrundaufgaben eines Platzes. Ein Platz gehört genau einem Chat (Einmalvergabe);
// die Kennung des Chats wird trotzdem bei jedem Zugriff geprüft.
type Registry struct {
	slot string
	run  Runner
	n    Notifier
	max  int

	ctx    context.Context
	cancel context.CancelFunc

	mu       sync.Mutex
	tasks    map[int]*task
	reserved int   // Plätze, deren Start gerade läuft (Grenze atomar, Review 3, N2)
	ended    []int // beendete Aufgaben im Register, älteste zuerst (höchstens KeepEnded)
	wg       sync.WaitGroup
}

func New(slot string, run Runner, n Notifier, max int) *Registry {
	if max <= 0 {
		max = execproto.DefaultBgMax
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Registry{slot: slot, run: run, n: n, max: max, ctx: ctx, cancel: cancel, tasks: map[int]*task{}}
}

// Max ist die Grenze gleichzeitiger Aufgaben.
func (r *Registry) Max() int { return r.max }

// Running zählt laufende Aufgaben.
func (r *Registry) Running() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.runningLocked()
}

// runningLocked: laufende Aufgaben; r.mu ist gesperrt.
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

// Start startet eine Aufgabe und kehrt zurück, sobald der Befehl läuft (oder gar nicht startet).
func (r *Registry) Start(ctx context.Context, p StartParams) (store.BackgroundTask, error) {
	if r.ctx.Err() != nil {
		return store.BackgroundTask{}, errors.New("execution sandbox closed")
	}
	// Platz unter der Sperre reservieren: Prüfen und Belegen sind ein Schritt, auch wenn das
	// Anlegen in der Datenbank dazwischen dauert.
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
		// Startet der Helfer nicht (etwa bei erschöpftem Prozesslimit), nicht ewig warten.
		cancel()
		<-t.done
		s := t.snapshot()
		return s, errors.New("background task not started in time")
	}
}

// Adopted ist ein Vordergrundbefehl, den der Nutzer in eine Hintergrundaufgabe umgewandelt hat: Der
// Befehl läuft weiter in seiner Operation (bash), deren Ausgabe und Ende der Aufrufer hierher leitet.
type Adopted struct {
	Task store.BackgroundTask
	// Write nimmt weitere Ausgabe entgegen; Finish das Ende der Operation (genau einmal).
	Write  func([]byte)
	Finish func(f execproto.Frame, err error)
}

// Adopt übernimmt einen laufenden Befehl als Hintergrundaufgabe. cancel bricht dessen Operation ab
// (bg_stop, Stopp in der UI, Abbau des Platzes); soFar ist die bisherige Ausgabe.
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
	stopWithSlot := context.AfterFunc(r.ctx, cancel) // Abbau des Platzes beendet auch diese Aufgabe
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

// release gibt eine beendete Aufgabe frei, sobald mehr als KeepEnded beendet im Register stehen
// (ihr Stand ist dann in background_tasks; Output und Stop fragen den Manager).
func (r *Registry) release(seq int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ended = append(r.ended, seq)
	for len(r.ended) > KeepEnded {
		delete(r.tasks, r.ended[0])
		r.ended = r.ended[1:]
	}
}

// write sammelt die Ausgabe: Prüfsumme, Anfang, rollendes Ende, Fortschritt gedrosselt. Im
// Dauerbetrieb legt ein Stück nichts neu an (Ringpuffer); den Durchsatz begrenzt agw-exec, das
// nach execproto.BgThrottleAfter nur noch gedrosselt liest (Review 3, N3).
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
			// Nachzügler: Die letzte Ausgabe einer ruhigen Phase kommt nach spätestens ProgressEvery.
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

// Tail hält die letzten Bytes einer Ausgabe (Ringpuffer, im Dauerbetrieb ohne neue Speicherbelegung);
// genutzt für die bisherige Ausgabe eines Vordergrundbefehls, der umgewandelt werden kann.
type Tail struct {
	r    ring
	size int
}

func NewTail(size int) *Tail { return &Tail{size: size} }

func (t *Tail) Write(b []byte) {
	t.r.init(t.size)
	t.r.write(b)
}

// Bytes liefert eine Kopie des gehaltenen Endes.
func (t *Tail) Bytes() []byte {
	if t.r.buf == nil {
		return nil
	}
	return t.r.bytes()
}

// ring hält die letzten n Bytes; write kopiert nur das neue Stück.
type ring struct {
	buf   []byte
	start int // Anfang der Daten
	n     int // Länge der Daten
}

// init legt den Puffer beim ersten Gebrauch an.
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

// last liefert die letzten n Bytes in Reihenfolge (Kopie).
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

// finish bestimmt den Endzustand aus dem letzten Rahmen.
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
		s += fmt.Sprintf("\n… [%d Bytes ausgelassen] …\n", omitted)
		s += string(ex)
	} else if t.t.OutputBytes > int64(len(t.head)) {
		// Anfang und Ende überlappen: nur den Teil nach dem Anfang anhängen.
		rest := t.t.OutputBytes - int64(len(t.head))
		s += string(ex[int64(len(ex))-rest:])
	}
	return strings.ToValidUTF8(s, "�")
}

// snapshot: Stand der Aufgabe; Tail ist das kurze Ende (ShortTailBytes).
func (t *task) snapshot() store.BackgroundTask {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.t
	s.Tail = cleanTail(t.tail.last(ShortTailBytes+4), ShortTailBytes)
	return s
}

// cleanTail: höchstens n Bytes vom Ende, an einer Zeichengrenze beginnend, gültiges UTF-8.
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

// Output liefert Stand und Ende der Ausgabe (bis TailBytes) einer Aufgabe. Kennt das Register
// sie nicht (etwa aus einer früheren Sandbox des Chats), fragt es den Manager.
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

// Stop beendet eine Aufgabe (by: agent oder user) und wartet kurz auf ihr Ende. Ist sie schon
// geendet, liefert Stop nur ihren Stand.
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

// List liefert den Stand aller Aufgaben des Chats in diesem Register.
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

// Close bricht alle Aufgaben ab (der Platz wird abgebaut) und wartet kurz auf ihr Ende.
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

package chat

// Hintergrundaufgaben (bash mit run_in_background). Das Register des Platzes (internal/bgtask)
// führt sie aus und meldet sich hier: Der Manager legt die Zeile an, verteilt Start, Fortschritt
// und Ende als SSE-Ereignis „background“ und benachrichtigt den Agenten beim Ende wie Claude Code:
//
//   - Arbeitet pi gerade (oder ist ein Auftrag unterwegs), kommt die Meldung als Systemeintrag in
//     die Warteschlange und geht mit dem Laufende an pi.
//   - Ist pi untätig, startet der Manager damit einen neuen Durchgang (Weckruf). Jede Übergabe, die
//     nur aus Meldungen besteht, zählt als Weckruf, auch die beim Laufende (deliverQueue): höchstens
//     Options.BgWakesPerHour je Chat und Stunde und Options.AutoTurnsMax in Folge ohne Nutzer.
//     Darüber bleibt die Meldung zurückgehalten in der Warteschlange (wie nach einem Abbruch), und
//     die UI zeigt einen Hinweis (SSE auto_held, hold_reason am Chat).
//   - Beendet der Agent eine Aufgabe selbst (bg_stop), gibt es keine Meldung.
//
// Ruht der Chat oder wird er beendet, sterben die Aufgaben mit der Sandbox. Der Manager markiert
// sie vorher (suspended, closed, lost) und sagt es dem Agenten beim nächsten Auftrag einmal.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"agw/internal/execproto"
	"agw/internal/store"
)

// Vorgaben für Options.BgWakesPerHour, Options.BgKeepAlive und Options.AutoTurnsMax.
const (
	DefaultBgWakesPerHour = 10
	DefaultBgKeepAlive    = time.Hour
	DefaultAutoTurnsMax   = 5
)

// BackgroundAgent: Ein Platz mit Hintergrundaufgaben (worker.Worker).
type BackgroundAgent interface {
	BackgroundList(chatID string) []store.BackgroundTask
	StopBackground(ctx context.Context, chatID, id, by string) (store.BackgroundTask, error)
}

// BackgroundEvent ist das SSE-Ereignis „background“.
type BackgroundEvent struct {
	// Change: started, output (gedrosselt), ended (mit notified_at, sobald die Meldung erzeugt ist).
	Change string               `json:"change"`
	Task   store.BackgroundTask `json:"task"`
}

// ErrNotRunning: Die Hintergrundaufgabe läuft nicht (mehr).
var ErrNotRunning = errors.New("Hintergrundaufgabe läuft nicht")

func (m *Manager) publishBackground(chatID, change string, t store.BackgroundTask) {
	m.publish(chatID, Event{Kind: "background", Data: BackgroundEvent{Change: change, Task: t}})
}

// BackgroundCreate legt eine Aufgabe an (bgtask.Notifier).
func (m *Manager) BackgroundCreate(ctx context.Context, t store.BackgroundTask) (store.BackgroundTask, error) {
	logPath := execproto.BgLogPath
	if t.LogPath != "" {
		// Umgewandelter Vordergrundbefehl (bgtask.Adopt): Seine ganze Ausgabe steht, wenn überhaupt,
		// in der Datei des Vordergrundbefehls.
		fixed := t.LogPath
		logPath = func(int) string { return fixed }
	}
	bt, err := m.st.CreateBackgroundTask(ctx, t, logPath)
	if err != nil {
		return bt, err
	}
	slog.Info("Hintergrundaufgabe gestartet", "chat", bt.ChatID, "aufgabe", bt.ID, "sitzung", bt.Session)
	m.publishBackground(bt.ChatID, "started", bt)
	m.publishChat(context.WithoutCancel(ctx), bt.ChatID)
	return bt, nil
}

// BackgroundProgress verteilt neue Ausgabe (bgtask.Notifier, schon gedrosselt).
func (m *Manager) BackgroundProgress(t store.BackgroundTask) {
	m.publishBackground(t.ChatID, "output", t)
}

// BackgroundLookup liefert eine Aufgabe aus der Datenbank (bgtask.Notifier).
func (m *Manager) BackgroundLookup(ctx context.Context, chatID string, seq int) (store.BackgroundTask, error) {
	return m.st.GetBackgroundTask(ctx, chatID, seq)
}

// BackgroundEnded trägt das Ende ein und benachrichtigt den Agenten (bgtask.Notifier).
func (m *Manager) BackgroundEnded(t store.BackgroundTask, notify bool) {
	ctx := context.Background()
	updated, err := m.st.FinishBackgroundTask(ctx, t)
	if err != nil {
		slog.Error("Ende der Hintergrundaufgabe nicht gespeichert", "chat", t.ChatID, "aufgabe", t.ID, "fehler", err)
	}
	if !updated {
		// Schon beim Ruhen oder Beenden markiert: den gespeicherten Stand verteilen, keine Meldung.
		if row, err := m.st.GetBackgroundTask(ctx, t.ChatID, t.Seq); err == nil {
			m.publishBackground(t.ChatID, "ended", row)
		}
		return
	}
	exit := "–"
	if t.ExitCode != nil {
		exit = fmt.Sprint(*t.ExitCode)
	}
	slog.Info("Hintergrundaufgabe beendet", "chat", t.ChatID, "aufgabe", t.ID, "zustand", t.State, "exit", exit)
	var wake *live
	if notify && !(t.State == store.BgStopped && t.StoppedBy == "agent") {
		wake = m.notifyBackground(ctx, t)
	}
	// Erst nach der Meldung verteilen: Das Ereignis trägt dann notified_at (Review 3, H1 c).
	if row, err := m.st.GetBackgroundTask(ctx, t.ChatID, t.Seq); err == nil {
		t.NotifiedAt, t.Woke, t.NoticePending = row.NotifiedAt, row.Woke, row.NoticePending
	}
	m.publishBackground(t.ChatID, "ended", t)
	m.publishChat(ctx, t.ChatID)
	if wake != nil {
		slog.Info("Weckruf durch Hintergrundaufgabe", "chat", t.ChatID, "aufgabe", t.ID)
		m.deliverQueue(t.ChatID, wake)
	}
}

// notifyBackground reiht die Meldung als Systemeintrag ein. Ist pi untätig und die Warteschlange
// nicht zurückgehalten, liefert es den Platz für einen Weckruf (deliverQueue prüft die Grenzen).
func (m *Manager) notifyBackground(ctx context.Context, t store.BackgroundTask) *live {
	n := BackgroundNote(t)
	unlock := m.queueLock(t.ChatID)
	m.mu.Lock()
	l := m.live[t.ChatID]
	busy := m.sending[t.ChatID] || (l != nil && (l.running || l.compacting))
	held := l == nil || l.holdQueue
	m.mu.Unlock()
	e, err := m.st.EnqueueSystem(ctx, t.ChatID, n.Type, n.Refs, n.Text())
	if err != nil {
		unlock()
		slog.Error("Meldung der Hintergrundaufgabe nicht eingereiht", "chat", t.ChatID, "aufgabe", t.ID, "fehler", err)
		return nil
	}
	_ = m.st.MarkBackgroundNotified(ctx, t.ChatID, t.Seq, false)
	unlock()
	m.publishQueue(ctx, t.ChatID, "queued", []string{e.ID}, "")
	if busy || held {
		return nil
	}
	return l
}

// BackgroundNote ist die Meldung an den Agenten beim Ende einer Aufgabe. Die Kopfzeile bildet der
// Orchestrator aus eigenen Angaben; Befehl, Fehlertext und Ausgabe stammen aus der Sandbox und
// kommen in den Zaun (siehe origin.go).
func BackgroundNote(t store.BackgroundTask) systemNote {
	var h strings.Builder
	h.WriteString("Hintergrundaufgabe ")
	h.WriteString(t.ID)
	if t.Session != "" && t.Session != "main" {
		s := t.Session
		if !safeSession.MatchString(s) {
			s = "?"
		}
		h.WriteString(" (gestartet von Subagent " + s + ")")
	}
	h.WriteString(" " + endPhrase(t))
	if t.EndedAt != nil && !t.StartedAt.IsZero() {
		h.WriteString(", Laufzeit " + FormatRuntime(t.EndedAt.Sub(t.StartedAt)))
	}
	var b strings.Builder
	b.WriteString("Befehl: ")
	b.WriteString(clipRunes(oneLine(t.Command), 200))
	if t.State == store.BgFailed && t.Error != "" {
		b.WriteString("\nFehler: " + clipRunes(oneLine(t.Error), 200))
	}
	lines := lastLines(t.Tail, 10, 1500)
	if lines == "" {
		b.WriteString("\nKeine Ausgabe.")
	} else {
		fmt.Fprintf(&b, "\nLetzte Zeilen (von %d):\n%s", t.OutputLines, lines)
	}
	if t.LogPath != "" && t.OutputBytes > 0 {
		b.WriteString("\nGanze Ausgabe: " + t.LogPath)
	}
	return systemNote{Type: store.NoteBackground, Refs: []string{t.ID}, Summary: h.String(), Body: b.String()}
}

func endPhrase(t store.BackgroundTask) string {
	switch t.State {
	case store.BgExited:
		if t.ExitCode != nil {
			return fmt.Sprintf("beendet: Exit %d", *t.ExitCode)
		}
		return "beendet"
	case store.BgTimeout:
		return "nach der Zeitgrenze abgebrochen"
	case store.BgStopped:
		if t.StoppedBy == "user" {
			return "vom Nutzer gestoppt"
		}
		return "gestoppt"
	case store.BgLost:
		return "abgebrochen: Ausführungs-Sandbox nicht mehr erreichbar"
	default:
		return "fehlgeschlagen" // der Fehlertext steht im Zaun
	}
}

// FormatRuntime: m:ss, ab einer Stunde h:mm:ss.
func FormatRuntime(d time.Duration) string {
	s := int(d.Round(time.Second) / time.Second)
	if s < 0 {
		s = 0
	}
	if s >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", s/3600, s/60%60, s%60)
	}
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func clipRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + " …"
	}
	return s
}

// lastLines: die letzten n Zeilen, höchstens max Bytes (am Anfang gekürzt).
func lastLines(s string, n, max int) string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return ""
	}
	parts := strings.Split(s, "\n")
	if len(parts) > n {
		parts = parts[len(parts)-n:]
	}
	out := strings.Join(parts, "\n")
	if len(out) > max {
		out = "…" + strings.ToValidUTF8(out[len(out)-max:], "")
	}
	return out
}

// endBackground markiert die laufenden Aufgaben eines Chats, bevor seine Sandbox verschwindet.
func (m *Manager) endBackground(ctx context.Context, chatID, state, reason string, notice bool) {
	ended, err := m.st.EndRunningBackground(context.WithoutCancel(ctx), chatID, state, reason, notice)
	if err != nil {
		slog.Warn("Hintergrundaufgaben nicht markiert", "chat", chatID, "fehler", err)
		return
	}
	for _, t := range ended {
		m.publishBackground(chatID, "ended", t)
	}
	if len(ended) > 0 {
		slog.Info("Hintergrundaufgaben mit der Sandbox beendet", "chat", chatID, "anzahl", len(ended), "zustand", state)
	}
}

// backgroundNotice: der einmalige Hinweis auf Aufgaben, die mit einer früheren Sandbox endeten.
func (m *Manager) backgroundNotice(ctx context.Context, chatID string) (*systemNote, []int) {
	notes, err := m.st.BackgroundNotices(ctx, chatID)
	if err != nil || len(notes) == 0 {
		return nil, nil
	}
	var ids, items []string
	var seqs []int
	for _, t := range notes {
		ids = append(ids, t.ID)
		items = append(items, t.ID+": "+clipRunes(oneLine(t.Command), 80))
		seqs = append(seqs, t.Seq)
	}
	return &systemNote{Type: store.NoteSandbox, Refs: ids,
		Summary: "Mit der vorigen Sandbox (Chat ruhte oder Sandbox beendet) sind diese Hintergrundaufgaben beendet worden: " +
			strings.Join(ids, ", ") + ". Bei Bedarf neu starten.",
		Body: strings.Join(items, "\n")}, seqs
}

// BackgroundTasks liefert alle Aufgaben des Chats; laufende mit dem aktuellen Stand des Platzes.
func (m *Manager) BackgroundTasks(ctx context.Context, chatID string) ([]store.BackgroundTask, error) {
	if _, err := m.st.GetChat(ctx, chatID); err != nil {
		return nil, err
	}
	list, err := m.st.ListBackgroundTasks(ctx, chatID)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	l := m.live[chatID]
	m.mu.Unlock()
	if l == nil {
		return list, nil
	}
	ba, ok := l.slot.Worker.(BackgroundAgent)
	if !ok {
		return list, nil
	}
	live := map[int]store.BackgroundTask{}
	for _, t := range ba.BackgroundList(chatID) {
		live[t.Seq] = t
	}
	for i, t := range list {
		if s, ok := live[t.Seq]; ok && t.State == store.BgRunning {
			s.NotifiedAt, s.Woke, s.NoticePending = t.NotifiedAt, t.Woke, t.NoticePending
			list[i] = s
		}
	}
	return list, nil
}

// ForegroundAgent steuert laufende Vordergrundbefehle (bash) eines Platzes.
type ForegroundAgent interface {
	StopForeground(chatID, toolCallID string) error
	BackgroundForeground(ctx context.Context, chatID, toolCallID string) (store.BackgroundTask, error)
	ForegroundRunning(chatID string) []string
}

// ErrNoForeground: kein laufender Vordergrundbefehl mit dieser Kennung (schon beendet).
var ErrNoForeground = errors.New("kein laufender Befehl mit dieser Kennung")

func (m *Manager) foreground(chatID string) (ForegroundAgent, error) {
	m.mu.Lock()
	l := m.live[chatID]
	m.mu.Unlock()
	if l == nil {
		return nil, ErrNoForeground
	}
	fa, ok := l.slot.Worker.(ForegroundAgent)
	if !ok {
		return nil, ErrNoForeground
	}
	return fa, nil
}

// StopTool stoppt einen laufenden bash-Befehl auf Wunsch des Nutzers; der Agent bekommt „Command
// stopped by the user“ als Ergebnis und arbeitet weiter.
func (m *Manager) StopTool(ctx context.Context, chatID, toolCallID string) error {
	fa, err := m.foreground(chatID)
	if err != nil {
		return err
	}
	m.userActive(chatID)
	if err := fa.StopForeground(chatID, toolCallID); err != nil {
		return err
	}
	slog.Info("Befehl vom Nutzer gestoppt", "chat", chatID, "aufruf", toolCallID)
	return nil
}

// BackgroundTool wandelt einen laufenden bash-Befehl in eine Hintergrundaufgabe um: Der Befehl
// läuft weiter, der Agent erfährt die Kennung sofort und wird bei ihrem Ende benachrichtigt.
func (m *Manager) BackgroundTool(ctx context.Context, chatID, toolCallID string) (store.BackgroundTask, error) {
	fa, err := m.foreground(chatID)
	if err != nil {
		return store.BackgroundTask{}, err
	}
	m.userActive(chatID)
	t, err := fa.BackgroundForeground(ctx, chatID, toolCallID)
	if err != nil {
		return t, err
	}
	slog.Info("Befehl vom Nutzer in den Hintergrund verschoben", "chat", chatID, "aufruf", toolCallID, "aufgabe", t.ID)
	return t, nil
}

// RunningTools nennt die laufenden bash-Befehle des Chats (toolCallIds), die sich stoppen oder
// umwandeln lassen.
func (m *Manager) RunningTools(chatID string) []string {
	fa, err := m.foreground(chatID)
	if err != nil {
		return []string{}
	}
	return fa.ForegroundRunning(chatID)
}

// StopBackground beendet eine laufende Aufgabe auf Wunsch des Nutzers; der Agent erfährt es.
func (m *Manager) StopBackground(ctx context.Context, chatID, id string) (store.BackgroundTask, error) {
	seq := store.ParseBgID(id)
	if seq == 0 {
		return store.BackgroundTask{}, fmt.Errorf("%w: Kennung %q", ErrInvalid, id)
	}
	row, err := m.st.GetBackgroundTask(ctx, chatID, seq)
	if err != nil {
		return store.BackgroundTask{}, err
	}
	if row.State != store.BgRunning {
		return row, ErrNotRunning
	}
	m.mu.Lock()
	l := m.live[chatID]
	m.mu.Unlock()
	if l == nil {
		return row, ErrNotRunning
	}
	ba, ok := l.slot.Worker.(BackgroundAgent)
	if !ok {
		return row, ErrNotRunning
	}
	m.userActive(chatID)
	slog.Info("Hintergrundaufgabe vom Nutzer gestoppt", "chat", chatID, "aufgabe", id)
	return ba.StopBackground(ctx, chatID, id, "user")
}

// keepAliveForBackground: Soll der Leerlauf das Ruhen verschieben, weil Aufgaben laufen? Nur bis
// Options.BgKeepAlive nach der letzten Aktion des Nutzers (Review 3, M1: Weckrufe verlängern den
// Aufschub nicht); danach ruht der Chat, und die Aufgaben enden.
func (m *Manager) keepAliveForBackground(chatID string, l *live) (time.Duration, bool) {
	c, err := m.st.GetChat(context.Background(), chatID)
	if err != nil || c.BackgroundRunning == 0 {
		return 0, false
	}
	m.mu.Lock()
	since := m.userAt[chatID]
	if since.IsZero() {
		since = l.activeAt
	}
	m.mu.Unlock()
	left := m.opt.BgKeepAlive - time.Since(since)
	if left <= 0 {
		return 0, false
	}
	return min(left, m.opt.IdleTimeout), true
}

package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"agw/internal/artifacts"
	"agw/internal/store"
)

// Senden und Warteschlange.
//
// Läuft nichts, geht eine Nachricht sofort an pi (ein ruhender Chat wird dabei fortgesetzt).
// Arbeitet pi, wird der Chat gerade fortgesetzt oder ist ein anderer Auftrag unterwegs, reiht der
// Orchestrator sie ein (Postgres, übersteht einen Neustart). Beim nächsten Laufende übergibt er
// alle offenen Einträge gemeinsam als einen Auftrag. Nach einem Abbruch hält er sie zurück; sie
// gehen dann mit der nächsten Nachricht oder über FlushQueue mit.

// promptTimeout: Frist für den Aufruf prompt (Tests verkürzen sie).
var promptTimeout atomic.Int64

func init() { promptTimeout.Store(int64(callTimeout)) }

// ErrQueueDelivered: Der Eintrag ist schon an pi übergeben und lässt sich nicht mehr entfernen.
var ErrQueueDelivered = store.ErrDelivered

// SendResult ist die Antwort auf eine Nachricht.
type SendResult struct {
	Resumed bool   `json:"resumed"`            // in einer frischen Sandbox fortgesetzt
	Queued  bool   `json:"queued,omitempty"`   // eingereiht statt sofort gesendet
	QueueID string `json:"queue_id,omitempty"` // Kennung des Eintrags (bei queued)
}

// QueueEvent ist das SSE-Ereignis „queue“: der neue Stand der Warteschlange und was geschah.
type QueueEvent struct {
	Entries []store.QueueEntry `json:"entries"` // offene Einträge in Reihenfolge
	// Change: queued, removed, delivered (an pi übergeben), restored (Übergabe gescheitert oder
	// Eingeschleustes nicht eingefügt, wieder offen).
	Change string   `json:"change"`
	IDs    []string `json:"ids,omitempty"`
	// Text: bei delivered der Auftrag, wie er an pi geht (Einträge zusammengefasst), mit Herkunft
	// und Teilen (Review 3, H1).
	Text    string         `json:"text,omitempty"`
	Origin  string         `json:"origin,omitempty"`
	Sources []store.Source `json:"sources,omitempty"`
}

// Gründe, aus denen eingereihte Einträge nicht von selbst übergeben werden (ChatView.HoldReason).
const (
	HoldAbort     = "abort"      // der Nutzer hat abgebrochen
	HoldWakeLimit = "wake_limit" // Weckrufe je Stunde erschöpft (BgWakesPerHour)
	HoldAutoTurns = "auto_turns" // zu viele Durchgänge ohne Nutzer in Folge (AutoTurnsMax)
)

// AutoHeldEvent ist das SSE-Ereignis „auto_held“: Meldungen bleiben eingereiht, weil eine Grenze
// für Durchgänge ohne Nutzer erreicht ist; sie gehen mit der nächsten Nachricht oder „Jetzt senden“.
type AutoHeldEvent struct {
	Reason string `json:"reason"` // HoldWakeLimit, HoldAutoTurns
	Limit  int    `json:"limit"`
	Count  int    `json:"count"`
}

func (m *Manager) queueLock(chatID string) func() {
	m.mu.Lock()
	l, ok := m.qMu[chatID]
	if !ok {
		l = &sync.Mutex{}
		m.qMu[chatID] = l
	}
	m.mu.Unlock()
	l.Lock()
	return l.Unlock
}

func (m *Manager) publishQueue(ctx context.Context, chatID, change string, ids []string, text string) {
	m.publishQueueEv(ctx, chatID, QueueEvent{Change: change, IDs: ids, Text: text})
}

func (m *Manager) publishQueueEv(ctx context.Context, chatID string, ev QueueEvent) {
	list, err := m.st.ListQueue(context.WithoutCancel(ctx), chatID)
	if err != nil {
		slog.Warn("Warteschlange nicht lesbar", "chat", chatID, "fehler", err)
		return
	}
	ev.Entries = list
	m.publish(chatID, Event{Kind: "queue", Data: ev})
	m.publishChat(context.WithoutCancel(ctx), chatID)
}

// Queue liefert die offenen Einträge der Warteschlange.
func (m *Manager) Queue(ctx context.Context, chatID string) ([]store.QueueEntry, error) {
	return m.st.ListQueue(ctx, chatID)
}

// checkAttachments prüft, dass jeder Anhang eine vom Nutzer hochgeladene Eingabe dieses Chats
// ist (liegt dann in /workspace/inputs/); doppelte Namen entfallen.
func (m *Manager) checkAttachments(ctx context.Context, chatID string, names []string) ([]string, error) {
	var clean []string
	seen := map[string]bool{}
	for _, n := range names {
		sn := artifacts.SanitizeName(n)
		if sn == "" || sn != n {
			return nil, fmt.Errorf("%w: ungültiger Anhang %q", ErrInvalid, n)
		}
		if seen[sn] {
			continue
		}
		if _, err := m.st.GetArtifact(ctx, chatID, store.KindInput, sn); err != nil {
			return nil, fmt.Errorf("%w: Anhang %q ist keine hochgeladene Datei dieses Chats", ErrInvalid, sn)
		}
		seen[sn] = true
		clean = append(clean, sn)
	}
	return clean, nil
}

// Send schickt eine Nachricht (siehe SendWithAttachments).
func (m *Manager) Send(ctx context.Context, chatID, text string) (SendResult, error) {
	return m.SendWithAttachments(ctx, chatID, text, nil)
}

// SendWithAttachments schickt eine Nachricht mit Anhängen oder reiht sie ein. Ein ruhender Chat
// wird dabei fortgesetzt; zurückgehaltene Einträge der Warteschlange gehen mit.
func (m *Manager) SendWithAttachments(ctx context.Context, chatID, text string, names []string) (SendResult, error) {
	text = strings.TrimSpace(text)
	if text == "" && len(names) == 0 {
		return SendResult{}, errors.New("leere Nachricht")
	}
	clean, err := m.checkAttachments(ctx, chatID, names)
	if err != nil {
		return SendResult{}, err
	}
	r, err := m.send(ctx, chatID, &store.QueueEntry{Text: text, Attachments: clean})
	if err == nil {
		m.autoTitle(context.WithoutCancel(ctx), chatID, text)
	}
	return r, err
}

// FlushQueue übergibt die zurückgehaltenen Einträge jetzt (nach einem Abbruch oder bei ruhendem
// Chat). Arbeitet pi, ErrRunning; ist nichts eingereiht, ErrInvalid.
func (m *Manager) FlushQueue(ctx context.Context, chatID string) (SendResult, error) {
	return m.send(ctx, chatID, nil)
}

// Unqueue entfernt einen offenen Eintrag; ErrQueueDelivered, wenn er schon übergeben ist.
func (m *Manager) Unqueue(ctx context.Context, chatID, id string) error {
	m.userActive(chatID)
	unlock := m.queueLock(chatID)
	err := m.st.RemoveQueued(ctx, chatID, id)
	unlock()
	if err != nil {
		return err
	}
	m.publishQueue(ctx, chatID, "removed", []string{id}, "")
	return nil
}

func (m *Manager) send(ctx context.Context, chatID string, msg *store.QueueEntry) (SendResult, error) {
	m.userActive(chatID)
	unlock := m.queueLock(chatID)
	_, err := m.st.GetChat(ctx, chatID)
	if err != nil {
		unlock()
		return SendResult{}, err
	}
	m.mu.Lock()
	l := m.live[chatID]
	busy := m.sending[chatID] || (l != nil && (l.running || l.compacting))
	gen := m.aborts[chatID]
	m.mu.Unlock()
	if busy {
		if msg == nil {
			unlock()
			return SendResult{}, ErrRunning
		}
		e, err := m.st.Enqueue(ctx, chatID, msg.Text, msg.Attachments)
		unlock()
		if err != nil {
			return SendResult{}, err
		}
		slog.Info("Nachricht eingereiht", "chat", chatID, "eintrag", e.ID)
		m.publishQueue(ctx, chatID, "queued", []string{e.ID}, "")
		if l != nil {
			// Läuft gerade ein Werkzeug, gleich einschleusen; sonst beim nächsten Werkzeugstart
			// oder am Ende des Durchgangs.
			go m.steerQueue(chatID, l)
		}
		return SendResult{Queued: true, QueueID: e.ID}, nil
	}
	// Nichts läuft: zurückgehaltene Einträge gehen zusammen mit dieser Nachricht.
	held, err := m.st.ClaimQueue(ctx, chatID)
	if err != nil {
		unlock()
		return SendResult{}, err
	}
	entries := held
	if msg != nil {
		entries = append(entries, *msg)
	}
	if len(entries) == 0 {
		unlock()
		return SendResult{}, fmt.Errorf("%w: keine eingereihten Nachrichten", ErrInvalid)
	}
	m.mu.Lock()
	m.sending[chatID] = true
	m.mu.Unlock()
	unlock()
	return m.deliver(ctx, chatID, entries, ids(held), store.TriggerUser, gen)
}

// turnMeta begleitet einen Auftrag an pi, bis pi die Nutzernachricht dazu meldet (handle ordnet
// sie zu und speichert sie mit Herkunft und Durchgang).
type turnMeta struct {
	id      int64
	trigger string
	origin  string
	sources []store.Source
	text    string
	at      time.Time

	// steered: während eines Laufs eingeschleust (prompt mit streamingBehavior steer); claimed:
	// die dabei übergebenen Einträge der Warteschlange.
	steered bool
	claimed []string

	mu       sync.Mutex
	consumed bool // pi hat die Nutzernachricht gemeldet (der Auftrag ist angenommen)
}

func (t *turnMeta) markConsumed() {
	t.mu.Lock()
	t.consumed = true
	t.mu.Unlock()
}

func (t *turnMeta) isConsumed() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.consumed
}

// errAbortedBeforePrompt: Der Nutzer hat abgebrochen, während der Auftrag unterwegs war (etwa beim
// Fortsetzen); der Auftrag geht nicht an pi, sondern bleibt zurückgehalten eingereiht.
var errAbortedBeforePrompt = errors.New("vor der Übergabe abgebrochen")

// deliver schickt die Einträge als einen Auftrag an pi; claimed sind die Kennungen der dabei
// übergebenen Einträge der Warteschlange (bei einem Fehler wieder offen). trigger: store.Trigger*;
// gen: Stand der Abbrüche beim Entschluss zu senden. Der Aufrufer hat sending gesetzt; deliver nimmt
// es zurück.
func (m *Manager) deliver(ctx context.Context, chatID string, entries []store.QueueEntry, claimed []string, trigger string, gen uint64) (SendResult, error) {
	// Einmalige Hinweise: bevorzugte Sprache (nur beim ersten Durchgang des Chats; scheitert er, wird
	// er zurückgenommen und der nächste Auftrag ist wieder der erste) und Hintergrundaufgaben, die mit
	// einer früheren Sandbox endeten.
	lang := m.firstTurnLanguage(ctx, chatID)
	notice, noticed := m.backgroundNotice(ctx, chatID)
	c := composeMessage(entries, lang, notice)
	tm := &turnMeta{trigger: trigger, origin: c.Origin, sources: c.Sources, text: c.Text, claimed: claimed}
	var err error
	if tm.id, err = m.st.CreateTurn(ctx, chatID, trigger, c.Origin, c.Sources, claimed); err != nil {
		slog.Warn("Durchgang nicht angelegt", "chat", chatID, "fehler", err)
	}
	if len(claimed) > 0 {
		m.publishQueueEv(ctx, chatID, QueueEvent{Change: "delivered", IDs: claimed, Text: c.Text, Origin: c.Origin, Sources: c.Sources})
	}
	res, err := m.dispatch(ctx, chatID, tm, gen)
	if err == nil {
		if len(noticed) > 0 {
			if cerr := m.st.ClearBackgroundNotices(context.WithoutCancel(ctx), chatID, noticed); cerr != nil {
				slog.Warn("Hinweis auf Hintergrundaufgaben nicht zurückgesetzt", "chat", chatID, "fehler", cerr)
			}
		}
		if trigger == store.TriggerWake {
			m.markWoke(context.WithoutCancel(ctx), chatID, c.Sources)
		}
	} else if tm.id > 0 {
		_ = m.st.DeleteTurn(context.WithoutCancel(ctx), tm.id)
	}
	m.mu.Lock()
	delete(m.sending, chatID)
	m.mu.Unlock()
	if errors.Is(err, errAbortedBeforePrompt) {
		return m.holdAfterAbort(ctx, chatID, entries, claimed, res)
	}
	if err != nil && len(claimed) > 0 {
		if uerr := m.st.UnclaimQueue(context.WithoutCancel(ctx), claimed); uerr != nil {
			slog.Error("Warteschlange nicht zurückgenommen", "chat", chatID, "fehler", uerr)
		}
		m.publishQueue(ctx, chatID, "restored", claimed, "")
	}
	if len(claimed) > 0 {
		m.publishChat(context.WithoutCancel(ctx), chatID) // queue_held neu berechnen
	}
	return res, err
}

// holdAfterAbort: Der Auftrag ging wegen eines Abbruchs nicht an pi. Übergebene Einträge werden
// wieder offen, eine neue Nachricht des Nutzers wird eingereiht; alles bleibt zurückgehalten.
func (m *Manager) holdAfterAbort(ctx context.Context, chatID string, entries []store.QueueEntry, claimed []string, res SendResult) (SendResult, error) {
	ctx = context.WithoutCancel(ctx)
	if len(claimed) > 0 {
		if err := m.st.UnclaimQueue(ctx, claimed); err != nil {
			slog.Error("Warteschlange nicht zurückgenommen", "chat", chatID, "fehler", err)
		}
	}
	res.Queued = len(claimed) > 0
	isClaimed := map[string]bool{}
	for _, id := range claimed {
		isClaimed[id] = true
	}
	for _, e := range entries {
		if e.ID != "" && isClaimed[e.ID] {
			continue
		}
		q, err := m.st.EnqueueKind(ctx, chatID, e.Kind, e.Text, e.Attachments)
		if err != nil {
			return res, err
		}
		res.Queued, res.QueueID = true, q.ID
	}
	slog.Info("Abbruch vor der Übergabe: Auftrag bleibt eingereiht", "chat", chatID)
	m.publishQueue(ctx, chatID, "restored", claimed, "")
	return res, nil
}

// markWoke hält an den Hintergrundaufgaben fest, dass ihre Meldung einen Weckruf ausgelöst hat.
func (m *Manager) markWoke(ctx context.Context, chatID string, sources []store.Source) {
	for _, s := range sources {
		if s.Kind != store.QueueSystem || s.Type != store.NoteBackground {
			continue
		}
		for _, r := range s.Refs {
			if seq := store.ParseBgID(r); seq > 0 {
				_ = m.st.MarkBackgroundWoke(ctx, chatID, seq)
			}
		}
	}
}

func ids(es []store.QueueEntry) []string {
	out := make([]string, 0, len(es))
	for _, e := range es {
		out = append(out, e.ID)
	}
	return out
}

// dispatch setzt den Chat bei Bedarf fort und schickt den Auftrag per prompt an pi.
func (m *Manager) dispatch(ctx context.Context, chatID string, tm *turnMeta, gen uint64) (SendResult, error) {
	l, resumed, err := m.ensureLive(ctx, chatID)
	if err != nil {
		return SendResult{}, err
	}
	m.mu.Lock()
	if m.live[chatID] != l { // zwischendurch ruhen gelassen (Leerlauf): erneut fortsetzen
		m.mu.Unlock()
		var again bool
		if l, again, err = m.ensureLive(ctx, chatID); err != nil {
			return SendResult{Resumed: resumed}, err
		}
		resumed = resumed || again
		m.mu.Lock()
	}
	if m.aborts[chatID] != gen {
		// Abgebrochen, während der Auftrag unterwegs war: nicht mehr an pi (Review 3, N5).
		l.holdQueue, l.holdReason = true, HoldAbort
		m.mu.Unlock()
		return SendResult{Resumed: resumed}, errAbortedBeforePrompt
	}
	running := l.running
	if !running {
		l.running = true // bis agent_start eintrifft, gilt der Chat als laufend
		l.runningSince = time.Now()
	}
	tm.steered = running
	if tm.trigger != store.TriggerWake {
		// Eine Nachricht des Nutzers hebt das Zurückhalten auf (das Zurückgehaltene geht mit).
		l.holdQueue, l.holdReason = false, ""
	}
	tm.at = time.Now()
	l.pendingTurns = append(l.pendingTurns, tm)
	if len(l.pendingTurns) > 16 {
		l.pendingTurns = l.pendingTurns[len(l.pendingTurns)-16:]
	}
	m.mu.Unlock()
	cmd := map[string]any{"type": "prompt", "message": tm.text}
	if running {
		// Einschleusen (steerQueue): pi fügt den Auftrag nach den laufenden Werkzeugen ein, vor
		// dem nächsten Modellaufruf.
		cmd["streamingBehavior"] = "steer"
	} else {
		l.slot.SetActivity("thinking", "")
	}
	if _, err := callT(l.slot.Worker, cmd, time.Duration(promptTimeout.Load())); err != nil {
		if m.acceptedAnyway(l, tm, running) {
			// Zeitlimit, aber pi hat den Auftrag angenommen: nicht zurücknehmen, sonst ginge er
			// ein zweites Mal an pi (Review 3, N5).
			slog.Warn("prompt ohne Antwort, aber von pi angenommen", "chat", chatID, "fehler", err)
		} else {
			m.mu.Lock()
			l.running = running
			for i, p := range l.pendingTurns {
				if p == tm {
					l.pendingTurns = append(l.pendingTurns[:i:i], l.pendingTurns[i+1:]...)
					break
				}
			}
			m.mu.Unlock()
			return SendResult{Resumed: resumed}, err
		}
	}
	m.touchIdle(chatID)
	m.publishChat(ctx, chatID)
	return SendResult{Resumed: resumed}, nil
}

// acceptedAnyway: Hat pi einen Auftrag angenommen, obwohl prompt mit Fehler (etwa Zeitlimit)
// zurückkam? Ja, wenn pi die Nutzernachricht schon gemeldet hat oder (ohne früheren Lauf) arbeitet.
func (m *Manager) acceptedAnyway(l *live, tm *turnMeta, wasRunning bool) bool {
	if tm.isConsumed() {
		return true
	}
	if wasRunning {
		return false
	}
	resp, err := callT(l.slot.Worker, map[string]any{"type": "get_state"}, 5*time.Second)
	if err != nil {
		return false
	}
	var st struct {
		IsStreaming bool `json:"isStreaming"`
	}
	return json.Unmarshal(resp.Data, &st) == nil && st.IsStreaming || tm.isConsumed()
}

// takeTurn ordnet die von pi gemeldete Nutzernachricht ihrem Auftrag zu: bevorzugt der mit genau
// diesem Text, sonst der älteste (pi kann den Text verändern, etwa bei Skills).
func (m *Manager) takeTurn(l *live, text string) *turnMeta {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(l.pendingTurns) == 0 {
		return nil
	}
	idx := 0
	for i, p := range l.pendingTurns {
		if p.text == text {
			idx = i
			break
		}
	}
	tm := l.pendingTurns[idx]
	l.pendingTurns = append(l.pendingTurns[:idx:idx], l.pendingTurns[idx+1:]...)
	l.turn = tm
	tm.markConsumed()
	return tm
}

// deliverQueue übergibt nach einem Laufende (oder als Weckruf) alle offenen Einträge als nächsten
// Auftrag, sofern der Nutzer nicht abgebrochen hat und nichts anderes unterwegs ist. Besteht die
// Übergabe nur aus Meldungen des Orchestrators, ist sie ein Weckruf und unterliegt dessen Grenzen
// (Review 3, H2): BgWakesPerHour je Stunde und AutoTurnsMax in Folge ohne Nutzer.
func (m *Manager) deliverQueue(chatID string, l *live) {
	ctx := context.Background()
	unlock := m.queueLock(chatID)
	m.mu.Lock()
	skip := m.sending[chatID] || l.running || l.compacting || l.holdQueue || m.live[chatID] != l
	gen := m.aborts[chatID]
	m.mu.Unlock()
	if skip {
		unlock()
		return
	}
	open, err := m.st.ListQueue(ctx, chatID)
	if err != nil || len(open) == 0 {
		unlock()
		if err != nil {
			slog.Warn("Warteschlange nicht lesbar", "chat", chatID, "fehler", err)
		}
		return
	}
	trigger := store.TriggerWake
	for _, e := range open {
		if e.Kind != store.QueueSystem {
			trigger = store.TriggerQueue
			break
		}
	}
	if trigger == store.TriggerWake {
		if ev, limited := m.autoLimit(ctx, chatID); limited {
			m.mu.Lock()
			l.holdQueue, l.holdReason = true, ev.Reason
			m.mu.Unlock()
			unlock()
			slog.Warn("Grenze für Durchgänge ohne Nutzer erreicht, Meldungen bleiben eingereiht", "chat", chatID, "grund", ev.Reason, "grenze", ev.Limit)
			m.publish(chatID, Event{Kind: "auto_held", Data: ev})
			m.publishChat(ctx, chatID)
			return
		}
	}
	entries, err := m.st.ClaimQueue(ctx, chatID)
	if err != nil || len(entries) == 0 {
		unlock()
		if err != nil {
			slog.Warn("Warteschlange nicht lesbar", "chat", chatID, "fehler", err)
		}
		return
	}
	m.mu.Lock()
	m.sending[chatID] = true
	m.mu.Unlock()
	unlock()
	slog.Info("Warteschlange übergeben", "chat", chatID, "einträge", len(entries), "auslöser", trigger)
	if _, err := m.deliver(ctx, chatID, entries, ids(entries), trigger, gen); err != nil {
		m.publish(chatID, Event{Kind: "error", Data: map[string]string{"message": "Eingereihte Nachrichten nicht übergeben (" + err.Error() + "). Sie bleiben eingereiht."}})
	}
}

// steerQueue schleust offene Einträge in den laufenden Durchgang ein, während ein Werkzeug läuft:
// pi fügt sie nach den laufenden Werkzeugen ein, vor dem nächsten Modellaufruf (wie Claude Code).
// Solange das Modell nur schreibt, bleiben sie eingereiht (und entfernbar); endet der Durchgang
// ohne weiteres Werkzeug, übergibt deliverQueue sie wie bisher.
func (m *Manager) steerQueue(chatID string, l *live) {
	ctx := context.Background()
	unlock := m.queueLock(chatID)
	m.mu.Lock()
	ok := l.running && l.toolsRunning > 0 && !l.compacting && !l.holdQueue && !m.sending[chatID] && m.live[chatID] == l
	gen := m.aborts[chatID]
	m.mu.Unlock()
	if !ok {
		unlock()
		return
	}
	// Nur mit einer Nachricht des Nutzers: Reine Meldungen des Orchestrators (Ende einer
	// Hintergrundaufgabe) gehen wie bisher über deliverQueue, das sie als Weckruf zählt und die
	// Grenzen für Durchgänge ohne Nutzer anwendet (Review 3, H2).
	open, err := m.st.ListQueue(ctx, chatID)
	user := false
	for _, e := range open {
		if e.Kind != store.QueueSystem {
			user = true
			break
		}
	}
	if err != nil || !user {
		unlock()
		return
	}
	entries, err := m.st.ClaimQueue(ctx, chatID)
	if err != nil || len(entries) == 0 {
		unlock()
		if err != nil {
			slog.Warn("Warteschlange nicht lesbar", "chat", chatID, "fehler", err)
		}
		return
	}
	m.mu.Lock()
	m.sending[chatID] = true
	m.mu.Unlock()
	unlock()
	slog.Info("Warteschlange eingeschleust", "chat", chatID, "einträge", len(entries))
	if _, err := m.deliver(ctx, chatID, entries, ids(entries), store.TriggerQueue, gen); err != nil {
		m.publish(chatID, Event{Kind: "error", Data: map[string]string{"message": "Eingereihte Nachrichten nicht eingeschleust (" + err.Error() + "). Sie bleiben eingereiht."}})
	}
}

// reclaimSteered holt eingeschleuste Aufträge, die pi noch nicht eingefügt hat, mit clear_queue aus
// pi zurück und öffnet ihre Einträge wieder (vor einem Abbruch und nach dem Laufende). Hat pi nichts
// mehr in der Warteschlange, aber einen neuen Lauf begonnen, war der Auftrag zu spät für den alten
// Lauf und startet den neuen; dann bleibt er, wie er ist, sonst käme er doppelt an.
func (m *Manager) reclaimSteered(ctx context.Context, chatID string, l *live) {
	m.mu.Lock()
	has := false
	for _, p := range l.pendingTurns {
		if p.steered && !p.isConsumed() {
			has = true
			break
		}
	}
	m.mu.Unlock()
	if !has {
		return
	}
	resp, err := callT(l.slot.Worker, map[string]any{"type": "clear_queue"}, callTimeout)
	if err != nil {
		slog.Warn("Warteschlange von pi nicht geleert", "chat", chatID, "fehler", err)
		return
	}
	var d struct {
		Steering []string `json:"steering"`
	}
	_ = json.Unmarshal(resp.Data, &d)
	if len(d.Steering) == 0 {
		if st, err := callT(l.slot.Worker, map[string]any{"type": "get_state"}, callTimeout); err == nil {
			var s struct {
				IsStreaming bool `json:"isStreaming"`
			}
			if json.Unmarshal(st.Data, &s) == nil && s.IsStreaming {
				return
			}
		}
	}
	var lost []*turnMeta
	m.mu.Lock()
	keep := l.pendingTurns[:0]
	for _, p := range l.pendingTurns {
		if p.steered && !p.isConsumed() {
			lost = append(lost, p)
		} else {
			keep = append(keep, p)
		}
	}
	l.pendingTurns = keep
	m.mu.Unlock()
	m.restoreSteered(ctx, chatID, lost)
}

// restoreSteered öffnet die Einträge eingeschleuster Aufträge wieder, die pi nicht eingefügt hat,
// und verwirft deren Durchgang.
func (m *Manager) restoreSteered(ctx context.Context, chatID string, lost []*turnMeta) {
	for _, tm := range lost {
		ctx := context.WithoutCancel(ctx)
		if len(tm.claimed) > 0 {
			if err := m.st.UnclaimQueue(ctx, tm.claimed); err != nil {
				slog.Error("Eingeschleuste Einträge nicht zurückgenommen", "chat", chatID, "fehler", err)
				continue
			}
		}
		if tm.id > 0 {
			_ = m.st.DeleteTurn(ctx, tm.id)
		}
		slog.Info("Eingeschleuster Auftrag nicht eingefügt, wieder eingereiht", "chat", chatID, "einträge", len(tm.claimed))
		m.publishQueue(ctx, chatID, "restored", tm.claimed, "")
	}
}

// autoLimitTurns prüft nur die Grenze für Durchgänge ohne Nutzer in Folge (AutoTurnsMax); für
// Durchgänge, die pi selbst beginnt (die Weckrufe je Stunde zählen Hintergrundaufgaben).
func (m *Manager) autoLimitTurns(ctx context.Context, chatID string) (AutoHeldEvent, bool) {
	k, err := m.st.AutoTurnsInRow(ctx, chatID)
	if err != nil || k >= m.opt.AutoTurnsMax {
		return AutoHeldEvent{Reason: HoldAutoTurns, Limit: m.opt.AutoTurnsMax, Count: k}, true
	}
	return AutoHeldEvent{}, false
}

// autoLimit prüft die Grenzen für einen Durchgang ohne Nutzer. Fehler beim Zählen gelten als
// erreichte Grenze (lieber anhalten als unbegrenzt weiterlaufen).
func (m *Manager) autoLimit(ctx context.Context, chatID string) (AutoHeldEvent, bool) {
	n, err := m.st.WakesSince(ctx, chatID, time.Now().Add(-time.Hour))
	if err != nil || n >= m.opt.BgWakesPerHour {
		return AutoHeldEvent{Reason: HoldWakeLimit, Limit: m.opt.BgWakesPerHour, Count: n}, true
	}
	k, err := m.st.AutoTurnsInRow(ctx, chatID)
	if err != nil || k >= m.opt.AutoTurnsMax {
		return AutoHeldEvent{Reason: HoldAutoTurns, Limit: m.opt.AutoTurnsMax, Count: k}, true
	}
	return AutoHeldEvent{}, false
}

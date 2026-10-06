package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"agw/internal/artifacts"
	"agw/internal/store"
)

// Sending and queue.
//
// If nothing is running, a message goes to pi right away (an idle chat is resumed on the way).
// If pi is working, the chat is being resumed or another message is on its way, the orchestrator
// enqueues it (Postgres, survives a restart). At the end of the next run it delivers all open
// entries together as one message. After an abort it holds them back; they then go along with the
// next message or via FlushQueue.

// promptTimeout: deadline for the prompt call (tests shorten it).
var promptTimeout atomic.Int64

func init() { promptTimeout.Store(int64(callTimeout)) }

// ErrQueueDelivered: the entry has already been delivered to pi and can no longer be removed.
var ErrQueueDelivered = store.ErrDelivered

// SendResult is the response to a message.
type SendResult struct {
	Resumed bool   `json:"resumed"`            // resumed in a fresh sandbox
	Queued  bool   `json:"queued,omitempty"`   // enqueued instead of sent right away
	QueueID string `json:"queue_id,omitempty"` // ID of the entry (when queued)
}

// QueueEvent is the SSE event "queue": the new state of the queue and what happened.
type QueueEvent struct {
	Entries []store.QueueEntry `json:"entries"` // open entries in order
	// Change: queued, removed, delivered (handed to pi), restored (delivery failed or steered entries
	// not inserted, open again).
	Change string   `json:"change"`
	IDs    []string `json:"ids,omitempty"`
	// Text: for delivered, the message as it goes to pi (entries combined), with origin and parts
	// (Review 3, H1).
	Text    string         `json:"text,omitempty"`
	Origin  string         `json:"origin,omitempty"`
	Sources []store.Source `json:"sources,omitempty"`
}

// Reasons why queued entries are not delivered automatically (ChatView.HoldReason).
const (
	HoldAbort     = "abort"      // the user aborted
	HoldWakeLimit = "wake_limit" // wake-ups per hour used up (BgWakesPerHour)
	HoldAutoTurns = "auto_turns" // too many turns in a row without the user (AutoTurnsMax)
)

// AutoHeldEvent is the SSE event "auto_held": notes stay queued because a limit for turns without
// the user has been reached; they go with the next message or "Send now".
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
		slog.Warn("queue not readable", "chat", chatID, "err", err)
		return
	}
	ev.Entries = list
	m.publish(chatID, Event{Kind: "queue", Data: ev})
	m.publishChat(context.WithoutCancel(ctx), chatID)
}

// Queue returns the open entries of the queue.
func (m *Manager) Queue(ctx context.Context, chatID string) ([]store.QueueEntry, error) {
	return m.st.ListQueue(ctx, chatID)
}

// QueueDelivery: queue entries handed to pi together as one message whose user message pi has not
// reported (and the orchestrator not stored) yet, e.g. steered in while a tool runs. Same content as
// the SSE event "queue" with change "delivered", so that a UI can rebuild its state after a reload.
type QueueDelivery struct {
	IDs         []string           `json:"ids"`
	Entries     []store.QueueEntry `json:"entries"` // the entries as they were queued, in order
	Text        string             `json:"text"`    // the message as it went to pi
	Origin      string             `json:"origin,omitempty"`
	Sources     []store.Source     `json:"sources,omitempty"`
	DeliveredAt time.Time          `json:"delivered_at"`
	Steered     bool               `json:"steered"` // steered into a running turn (read after the current step)
}

// DeliveredQueue returns the deliveries of queue entries that pi has not read yet, oldest first.
// The state lives only in memory, next to pi's process: after a restart of the orchestrator pi is
// gone too, and the list is empty.
func (m *Manager) DeliveredQueue(chatID string) []QueueDelivery {
	m.mu.Lock()
	tms := slices.Clone(m.delivering[chatID])
	if l := m.live[chatID]; l != nil {
		for _, p := range l.pendingTurns {
			if !slices.Contains(tms, p) {
				tms = append(tms, p)
			}
		}
	}
	out := []QueueDelivery{}
	for _, tm := range tms {
		if len(tm.claimed) == 0 || tm.isConsumed() {
			continue
		}
		// steered is written under m.mu (dispatch); the other fields do not change after deliver
		out = append(out, QueueDelivery{IDs: tm.claimed, Entries: tm.entries, Text: tm.text, Origin: tm.origin,
			Sources: tm.sources, DeliveredAt: tm.deliveredAt, Steered: tm.steered})
	}
	m.mu.Unlock()
	slices.SortStableFunc(out, func(a, b QueueDelivery) int { return a.DeliveredAt.Compare(b.DeliveredAt) })
	return out
}

// checkAttachments checks that every attachment is an input of this chat uploaded by the user
// (it then lives in /workspace/inputs/); duplicate names are dropped.
func (m *Manager) checkAttachments(ctx context.Context, chatID string, names []string) ([]string, error) {
	var clean []string
	seen := map[string]bool{}
	for _, n := range names {
		sn := artifacts.SanitizeName(n)
		if sn == "" || sn != n {
			return nil, fmt.Errorf("%w: invalid attachment %q", ErrInvalid, n)
		}
		if seen[sn] {
			continue
		}
		if _, err := m.st.GetArtifact(ctx, chatID, store.KindInput, sn); err != nil {
			return nil, fmt.Errorf("%w: attachment %q is not an uploaded file of this chat", ErrInvalid, sn)
		}
		seen[sn] = true
		clean = append(clean, sn)
	}
	return clean, nil
}

// Send sends a message (see SendWithAttachments).
func (m *Manager) Send(ctx context.Context, chatID, text string) (SendResult, error) {
	return m.SendWithAttachments(ctx, chatID, text, nil)
}

// SendWithAttachments sends a message with attachments or enqueues it. An idle chat is resumed on
// the way; held entries of the queue go along.
func (m *Manager) SendWithAttachments(ctx context.Context, chatID, text string, names []string) (SendResult, error) {
	text = strings.TrimSpace(text)
	if text == "" && len(names) == 0 {
		return SendResult{}, errors.New("empty message")
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

// FlushQueue delivers the held entries now (after an abort or when the chat is idle). If pi is
// working, ErrRunning; if nothing is queued, ErrInvalid.
func (m *Manager) FlushQueue(ctx context.Context, chatID string) (SendResult, error) {
	return m.send(ctx, chatID, nil)
}

// Unqueue removes an open entry; ErrQueueDelivered if it has already been delivered.
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
		slog.Info("message enqueued", "chat", chatID, "entry", e.ID)
		m.publishQueue(ctx, chatID, "queued", []string{e.ID}, "")
		if l != nil {
			// If a tool is running, steer it in right away; otherwise at the next tool start or at
			// the end of the turn.
			go m.steerQueue(chatID, l)
		}
		return SendResult{Queued: true, QueueID: e.ID}, nil
	}
	// Nothing is running: held entries go together with this message.
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
		return SendResult{}, fmt.Errorf("%w: no queued messages", ErrInvalid)
	}
	m.mu.Lock()
	m.sending[chatID] = true
	m.mu.Unlock()
	unlock()
	return m.deliver(ctx, chatID, entries, ids(held), store.TriggerUser, gen)
}

// turnMeta accompanies a message to pi until pi reports the matching user message (handle assigns
// it and stores it with origin and turn).
type turnMeta struct {
	id      int64
	trigger string
	origin  string
	sources []store.Source
	text    string
	at      time.Time

	// steered: steered in during a run (prompt with streamingBehavior steer); claimed: the queue
	// entries delivered with it.
	steered bool
	claimed []string
	// entries: the queue entries named in claimed, as they were queued; deliveredAt: claimed and
	// published as "delivered" (DeliveredQueue).
	entries     []store.QueueEntry
	deliveredAt time.Time

	mu       sync.Mutex
	consumed bool // pi has reported the user message (the message has been accepted)
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

// errAbortedBeforePrompt: the user aborted while the message was on its way (e.g. while resuming);
// the message does not go to pi but stays queued and held.
var errAbortedBeforePrompt = errors.New("aborted before delivery")

// deliver sends the entries to pi as one message; claimed are the IDs of the queue entries
// delivered with it (open again on an error). trigger: store.Trigger*; gen: abort counter at the
// moment of deciding to send. The caller has set sending; deliver resets it.
func (m *Manager) deliver(ctx context.Context, chatID string, entries []store.QueueEntry, claimed []string, trigger string, gen uint64) (SendResult, error) {
	// One-off notices: preferred language (only on the chat's first turn; if it fails, it is rolled
	// back and the next message is the first again) and background tasks that ended with an earlier
	// sandbox.
	lang := m.firstTurnLanguage(ctx, chatID)
	notice, noticed := m.backgroundNotice(ctx, chatID)
	c := composeMessage(entries, lang, notice)
	tm := &turnMeta{trigger: trigger, origin: c.Origin, sources: c.Sources, text: c.Text, claimed: claimed,
		entries: claimedEntries(entries, claimed), deliveredAt: time.Now()}
	var err error
	if tm.id, err = m.st.CreateTurn(ctx, chatID, trigger, c.Origin, c.Sources, claimed); err != nil {
		slog.Warn("turn not created", "chat", chatID, "err", err)
	}
	if len(claimed) > 0 {
		// Visible in DeliveredQueue until dispatch has added it to pendingTurns (or failed).
		m.mu.Lock()
		m.delivering[chatID] = append(m.delivering[chatID], tm)
		m.mu.Unlock()
		m.publishQueueEv(ctx, chatID, QueueEvent{Change: "delivered", IDs: claimed, Text: c.Text, Origin: c.Origin, Sources: c.Sources})
	}
	res, err := m.dispatch(ctx, chatID, tm, gen)
	if err == nil {
		if len(noticed) > 0 {
			if cerr := m.st.ClearBackgroundNotices(context.WithoutCancel(ctx), chatID, noticed); cerr != nil {
				slog.Warn("background task notice not reset", "chat", chatID, "err", cerr)
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
	if d := slices.DeleteFunc(m.delivering[chatID], func(p *turnMeta) bool { return p == tm }); len(d) > 0 {
		m.delivering[chatID] = d
	} else {
		delete(m.delivering, chatID)
	}
	m.mu.Unlock()
	if errors.Is(err, errAbortedBeforePrompt) {
		return m.holdAfterAbort(ctx, chatID, entries, claimed, res)
	}
	if err != nil && len(claimed) > 0 {
		if uerr := m.st.UnclaimQueue(context.WithoutCancel(ctx), claimed); uerr != nil {
			slog.Error("queue not restored", "chat", chatID, "err", uerr)
		}
		m.publishQueue(ctx, chatID, "restored", claimed, "")
	}
	if len(claimed) > 0 {
		m.publishChat(context.WithoutCancel(ctx), chatID) // recompute queue_held
	}
	return res, err
}

// holdAfterAbort: the message did not go to pi because of an abort. Delivered entries become open
// again, a new message from the user is enqueued; everything stays held.
func (m *Manager) holdAfterAbort(ctx context.Context, chatID string, entries []store.QueueEntry, claimed []string, res SendResult) (SendResult, error) {
	ctx = context.WithoutCancel(ctx)
	if len(claimed) > 0 {
		if err := m.st.UnclaimQueue(ctx, claimed); err != nil {
			slog.Error("queue not restored", "chat", chatID, "err", err)
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
	slog.Info("abort before delivery: message stays queued", "chat", chatID)
	m.publishQueue(ctx, chatID, "restored", claimed, "")
	return res, nil
}

// markWoke records on the background tasks that their note triggered a wake-up.
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

// claimedEntries returns the entries whose ID is in claimed, in order.
func claimedEntries(es []store.QueueEntry, claimed []string) []store.QueueEntry {
	out := []store.QueueEntry{}
	for _, e := range es {
		if e.ID != "" && slices.Contains(claimed, e.ID) {
			out = append(out, e)
		}
	}
	return out
}

func ids(es []store.QueueEntry) []string {
	out := make([]string, 0, len(es))
	for _, e := range es {
		out = append(out, e.ID)
	}
	return out
}

// dispatch resumes the chat if needed and sends the message to pi via prompt.
func (m *Manager) dispatch(ctx context.Context, chatID string, tm *turnMeta, gen uint64) (SendResult, error) {
	l, resumed, err := m.ensureLive(ctx, chatID)
	if err != nil {
		return SendResult{}, err
	}
	m.mu.Lock()
	if m.live[chatID] != l { // put to idle in the meantime (idle timer): resume again
		m.mu.Unlock()
		var again bool
		if l, again, err = m.ensureLive(ctx, chatID); err != nil {
			return SendResult{Resumed: resumed}, err
		}
		resumed = resumed || again
		m.mu.Lock()
	}
	if m.aborts[chatID] != gen {
		// Aborted while the message was on its way: no longer to pi (Review 3, N5).
		l.holdQueue, l.holdReason = true, HoldAbort
		m.mu.Unlock()
		return SendResult{Resumed: resumed}, errAbortedBeforePrompt
	}
	running := l.running
	if !running {
		l.running = true // until agent_start arrives, the chat counts as running
		l.runningSince = time.Now()
	}
	tm.steered = running
	if tm.trigger != store.TriggerWake {
		// A message from the user lifts the hold (the held entries go along).
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
		// Steering (steerQueue): pi inserts the message after the running tools, before the next
		// model call.
		cmd["streamingBehavior"] = "steer"
	} else {
		l.slot.SetActivity("thinking", "")
	}
	if _, err := callT(l.slot.Worker, cmd, time.Duration(promptTimeout.Load())); err != nil {
		if m.acceptedAnyway(l, tm, running) {
			// Timeout, but pi has accepted the message: do not roll back, or it would go to pi a
			// second time (Review 3, N5).
			slog.Warn("prompt without response, but accepted by pi", "chat", chatID, "err", err)
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

// acceptedAnyway: has pi accepted a message although prompt returned an error (e.g. timeout)? Yes,
// if pi has already reported the user message or (without an earlier run) is working.
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

// takeTurn assigns the user message reported by pi to its message: preferably the one with exactly
// this text, otherwise the oldest (pi can change the text, e.g. with skills).
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

// deliverQueue delivers all open entries as the next message after the end of a run (or as a
// wake-up), provided the user has not aborted and nothing else is on its way. If the delivery
// consists only of orchestrator notes, it is a wake-up and subject to its limits (Review 3, H2):
// BgWakesPerHour per hour and AutoTurnsMax in a row without the user.
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
			slog.Warn("queue not readable", "chat", chatID, "err", err)
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
			slog.Warn("limit for turns without the user reached, notes stay queued", "chat", chatID, "reason", ev.Reason, "limit", ev.Limit)
			m.publish(chatID, Event{Kind: "auto_held", Data: ev})
			m.publishChat(ctx, chatID)
			return
		}
	}
	entries, err := m.st.ClaimQueue(ctx, chatID)
	if err != nil || len(entries) == 0 {
		unlock()
		if err != nil {
			slog.Warn("queue not readable", "chat", chatID, "err", err)
		}
		return
	}
	m.mu.Lock()
	m.sending[chatID] = true
	m.mu.Unlock()
	unlock()
	slog.Info("queue delivered", "chat", chatID, "entries", len(entries), "trigger", trigger)
	if _, err := m.deliver(ctx, chatID, entries, ids(entries), trigger, gen); err != nil {
		m.publish(chatID, Event{Kind: "error", Data: map[string]string{"message": "Queued messages not delivered (" + err.Error() + "). They stay queued."}})
	}
}

// steerQueue steers open entries into the running turn while a tool is running: pi inserts them
// after the running tools, before the next model call (like Claude Code). As long as the model is
// only writing, they stay queued (and removable); if the turn ends without another tool,
// deliverQueue delivers them as before.
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
	// Only with a message from the user: pure orchestrator notes (end of a background task) go
	// through deliverQueue as before, which counts them as a wake-up and applies the limits for
	// turns without the user (Review 3, H2).
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
			slog.Warn("queue not readable", "chat", chatID, "err", err)
		}
		return
	}
	m.mu.Lock()
	m.sending[chatID] = true
	m.mu.Unlock()
	unlock()
	slog.Info("queue steered in", "chat", chatID, "entries", len(entries))
	if _, err := m.deliver(ctx, chatID, entries, ids(entries), store.TriggerQueue, gen); err != nil {
		m.publish(chatID, Event{Kind: "error", Data: map[string]string{"message": "Queued messages not steered in (" + err.Error() + "). They stay queued."}})
	}
}

// reclaimSteered takes steered messages that pi has not inserted yet back from pi with clear_queue
// and reopens their entries (before an abort and after the end of a run). If pi has nothing left in
// its queue but has started a new run, the message came too late for the old run and starts the new
// one; then it stays as it is, otherwise it would arrive twice.
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
		slog.Warn("pi's queue not cleared", "chat", chatID, "err", err)
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

// restoreSteered reopens the entries of steered messages that pi did not insert and discards their
// turn.
func (m *Manager) restoreSteered(ctx context.Context, chatID string, lost []*turnMeta) {
	for _, tm := range lost {
		ctx := context.WithoutCancel(ctx)
		if len(tm.claimed) > 0 {
			if err := m.st.UnclaimQueue(ctx, tm.claimed); err != nil {
				slog.Error("steered entries not restored", "chat", chatID, "err", err)
				continue
			}
		}
		if tm.id > 0 {
			_ = m.st.DeleteTurn(ctx, tm.id)
		}
		slog.Info("steered message not inserted, enqueued again", "chat", chatID, "entries", len(tm.claimed))
		m.publishQueue(ctx, chatID, "restored", tm.claimed, "")
	}
}

// autoLimitTurns checks only the limit for turns in a row without the user (AutoTurnsMax); for
// turns that pi starts itself (the wake-ups per hour count background tasks).
func (m *Manager) autoLimitTurns(ctx context.Context, chatID string) (AutoHeldEvent, bool) {
	k, err := m.st.AutoTurnsInRow(ctx, chatID)
	if err != nil || k >= m.opt.AutoTurnsMax {
		return AutoHeldEvent{Reason: HoldAutoTurns, Limit: m.opt.AutoTurnsMax, Count: k}, true
	}
	return AutoHeldEvent{}, false
}

// autoLimit checks the limits for a turn without the user. Errors while counting count as a
// reached limit (better to stop than to run on without limit).
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

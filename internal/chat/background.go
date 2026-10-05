package chat

// Background tasks (bash with run_in_background). The slot's register (internal/bgtask) runs them
// and reports here: the manager creates the row, distributes start, progress and end as the SSE
// event "background" and notifies the agent at the end, like Claude Code:
//
//   - If pi is working (or a message is on its way), the note goes into the queue as a system
//     entry and reaches pi when the run ends.
//   - If pi is idle, the manager starts a new turn with it (wake-up). Every delivery that consists
//     only of notes counts as a wake-up, including the one at the end of a run (deliverQueue): at
//     most Options.BgWakesPerHour per chat and hour and Options.AutoTurnsMax in a row without the
//     user. Beyond that the note stays held in the queue (as after an abort), and the UI shows a
//     notice (SSE auto_held, hold_reason on the chat).
//   - If the agent ends a task itself (bg_stop), there is no note.
//
// When the chat idles or is closed, the tasks die with the sandbox. The manager marks them
// beforehand (suspended, closed, lost) and tells the agent once with the next message.

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

// Defaults for Options.BgWakesPerHour, Options.BgKeepAlive and Options.AutoTurnsMax.
const (
	DefaultBgWakesPerHour = 10
	DefaultBgKeepAlive    = time.Hour
	DefaultAutoTurnsMax   = 5
)

// BackgroundAgent: a slot with background tasks (worker.Worker).
type BackgroundAgent interface {
	BackgroundList(chatID string) []store.BackgroundTask
	StopBackground(ctx context.Context, chatID, id, by string) (store.BackgroundTask, error)
}

// BackgroundEvent is the SSE event "background".
type BackgroundEvent struct {
	// Change: started, output (throttled), ended (with notified_at once the note has been created).
	Change string               `json:"change"`
	Task   store.BackgroundTask `json:"task"`
}

// ErrNotRunning: the background task is not (or no longer) running.
var ErrNotRunning = errors.New("background task is not running")

func (m *Manager) publishBackground(chatID, change string, t store.BackgroundTask) {
	m.publish(chatID, Event{Kind: "background", Data: BackgroundEvent{Change: change, Task: t}})
}

// BackgroundCreate creates a task (bgtask.Notifier).
func (m *Manager) BackgroundCreate(ctx context.Context, t store.BackgroundTask) (store.BackgroundTask, error) {
	logPath := execproto.BgLogPath
	if t.LogPath != "" {
		// Converted foreground command (bgtask.Adopt): its full output, if any, is in the foreground
		// command's file.
		fixed := t.LogPath
		logPath = func(int) string { return fixed }
	}
	bt, err := m.st.CreateBackgroundTask(ctx, t, logPath)
	if err != nil {
		return bt, err
	}
	slog.Info("background task started", "chat", bt.ChatID, "task", bt.ID, "session", bt.Session)
	m.publishBackground(bt.ChatID, "started", bt)
	m.publishChat(context.WithoutCancel(ctx), bt.ChatID)
	return bt, nil
}

// BackgroundProgress distributes new output (bgtask.Notifier, already throttled).
func (m *Manager) BackgroundProgress(t store.BackgroundTask) {
	m.publishBackground(t.ChatID, "output", t)
}

// BackgroundLookup returns a task from the database (bgtask.Notifier).
func (m *Manager) BackgroundLookup(ctx context.Context, chatID string, seq int) (store.BackgroundTask, error) {
	return m.st.GetBackgroundTask(ctx, chatID, seq)
}

// BackgroundEnded records the end and notifies the agent (bgtask.Notifier).
func (m *Manager) BackgroundEnded(t store.BackgroundTask, notify bool) {
	ctx := context.Background()
	updated, err := m.st.FinishBackgroundTask(ctx, t)
	if err != nil {
		slog.Error("end of background task not stored", "chat", t.ChatID, "task", t.ID, "err", err)
	}
	if !updated {
		// Already marked when idling or closing: distribute the stored state, no note.
		if row, err := m.st.GetBackgroundTask(ctx, t.ChatID, t.Seq); err == nil {
			m.publishBackground(t.ChatID, "ended", row)
		}
		return
	}
	exit := "–"
	if t.ExitCode != nil {
		exit = fmt.Sprint(*t.ExitCode)
	}
	slog.Info("background task ended", "chat", t.ChatID, "task", t.ID, "state", t.State, "exit", exit)
	var wake *live
	if notify && !(t.State == store.BgStopped && t.StoppedBy == "agent") {
		wake = m.notifyBackground(ctx, t)
	}
	// Distribute only after the note: the event then carries notified_at (Review 3, H1 c).
	if row, err := m.st.GetBackgroundTask(ctx, t.ChatID, t.Seq); err == nil {
		t.NotifiedAt, t.Woke, t.NoticePending = row.NotifiedAt, row.Woke, row.NoticePending
	}
	m.publishBackground(t.ChatID, "ended", t)
	m.publishChat(ctx, t.ChatID)
	if wake != nil {
		slog.Info("wake-up by background task", "chat", t.ChatID, "task", t.ID)
		m.deliverQueue(t.ChatID, wake)
	}
}

// notifyBackground enqueues the note as a system entry. If pi is idle and the queue is not held, it
// returns the slot for a wake-up (deliverQueue checks the limits).
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
		slog.Error("background task note not enqueued", "chat", t.ChatID, "task", t.ID, "err", err)
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

// BackgroundNote is the note to the agent at the end of a task. The orchestrator builds the summary
// line from its own data; command, error text and output come from the sandbox and go into the
// fence (see origin.go). The note texts are parsed by the web UI (web/src/lib/systemnote.ts) and
// stay German until they are changed on both sides.
func BackgroundNote(t store.BackgroundTask) systemNote {
	var h strings.Builder
	h.WriteString("Background task ")
	h.WriteString(t.ID)
	if t.Session != "" && t.Session != "main" {
		s := t.Session
		if !safeSession.MatchString(s) {
			s = "?"
		}
		h.WriteString(" (started by subagent " + s + ")")
	}
	h.WriteString(" " + endPhrase(t))
	if t.EndedAt != nil && !t.StartedAt.IsZero() {
		h.WriteString(", runtime " + FormatRuntime(t.EndedAt.Sub(t.StartedAt)))
	}
	var b strings.Builder
	b.WriteString("Command: ")
	b.WriteString(clipRunes(oneLine(t.Command), 200))
	if t.State == store.BgFailed && t.Error != "" {
		b.WriteString("\nError: " + clipRunes(oneLine(t.Error), 200))
	}
	lines := lastLines(t.Tail, 10, 1500)
	if lines == "" {
		b.WriteString("\nNo output.")
	} else {
		fmt.Fprintf(&b, "\nLast lines (of %d):\n%s", t.OutputLines, lines)
	}
	if t.LogPath != "" && t.OutputBytes > 0 {
		b.WriteString("\nFull output: " + t.LogPath)
	}
	return systemNote{Type: store.NoteBackground, Refs: []string{t.ID}, Summary: h.String(), Body: b.String()}
}

func endPhrase(t store.BackgroundTask) string {
	switch t.State {
	case store.BgExited:
		if t.ExitCode != nil {
			return fmt.Sprintf("finished: exit %d", *t.ExitCode)
		}
		return "finished"
	case store.BgTimeout:
		return "aborted at the time limit"
	case store.BgStopped:
		if t.StoppedBy == "user" {
			return "stopped by the user"
		}
		return "stopped"
	case store.BgLost:
		return "aborted: execution sandbox no longer reachable"
	default:
		return "failed" // the error text is in the fence
	}
}

// FormatRuntime: m:ss, from one hour on h:mm:ss.
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

// lastLines: the last n lines, at most max bytes (cut at the start).
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

// endBackground marks a chat's running tasks before its sandbox goes away.
func (m *Manager) endBackground(ctx context.Context, chatID, state, reason string, notice bool) {
	ended, err := m.st.EndRunningBackground(context.WithoutCancel(ctx), chatID, state, reason, notice)
	if err != nil {
		slog.Warn("background tasks not marked", "chat", chatID, "err", err)
		return
	}
	for _, t := range ended {
		m.publishBackground(chatID, "ended", t)
	}
	if len(ended) > 0 {
		slog.Info("background tasks ended with the sandbox", "chat", chatID, "count", len(ended), "state", state)
	}
}

// backgroundNotice: the one-off notice about tasks that ended with an earlier sandbox.
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
		Summary: "These background tasks ended with the previous sandbox (chat was idle or sandbox ended): " +
			strings.Join(ids, ", ") + ". Restart them if needed.",
		Body: strings.Join(items, "\n")}, seqs
}

// BackgroundTasks returns all tasks of the chat; running ones with the slot's current state.
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

// ForegroundAgent controls a slot's running foreground commands (bash).
type ForegroundAgent interface {
	StopForeground(chatID, toolCallID string) error
	BackgroundForeground(ctx context.Context, chatID, toolCallID string) (store.BackgroundTask, error)
	ForegroundRunning(chatID string) []string
}

// ErrNoForeground: no running foreground command with this ID (already ended).
var ErrNoForeground = errors.New("no running command with this ID")

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

// StopTool stops a running bash command at the user's request; the agent gets "Command stopped by
// the user" as the result and carries on.
func (m *Manager) StopTool(ctx context.Context, chatID, toolCallID string) error {
	fa, err := m.foreground(chatID)
	if err != nil {
		return err
	}
	m.userActive(chatID)
	if err := fa.StopForeground(chatID, toolCallID); err != nil {
		return err
	}
	slog.Info("command stopped by the user", "chat", chatID, "call", toolCallID)
	return nil
}

// BackgroundTool converts a running bash command into a background task: the command keeps running,
// the agent learns the ID right away and is notified when the task ends.
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
	slog.Info("command moved to the background by the user", "chat", chatID, "call", toolCallID, "task", t.ID)
	return t, nil
}

// RunningTools lists the chat's running bash commands (toolCallIds) that can be stopped or
// converted.
func (m *Manager) RunningTools(chatID string) []string {
	fa, err := m.foreground(chatID)
	if err != nil {
		return []string{}
	}
	return fa.ForegroundRunning(chatID)
}

// StopBackground ends a running task at the user's request; the agent is told.
func (m *Manager) StopBackground(ctx context.Context, chatID, id string) (store.BackgroundTask, error) {
	seq := store.ParseBgID(id)
	if seq == 0 {
		return store.BackgroundTask{}, fmt.Errorf("%w: ID %q", ErrInvalid, id)
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
	slog.Info("background task stopped by the user", "chat", chatID, "task", id)
	return ba.StopBackground(ctx, chatID, id, "user")
}

// keepAliveForBackground: should the idle timer postpone idling because tasks are running? Only up
// to Options.BgKeepAlive after the user's last action (Review 3, M1: wake-ups do not extend the
// postponement); after that the chat idles and the tasks end.
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

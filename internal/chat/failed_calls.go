// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"agw/internal/platform"
	"agw/internal/store"
)

// A hard limit for failed tool calls in a row (#217). The gateway counts at the three entries of the socket (the
// command line, the MCP tools and the REST binding), one result per call of the agent, so the count is the same in
// every binding and the agent cannot talk it away. Two counters per chat, over all sessions of the chat (main agent
// and subagents): total (every success resets it) and one per tool (only a success of the same tool resets it).
// Reaching a limit ends the turn, pauses an autonomous plan and refuses every further call and model call of the
// chat until the user writes (or resumes the plan).

// FailedCallsStop is the SSE event "failed_calls_limit" and the record of the stop.
type FailedCallsStop struct {
	Scope     string `json:"scope"` // "tool" or "total"
	Tool      string `json:"tool"`  // the key of the tool; the last tool for scope "total"
	Count     int    `json:"count"`
	Limit     int    `json:"limit"`
	LastError string `json:"last_error"`
	Detail    string `json:"detail"` // what abort_detail and the activity show
}

const (
	stopScopeTool  = "tool"
	stopScopeTotal = "total"
	// maxLastError is how many characters of the last error the stop keeps.
	maxLastError = 300
)

// failStreak is the state of one chat. Under Manager.mu.
type failStreak struct {
	total       int
	perTool     map[string]int
	warnedTotal bool
	warnedTool  map[string]bool
	lastTool    string
	lastErr     string
	stop        *FailedCallsStop
}

func newFailStreak() *failStreak {
	return &failStreak{perTool: map[string]int{}, warnedTool: map[string]bool{}}
}

// summary is the stop without the last error.
func (s FailedCallsStop) summary() string {
	if s.Scope == stopScopeTool {
		return fmt.Sprintf("%d failed calls of %s in a row (limit %d)", s.Count, s.Tool, s.Limit)
	}
	return fmt.Sprintf("%d failed tool calls in a row (limit %d), the last one %s", s.Count, s.Limit, s.Tool)
}

// FailedCallsStopped returns the text with which the gateway refuses every call of a chat it stopped for failed
// calls, "" while the chat is not stopped.
func (m *Manager) FailedCallsStopped(chatID string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.failed[chatID]
	if st == nil || st.stop == nil {
		return ""
	}
	return fmt.Sprintf("refused: the gateway stopped this chat after %s; no further calls until the user writes", st.stop.summary())
}

// resetFailedCallsLocked forgets the counters and the stop of a chat (m.mu held): the user is back in the game.
func (m *Manager) resetFailedCallsLocked(chatID string) {
	delete(m.failed, chatID)
}

// resetFailedCalls is resetFailedCallsLocked for callers that do not hold m.mu.
func (m *Manager) resetFailedCalls(chatID string) {
	m.mu.Lock()
	m.resetFailedCallsLocked(chatID)
	m.mu.Unlock()
}

// lastErrorOf is the error text of a result as the stop quotes it: redacted, on one line, at most maxLastError
// characters.
func lastErrorOf(res platform.Result, err error) string {
	var s string
	switch {
	case err != nil:
		s = err.Error()
	case res.Message != "":
		s = res.Message
	default:
		s = fmt.Sprintf("HTTP %d %s", res.HTTPStatus, res.Body)
	}
	s = strings.Join(strings.Fields(platform.RedactText(s)), " ")
	if r := []rune(s); len(r) > maxLastError {
		s = string(r[:maxLastError]) + "…"
	}
	return s
}

// NoteCallOutcome counts the result of one call of the agent (key: the name of the tool) and returns what the
// gateway adds to the answer: a warning shortly before a limit, the stop notice at the call that reaches it, or "".
// Experiment chats are not counted: the limits of a run are the experiment plan's.
func (m *Manager) NoteCallOutcome(ctx context.Context, chatID, slotID, key string, res platform.Result, err error) string {
	maxTotal, maxTool := m.opt.MaxFailedCalls, m.opt.MaxFailedSameTool
	if maxTotal == 0 && maxTool == 0 {
		return ""
	}
	if _, exp, _ := m.chatGate(chatID); exp != nil {
		return ""
	}
	outcome := platform.OutcomeOf(res, err)
	if outcome == platform.OutcomeNeutral {
		return ""
	}
	if outcome == platform.OutcomeOK {
		m.mu.Lock()
		if st := m.failed[chatID]; st != nil && st.stop == nil {
			st.total, st.warnedTotal = 0, false
			delete(st.perTool, key)
			delete(st.warnedTool, key)
		}
		m.mu.Unlock()
		return ""
	}
	lastErr := lastErrorOf(res, err)
	m.mu.Lock()
	st := m.failed[chatID]
	if st == nil {
		st = newFailStreak()
		m.failed[chatID] = st
	}
	if st.stop != nil { // a call that was already on its way when the chat was stopped
		m.mu.Unlock()
		return ""
	}
	st.total++
	st.perTool[key]++
	st.lastTool, st.lastErr = key, lastErr
	nTool := st.perTool[key]
	var stop *FailedCallsStop
	switch {
	case maxTool > 0 && nTool >= maxTool:
		stop = &FailedCallsStop{Scope: stopScopeTool, Tool: key, Count: nTool, Limit: maxTool}
	case maxTotal > 0 && st.total >= maxTotal:
		stop = &FailedCallsStop{Scope: stopScopeTotal, Tool: key, Count: st.total, Limit: maxTotal}
	}
	if stop != nil {
		stop.LastError = lastErr
		stop.Detail = stop.summary() + ". Last error: " + lastErr
		st.stop = stop
		m.mu.Unlock()
		m.logFailedCallsStop(ctx, chatID, slotID, stop)
		m.publish(chatID, Event{Kind: "failed_calls_limit", Data: *stop})
		go m.stopForFailedCalls(chatID, *stop)
		if stop.Scope == stopScopeTool {
			return fmt.Sprintf("stopped by the gateway: %d failed calls of %s in a row (limit %d). The turn ends now; the user decides how to go on.", stop.Count, stop.Tool, stop.Limit)
		}
		return fmt.Sprintf("stopped by the gateway: %d failed tool calls in a row (limit %d). The turn ends now; the user decides how to go on.", stop.Count, stop.Limit)
	}
	var warn []string
	if maxTool >= 3 && nTool == maxTool-2 && !st.warnedTool[key] {
		st.warnedTool[key] = true
		warn = append(warn, fmt.Sprintf("note from the gateway: %d failed calls of %s in a row; at %d the gateway stops this turn. Change the approach instead of repeating the call.", nTool, key, maxTool))
	}
	if maxTotal >= 10 && st.total == maxTotal-5 && !st.warnedTotal {
		st.warnedTotal = true
		warn = append(warn, fmt.Sprintf("note from the gateway: %d failed tool calls in a row; at %d the gateway stops this turn.", st.total, maxTotal))
	}
	m.mu.Unlock()
	return strings.Join(warn, "\n")
}

// logFailedCallsStop writes the Activity row of the stop. It carries the tool call of the call that reached the
// limit (from ctx); the activity lists it but does not count it among the agent's calls.
func (m *Manager) logFailedCallsStop(ctx context.Context, chatID, slotID string, stop *FailedCallsStop) {
	m.LogCallBy(ctx, slotID, chatID, "orchestrator", "failed_calls_limit", stop.Detail, "refused: stopped after "+stop.summary())
}

// stopForFailedCalls ends the running turn (an autonomous plan is paused with the reason failed_calls, the queue is
// held with HoldFailedCalls) and queues a passive note that tells the agent why, once the user writes on.
func (m *Manager) stopForFailedCalls(chatID string, stop FailedCallsStop) {
	ctx := context.Background()
	if _, err := m.abortWithDetail(ctx, chatID, AbortFailedCalls, stop.Detail); err != nil {
		slog.Warn("turn not aborted after failed calls", "chat", chatID, "error", err)
	}
	m.noteFailedCalls(ctx, chatID, stop)
	m.publishChat(ctx, chatID)
}

// noteFailedCalls tells the agent why its turn ended. Like the note about a stopped subagent it is passive: it
// never wakes the agent and goes with the user's next message.
func (m *Manager) noteFailedCalls(ctx context.Context, chatID string, stop FailedCallsStop) {
	detail := stop.Detail
	if !strings.HasSuffix(detail, ".") && !strings.HasSuffix(detail, "!") && !strings.HasSuffix(detail, "?") && !strings.HasSuffix(detail, "…") {
		detail += "."
	}
	n := systemNote{
		Type:    store.NoteFailedCalls,
		Summary: fmt.Sprintf("The gateway stopped your last turn: %s Do not repeat the failing call unchanged; explain the problem to the user or try another way.", detail),
	}
	unlock := m.queueLock(chatID)
	e, err := m.st.EnqueueSystem(ctx, chatID, n.Type, n.Refs, n.Text())
	unlock()
	if err != nil {
		slog.Warn("note about the failed-calls stop not stored", "chat", chatID, "err", err)
		return
	}
	m.publishQueue(ctx, chatID, "queued", []string{e.ID}, "")
}

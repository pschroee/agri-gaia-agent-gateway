// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

// Stopping a single subagent (issue #120, L3): the user stops one running subagent without stopping the chat.
//
// The request goes the same way as a message to a subagent (issue #81): the gateway sends the relay command
// /agw-subagent-relay to pi, the extension passes it to pi-subagents' in-process RPC and reports the reply over the
// socket. The gateway decides what is sent and which methods the bridge lets through (stop carries nothing but the
// run). The record is the socket call with op subagent_stop; the main agent learns about the stop through a passive
// note that never wakes it.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"agw/internal/store"
	"agw/internal/toolset"
)

// Who stopped a run (subagent_runs.stopped_by, issue #120, L3).
const (
	// StoppedByUser: the user stopped the run over the route.
	StoppedByUser = "user"
	// StoppedByLimit: the orchestrator ended the run because too many ran at the same time.
	StoppedByLimit = "limit"
)

// ErrSubagentNotRunning: the run is not running, so there is nothing to stop.
var ErrSubagentNotRunning = errors.New("the subagent has already ended")

// SubagentRelayError is a refusal of the bridge or of pi-subagents; Code travels to the UI, which turns it into a
// text (src/agent/subagents.ts, subagentStopErrorText).
type SubagentRelayError struct {
	Code    string
	Message string
}

func (e *SubagentRelayError) Error() string {
	if e.Message == "" {
		return "the stop was refused (" + e.Code + ")"
	}
	return "the stop was refused (" + e.Code + "): " + e.Message
}

// StopSubagent stops one running subagent of a chat. It returns the run with the recorded reason. Refusals:
// ErrNotFound (unknown or another chat's run), ErrSubagentNotRunning (the run has already ended), ErrTrashed (the chat is in
// the trash), ErrSubagentsUnavailable (the chat rests, or the binding has no subagents) and SubagentRelayError (the
// bridge or pi-subagents refused).
func (m *Manager) StopSubagent(ctx context.Context, chatID, run string) (store.SubagentRun, error) {
	if !safeSession.MatchString(run) {
		return store.SubagentRun{}, ErrNotFound
	}
	m.userActive(chatID)
	trashed, err := m.st.ChatTrashed(ctx, chatID)
	if err != nil {
		return store.SubagentRun{}, err
	}
	if trashed {
		return store.SubagentRun{}, ErrTrashed
	}
	rec, err := m.st.SubagentRunOf(ctx, chatID, run)
	if err != nil {
		return store.SubagentRun{}, err // a run of another chat is not found
	}
	c, err := m.st.GetChat(ctx, chatID)
	if err != nil {
		return store.SubagentRun{}, err
	}
	if ts, err := toolset.FromVariant(c.Variant); err != nil || !ts.Has(toolset.CLI) {
		return store.SubagentRun{}, ErrSubagentsUnavailable
	}
	l := m.liveChat(chatID)
	if l == nil {
		// The chat rests: nothing runs, and a stop must not wake it (like a message, issue #81).
		return store.SubagentRun{}, ErrSubagentsUnavailable
	}
	if !m.runIsRunning(l, run, time.Now()) {
		return store.SubagentRun{}, ErrSubagentNotRunning
	}
	ref := newSubagentRef()
	m.mu.Lock()
	if m.stopRefs == nil {
		m.stopRefs = map[string]string{}
	}
	m.stopRefs[ref] = chatID
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.stopRefs, ref)
		m.mu.Unlock()
	}()
	// The bridge lets through nothing but the run's ID (DL3.1): the stop ends that run, not a single child of it.
	reply, err := m.relaySubagent(ctx, chatID, l, ref, "stop", map[string]any{"id": rec.PiRunID})
	if err != nil {
		m.LogCall(l.slot.ID, chatID, viaSubagentUI, opSubagentStop, rec.RunID, "error: "+err.Error())
		return store.SubagentRun{}, err
	}
	if !reply.OK {
		code := reply.Code
		if code == "" {
			code = "refused"
		}
		m.LogCall(l.slot.ID, chatID, viaSubagentUI, opSubagentStop, rec.RunID, "error: "+code)
		return store.SubagentRun{}, &SubagentRelayError{Code: code, Message: reply.Message}
	}
	if err := m.st.SetSubagentStoppedBy(ctx, chatID, rec.RunID, StoppedByUser); err != nil {
		slog.Warn("stopped_by not stored", "chat", chatID, "run", rec.RunID, "err", err)
	}
	rec.StoppedBy = StoppedByUser
	m.LogCall(l.slot.ID, chatID, viaSubagentUI, opSubagentStop, rec.RunID, "ok")
	m.noteSubagentStopped(ctx, chatID, rec)
	// Publish the run so the UI leaves "stopping …" without a reload and names the reason.
	m.publish(chatID, Event{Kind: "subagent_run", Data: rec})
	return rec, nil
}

// The socket log entry of a stop (issue #120, L3): the operation, and "ui" as who asked (the user in the interface;
// the activity names it "UI"). DL3.2 fixes both.
const (
	opSubagentStop = "subagent_stop"
	viaSubagentUI  = "ui"
)

// noteSubagentStopped tells the main agent that the user stopped a run. Like notePassive (issue #81, L3 DL3.5) it
// never wakes the agent: the note waits in the queue and goes with the next message or the next tool step.
func (m *Manager) noteSubagentStopped(ctx context.Context, chatID string, rec store.SubagentRun) {
	agent := rec.Agent
	if !agentNameRe.MatchString(agent) {
		agent = "subagent"
	}
	n := systemNote{
		Type:    store.NoteSubagentStop,
		Refs:    []string{rec.RunID},
		Summary: fmt.Sprintf("The user stopped subagent %s (%s); do not restart it unless the user asks.", agent, rec.RunID),
	}
	unlock := m.queueLock(chatID)
	e, err := m.st.EnqueueSystem(ctx, chatID, n.Type, n.Refs, n.Text())
	unlock()
	if err != nil {
		slog.Warn("note about a stopped subagent not stored", "chat", chatID, "err", err)
		return
	}
	m.publishQueue(ctx, chatID, "queued", []string{e.ID}, "")
}

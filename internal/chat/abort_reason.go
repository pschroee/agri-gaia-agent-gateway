// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"log/slog"
)

// Why a turn was aborted (issue #120, L3, F9). The values land in chat_turns.abort_reason and travel with every
// message of the turn, so the UI names the reason a step was stopped instead of always claiming "stopped by you".
// The same strings are the keys the frontend knows (src/agent/transcript.ts, abortLabel).
const (
	// AbortUser: the user stopped the chat (POST /api/chats/{id}/abort).
	AbortUser = "user"
	// AbortTrash: the chat was moved to the trash, which aborts its running turn.
	AbortTrash = "trash"
	// AbortTimeLimit: the turn ran longer than maxRunTime (30 minutes).
	AbortTimeLimit = "time_limit"
	// AbortTurnLimit: too many turns in a row without the user (Options.AutoTurnsMax, Review 3, H2).
	AbortTurnLimit = "turn_limit"
	// AbortSubagentLimit: more subagents ran at the same time than allowed (Options.MaxSubagents).
	AbortSubagentLimit = "subagent_limit"
)

// markTurnAborted records why the turn now running was cut short. The first reason wins (store.SetTurnAbortReason),
// so a turn ended by the time limit and only afterwards moved to the trash keeps the time limit.
//
// Nothing is recorded when no turn runs: aborting an idle chat (or one that is only being torn down, or is already
// moving to the trash) must not make a finished turn look stopped — that would be exactly the wrong claim this issue
// is about (F9). "Running" is the manager's own view of a turn in flight, the same one the UI shows.
func (m *Manager) markTurnAborted(chatID string, l *live, reason string) {
	if l == nil {
		return
	}
	m.mu.Lock()
	var id int64
	if l.running {
		if t := l.turn; t != nil {
			id = t.id
		}
	}
	m.mu.Unlock()
	if id == 0 {
		return
	}
	if err := m.st.SetTurnAbortReason(context.Background(), id, reason); err != nil {
		slog.Warn("abort reason not stored", "chat", chatID, "turn", id, "reason", reason, "err", err)
	}
}

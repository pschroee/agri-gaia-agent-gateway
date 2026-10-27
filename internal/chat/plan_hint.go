// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"log/slog"
	"time"
)

// planHintWindow is the time a run of single approvals is counted over, and the time between two hints in a chat.
const planHintWindow = 30 * time.Minute

// secondApprovalNote is added to the answer of a call after the second single approval in a row (#254).
const secondApprovalNote = "This is the second single approval in a row: propose the remaining writing steps of this task as ONE plan with {{tool:propose_plan}}."

// secondApprovalHint returns the hint when the call that was just approved is the second (or a later) single
// approval of a writing call in this chat within planHintWindow and no hint was given in that time; otherwise "".
// The caller has checked that the call was approved singly, outside any plan step and any trial of a search.
func (m *Manager) secondApprovalHint(ctx context.Context, chatID string) string {
	now := time.Now()
	n, err := m.st.CountSingleApprovalsSince(context.WithoutCancel(ctx), chatID, now.Add(-planHintWindow))
	if err != nil {
		slog.Warn("single approvals not counted", "chat", chatID, "error", err)
		return ""
	}
	if n < 2 {
		return ""
	}
	m.planHintMu.Lock()
	defer m.planHintMu.Unlock()
	if last, ok := m.planHintAt[chatID]; ok && now.Sub(last) < planHintWindow {
		return ""
	}
	if m.planHintAt == nil {
		m.planHintAt = map[string]time.Time{}
	}
	m.planHintAt[chatID] = now
	return secondApprovalNote
}

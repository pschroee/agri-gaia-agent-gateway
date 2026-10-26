// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"log/slog"
	"time"

	"agw/internal/plan"
	"agw/internal/platform"
	"agw/internal/store"
)

// runSummaryTimeout bounds the reads for the summary of a step as it is sent: the step must not wait long for a text.
const runSummaryTimeout = 5 * time.Second

// recordRunSummary stores the summary of the call of a plan step as it goes out (issue #225), when it differs from
// the stored call: a placeholder resolved to the id it stands for, or a value the tool filled in. The summary the
// user approved stays in plan_steps.summary. A single approval given in this very call already carries a summary
// of this call (approvalSummary); it is taken over without further reads. A failed read or a timeout stores nothing,
// so that a step never shows a text with a read error after it succeeded. Errors are logged only.
func (m *Manager) recordRunSummary(ctx context.Context, chatID string, st *store.PlanStep, req platform.Request, approvalSummary string) {
	if st == nil || !plan.CallChanged(*st, req) {
		return
	}
	bg := context.WithoutCancel(ctx)
	text := approvalSummary
	if text == "" {
		rl := &readLog{}
		ctxT, cancel := context.WithTimeout(bg, runSummaryTimeout)
		defer cancel()
		text = platform.VisibleControls(platform.Summarize(ctxT, m.loggedSummaryReader(chatID, rl), platform.SummaryCallOf(st.Tool, req)))
		m.logRunSummaryReads(bg, chatID, st.PlanID, st.Idx, rl)
		if rl.failedReads() > 0 || ctxT.Err() != nil {
			slog.Info("summary of the sent call not stored", "plan", st.PlanID, "step", st.Idx, "failed_reads", rl.failedReads(), "timeout", ctxT.Err() != nil)
			return
		}
	}
	if text == "" {
		return
	}
	if err := m.st.SetPlanStepRunSummary(bg, st.PlanID, st.Idx, text); err != nil {
		slog.Error("summary of the sent call not stored", "plan", st.PlanID, "step", st.Idx, "error", err)
		return
	}
	m.publishPlan(bg, st.PlanID)
}

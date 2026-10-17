// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"

	"agw/internal/plan"
	"agw/internal/platform"
	"agw/internal/store"
)

// The plan alerts (A-E4, D4.3): one SSE event "plan_alert" per state an autonomous plan reaches that the
// user has to know about, so that the platform frontend can show a snackbar and a browser notification
// without the chat being open. The event carries no plan state: the card reads that from the plan, which
// the chat's own stream ("plan") and GET …/plans deliver.

// PlanAlert is the payload of the SSE event "plan_alert" (A-E4, D4.3).
type PlanAlert struct {
	ChatID string `json:"chat_id"`
	PlanID string `json:"plan_id"`
	// Kind: completed | failed | paused | needs_input | attention. A plan the user cancels itself sends
	// none: the user caused it and sees it in the card.
	Kind string `json:"kind"`
	// Title is the plan's title as the agent wrote it (unverified), as on the card; empty without one.
	Title string `json:"title"`
	// Step: the step the alert is about (failed: the failed one, needs_input: the waiting one); zero when
	// the alert names no single step.
	Step int `json:"step,omitempty"`
	// Reason: why the plan paused (the stored pause reason) or what the observer could not read; empty when
	// the kind alone says what happened.
	Reason string `json:"reason,omitempty"`
}

// Kinds of the plan alerts (D4.3).
const (
	AlertCompleted  = "completed"
	AlertFailed     = "failed"
	AlertPaused     = "paused"
	AlertNeedsInput = "needs_input"
	AlertAttention  = "attention"
)

// publishPlanAlerts sends the alert for the state a plan has just reached (A-E4, D4.3). Every state is
// reported once: the mark of the last published state is kept per plan, so publishing the same state again
// sends nothing, and a plan whose state was already read when the gateway started sends nothing either
// (nobody needs a notification for something that happened before the process). The mark is kept for the
// whole life of the plan; a superseded plan's mark is dropped when the plan's chat is closed.
func (m *Manager) publishPlanAlerts(ctx context.Context, p store.Plan) {
	if p.Mode != plan.ModeAutonomous {
		return
	}
	kind, step, reason := m.planAlertOf(ctx, p)
	mark := kind + "\x00" + p.PauseReason
	m.mu.Lock()
	if m.planAlertMarks == nil {
		m.planAlertMarks = map[string]string{}
	}
	prev, seen := m.planAlertMarks[p.ID]
	m.planAlertMarks[p.ID] = mark
	m.mu.Unlock()
	// An empty kind means the user has nothing to see or decide (a plan that runs on, a resumed one): the
	// state is marked, so that the next change is measured against it, but no alert goes out — an alert
	// without a kind is nothing the stream can show.
	if kind == "" || !seen || prev == mark {
		return
	}
	m.sendPlanAlert(p, PlanAlert{Kind: kind, Step: step, Reason: reason})
}

// planAlertOf names the alert a plan's state deserves (D4.3); an empty kind means the user has nothing to
// see or decide.
func (m *Manager) planAlertOf(ctx context.Context, p store.Plan) (kind string, step int, reason string) {
	switch p.State {
	case plan.StateCompleted:
		return AlertCompleted, 0, ""
	case plan.StateFailed:
		if st, ok := failedStep(p); ok {
			return AlertFailed, st.Idx, st.Result
		}
		return AlertFailed, 0, ""
	case plan.StatePaused:
		return AlertPaused, 0, p.PauseReason
	case plan.StateApproved:
		if st, ok := m.waitingStep(ctx, p); ok {
			return AlertNeedsInput, st.Idx, ""
		}
	}
	return "", 0, ""
}

// failedStep is the step a plan failed on: the first one in state failed.
func failedStep(p store.Plan) (store.PlanStep, bool) {
	for _, s := range p.Steps {
		if s.State == plan.StepFailed {
			return s, true
		}
	}
	return store.PlanStep{}, false
}

// waitingStep is the step of a running plan that waits for the user's own decision (D2.8): it carries the
// approval it was parked on, and that approval is still open. ok is false when nothing waits.
func (m *Manager) waitingStep(ctx context.Context, p store.Plan) (store.PlanStep, bool) {
	for _, s := range p.Steps {
		if s.ParkedApproval == "" || s.State != plan.StepOpen {
			continue
		}
		ap, err := m.st.GetApproval(ctx, s.ParkedApproval)
		if err != nil || ap.State != store.ApprovalPending {
			continue
		}
		return s, true
	}
	return store.PlanStep{}, false
}

// publishPlanAttention is the alert for an observed platform job of a plan that the gateway cannot read
// (A-E2, D4.3): the plan keeps waiting, so the user should see why nothing moves. The observer calls this
// once per run of failures, not on every attempt.
func (m *Manager) publishPlanAttention(chatID, planID, reason string) {
	ctx := context.Background()
	p, err := m.st.GetPlan(ctx, planID)
	if err != nil || p.Mode != plan.ModeAutonomous || p.ChatID != chatID {
		return
	}
	m.sendPlanAlert(p, PlanAlert{Kind: AlertAttention, Reason: reason})
}

// sendPlanAlert distributes one alert: to the subscribers of the plan's chat and, because "plan_alert" is
// in crossChatKinds, to the stream across chats (GET /api/events), which filters it by the chat's owner
// like an approval.
func (m *Manager) sendPlanAlert(p store.Plan, a PlanAlert) {
	a.ChatID = p.ChatID
	a.PlanID = p.ID
	a.Title = p.Title
	a.Reason = platform.RedactText(a.Reason)
	m.publish(p.ChatID, Event{Kind: "plan_alert", Data: a})
}

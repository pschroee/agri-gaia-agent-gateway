// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"log/slog"

	"agw/internal/store"
)

// Reasons the experiment profile gives for a decision. The approvals table has no reason column; the
// reason is logged and, for a refusal, part of the answer the agent reads.
const (
	ReasonExperimentProfile    = "experiment profile"
	ReasonExperimentNoInternet = "experiment: no internet in this task"
)

// experimentDecides is the one rule for approvals in experiment chats (BM2): nobody sits in front of the
// card, so the gateway decides it right after it is written and the decision is recorded with decided_by
// "experiment". The rule is "approve", with one exception: an internet access is refused unless the task
// allows it (Experiment.Internet == "request"). The limits of an experiment do not come from the approval
// but from the delegation, the package blocklist and the guards, which all run before the approval is
// created.
//
// ok is false for a product chat: the approval is left pending and the caller waits for the user. Otherwise
// decided is the approval as stored (approved or rejected) and a decided event has gone out; the caller must
// not register a wait for it.
func (m *Manager) experimentDecides(ctx context.Context, chatID string, ap store.Approval) (decided store.Approval, ok bool, err error) {
	exp := m.experimentOf(ctx, chatID)
	if exp == nil {
		return ap, false, nil
	}
	state, reason := store.ApprovalApproved, ReasonExperimentProfile
	if ap.Kind == "internet_access" && exp.Internet != "request" {
		state, reason = store.ApprovalRejected, ReasonExperimentNoInternet
	}
	decided, _, err = m.st.DecideApprovalBy(ctx, ap.ID, state, store.DecidedByExperiment)
	if err != nil {
		return ap, true, err
	}
	m.publish(chatID, Event{Kind: "approval", Data: decided})
	slog.Info("approval decided by the experiment profile", "chat", chatID, "approval", decided.ID, "kind", decided.Kind,
		"state", decided.State, "reason", reason)
	return decided, true, nil
}

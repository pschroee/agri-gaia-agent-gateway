// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"sync"
)

// planDecision writes the outcome of a plan's approval exactly once: from Decide, before the waiting call is woken
// (issue #166), or from ProposePlan after settle when the approval expired or was rejected elsewhere.
type planDecision struct {
	once  sync.Once
	ok    bool
	err   error
	apply func(ctx context.Context, final string) (bool, error)
}

// decide runs apply on the first call; later calls return the stored result.
func (d *planDecision) decide(ctx context.Context, final string) (bool, error) {
	d.once.Do(func() { d.ok, d.err = d.apply(context.WithoutCancel(ctx), final) })
	return d.ok, d.err
}

// registerPlanDecision notes how to record the decision of a pending plan approval and returns the way to forget it.
// Callers register before the approval row is inserted, like registerRecheck (issue #154).
func (m *Manager) registerPlanDecision(approvalID string, d *planDecision) func() {
	m.mu.Lock()
	if m.planDecisions == nil {
		m.planDecisions = map[string]*planDecision{}
	}
	m.planDecisions[approvalID] = d
	m.mu.Unlock()
	return func() {
		m.mu.Lock()
		delete(m.planDecisions, approvalID)
		m.mu.Unlock()
	}
}

// planDecisionFor returns the plan decision of a pending approval; nil for approvals that belong to no waiting plan.
func (m *Manager) planDecisionFor(approvalID string) *planDecision {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.planDecisions[approvalID]
}

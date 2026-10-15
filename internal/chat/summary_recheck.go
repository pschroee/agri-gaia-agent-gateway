// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"errors"
	"sync"

	"agw/internal/plan"
	"agw/internal/platform"
	"agw/internal/store"
)

// ErrSummaryChanged says that the platform state behind a summary changed between the proposal and the click on
// "Approve" (issue L2/#119). The approval stays open, carries the newly read summary and, next to it, the text as it
// was proposed; the API answers 409 with code summary_changed. The next click approves when nothing changed again.
var ErrSummaryChanged = errors.New("the platform changed since this was proposed")

// recheck re-reads the state a summary was written from and returns the summaries as they are now, the preview of a
// plan built from them, and whether at least one read failed. failed true means: approve what was stored, because a
// platform that is briefly unreachable must not block the user (DL2.3).
type recheck func(ctx context.Context) (summaries []string, preview string, failed bool)

// registerRecheck notes how to re-read the summary of a pending approval and returns the way to forget it again.
func (m *Manager) registerRecheck(approvalID string, f recheck) func() {
	m.mu.Lock()
	if m.rechecks == nil {
		m.rechecks = map[string]recheck{}
	}
	m.rechecks[approvalID] = f
	m.mu.Unlock()
	return func() {
		m.mu.Lock()
		delete(m.rechecks, approvalID)
		m.mu.Unlock()
	}
}

// recheckFor returns the recheck of a pending approval; nil when the approval belongs to no waiting call any more
// (expired, already decided, or from a chat that no call is waiting for).
func (m *Manager) recheckFor(approvalID string) recheck {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.rechecks[approvalID]
}

// approvalLock serializes the decisions on one approval: a second click waits until the first has decided.
func (m *Manager) approvalLock(approvalID string) func() {
	m.mu.Lock()
	if m.apMu == nil {
		m.apMu = map[string]*sync.Mutex{}
	}
	l, ok := m.apMu[approvalID]
	if !ok {
		l = &sync.Mutex{}
		m.apMu[approvalID] = l
	}
	m.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// platformRecheck reads the summary of a single platform call again, through the logging reader (the entry shows it).
func (m *Manager) platformRecheck(chatID, approvalID, tool string, req platform.Request) recheck {
	return func(ctx context.Context) ([]string, string, bool) {
		rl := &readLog{}
		s := platform.Summarize(ctx, m.loggedSummaryReader(chatID, rl), platform.SummaryCallOf(tool, req))
		m.logSummaryReads(ctx, chatID, approvalID, "approval", rl)
		return []string{visibleControls(s)}, "", rl.failedReads() > 0
	}
}

// planRecheck reads the summary of every step of a plan again and rebuilds the preview from them (DL2.2). title and
// steps come from the proposal: only ProposePlan has them.
func (m *Manager) planRecheck(chatID, approvalID, title string, steps []store.PlanStep, calls []platform.SummaryCall) recheck {
	base := append([]store.PlanStep(nil), steps...)
	return func(ctx context.Context) ([]string, string, bool) {
		rl := &readLog{}
		sums := platform.SummarizePlan(ctx, m.loggedSummaryReader(chatID, rl), calls)
		fresh := append([]store.PlanStep(nil), base...)
		for i := range fresh {
			if i < len(sums) {
				fresh[i].Summary = visibleControls(sums[i])
			}
		}
		out := make([]string, len(fresh))
		for i := range fresh {
			out[i] = fresh[i].Summary
		}
		m.logSummaryReads(ctx, chatID, approvalID, "approval", rl)
		return out, visibleControls(plan.Preview(title, fresh)), rl.failedReads() > 0
	}
}

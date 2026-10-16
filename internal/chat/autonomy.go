// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"agw/internal/delegation"
	"agw/internal/plan"
	"agw/internal/platform"
	"agw/internal/store"
)

// The autonomy of a plan (A-E1): the derived delegation bounds every platform call of the chat, the LLM
// proxy enforces the model-cost budget, and the active plan is held in memory for a moment so that neither
// puts a query behind every call.

func init() { registerGuard(autonomousGuard) }

// planCacheTTL: how long the active autonomous plan of a chat is held in memory (A-E1). Every change of a
// plan's state goes through publishPlan, which drops the cache, so the window only covers an unchanged plan.
const planCacheTTL = 2 * time.Second

type planCacheEntry struct {
	plan store.Plan
	err  error
	at   time.Time
}

// invalidatePlanCache forgets the cached active plan of a chat.
func (m *Manager) invalidatePlanCache(chatID string) {
	m.mu.Lock()
	delete(m.planCache, chatID)
	m.mu.Unlock()
}

// activeAutonomousPlan returns the chat's active autonomous plan (approved or paused), from memory for
// planCacheTTL. ErrNotFound when there is none.
func (m *Manager) activeAutonomousPlan(ctx context.Context, chatID string) (store.Plan, error) {
	now := time.Now()
	m.mu.Lock()
	if e, ok := m.planCache[chatID]; ok && now.Sub(e.at) < planCacheTTL {
		m.mu.Unlock()
		return e.plan, e.err
	}
	m.mu.Unlock()
	ap, err := m.st.ActiveAutonomousPlan(ctx, chatID)
	m.mu.Lock()
	if m.planCache == nil {
		m.planCache = map[string]planCacheEntry{}
	}
	m.planCache[chatID] = planCacheEntry{plan: ap, err: err, at: now}
	m.mu.Unlock()
	return ap, err
}

// autonomousGuard bounds every platform call of a chat while an autonomous plan runs (A-E1, D1.1/D1.11):
// the call must lie inside the plan's grant, from any session and binding. A paused plan allows reading
// only. Outside the grant the call is refused without asking; an unreadable grant grants nothing.
func autonomousGuard(ctx context.Context, m *Manager, chatID string, req platform.Request, access delegation.Access) (string, error) {
	ap, err := m.activeAutonomousPlan(ctx, chatID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", nil // no autonomous plan: the delegation and the approval decide
		}
		return "", err // fail closed: an unreadable plan does not let a call through
	}
	g, gerr := delegation.Parse(ap.Grant)
	if gerr != nil {
		return "the running plan's rights are unreadable", nil
	}
	own := func(res, id string) bool {
		ok, _ := m.st.IsDelegationObject(ctx, chatID, res, id)
		return ok
	}
	if dec := g.Check(access, time.Now(), own); !dec.Allowed {
		return fmt.Sprintf("outside the running plan %q: %s. Nothing executed. Propose a changed plan with \"replaces\": %q.",
			ap.ID, dec.Reason, ap.ID), nil
	}
	if ap.State == plan.StatePaused && (access.Effect != delegation.EffectRead || req.Writes()) {
		if ap.PauseReason == store.PauseLogin {
			// A-E3: nobody has to resume this one — the owner's next login continues the plan by itself (D3.6).
			return "the plan is paused because the user's login is gone; nothing executed. It continues by itself once the user is logged in to the platform again.", nil
		}
		return "the plan is paused (" + ap.PauseReason + "); nothing executed. Wait until the user resumes it.", nil
	}
	return "", nil
}

// planBudget enforces the model-cost budget of an autonomous plan at the LLM proxy (A-E1, D1.9). It is
// called before every model call of the chat: the cost of the calls since the plan was approved must stay
// below the budget, otherwise the plan is paused (reason budget_cost) and the call is refused.
func (m *Manager) planBudget(chatID string) error {
	ctx := context.Background()
	ap, err := m.activeAutonomousPlan(ctx, chatID)
	if err != nil || ap.State != plan.StateApproved || ap.Budget.MaxLLMCost <= 0 {
		return nil
	}
	since := time.Time{}
	if ap.DecidedAt != nil {
		since = *ap.DecidedAt
	}
	used, err := m.st.PlanLLMCost(ctx, chatID, since)
	if err != nil {
		slog.Error("plan model cost not readable", "chat", chatID, "plan", ap.ID, "error", err)
		return nil // an unreadable cost must not block the chat; the grant still bounds the calls
	}
	if used < ap.Budget.MaxLLMCost {
		return nil
	}
	if ok, perr := m.st.PausePlan(ctx, ap.ID, "budget_cost"); perr != nil {
		slog.Error("plan not paused after the cost budget was used up", "chat", chatID, "plan", ap.ID, "error", perr)
	} else if ok {
		m.publishPlan(ctx, ap.ID)
		slog.Info("plan paused, model cost budget used up", "chat", chatID, "plan", ap.ID, "used", used, "budget", ap.Budget.MaxLLMCost)
	}
	return fmt.Errorf("plan budget for model calls used up (%.2f of %.2f USD); the plan is paused", used, ap.Budget.MaxLLMCost)
}

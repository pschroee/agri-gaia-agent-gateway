// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"fmt"
	"log/slog"

	"agw/internal/delegation"
	"agw/internal/plan"
	"agw/internal/platform"
	"agw/internal/store"
)

// A step of an approved plan that waits for a platform job blocks the steps after it (planMatch takes the
// first step that is not done). A call that looks like the next step would then fall through to a single
// approval for a step the user already approved in the plan, with an id the agent had to guess. This guard
// refuses such a call with the reason instead (#169). It is no rights check: any read error lets the call
// pass to the plan match.
func init() { registerGuardFirst(planWaitGuard) }

func planWaitGuard(ctx context.Context, m *Manager, chatID string, req platform.Request, a delegation.Access) (string, error) {
	if a.Effect == delegation.EffectRead && !req.Writes() {
		return "", nil
	}
	p, err := m.st.ActivePlan(ctx, chatID, store.SessionFrom(ctx))
	if err != nil {
		return "", nil // no plan, or not readable: planMatch decides
	}
	var wait *store.PlanStep
	for i := range p.Steps {
		if p.Steps[i].State != plan.StepDone {
			wait = &p.Steps[i]
			break
		}
	}
	if wait == nil || wait.State != plan.StepWaiting {
		return "", nil
	}
	var next *store.PlanStep
	for i := range p.Steps {
		if p.Steps[i].Idx > wait.Idx && p.Steps[i].State == plan.StepOpen {
			next = &p.Steps[i]
			break
		}
	}
	if next == nil {
		return "", nil
	}
	inv, _ := platform.InvocationFrom(ctx)
	if !plan.SameShape(*next, inv.Tool, req) {
		return "", nil
	}
	kind := "platform job"
	if job, err := m.st.GetWatchJob(ctx, chatID, wait.WatchJob); err != nil {
		slog.Warn("job of a waiting step not readable", "chat", chatID, "job", wait.WatchJob, "error", err)
	} else {
		kind = job.Kind
	}
	return fmt.Sprintf("plan step %d cannot run yet: step %d still waits for platform job %s (%s). The object {{step%d.id}} stands for is known only when that job ends; the gateway's note then names it. End your turn now and make the call of step %d after that note.",
		next.Idx, wait.Idx, wait.WatchJob, kind, wait.Idx, next.Idx), nil
}

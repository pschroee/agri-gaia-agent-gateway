// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"agw/internal/delegation"
	"agw/internal/plan"
	"agw/internal/platform"
	"agw/internal/store"
	"agw/internal/watch"
)

// A step of an approved plan that waits for a platform job blocks the steps after it (planMatch takes the
// first step that is not done). A call that looks like the next step would then fall through to a single
// approval for a step the user already approved in the plan, with an id the agent had to guess. This guard
// refuses such a call with the reason instead (#169). It is no rights check: any read error lets the call
// pass to the plan match.
func init() { registerGuardFirst(planWaitGuard) }

// The watcher looks at an old job only every 30 s, so the agent may see the end of a job before the gateway
// does. An early call therefore makes the gateway read the job at once (issue #180): at most planWaitRefresh
// long, and not again within planWaitMinGap of the last read of that job.
const (
	planWaitRefresh = 20 * time.Second
	planWaitMinGap  = 3 * time.Second
)

func planWaitGuard(ctx context.Context, m *Manager, chatID string, req platform.Request, a delegation.Access) (string, error) {
	if a.Effect == delegation.EffectRead && !req.Writes() {
		return "", nil
	}
	p, err := m.st.ActivePlan(ctx, chatID, planSession(ctx))
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
	if wait.SearchID != "" {
		// A search step ends with the search, not with a job (issue #244): nothing to refresh.
		if s, serr := m.st.GetSearch(ctx, chatID, wait.SearchID); serr == nil && s.State == store.SearchRunning {
			return fmt.Sprintf("plan step %d cannot run yet: step %d is the training search %s, which is still running. {{step%d.id}} is the training container of the winner and is known only when the search ends; "+
				"the gateway's note then names the winner. Run the search with {{tool:search_next}} until it ends, then make the call of step %d.",
				next.Idx, wait.Idx, wait.SearchID, wait.Idx, next.Idx), nil
		}
		return "", nil
	}
	m.refreshWaitingJob(ctx, chatID, p.ID, wait.WatchJob, wait.Idx)
	p2, err := m.st.GetPlan(ctx, p.ID)
	if err != nil {
		return "", nil // no rights check
	}
	var cur *store.PlanStep
	for i := range p2.Steps {
		if p2.Steps[i].State != plan.StepDone {
			cur = &p2.Steps[i]
			break
		}
	}
	if cur == nil || cur.Idx >= next.Idx {
		return "", nil // the waiting step is done: planNext matches the call to the plan
	}
	if p2.State != plan.StateApproved && cur.State != plan.StepFailed {
		return "", nil
	}
	switch cur.State {
	case plan.StepFailed:
		return fmt.Sprintf("plan step %d did not run: step %d failed (%s) and the plan ended. Nothing was sent. Read the gateway's note on step %d and propose a new plan if the task should go on.",
			next.Idx, cur.Idx, cur.Result, cur.Idx), nil
	case plan.StepOpen, plan.StepRunning:
		return fmt.Sprintf("plan step %d cannot run yet: step %d failed (%s) and is open for another attempt. Make the call of step %d first.",
			next.Idx, cur.Idx, cur.Result, cur.Idx), nil
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

// refreshWaitingJob reads the job of a waiting step now instead of at the watcher's next tick, so that a
// call that comes after the end of the job is judged on what the gateway knows then. The id of the object
// comes only from this read of the gateway, never from the agent's call. It reports whether a read ran or
// was awaited.
func (m *Manager) refreshWaitingJob(ctx context.Context, chatID, planID, jobID string, step int) bool {
	job, err := m.st.GetWatchJob(ctx, chatID, jobID)
	if err != nil || job.State != watch.StateRunning {
		return false
	}
	if job.CheckedAt != nil && time.Since(*job.CheckedAt) < planWaitMinGap {
		return false
	}
	kind, ok := watchKind(job.Kind)
	if !ok || kind.Poll == nil {
		return false
	}
	start := time.Now()
	ch := m.pollJobShared(context.WithoutCancel(ctx), job, kind)
	select {
	case <-ch:
	case <-time.After(planWaitRefresh):
	case <-ctx.Done():
		return false
	}
	slog.Info("waiting job read again for an early plan call", "chat", chatID, "job", jobID, "step", step,
		"state", m.waitingStepState(ctx, planID, step), "ms", time.Since(start).Milliseconds())
	return true
}

// waitingStepState names the state of a plan step of a plan for the log.
func (m *Manager) waitingStepState(ctx context.Context, planID string, step int) string {
	p, err := m.st.GetPlan(ctx, planID)
	if err != nil {
		return "unknown"
	}
	for _, st := range p.Steps {
		if st.Idx == step {
			return st.State
		}
	}
	return "unknown"
}

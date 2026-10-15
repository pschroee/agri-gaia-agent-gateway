// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"agw/internal/delegation"
	"agw/internal/plan"
	"agw/internal/platform"
	"agw/internal/store"
)

// planNow is the clock of plan validity; tests set it.
var planNow = time.Now

// planClassify gives the effect and delegation action of a call, the same classification that
// PlatformCall uses (delegation.Classify, issue #63).
func planClassify(r platform.Request) (effect, action string) {
	a := delegation.Classify(r)
	return string(a.Effect), a.Action
}

// ProposePlan takes a plan proposal (POST /_agw/plan): one approval covers all steps. An approved
// plan answers with its steps; the steps then run through the next-step match (planMatch).
func (m *Manager) ProposePlan(ctx context.Context, chatID, slotID, via string, req platform.Request) (platform.Result, error) {
	if m.inTrash(ctx, chatID) {
		return platform.Result{Status: "error", Message: platform.RefusedPrefix + trashRefusal}, nil
	}
	p, err := plan.ParseProposal(req.Body)
	if err != nil {
		return platform.Result{Status: "error", Message: err.Error()}, nil
	}
	steps, err := plan.Build(p, planClassify)
	if err != nil {
		return platform.Result{Status: "error", Message: err.Error()}, nil
	}
	if err := m.checkPendingLimit(ctx, chatID); err != nil {
		return platform.Result{}, err
	}
	// What each step does is written by the gateway from the stored step and platform state read now (issue #72);
	// the agent's description stays a separate note.
	calls := make([]platform.SummaryCall, len(steps))
	for i := range steps {
		calls[i] = plan.SummaryCall(steps[i])
	}
	rl := &readLog{}
	for i, s := range platform.SummarizePlan(ctx, m.loggedSummaryReader(chatID, rl), calls) {
		steps[i].Summary = visibleControls(s)
	}
	preview := visibleControls(plan.Preview(p.Title, steps))
	if n := len([]rune(preview)); n > maxPlatformPreview {
		return platform.Result{Status: "error", Message: fmt.Sprintf("plan too large for an approval (%d characters, at most %d); split it", n, maxPlatformPreview)}, nil
	}
	session := store.SessionFrom(ctx)
	pl, err := m.st.CreatePlan(ctx, store.Plan{ChatID: chatID, Title: p.Title, Session: session, ToolCallID: store.ToolCallFrom(ctx), Via: via, Steps: steps})
	if err != nil {
		return platform.Result{}, err
	}
	ap, err := m.st.CreateApproval(ctx, store.Approval{ChatID: chatID, Kind: "plan", Via: via, Name: plan.ApprovalName(len(steps)), Size: int64(len(steps)),
		ContentType: "application/json", PendingKey: "-", Preview: preview, Summary: plan.ApprovalSummary(steps), Session: session, ToolCallID: store.ToolCallFrom(ctx)})
	if err != nil {
		return platform.Result{}, err
	}
	if err := m.st.SetPlanApproval(ctx, pl.ID, ap.ID); err != nil {
		return platform.Result{}, err
	}
	// The reads of the summaries are one activity entry of the system (issue L2/#119), and while the plan waits,
	// approving re-reads them first (DL2.1).
	m.logSummaryReads(ctx, chatID, ap.ID, "proposed", rl)
	defer m.registerRecheck(ap.ID, m.planRecheck(chatID, ap.ID, p.Title, steps, calls))()
	w := m.broker.Register(ap.ID)
	if s, ok := m.pool.Get(slotID); ok {
		s.SetActivity("waiting_approval", "")
	}
	m.publish(chatID, Event{Kind: "approval", Data: ap})
	m.publishPlan(ctx, pl.ID)
	m.publishChat(ctx, chatID)
	slog.Info("plan proposed", "chat", chatID, "approval", ap.ID, "plan", pl.ID, "steps", len(steps), "via", via)
	approved, werr := w.Wait(ctx, m.opt.ApprovalTimeout)
	bg := context.WithoutCancel(ctx)
	final := m.settle(bg, chatID, ap.ID, approved, werr)
	if s, ok := m.pool.Get(slotID); ok {
		s.SetActivity("tool", "")
	}
	m.publishChat(bg, chatID)
	if final != store.ApprovalApproved {
		state, msg := plan.StateRejected, "plan not approved by the user; nothing executed. Ask the user what to change and propose a new plan"
		if final == store.ApprovalExpired {
			state, msg = plan.StateExpired, "no decision on the plan within the waiting time; nothing executed"
		}
		if _, err := m.st.DecidePlan(bg, pl.ID, state, nil); err != nil {
			slog.Error("plan decision not recorded", "plan", pl.ID, "error", err)
		}
		m.publishPlan(bg, pl.ID)
		return platform.Result{Status: "rejected", Message: msg}, nil
	}
	expires := planNow().Add(plan.ValidFor)
	ok, err := m.st.DecidePlan(bg, pl.ID, plan.StateApproved, &expires)
	if err != nil {
		return platform.Result{}, err
	}
	if !ok {
		return platform.Result{Status: "error", Message: "plan was decided elsewhere; nothing executed"}, nil
	}
	if err := m.st.SupersedePlans(bg, chatID, session, pl.ID); err != nil {
		slog.Error("older plans not superseded", "chat", chatID, "plan", pl.ID, "error", err)
	}
	m.publishPlan(bg, pl.ID)
	stored, err := m.st.GetPlan(bg, pl.ID)
	if err != nil {
		return platform.Result{}, err
	}
	return platform.Result{Status: "ok", HTTPStatus: 200, Body: plan.ApprovedText(stored)}, nil
}

// summaryReader reads the platform as the user for the summaries of approvals (issue #72): only reading GETs, straight
// through the platform client. These reads belong to the gateway, not to the agent; they are logged as one
// summary_read entry of the orchestrator (issue L2/#119), never as a call of the agent. Use loggedSummaryReader
// wherever a summary is written or re-read.
func (m *Manager) summaryReader(chatID string) platform.Doer {
	if m.opt.Platform == nil {
		return nil
	}
	return func(ctx context.Context, r platform.Request) (platform.Result, error) {
		if r.Method != http.MethodGet || r.Writes() {
			return platform.Result{Status: "error", Message: "not a reading call"}, nil
		}
		return m.opt.Platform.Do(ctx, chatID, r)
	}
}

// planMatch returns the next step of the session's approved plan if the call is exactly that step.
// A covered step is claimed (open to running) and the caller runs it without a new approval; an
// irreversible step is returned unclaimed, so the call still asks the user. nil: no match.
func (m *Manager) planMatch(ctx context.Context, chatID string, req platform.Request) *store.PlanStep {
	p, err := m.st.ActivePlan(ctx, chatID, store.SessionFrom(ctx))
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			slog.Error("active plan not readable", "chat", chatID, "error", err)
		}
		return nil
	}
	if p.ExpiresAt != nil && !planNow().Before(*p.ExpiresAt) {
		if ok, err := m.st.SetPlanState(ctx, p.ID, plan.StateApproved, plan.StateExpired); err == nil && ok {
			m.publishPlan(ctx, p.ID)
		}
		return nil
	}
	var next *store.PlanStep
	for i := range p.Steps {
		if p.Steps[i].State != plan.StepDone {
			next = &p.Steps[i]
			break
		}
	}
	if next == nil || next.State != plan.StepOpen {
		return nil
	}
	resolve := func(n int) (string, bool) {
		if n < 1 || n > len(p.Steps) {
			return "", false
		}
		ref := p.Steps[n-1]
		if ref.State != plan.StepDone || ref.CreatedID == "" {
			return "", false
		}
		ok, err := m.st.IsDelegationObject(ctx, chatID, ref.CreatedResource, ref.CreatedID)
		return ref.CreatedID, err == nil && ok
	}
	var ok bool
	var why string
	if plan.IsToolStep(*next) {
		// A tool step matches the tool invocation, not the HTTP request: only the socket sets it, so a raw
		// request with the same target is not covered (issue #70).
		inv, has := platform.InvocationFrom(ctx)
		if !has {
			ok, why = false, "no tool invocation"
		} else {
			ok, why = plan.MatchesTool(*next, inv.Tool, inv.Args, req, resolve)
		}
	} else {
		ok, why = plan.Matches(*next, req, resolve)
	}
	if !ok {
		slog.Info("call is not the plan's next step", "chat", chatID, "plan", p.ID, "step", next.Idx, "differs_in", why)
		return nil
	}
	if !next.Covered {
		return next
	}
	claimed, err := m.st.ClaimPlanStep(ctx, p.ID, next.Idx)
	if err != nil || !claimed {
		return nil
	}
	next.State = plan.StepRunning
	return next
}

// planFinish records the outcome of a step. A created object enters the provenance register, so a
// later step can name it. A failed step fails the plan; when every step is done the plan completes.
func (m *Manager) planFinish(ctx context.Context, chatID string, st *store.PlanStep, access delegation.Access, res platform.Result, callErr error) {
	if st == nil {
		return
	}
	bg := context.WithoutCancel(ctx)
	from := plan.StepOpen
	if st.Covered {
		from = plan.StepRunning
	}
	to, resource, id := plan.StepFailed, "", ""
	result := fmt.Sprintf("%s %d", res.Status, res.HTTPStatus)
	if callErr != nil {
		result = callErr.Error()
	} else if res.Status == "ok" && res.HTTPStatus >= 200 && res.HTTPStatus < 300 {
		to = plan.StepDone
		if r, i, ok := delegation.Created(access, res); ok {
			resource, id = r, i
			if err := m.st.AddDelegationObject(bg, chatID, r, i); err != nil {
				slog.Error("provenance of a plan step not recorded", "chat", chatID, "object", r+" "+i, "error", err)
			}
		}
	}
	ok, err := m.st.FinishPlanStep(bg, st.PlanID, st.Idx, from, to, result, resource, id)
	if err != nil || !ok {
		slog.Error("plan step not finished", "chat", chatID, "plan", st.PlanID, "step", st.Idx, "error", err)
		return
	}
	if to == plan.StepFailed {
		if _, err := m.st.SetPlanState(bg, st.PlanID, plan.StateApproved, plan.StateFailed); err != nil {
			slog.Error("plan not failed", "plan", st.PlanID, "error", err)
		}
	} else if p, err := m.st.GetPlan(bg, st.PlanID); err == nil && allDone(p) {
		if _, err := m.st.SetPlanState(bg, st.PlanID, plan.StateApproved, plan.StateCompleted); err != nil {
			slog.Error("plan not completed", "plan", st.PlanID, "error", err)
		}
	}
	m.publishPlan(bg, st.PlanID)
}

func allDone(p store.Plan) bool {
	for _, s := range p.Steps {
		if s.State != plan.StepDone {
			return false
		}
	}
	return true
}

// publishPlan sends the plan's current state to the subscribers of its chat.
func (m *Manager) publishPlan(ctx context.Context, planID string) {
	p, err := m.st.GetPlan(ctx, planID)
	if err != nil {
		slog.Error("plan event not published", "plan", planID, "error", err)
		return
	}
	m.publish(p.ChatID, Event{Kind: "plan", Data: p})
}

// Plans lists the plans of a chat, oldest first.
func (m *Manager) Plans(ctx context.Context, chatID string) ([]store.Plan, error) {
	return m.st.ListPlans(ctx, chatID)
}

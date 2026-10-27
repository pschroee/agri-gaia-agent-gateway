// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"agw/internal/delegation"
	"agw/internal/plan"
	"agw/internal/platform"
	"agw/internal/search"
	"agw/internal/store"
)

// A training search as a plan step (issue #244, K1). The approval of the plan covers the search: the call of the
// agent is checked like a search of its own (base container, space against the template, hold-out, compute calls),
// but when it is exactly the next step of the approved plan, no card is shown and nobody waits. The step then
// waits until the search ends and is done with the training container of the winner as its object.

// searchStepProblems refuses what a plan with a search step cannot hold, for the proposal (issue #244, E4, E9): a
// search in an experiment chat, and more compute calls than the chat has left, counting the trials of the search.
func (m *Manager) searchStepProblems(ctx context.Context, chatID string, steps []store.PlanStep) error {
	searches := 0
	for _, st := range steps {
		if plan.IsSearchStep(st) {
			searches++
		}
	}
	if searches == 0 {
		return nil
	}
	var probs plan.Problems
	if m.experimentOf(ctx, chatID) != nil {
		for _, st := range steps {
			if plan.IsSearchStep(st) {
				probs = append(probs, plan.Problem{Step: st.Idx, Msg: "a training search is not available in an experiment chat"})
			}
		}
		return probs
	}
	runs := plan.SearchRuns(steps)
	need := plan.ComputeSteps(steps)
	if left := m.computeLeft(ctx, chatID); need > left {
		first := 0
		for _, st := range steps {
			if plan.IsSearchStep(st) {
				first = st.Idx
				break
			}
		}
		probs = append(probs, plan.Problem{Step: first, Msg: fmt.Sprintf("the plan needs %d compute calls (search: %d trials), this chat has %d left", need, runs, left)})
		return probs
	}
	return nil
}

// planExpiry is how long an approved interactive plan stays valid: the usual time, plus the compute minutes of its
// search step (at most the longest a search runs), so that the cover does not end in the middle of the search.
func (m *Manager) planExpiry(steps []store.PlanStep) time.Duration {
	d := plan.ValidFor
	if extra := plan.SearchMinutes(steps); extra > 0 {
		d += time.Duration(extra) * time.Minute
		if limit := time.Duration(m.searchMaxHours()) * time.Hour; d > limit && limit > plan.ValidFor {
			d = limit
		}
	}
	return d
}

// startSearchByPlan starts the search that the claimed plan step stands for. The step is claimed already; every
// failure from here on fails it.
func (m *Manager) startSearchByPlan(ctx context.Context, chatID string, step *store.PlanStep, p search.Proposal, data SearchApprovalData,
	base json.RawMessage, req platform.Request) (platform.Result, error) {
	fail := func(msg string) (platform.Result, error) {
		m.planFinish(ctx, chatID, step, delegation.Access{}, req, platform.Result{Status: "error", Message: msg}, errors.New(msg))
		return searchErr(msg)
	}
	pl, err := m.st.GetPlan(ctx, step.PlanID)
	if err != nil {
		return fail("the plan of this step is not readable")
	}
	var b [6]byte
	u := uuid.New()
	copy(b[:], u[:6])
	searchID := "s" + hex.EncodeToString(b[:])
	data.SearchID = searchID
	holdout, _ := json.Marshal(data.Holdout)
	objective, _ := json.Marshal(p.Objective)
	space, _ := json.Marshal(p.Space)
	session := store.SessionFrom(ctx)
	if _, err := m.st.CreateSearch(ctx, store.Search{ID: searchID, ChatID: chatID, Title: data.Title, Path: p.Path, Objective: objective,
		Space: space, Budget: search.MarshalBudget(p.Budget), Base: base, Holdout: holdout, Note: data.Note, Session: session,
		ApprovalID: pl.ApprovalID, ToolCallID: store.ToolCallFrom(ctx)}); err != nil {
		return fail("the search could not be stored")
	}
	ends := planNow().Add(time.Duration(m.searchMaxHours()) * time.Hour)
	if se, ok := m.planSessionEnd(ctx, chatID); ok && se.Before(ends) {
		ends = se
	}
	if len(base) > 0 {
		if err := m.st.SetSearchBase(ctx, searchID, base); err != nil {
			return fail("the search could not be stored")
		}
	}
	if _, ok, err := m.st.DecideSearch(ctx, searchID, store.SearchRunning, &ends); err != nil || !ok {
		return fail("the search could not be started")
	}
	m.addSearchEvent(ctx, searchID, "approved", map[string]any{"ends_by": ends, "plan": pl.ID, "step": step.Idx})
	result := fmt.Sprintf("search %s started; the step is done when the search ends", searchID)
	if ok, err := m.st.WaitPlanStepSearch(ctx, pl.ID, step.Idx, searchID, result); err != nil || !ok {
		slog.Error("plan step does not wait for its search", "plan", pl.ID, "step", step.Idx, "search", searchID, "error", err)
	}
	m.publishSearch(ctx, chatID, searchID)
	m.publishPlan(context.WithoutCancel(ctx), pl.ID)
	slog.Info("search started by plan", "chat", chatID, "plan", pl.ID, "step", step.Idx, "search", searchID)
	idx := step.Idx
	text := fmt.Sprintf("Search %s approved by plan %s step %d: %q, %s, objective %s (%s). ", searchID, pl.ID, step.Idx, data.Title,
		budgetText(data.Budget), p.Objective.Metric, p.Objective.Mode) + searchGoOnText
	text += fmt.Sprintf(" Plan step %d is done when the search ends; {{step%d.id}} is then the training container of the winner.", step.Idx, step.Idx)
	return platform.Result{Status: "ok", HTTPStatus: 200, Body: text, PlanStep: &idx, PlanID: pl.ID}, nil
}

// searchStepEnded finishes the plan step that waits for a search that has ended (issue #244, E3): done, with the
// training container of the winner as the object the step created, or failed when no trial finished.
func (m *Manager) searchStepEnded(ctx context.Context, s store.Search, winner *store.Trial) {
	planID, idx, err := m.st.PlanStepOfSearch(ctx, s.ID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			slog.Error("plan step of a search not readable", "search", s.ID, "error", err)
		}
		return
	}
	to, result, resource, id := plan.StepDone, "", "", ""
	if winner != nil && winner.State == store.TrialDone && winner.Ref != "" && s.Path == store.SearchPathTemplate {
		resource, id = delegation.Training, winner.Ref
		result = fmt.Sprintf("search %s ended, winner: training container %s", s.ID, winner.Ref)
		m.recordDelegationObject(ctx, s.ChatID, resource, id)
	} else {
		to, result = plan.StepFailed, fmt.Sprintf("search %s ended without a winner", s.ID)
	}
	ok, err := m.st.FinishPlanStep(ctx, planID, idx, plan.StepWaiting, to, result, resource, id)
	if err != nil || !ok {
		slog.Error("search step not finished", "plan", planID, "step", idx, "error", err)
		return
	}
	pl, gerr := m.st.GetPlan(ctx, planID)
	switch {
	case to == plan.StepFailed:
		if _, err := m.st.FinishPlan(ctx, planID, plan.StateFailed); err != nil {
			slog.Error("plan not failed", "plan", planID, "error", err)
		}
	case gerr == nil && allDone(pl):
		if _, err := m.st.FinishPlan(ctx, planID, plan.StateCompleted); err != nil {
			slog.Error("plan not completed", "plan", planID, "error", err)
		}
	}
	if to == plan.StepDone {
		// The approval counts its minutes again from the end of the search, like after a step that waited for a job.
		if _, err := m.st.ExtendPlan(ctx, planID, planNow().Add(plan.ValidFor)); err != nil {
			slog.Error("plan validity not extended", "plan", planID, "error", err)
		}
	}
	m.publishPlan(ctx, planID)
}

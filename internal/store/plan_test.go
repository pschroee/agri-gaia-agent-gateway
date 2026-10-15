// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func newPlanChat(t *testing.T, s *Store) Chat {
	t.Helper()
	c, err := s.CreateChat(context.Background(), NewChat{Title: "Plan", Model: "deepseek/deepseek-flash", Variant: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func twoStepPlan(chatID, session string) Plan {
	return Plan{ChatID: chatID, Title: "Model and image", Session: session, Via: "cli", Steps: []PlanStep{
		{Idx: 1, Method: "GET", Path: "/train/containers/7/model", Effect: "write"},
		{Idx: 2, Method: "DELETE", Path: "/models/{{step1.id}}", Effect: "irreversible"},
	}}
}

func TestPlanLifecycle(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c := newPlanChat(t, s)
	p, err := s.CreatePlan(ctx, twoStepPlan(c.ID, "main"))
	if err != nil {
		t.Fatal(err)
	}
	if p.State != "proposed" || len(p.Steps) != 2 {
		t.Fatalf("new plan: %+v", p)
	}
	if p.Steps[0].State != "open" || !p.Steps[0].Covered || p.Steps[1].Covered {
		t.Fatalf("steps: %+v", p.Steps)
	}
	expires := time.Now().Add(30 * time.Minute)
	if ok, err := s.DecidePlan(ctx, p.ID, "approved", &expires); err != nil || !ok {
		t.Fatalf("decide: ok=%v err=%v", ok, err)
	}
	a, err := s.ActivePlan(ctx, c.ID, "main")
	if err != nil || a.ID != p.ID {
		t.Fatalf("active plan: %+v %v", a, err)
	}
	if ok, err := s.ClaimPlanStep(ctx, p.ID, 1); err != nil || !ok {
		t.Fatalf("first claim: ok=%v err=%v", ok, err)
	}
	if ok, err := s.ClaimPlanStep(ctx, p.ID, 1); err != nil || ok {
		t.Fatalf("second claim must fail: ok=%v err=%v", ok, err)
	}
	if ok, err := s.FinishPlanStep(ctx, p.ID, 1, "running", "done", "ok", "models", "42"); err != nil || !ok {
		t.Fatalf("finish: ok=%v err=%v", ok, err)
	}
	list, err := s.ListPlans(ctx, c.ID)
	if err != nil || len(list) != 1 || len(list[0].Steps) != 2 {
		t.Fatalf("list: %+v %v", list, err)
	}
	if st := list[0].Steps[0]; st.State != "done" || st.CreatedID != "42" {
		t.Fatalf("step 1 after finish: %+v", st)
	}
}

func TestSupersedePlans(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c := newPlanChat(t, s)
	p1, err := s.CreatePlan(ctx, twoStepPlan(c.ID, "main"))
	if err != nil {
		t.Fatal(err)
	}
	p2, err := s.CreatePlan(ctx, twoStepPlan(c.ID, "main"))
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Now().Add(30 * time.Minute)
	for _, id := range []string{p1.ID, p2.ID} {
		if ok, err := s.DecidePlan(ctx, id, "approved", &expires); err != nil || !ok {
			t.Fatalf("decide %s: ok=%v err=%v", id, ok, err)
		}
	}
	if err := s.SupersedePlans(ctx, c.ID, "main", p2.ID); err != nil {
		t.Fatal(err)
	}
	old, err := s.GetPlan(ctx, p1.ID)
	if err != nil || old.State != "superseded" {
		t.Fatalf("old plan: %+v %v", old, err)
	}
	active, err := s.ActivePlan(ctx, c.ID, "main")
	if err != nil || active.ID != p2.ID {
		t.Fatalf("active after supersede: %+v %v", active, err)
	}
}

func TestRecoverPlans(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c := newPlanChat(t, s)
	proposed, err := s.CreatePlan(ctx, twoStepPlan(c.ID, "main"))
	if err != nil {
		t.Fatal(err)
	}
	running, err := s.CreatePlan(ctx, twoStepPlan(c.ID, "main"))
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Now().Add(30 * time.Minute)
	if ok, err := s.DecidePlan(ctx, running.ID, "approved", &expires); err != nil || !ok {
		t.Fatalf("decide: ok=%v err=%v", ok, err)
	}
	if ok, err := s.ClaimPlanStep(ctx, running.ID, 1); err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	if err := s.RecoverPlans(ctx); err != nil {
		t.Fatal(err)
	}
	p, err := s.GetPlan(ctx, proposed.ID)
	if err != nil || p.State != "expired" {
		t.Fatalf("proposed plan after restart: %+v %v", p, err)
	}
	r, err := s.GetPlan(ctx, running.ID)
	if err != nil || r.State != "failed" {
		t.Fatalf("running plan after restart: %+v %v", r, err)
	}
	if st := r.Steps[0]; st.State != "failed" || st.Result != "orchestrator restarted" {
		t.Fatalf("running step after restart: %+v", st)
	}
}

// The gateway's summary is stored with a step and an approval; rows from before issue #72 have none and still read.
func TestPlanAndApprovalSummary(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c := newPlanChat(t, s)
	p := twoStepPlan(c.ID, "main")
	p.Steps[0].Summary = "Create a new model from training container 7"
	p.Steps[0].Description = "the agent's note"
	stored, err := s.CreatePlan(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Steps[0].Summary != "Create a new model from training container 7" || stored.Steps[0].Description != "the agent's note" {
		t.Fatalf("step 1: %+v", stored.Steps[0])
	}
	// an old row: the column is empty, the JSON leaves the summary out
	if stored.Steps[1].Summary != "" {
		t.Fatalf("step 2: %+v", stored.Steps[1])
	}
	raw, _ := json.Marshal(stored.Steps[1])
	if strings.Contains(string(raw), "summary") {
		t.Fatalf("empty summary in JSON: %s", raw)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE plan_steps SET summary=DEFAULT WHERE plan_id=$1`, stored.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := s.GetPlan(ctx, stored.ID); err != nil || got.Steps[0].Summary != "" || got.Steps[0].Method != "GET" {
		t.Fatalf("old plan: %+v %v", got, err)
	}

	a, err := s.CreateApproval(ctx, Approval{ChatID: c.ID, Kind: "platform_write", Via: "cli", Name: "PATCH /datasets/16",
		ContentType: "application/json", PendingKey: "-", Summary: `Rename dataset 16 "a" → "b"`})
	if err != nil || a.Summary != `Rename dataset 16 "a" → "b"` {
		t.Fatalf("approval: %+v %v", a, err)
	}
	old, err := s.CreateApproval(ctx, Approval{ChatID: c.ID, Kind: "platform_write", Via: "cli", Name: "PATCH /datasets/17",
		ContentType: "application/json", PendingKey: "-"})
	if err != nil || old.Summary != "" {
		t.Fatalf("approval without summary: %+v %v", old, err)
	}
	if raw, _ := json.Marshal(old); strings.Contains(string(raw), "summary") {
		t.Fatalf("empty summary in JSON: %s", raw)
	}
}

// A-E1: an autonomous plan stores its mode, budget, grant, replaces, ended_at and its steps' attempts.
func TestAutonomousPlanColumns(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c := newPlanChat(t, s)
	p, err := s.CreatePlan(ctx, Plan{ChatID: c.ID, Title: "Auto", Session: "main", Via: "cli", Mode: "autonomous",
		Budget:   PlanBudget{MaxDurationS: 3600, MaxLLMCost: 1.5, MaxAutoTurns: 20},
		Grant:    json.RawMessage(`{"rules":[{"action":"create","resource":"dataset"}],"enforce":true}`),
		Replaces: "00000000-0000-0000-0000-0000000000aa",
		Steps: []PlanStep{
			{Idx: 1, Method: "POST", Path: "/datasets", Effect: "write", MaxAttempts: 2},
			{Idx: 2, Method: "DELETE", Path: "/models/5", Effect: "irreversible"},
		}})
	if err != nil {
		t.Fatal(err)
	}
	if p.Mode != "autonomous" || p.Budget.MaxDurationS != 3600 || p.Budget.MaxLLMCost != 1.5 || p.Budget.MaxAutoTurns != 20 {
		t.Fatalf("budget: %+v", p)
	}
	if string(p.Grant) == "" || p.Replaces == "" || p.EndedAt != nil {
		t.Fatalf("grant/replaces/ended: %+v", p)
	}
	if p.Steps[0].MaxAttempts != 2 || p.Steps[1].MaxAttempts != 1 {
		t.Fatalf("max_attempts: %+v", p.Steps)
	}
	// An interactive plan keeps the default mode and zero budget.
	q, err := s.CreatePlan(ctx, twoStepPlan(c.ID, "main"))
	if err != nil {
		t.Fatal(err)
	}
	if q.Mode != ModeInteractive || q.Budget.MaxDurationS != 0 || q.Grant != nil {
		t.Fatalf("interactive plan: %+v", q)
	}
}

// A-E1: the pause/resume/cancel transitions; invalid transitions do not take effect, cancel skips open steps.
func TestPauseResumeCancel(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c := newPlanChat(t, s)
	p, err := s.CreatePlan(ctx, twoStepPlan(c.ID, "main"))
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Now().Add(time.Hour)
	if ok, err := s.DecidePlan(ctx, p.ID, "approved", &expires); err != nil || !ok {
		t.Fatalf("decide: %v %v", ok, err)
	}
	// Resume before pause does nothing.
	if ok, err := s.ResumePlan(ctx, p.ID); err != nil || ok {
		t.Fatalf("resume from approved: %v %v", ok, err)
	}
	if ok, err := s.PausePlan(ctx, p.ID, "user"); err != nil || !ok {
		t.Fatalf("pause: %v %v", ok, err)
	}
	if ok, err := s.PausePlan(ctx, p.ID, "user"); err != nil || ok {
		t.Fatalf("second pause: %v %v", ok, err)
	}
	if got, err := s.GetPlan(ctx, p.ID); err != nil || got.State != "paused" || got.PauseReason != "user" {
		t.Fatalf("paused plan: %+v %v", got, err)
	}
	if ok, err := s.ResumePlan(ctx, p.ID); err != nil || !ok {
		t.Fatalf("resume: %v %v", ok, err)
	}
	if got, _ := s.GetPlan(ctx, p.ID); got.State != "approved" || got.PauseReason != "" {
		t.Fatalf("resumed plan: %+v", got)
	}
	if _, err := s.ActiveAutonomousPlan(ctx, c.ID); err == nil {
		t.Fatalf("an interactive plan must not be the active autonomous one")
	}
	if ok, err := s.CancelPlan(ctx, p.ID); err != nil || !ok {
		t.Fatalf("cancel: %v %v", ok, err)
	}
	got, err := s.GetPlan(ctx, p.ID)
	if err != nil || got.State != "cancelled" || got.EndedAt == nil {
		t.Fatalf("cancelled plan: %+v %v", got, err)
	}
	if got.Steps[0].State != "skipped" || got.Steps[1].State != "skipped" {
		t.Fatalf("open steps must be skipped: %+v", got.Steps)
	}
	if ok, err := s.CancelPlan(ctx, p.ID); err != nil || ok {
		t.Fatalf("second cancel: %v %v", ok, err)
	}
}

// A-E1: a failed step retries only while attempts < max_attempts.
func TestRetryPlanStep(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c := newPlanChat(t, s)
	p, err := s.CreatePlan(ctx, Plan{ChatID: c.ID, Title: "retry", Session: "main", Via: "cli", Mode: "autonomous",
		Steps: []PlanStep{{Idx: 1, Method: "POST", Path: "/datasets", Effect: "write", MaxAttempts: 2}}})
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Now().Add(time.Hour)
	s.DecidePlan(ctx, p.ID, "approved", &expires)
	if ok, err := s.ClaimPlanStep(ctx, p.ID, 1); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	if got, _ := s.GetPlan(ctx, p.ID); got.Steps[0].Attempts != 1 {
		t.Fatalf("attempts after claim: %+v", got.Steps[0])
	}
	if ok, err := s.FinishPlanStep(ctx, p.ID, 1, "running", "failed", "500", "", ""); err != nil || !ok {
		t.Fatalf("fail step: %v %v", ok, err)
	}
	if ok, err := s.RetryPlanStep(ctx, p.ID, 1); err != nil || !ok {
		t.Fatalf("first retry: %v %v", ok, err)
	}
	// second try fails and no attempt is left
	s.ClaimPlanStep(ctx, p.ID, 1)
	s.FinishPlanStep(ctx, p.ID, 1, "running", "failed", "500", "", "")
	if ok, err := s.RetryPlanStep(ctx, p.ID, 1); err != nil || ok {
		t.Fatalf("retry beyond max_attempts: %v %v", ok, err)
	}
}

// A-E1: PlanLLMCost sums only the calls from the given moment on.
func TestPlanLLMCost(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c := newPlanChat(t, s)
	since := time.Now()
	for _, cost := range []float64{0.10, 0.20} {
		if _, err := s.AddLLMCall(ctx, LLMCall{ChatID: c.ID, SlotID: "s", SourceIP: "1.2.3.4", Model: "p/m", StartedAt: since.Add(time.Second), Cost: cost}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.AddLLMCall(ctx, LLMCall{ChatID: c.ID, SlotID: "s", SourceIP: "1.2.3.4", Model: "p/m", StartedAt: since.Add(-time.Minute), Cost: 9}); err != nil {
		t.Fatal(err)
	}
	got, err := s.PlanLLMCost(ctx, c.ID, since)
	if err != nil || got < 0.29 || got > 0.31 {
		t.Fatalf("cost since: %v %v", got, err)
	}
}

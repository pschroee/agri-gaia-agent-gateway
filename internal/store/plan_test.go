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

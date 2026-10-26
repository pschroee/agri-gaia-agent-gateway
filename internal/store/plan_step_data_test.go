// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"encoding/json"
	"testing"
)

// Issue #221: a platform call of a plan step is stored with plan and step, and the Activity separates two plans
// whose steps carry the same number.
func TestActivityFilterPlanStep(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	mk := func(owner string) Chat {
		c, err := s.CreateChat(ctx, NewChat{Title: "Plan", Model: "p/m", Variant: "cli", Owner: owner})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	anna, bert := mk("sub-anna"), mk("sub-bert")
	p1, err := s.CreatePlan(ctx, twoStepPlan(anna.ID, "main"))
	if err != nil {
		t.Fatal(err)
	}
	p2, err := s.CreatePlan(ctx, twoStepPlan(anna.ID, "other"))
	if err != nil {
		t.Fatal(err)
	}
	step := func(n int) *int { return &n }
	add := func(chat, detail string, plan string, n int) {
		rec := CallRecord{SlotID: "p", ChatID: chat, Via: "cli", Op: "platform", Detail: detail, Result: "ok 200"}
		if plan != "" {
			rec.PlanID, rec.PlanStep = plan, step(n)
		}
		if _, err := s.AddSocketCallRecord(ctx, rec); err != nil {
			t.Fatal(err)
		}
	}
	add(anna.ID, "POST /a", p1.ID, 1)
	add(anna.ID, "POST /b", p2.ID, 1)
	add(anna.ID, "POST /c", p2.ID, 2)
	add(anna.ID, "GET /outside", "", 0)
	owner := "sub-anna"
	page, err := s.ListActivity(ctx, ActivityFilter{Owner: &owner, PlanID: p1.ID, PlanStep: 1})
	if err != nil || len(page.Calls) != 1 || page.Calls[0].Detail != "POST /a" {
		t.Fatalf("plan 1 step 1: %+v %v", page.Calls, err)
	}
	if c := page.Calls[0]; c.PlanID != p1.ID || c.PlanStep == nil || *c.PlanStep != 1 {
		t.Fatalf("call carries plan and step: %+v", c)
	}
	page, _ = s.ListActivity(ctx, ActivityFilter{Owner: &owner, PlanID: p2.ID})
	if len(page.Calls) != 2 {
		t.Fatalf("plan 2 without step: %+v", page.Calls)
	}
	page, _ = s.ListActivity(ctx, ActivityFilter{Owner: &owner, PlanID: p2.ID, PlanStep: 2})
	if len(page.Calls) != 1 || page.Calls[0].Detail != "POST /c" {
		t.Fatalf("plan 2 step 2: %+v", page.Calls)
	}
	other := "sub-bert"
	_ = bert
	page, _ = s.ListActivity(ctx, ActivityFilter{Owner: &other, PlanID: p1.ID, PlanStep: 1})
	if len(page.Calls) != 0 {
		t.Fatalf("a foreign owner sees nothing: %+v", page.Calls)
	}
	list, _ := s.ListSocketCalls(ctx, anna.ID)
	if len(list) != 4 || list[3].PlanID != "" || list[3].PlanStep != nil {
		t.Fatalf("call outside a plan has neither: %+v", list)
	}
}

func TestClaimPlanStepSetsClaimedAt(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c := newPlanChat(t, s)
	p, err := s.CreatePlan(ctx, twoStepPlan(c.ID, "main"))
	if err != nil {
		t.Fatal(err)
	}
	if st := p.Steps[0]; st.StepStartedAt != nil {
		t.Fatalf("an open step has no start: %+v", st)
	}
	if ok, err := s.ClaimPlanStep(ctx, p.ID, 1); err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	list, _ := s.ListPlans(ctx, c.ID)
	st := list[0].Steps[0]
	if st.StepStartedAt == nil {
		t.Fatalf("claimed step has step_started_at: %+v", st)
	}
	b, _ := json.Marshal(st)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	if _, ok := m["step_started_at"]; !ok {
		t.Fatalf("JSON carries step_started_at: %s", b)
	}
	if list[0].Steps[1].StepStartedAt != nil {
		t.Fatal("the other step stays without start")
	}
}

// The last epoch of a job comes back on the job and on the plan step that waits for it; a poll without an epoch
// keeps the stored value.
func TestUpdateJobProgressLastEpoch(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c := newPlanChat(t, s)
	p, err := s.CreatePlan(ctx, twoStepPlan(c.ID, "main"))
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.AddWatchJob(ctx, WatchJob{ChatID: c.ID, PlanID: p.ID, StepIdx: 1, Kind: "training_run", Ref: "abc"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE plan_steps SET watch_job=$3 WHERE plan_id=$1 AND idx=$2`, p.ID, 1, id); err != nil {
		t.Fatal(err)
	}
	e, _ := json.Marshal(EpochFigures{Epoch: 3, Epochs: 10, Values: map[string]float64{"val_accuracy": 0.9}})
	if _, err := s.UpdateJobProgress(ctx, id, WatchProgress{State: JobRunning, LastEpoch: e}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateJobProgress(ctx, id, WatchProgress{State: JobRunning}); err != nil {
		t.Fatal(err)
	}
	list, _ := s.ListPlans(ctx, c.ID)
	var got EpochFigures
	if err := json.Unmarshal(list[0].Steps[0].LastEpoch, &got); err != nil || got.Epoch != 3 || got.Values["val_accuracy"] != 0.9 {
		t.Fatalf("step last_epoch: %s %v", list[0].Steps[0].LastEpoch, err)
	}
	jobs, _ := s.ChatWatchJobs(ctx, c.ID)
	if len(jobs) != 1 || len(jobs[0].LastEpoch) == 0 {
		t.Fatalf("job last_epoch: %+v", jobs)
	}
}

// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// #169: ExtendPlan only moves the end of an approved interactive plan later.
func TestExtendPlanOnlyInteractiveApproved(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c := newPlanChat(t, s)
	now := time.Now()

	p, err := s.CreatePlan(ctx, twoStepPlan(c.ID, "main"))
	if err != nil {
		t.Fatal(err)
	}
	// Not approved yet: nothing changes.
	if ok, err := s.ExtendPlan(ctx, p.ID, now.Add(time.Hour)); err != nil || ok {
		t.Fatalf("proposed plan: ok=%v err=%v", ok, err)
	}
	soon := now.Add(10 * time.Minute)
	if ok, err := s.DecidePlan(ctx, p.ID, "approved", &soon); err != nil || !ok {
		t.Fatalf("decide: %v %v", ok, err)
	}
	// Later: moved. Earlier: never shortened.
	if ok, err := s.ExtendPlan(ctx, p.ID, now.Add(40*time.Minute)); err != nil || !ok {
		t.Fatalf("extend: ok=%v err=%v", ok, err)
	}
	got, _ := s.GetPlan(ctx, p.ID)
	if got.ExpiresAt == nil || got.ExpiresAt.Before(now.Add(39*time.Minute)) {
		t.Fatalf("expires_at not moved: %v", got.ExpiresAt)
	}
	before := *got.ExpiresAt
	if _, err := s.ExtendPlan(ctx, p.ID, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetPlan(ctx, p.ID)
	if !got.ExpiresAt.Equal(before) {
		t.Fatalf("expires_at was shortened: %v -> %v", before, got.ExpiresAt)
	}
	// An expired plan is not revived.
	if ok, err := s.SetPlanState(ctx, p.ID, "approved", "expired"); err != nil || !ok {
		t.Fatalf("expire: %v %v", ok, err)
	}
	if ok, err := s.ExtendPlan(ctx, p.ID, now.Add(time.Hour)); err != nil || ok {
		t.Fatalf("expired plan: ok=%v err=%v", ok, err)
	}
	// An autonomous plan keeps its own budget.
	a, err := s.CreatePlan(ctx, Plan{ChatID: c.ID, Title: "Auto", Session: "other", Via: "cli", Mode: "autonomous",
		Budget: PlanBudget{MaxDurationS: 28800, MaxLLMCost: 1, MaxAutoTurns: 20},
		Grant:  json.RawMessage(`{"rules":[]}`),
		Steps:  []PlanStep{{Idx: 1, Method: "PATCH", Path: "/models/5", Effect: "write"}}})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.DecidePlan(ctx, a.ID, "approved", &soon); err != nil || !ok {
		t.Fatalf("decide: %v %v", ok, err)
	}
	if ok, err := s.ExtendPlan(ctx, a.ID, now.Add(time.Hour)); err != nil || ok {
		t.Fatalf("autonomous plan: ok=%v err=%v", ok, err)
	}
}

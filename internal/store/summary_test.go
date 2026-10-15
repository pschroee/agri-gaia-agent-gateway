// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"testing"
)

// summaryTestChat creates a chat for the tests of issue L2/#119.
func summaryTestChat(t *testing.T, s *Store, ctx context.Context) Chat {
	t.Helper()
	c, err := s.CreateChat(ctx, NewChat{Title: "summary", Model: "p/m", Variant: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// A re-read at the moment of the approval replaces the summary and keeps the first proposal (DL2.4); an empty
// preview leaves the stored one alone.
func TestReplaceApprovalSummaryKeepsFirstProposed(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c := summaryTestChat(t, s, ctx)
	const first = `Rename dataset 16 "alt" → "neu"`
	ap, err := s.CreateApproval(ctx, Approval{ChatID: c.ID, Kind: "platform_write", Via: "cli", Name: "PATCH /datasets/16",
		PendingKey: "-", Preview: "preview 1", Summary: first})
	if err != nil {
		t.Fatal(err)
	}
	upd, ok, err := s.ReplaceApprovalSummary(ctx, ap.ID, `Rename dataset 16 "anderen" → "neu"`, "")
	if err != nil || !ok {
		t.Fatalf("first replace: ok=%v err=%v", ok, err)
	}
	if upd.Summary != `Rename dataset 16 "anderen" → "neu"` || upd.SummaryProposed != first || upd.SummaryChangedAt == nil {
		t.Fatalf("after the first change: %+v", upd)
	}
	if upd.Preview != "preview 1" {
		t.Fatalf("empty preview overwrote the stored one: %q", upd.Preview)
	}
	// The first proposal survives every further change.
	upd2, ok, err := s.ReplaceApprovalSummary(ctx, ap.ID, `Rename dataset 16 "drittens" → "neu"`, "preview 2")
	if err != nil || !ok {
		t.Fatalf("second replace: ok=%v err=%v", ok, err)
	}
	if upd2.Summary != `Rename dataset 16 "drittens" → "neu"` || upd2.SummaryProposed != first || upd2.Preview != "preview 2" {
		t.Fatalf("after the second change: %+v", upd2)
	}
	stored, err := s.GetApproval(ctx, ap.ID)
	if err != nil || stored.SummaryProposed != first || stored.SummaryChangedAt == nil {
		t.Fatalf("stored: %+v %v", stored, err)
	}
}

// Only a pending approval is replaced: a decided one keeps its text.
func TestReplaceApprovalSummaryOnlyPending(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c := summaryTestChat(t, s, ctx)
	ap, err := s.CreateApproval(ctx, Approval{ChatID: c.ID, Kind: "platform_write", Via: "cli", Name: "PATCH /datasets/16",
		PendingKey: "-", Summary: "before"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.DecideApproval(ctx, ap.ID, ApprovalRejected); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := s.ReplaceApprovalSummary(ctx, ap.ID, "after", ""); err != nil || ok {
		t.Fatalf("decided approval: ok=%v err=%v got=%+v", ok, err, got)
	}
	stored, err := s.GetApproval(ctx, ap.ID)
	if err != nil || stored.Summary != "before" || stored.SummaryProposed != "" || stored.SummaryChangedAt != nil {
		t.Fatalf("decided approval was overwritten: %+v %v", stored, err)
	}
}

// ReplacePlanStepSummaries keeps every step's first proposal (DL2.4) and writes only the steps it is given.
func TestReplacePlanStepSummaries(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c := summaryTestChat(t, s, ctx)
	pl, err := s.CreatePlan(ctx, Plan{ChatID: c.ID, Title: "t", Via: "cli", Steps: []PlanStep{
		{Idx: 1, Method: "GET", Path: "/datasets", Effect: "read", Summary: "one"},
		{Idx: 2, Method: "DELETE", Path: "/models/5", Effect: "irreversible", Summary: "two"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ReplacePlanStepSummaries(ctx, pl.ID, []string{"one new", "two new"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetPlan(ctx, pl.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Steps[0].Summary != "one new" || got.Steps[0].SummaryProposed != "one" ||
		got.Steps[1].Summary != "two new" || got.Steps[1].SummaryProposed != "two" {
		t.Fatalf("after the first change: %+v", got.Steps)
	}
	// A second change keeps the first proposal and leaves a step the list does not mention alone.
	if err := s.ReplacePlanStepSummaries(ctx, pl.ID, []string{"one again"}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetPlan(ctx, pl.ID)
	if got.Steps[0].Summary != "one again" || got.Steps[0].SummaryProposed != "one" || got.Steps[1].Summary != "two new" {
		t.Fatalf("after the second change: %+v", got.Steps)
	}
}

// PlanByApproval finds the plan of an approval request and says ErrNotFound for one without a plan.
func TestPlanByApproval(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c := summaryTestChat(t, s, ctx)
	pl, err := s.CreatePlan(ctx, Plan{ChatID: c.ID, Title: "t", Via: "cli", Steps: []PlanStep{
		{Idx: 1, Method: "PATCH", Path: "/datasets/16", Effect: "write", Summary: "one"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	ap, err := s.CreateApproval(ctx, Approval{ChatID: c.ID, Kind: "plan", Via: "cli", Name: "Plan with 1 step",
		Size: 1, ContentType: "application/json", PendingKey: "-", Summary: "Plan with 1 step\n1. one"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetPlanApproval(ctx, pl.ID, ap.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.PlanByApproval(ctx, ap.ID)
	if err != nil || got.ID != pl.ID || len(got.Steps) != 1 || got.Steps[0].Summary != "one" {
		t.Fatalf("plan by approval: %+v %v", got, err)
	}
	if _, err := s.PlanByApproval(ctx, "00000000-0000-0000-0000-000000000000"); err != ErrNotFound {
		t.Fatalf("approval without a plan: %v", err)
	}
}

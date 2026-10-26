// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func acrossChat(t *testing.T, s *Store, title, owner string) Chat {
	t.Helper()
	c, err := s.CreateChat(context.Background(), NewChat{Title: title, Model: "deepseek/deepseek-flash", Variant: "cli", Owner: owner})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// acrossPlan creates a plan with n steps and moves it to state (proposed stays as created).
func acrossPlan(t *testing.T, s *Store, chatID, title, mode, state string, n int) Plan {
	t.Helper()
	ctx := context.Background()
	steps := []PlanStep{}
	for i := 1; i <= n; i++ {
		steps = append(steps, PlanStep{Idx: i, Method: "GET", Path: fmt.Sprintf("/models/%d", i), Effect: "read"})
	}
	p := Plan{ChatID: chatID, Title: title, Session: "main", Via: "cli", Mode: mode, Steps: steps}
	if mode == "autonomous" {
		p.Budget = PlanBudget{MaxDurationS: 3600, MaxLLMCost: 1, MaxAutoTurns: 10}
		p.Grant = json.RawMessage(`{"rules":[]}`)
	}
	p, err := s.CreatePlan(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if state != "proposed" {
		exp := time.Now().Add(time.Hour)
		if ok, err := s.DecidePlan(ctx, p.ID, "approved", &exp); err != nil || !ok {
			t.Fatalf("decide: %v %v", ok, err)
		}
		if state != "approved" {
			if ok, err := s.SetPlanState(ctx, p.ID, "approved", state); err != nil || !ok {
				t.Fatalf("state %s: %v %v", state, ok, err)
			}
		}
	}
	return p
}

func titlesOf(items []PlanListItem) []string {
	out := []string{}
	for _, it := range items {
		out = append(out, it.Title)
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestListPlansAcrossOwnerAndTrash(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	anna, bert := "sub-anna", "sub-bert"
	ca, cb, ct := acrossChat(t, s, "Anna chat", anna), acrossChat(t, s, "Bert chat", bert), acrossChat(t, s, "Anna trashed", anna)
	acrossPlan(t, s, ca.ID, "A", "interactive", "proposed", 1)
	acrossPlan(t, s, cb.ID, "B", "interactive", "proposed", 1)
	acrossPlan(t, s, ct.ID, "T", "interactive", "proposed", 1)
	if ok, err := s.MoveToTrash(ctx, ct.ID, time.Now()); err != nil || !ok {
		t.Fatalf("trash: %v %v", ok, err)
	}
	items, total, err := s.ListPlansAcross(ctx, PlanListFilter{Owner: &anna, Limit: 25})
	if err != nil || total != 1 || len(items) != 1 || items[0].Title != "A" || items[0].ChatTitle != "Anna chat" {
		t.Fatalf("Anna: %v total=%d err=%v", titlesOf(items), total, err)
	}
	if _, total, _ := s.ListPlansAcross(ctx, PlanListFilter{Limit: 25}); total != 2 {
		t.Fatalf("all owners: total=%d, want 2 (the trashed chat stays out)", total)
	}
	titles, err := s.OwnerChatTitles(ctx, &anna)
	if err != nil || len(titles) != 1 || titles[ca.ID] != "Anna chat" {
		t.Fatalf("OwnerChatTitles: %v %v", titles, err)
	}
}

func TestListPlansAcrossOrder(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	o := "sub-anna"
	c := acrossChat(t, s, "chat", o)
	acrossPlan(t, s, c.ID, "old-done", "interactive", "completed", 1)
	acrossPlan(t, s, c.ID, "old-running", "autonomous", "approved", 1)
	acrossPlan(t, s, c.ID, "new-done", "interactive", "completed", 1)
	acrossPlan(t, s, c.ID, "new-proposed", "interactive", "proposed", 1)
	items, _, err := s.ListPlansAcross(ctx, PlanListFilter{Owner: &o, Limit: 25})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := titlesOf(items), []string{"new-proposed", "old-running", "new-done", "old-done"}; !equalStrings(got, want) {
		t.Fatalf("order: %v, want %v", got, want)
	}
}

func TestListPlansAcrossFilters(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	o := "sub-anna"
	c1, c2 := acrossChat(t, s, "one", o), acrossChat(t, s, "two", o)
	foreign := acrossChat(t, s, "foreign", "sub-bert")
	acrossPlan(t, s, c1.ID, "p-done", "interactive", "completed", 1)
	acrossPlan(t, s, c1.ID, "p-auto", "autonomous", "paused", 1)
	acrossPlan(t, s, c2.ID, "p-failed", "autonomous", "failed", 1)
	acrossPlan(t, s, foreign.ID, "p-foreign", "autonomous", "paused", 1)
	list := func(f PlanListFilter) []string {
		f.Owner, f.Limit = &o, 25
		items, _, err := s.ListPlansAcross(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		return titlesOf(items)
	}
	if got := list(PlanListFilter{States: []string{"paused", "failed"}}); len(got) != 2 {
		t.Errorf("states: %v", got)
	}
	if got := list(PlanListFilter{Mode: "autonomous"}); len(got) != 2 {
		t.Errorf("mode: %v", got)
	}
	if got := list(PlanListFilter{ChatID: c1.ID}); len(got) != 2 {
		t.Errorf("chat: %v", got)
	}
	if got := list(PlanListFilter{ChatID: foreign.ID}); len(got) != 0 {
		t.Errorf("foreign chat must give an empty page: %v", got)
	}
	if got := list(PlanListFilter{ChatID: "not-a-uuid"}); len(got) != 0 {
		t.Errorf("bad chat id: %v", got)
	}
}

func TestListPlansAcrossPaging(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	o := "sub-anna"
	c := acrossChat(t, s, "chat", o)
	for i := 0; i < 5; i++ {
		acrossPlan(t, s, c.ID, fmt.Sprintf("p%d", i), "interactive", "completed", 3)
	}
	p1, total, err := s.ListPlansAcross(ctx, PlanListFilter{Owner: &o, Limit: 2})
	if err != nil || total != 5 || len(p1) != 2 {
		t.Fatalf("page 1: %d total=%d err=%v", len(p1), total, err)
	}
	p3, total, err := s.ListPlansAcross(ctx, PlanListFilter{Owner: &o, Limit: 2, Offset: 4})
	if err != nil || total != 5 || len(p3) != 1 {
		t.Fatalf("page 3: %d total=%d err=%v", len(p3), total, err)
	}
	for _, it := range append(p1, p3...) {
		if len(it.Steps) != 3 {
			t.Fatalf("plan %s has %d steps, want 3", it.Title, len(it.Steps))
		}
		for i, st := range it.Steps {
			if st.Idx != i+1 || st.PlanID != it.ID {
				t.Fatalf("steps of %s out of order: %+v", it.Title, it.Steps)
			}
		}
	}
}

func TestRunningPlanCount(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	o := "sub-anna"
	c, ct := acrossChat(t, s, "chat", o), acrossChat(t, s, "trashed", o)
	other := acrossChat(t, s, "other", "sub-bert")
	acrossPlan(t, s, c.ID, "running", "autonomous", "approved", 1)
	acrossPlan(t, s, c.ID, "paused", "autonomous", "paused", 1)
	acrossPlan(t, s, c.ID, "waiting", "interactive", "proposed", 1)
	acrossPlan(t, s, c.ID, "done", "interactive", "completed", 1)
	acrossPlan(t, s, ct.ID, "in trash", "autonomous", "approved", 1)
	acrossPlan(t, s, other.ID, "foreign", "autonomous", "approved", 1)
	if _, err := s.MoveToTrash(ctx, ct.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if n, err := s.RunningPlanCount(ctx, &o); err != nil || n != 2 {
		t.Fatalf("Anna: %d %v, want 2", n, err)
	}
	if n, err := s.RunningPlanCount(ctx, nil); err != nil || n != 3 {
		t.Fatalf("all: %d %v, want 3", n, err)
	}
}

// An approved plan whose time is over counts as expired in the overview, also when no call of it came since.
func TestLapsedPlanIsExpired(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	o := "sub-anna"
	c := acrossChat(t, s, "chat", o)
	lapsed := acrossPlan(t, s, c.ID, "lapsed", "interactive", "approved", 1)
	acrossPlan(t, s, c.ID, "running", "interactive", "approved", 1)
	if _, err := s.pool.Exec(ctx, `UPDATE plans SET expires_at = now() - interval '1 hour' WHERE id = $1`, lapsed.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := s.RunningPlanCount(ctx, &o); err != nil || n != 1 {
		t.Fatalf("running: %d %v, want 1", n, err)
	}
	items, _, err := s.ListPlansAcross(ctx, PlanListFilter{Owner: &o})
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if it.ID == lapsed.ID && it.State != "expired" {
			t.Fatalf("lapsed plan: %s", it.State)
		}
	}
}

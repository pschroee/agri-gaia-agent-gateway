// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"testing"
	"time"

	"agw/internal/plan"
	"agw/internal/store"
)

// alertWait: how long these tests wait for a plan event.
const alertWait = 3 * time.Second

// drain empties what a subscriber has buffered right now.
func drain(ch <-chan Event) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

// autonomousPlan stores an approved autonomous plan with one open step in the chat.
func autonomousPlan(t *testing.T, e *env, chatID string) store.Plan {
	t.Helper()
	ctx := context.Background()
	pl, err := e.st.CreatePlan(ctx, store.Plan{ChatID: chatID, Title: "Train pigs v1", Via: "cli", Mode: plan.ModeAutonomous,
		Budget: store.PlanBudget{MaxDurationS: 3600, MaxLLMCost: 1.5, MaxAutoTurns: 20},
		Steps: []store.PlanStep{{Idx: 1, Method: "POST", Path: "/datasets", Effect: plan.EffectWrite, State: plan.StepOpen}}})
	if err != nil {
		t.Fatal(err)
	}
	ok, err := e.st.DecidePlan(ctx, pl.ID, plan.StateApproved, nil)
	if err != nil || !ok {
		t.Fatalf("plan not approved: %v %v", ok, err)
	}
	return pl
}

// alertOf reads a plan_alert of one chat, or fails (a while waiting, b at most once).
func alertOf(t *testing.T, ch <-chan Event, wantChat string) PlanAlert {
	t.Helper()
	for {
		select {
		case ev := <-ch:
			if ev.Kind != "plan_alert" {
				continue
			}
			a, ok := ev.Data.(PlanAlert)
			if !ok {
				t.Fatalf("plan_alert carries %T", ev.Data)
			}
			if a.ChatID != wantChat {
				t.Fatalf("alert of chat %s, want %s", a.ChatID, wantChat)
			}
			return a
		case <-time.After(alertWait):
			t.Fatalf("no plan alert for chat %s", wantChat)
		}
	}
}

func TestPlanAlertStates(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, err := e.st.CreateChat(ctx, store.NewChat{Title: "auto", Model: "deepseek/deepseek-flash", Variant: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	pl := autonomousPlan(t, e, c.ID)

	ch, cancel := e.m.Subscribe(c.ID)
	defer cancel()
	// The first publish is the state the plan already has: no alert goes out for what happened before.
	e.m.publishPlan(ctx, pl.ID)
	select {
	case ev := <-ch:
		if ev.Kind == "plan_alert" {
			t.Fatalf("alert for the plan's first state: %+v", ev.Data)
		}
	case <-time.After(alertWait):
		t.Fatal("the plan event was not published")
	}

	// Pausing is a change: one alert with the kind and the reason of the pause.
	if _, err := e.m.pausePlan(ctx, c.ID, pl.ID, "login"); err != nil {
		t.Fatal(err)
	}
	if a := alertOf(t, ch, c.ID); a.Kind != AlertPaused || a.Reason != "login" || a.PlanID != pl.ID {
		t.Fatalf("paused alert: %+v", a)
	}
	// Resuming and pausing again reports again: the mark follows the state, not the plan.
	if _, err := e.m.ResumePlan(ctx, c.ID, pl.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.pausePlan(ctx, c.ID, pl.ID, "abort"); err != nil {
		t.Fatal(err)
	}
	if a := alertOf(t, ch, c.ID); a.Kind != AlertPaused || a.Reason != "abort" {
		t.Fatalf("second paused alert: %+v", a)
	}

	// Cancelling ends the plan: the user did it, no alert follows.
	if _, err := e.m.CancelPlan(ctx, c.ID, pl.ID); err != nil {
		t.Fatal(err)
	}
	drain(ch)
	e.m.publishPlan(ctx, pl.ID)
	select {
	case ev := <-ch:
		if ev.Kind == "plan_alert" {
			t.Fatalf("alert for a cancelled plan: %+v", ev.Data)
		}
	case <-time.After(alertWait):
		t.Fatal("the plan event of the cancelled plan was not published")
	}
}

// A plan's alert goes to the subscribers of its chat and, with the chat's id, to the stream across chats
// (A-E4, D4.3); the API filters that stream by owner (TestPlanAlertCrossChatOwnerOnly).
func TestPlanAlertCrossChat(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	mk := func(title string) store.Chat {
		c, err := e.st.CreateChat(ctx, store.NewChat{Title: title, Model: "deepseek/deepseek-flash", Variant: "cli"})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	mine := mk("mine")
	_ = mk("other") // a second chat, so the stream across chats carries more than one chat's events
	pl := autonomousPlan(t, e, mine.ID)
	all, cancel := e.m.SubscribeAll()
	defer cancel()
	e.m.publishPlan(ctx, pl.ID) // the first state, no alert
	if _, err := e.m.pausePlan(ctx, mine.ID, pl.ID, "user"); err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case ev := <-all:
			if ev.Kind != "plan_alert" {
				continue
			}
			a, ok := ev.Data.(PlanAlert)
			if !ok || a.ChatID != mine.ID || a.PlanID != pl.ID || a.Kind != AlertPaused || a.Title != "Train pigs v1" {
				t.Fatalf("cross-chat alert: %#v", ev.Data)
			}
			return
		case <-time.After(alertWait):
			t.Fatal("no plan alert on the stream across chats")
		}
	}
}

// A plan read for the API carries what it has used of its budget (A-E4, D4.5): the model cost of the
// chat since it was approved, the turns without the user and the chat's compute calls with the gateway's
// limit. An interactive plan carries none.
func TestPlanUsageFields(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, err := e.st.CreateChat(ctx, store.NewChat{Title: "auto", Model: "deepseek/deepseek-flash", Variant: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	pl := autonomousPlan(t, e, c.ID)
	if _, err := e.st.IncPlanAutoTurns(ctx, pl.ID); err != nil {
		t.Fatal(err)
	}
	inter, err := e.st.CreatePlan(ctx, store.Plan{ChatID: c.ID, Title: "plain", Via: "cli",
		Steps: []store.PlanStep{{Idx: 1, Method: "GET", Path: "/datasets/1", Effect: plan.EffectRead, State: plan.StepOpen}}})
	if err != nil {
		t.Fatal(err)
	}
	list, err := e.m.Plans(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]store.Plan{}
	for _, p := range list {
		byID[p.ID] = p
	}
	u := byID[pl.ID].Usage
	if u == nil {
		t.Fatal("the autonomous plan carries no usage")
	}
	if u.ComputeMax != e.m.computeMax() {
		t.Fatalf("compute max %d, want %d", u.ComputeMax, e.m.computeMax())
	}
	if u.ComputeUsed != 0 || u.AutoTurns != 1 || u.LLMCost != 0 {
		t.Fatalf("usage: %+v", u)
	}
	if byID[inter.ID].Usage != nil {
		t.Fatalf("an interactive plan carries usage: %+v", byID[inter.ID].Usage)
	}
	// The published plan carries the same usage: the card lives on the stream as well as on the list.
	ch, cancel := e.m.Subscribe(c.ID)
	defer cancel()
	e.m.publishPlan(ctx, pl.ID)
	for {
		select {
		case ev := <-ch:
			if ev.Kind != "plan" {
				continue
			}
			p, ok := ev.Data.(store.Plan)
			if !ok || p.ID != pl.ID || p.Usage == nil || p.Usage.ComputeMax != e.m.computeMax() {
				t.Fatalf("published plan: %#v", ev.Data)
			}
			return
		case <-time.After(alertWait):
			t.Fatal("no plan event")
		}
	}
}

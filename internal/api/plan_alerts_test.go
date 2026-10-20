// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package api

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"agw/internal/chat"
	"agw/internal/plan"
	"agw/internal/store"
)

func planAlertOf(t *testing.T, ev streamEvent) chat.PlanAlert {
	t.Helper()
	if ev.Kind != "plan_alert" {
		t.Fatalf("expected a plan_alert event, got %s %s", ev.Kind, ev.Data)
	}
	var a chat.PlanAlert
	if err := json.Unmarshal(ev.Data, &a); err != nil {
		t.Fatal(err)
	}
	return a
}

// The alerts of autonomous plans (A-E4, D4.3) travel the stream across chats with the plan's chat id, so
// that the frontend can notify on any platform page. They reach only the owner of that chat: "plan_alert"
// goes the same way as an approval, through the owner filter of GET /api/events.
func TestPlanAlertCrossChatOwnerOnly(t *testing.T) {
	m, p, cat, st := ownershipEnv(t)
	srv, is := oidcServer(t, m, p, cat, nil)
	a, b := loggedIn(t, srv, is, anna), loggedIn(t, srv, is, bert)
	ctx := context.Background()
	mk := func(title, owner string) store.Chat {
		c, err := st.CreateChat(ctx, store.NewChat{Title: title, Model: "p/m", Variant: "cli", Owner: owner})
		if err != nil {
			t.Fatal(err)
		}
		_ = st.SetState(ctx, c.ID, store.StateDormant)
		return c
	}
	chatA, chatB := mk("Anna's plan chat", anna.Sub), mk("Bert's plan chat", bert.Sub)
	// An approved autonomous plan per chat. Pausing one is a state change publishPlan reports, and the
	// alert follows it; nothing else has to run for that (no sandbox, no platform).
	mkPlan := func(chatID, title string) store.Plan {
		pl, err := st.CreatePlan(ctx, store.Plan{ChatID: chatID, Title: title, Via: "cli", Mode: plan.ModeAutonomous,
			Budget: store.PlanBudget{MaxDurationS: 3600, MaxLLMCost: 1.5, MaxAutoTurns: 20},
			Steps:  []store.PlanStep{{Idx: 1, Method: "POST", Path: "/datasets", Effect: plan.EffectWrite, State: plan.StepOpen}}})
		if err != nil {
			t.Fatal(err)
		}
		ok, err := st.DecidePlan(ctx, pl.ID, plan.StateApproved, nil)
		if err != nil || !ok {
			t.Fatalf("plan not approved: %v %v", ok, err)
		}
		return pl
	}
	planA, planB := mkPlan(chatA.ID, "Train pigs v1"), mkPlan(chatB.ID, "Bert's own plan")

	evA, _, _ := openEvents(t, a, srv.URL)
	evB, _, _ := openEvents(t, b, srv.URL)
	snapshotOf(t, nextEvent(t, evA, "Anna's snapshot"))
	snapshotOf(t, nextEvent(t, evB, "Bert's snapshot"))

	// The plan was approved with the store, so no publish has marked its state yet: the first publish of a
	// plan reports nothing for the state it already had (publishPlanAlerts, A-E4). The pause below is that
	// first publish; the alert is expected for the round after it.
	if code, body := call(t, a, "POST", srv.URL+"/api/chats/"+chatA.ID+"/plans/"+planA.ID+"/pause", ""); code != 200 {
		t.Fatalf("pause: %d %s", code, body)
	}
	if code, body := call(t, a, "POST", srv.URL+"/api/chats/"+chatA.ID+"/plans/"+planA.ID+"/resume", ""); code != 200 {
		t.Fatalf("resume: %d %s", code, body)
	}
	if code, body := call(t, a, "POST", srv.URL+"/api/chats/"+chatA.ID+"/plans/"+planA.ID+"/pause", ""); code != 200 {
		t.Fatalf("second pause: %d %s", code, body)
	}
	al := planAlertOf(t, nextEvent(t, evA, "Anna's plan alert"))
	if al.Kind != chat.AlertPaused || al.ChatID != chatA.ID || al.PlanID != planA.ID {
		t.Fatalf("alert: %+v", al)
	}
	if al.Title != "Train pigs v1" || al.Reason != "user" {
		t.Fatalf("alert payload: %+v", al)
	}
	select {
	case ev := <-evB:
		t.Fatalf("Bert got a foreign alert: %s %s", ev.Kind, ev.Data)
	case <-time.After(200 * time.Millisecond):
	}

	// Bert's own plan: his alert arrives, and Anna's stream stays quiet. The same two rounds as above, so
	// that the plan's state is marked before the pause the alert belongs to.
	if code, body := call(t, b, "POST", srv.URL+"/api/chats/"+chatB.ID+"/plans/"+planB.ID+"/pause", ""); code != 200 {
		t.Fatalf("pause: %d %s", code, body)
	}
	if code, body := call(t, b, "POST", srv.URL+"/api/chats/"+chatB.ID+"/plans/"+planB.ID+"/resume", ""); code != 200 {
		t.Fatalf("resume: %d %s", code, body)
	}
	if code, body := call(t, b, "POST", srv.URL+"/api/chats/"+chatB.ID+"/plans/"+planB.ID+"/pause", ""); code != 200 {
		t.Fatalf("second pause: %d %s", code, body)
	}
	if got := planAlertOf(t, nextEvent(t, evB, "Bert's plan alert")); got.PlanID != planB.ID || got.ChatID != chatB.ID {
		t.Fatalf("Bert's alert: %+v", got)
	}
	select {
	case ev := <-evA:
		t.Fatalf("Anna got a foreign alert: %s %s", ev.Kind, ev.Data)
	case <-time.After(200 * time.Millisecond):
	}
}

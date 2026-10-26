// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package api

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"agw/internal/store"
)

// Issue #221: GET /api/activity?plan=&step= validates its parameters, separates plans and steps, and never shows
// another user's calls.
func TestActivityPlanParamValidation(t *testing.T) {
	m, p, cat, st := ownershipEnv(t)
	srv, is := oidcServer(t, m, p, cat, nil)
	a := loggedIn(t, srv, is, anna)
	b := loggedIn(t, srv, is, bert)
	ctx := context.Background()
	chat, err := st.CreateChat(ctx, store.NewChat{Title: "by Anna", Model: "p/m", Variant: "cli", Owner: anna.Sub})
	if err != nil {
		t.Fatal(err)
	}
	_ = st.SetState(ctx, chat.ID, store.StateDormant)
	mkPlan := func(session string) store.Plan {
		pl, err := st.CreatePlan(ctx, store.Plan{ChatID: chat.ID, Title: "P", Session: session, Via: "cli", Steps: []store.PlanStep{
			{Idx: 1, Method: "POST", Path: "/models", Effect: "write"},
			{Idx: 2, Method: "PATCH", Path: "/models/{{step1.id}}", Effect: "write"},
		}})
		if err != nil {
			t.Fatal(err)
		}
		return pl
	}
	p1, p2 := mkPlan("main"), mkPlan("other")
	add := func(detail, plan string, step int) {
		rec := store.CallRecord{ChatID: chat.ID, SlotID: "p", Via: "cli", Op: "platform", Detail: detail, Result: "ok 200", PlanID: plan}
		if plan != "" {
			rec.PlanStep = &step
		}
		if _, err := st.AddSocketCallRecord(ctx, rec); err != nil {
			t.Fatal(err)
		}
	}
	add("POST /models", p1.ID, 1)
	add("POST /models", p2.ID, 1)
	add("PATCH /models/1", p2.ID, 2)
	add("GET /datasets", "", 0)

	list := func(q string) (int, store.ActivityPage, string) {
		code, body := call(t, a, "GET", srv.URL+"/api/activity"+q, "")
		var pg store.ActivityPage
		_ = json.Unmarshal([]byte(body), &pg)
		return code, pg, body
	}
	for _, bad := range []string{
		"?plan=not-a-uuid",
		"?plan=" + url.QueryEscape(p1.ID) + "&step=0",
		"?plan=" + url.QueryEscape(p1.ID) + "&step=100",
		"?plan=" + url.QueryEscape(p1.ID) + "&step=x",
		"?step=1",
	} {
		if code, _, body := list(bad); code != 400 {
			t.Errorf("%s: want 400, got %d %s", bad, code, body)
		}
	}
	if code, _, body := list("?step=1"); !strings.Contains(body, "step: needs plan") {
		t.Errorf("step without plan: %d %s", code, body)
	}
	code, pg, body := list("?plan=" + p1.ID + "&step=1")
	if code != 200 || len(pg.Calls) != 1 || pg.Calls[0].PlanID != p1.ID {
		t.Fatalf("plan 1 step 1: %d %s", code, body)
	}
	code, pg, body = list("?plan=" + p2.ID + "&step=1")
	if code != 200 || len(pg.Calls) != 1 || pg.Calls[0].PlanID != p2.ID || pg.Calls[0].Detail != "POST /models" {
		t.Fatalf("plan 2 step 1: %d %s", code, body)
	}
	if code, pg, body = list("?plan=" + p2.ID); code != 200 || len(pg.Calls) != 2 {
		t.Fatalf("plan 2: %d %s", code, body)
	}
	// Bert asks for Anna's plan: nothing, no error that tells a plan exists.
	code, body = call(t, b, "GET", srv.URL+"/api/activity?plan="+p1.ID+"&step=1", "")
	var other store.ActivityPage
	if code != 200 || json.Unmarshal([]byte(body), &other) != nil || len(other.Calls) != 0 {
		t.Fatalf("Bert: %d %s", code, body)
	}
}

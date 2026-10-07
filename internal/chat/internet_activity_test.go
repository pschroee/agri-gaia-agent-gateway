// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agw/internal/sock"
	"agw/internal/store"
)

// Issue #37: every internet switch reaches the activity. Requests through the socket (approved, expired, already
// on), the agent's switch-off and the user's switch, each with its origin; the internal switch that follows an
// approval is not logged twice, and the platform figures stay untouched.
func TestInternetSwitchesInActivity(t *testing.T) {
	e := setup(t)
	e.m.opt.ApprovalTimeout = 150 * time.Millisecond
	ctx := context.Background()
	off := false
	c, _ := e.m.Create(ctx, NewChat{Internet: &off})
	slot := e.m.live[c.ID].slot.ID
	h := sock.NewHandler(slot, e.m, 1<<20)
	post := func(path, body, toolCall string) int {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "http://agw"+path, strings.NewReader(body))
		if toolCall != "" {
			r.Header.Set("X-Agw-Tool-Call", toolCall)
		}
		h.ServeHTTP(rec, r)
		return rec.Code
	}

	// 1. Request without a decision: expired (logged as such, not as "rejected").
	if code := post("/internet", `{"reason":"pip install pandas"}`, "tc-exp"); code != 200 {
		t.Fatalf("expired request: %d", code)
	}
	// 2. Request, approved by the user.
	events, cancel := e.m.Subscribe(c.ID)
	defer cancel()
	done := make(chan int, 1)
	go func() { done <- post("/internet", `{"reason":"read the docs"}`, "tc-ok") }()
	ap := waitEvent(t, events, "approval", "").Data.(store.Approval)
	if _, err := e.m.Decide(ctx, ap.ID, true); err != nil {
		t.Fatal(err)
	}
	if code := <-done; code != 200 {
		t.Fatalf("approved request: %d", code)
	}
	// 3. Asked again while on: no approval.
	post("/internet", `{"reason":"again"}`, "tc-again")
	// 4. The agent switches off.
	post("/internet/off", "", "tc-off")
	// 5. The user switches on and on once more, then off.
	for _, on := range []bool{true, true, false} {
		if _, err := e.m.SetInternetByUser(ctx, c.ID, on); err != nil {
			t.Fatal(err)
		}
	}
	// A platform call next to them.
	if _, err := e.st.AddSocketCall(ctx, store.SocketCall{ChatID: c.ID, SlotID: slot, Via: "cli", Op: "platform", Detail: "GET /datasets", Result: "ok 200"}); err != nil {
		t.Fatal(err)
	}

	p, err := e.st.ListActivity(ctx, store.ActivityFilter{Kind: store.KindInternet})
	if err != nil {
		t.Fatal(err)
	}
	type row struct{ action, origin, result, via, approval string }
	want := []row{ // newest first
		{"switch", "user", "off", "user", ""},
		{"switch", "user", "already_on", "user", ""},
		{"switch", "user", "on", "user", ""},
		{"off", "agent", "off", "cli", ""},
		{"request", "agent", "already_on", "cli", ""},
		{"request", "agent", "approved", "cli", store.ApprovalApproved},
		{"request", "agent", "expired", "cli", store.ApprovalExpired},
	}
	if len(p.Calls) != len(want) {
		t.Fatalf("internet entries: %d, want %d: %+v", len(p.Calls), len(want), p.Calls)
	}
	for i, w := range want {
		c := p.Calls[i]
		if c.Kind != store.KindInternet || c.Outcome != "" || c.Internet == nil {
			t.Fatalf("%d: %+v", i, c)
		}
		got := row{c.Internet.Action, c.Internet.Origin, c.Internet.Result, c.Via, ""}
		if c.Approval != nil {
			got.approval = c.Approval.State
		}
		if got != w {
			t.Errorf("%d: got %+v, want %+v", i, got, w)
		}
	}
	if p.Calls[6].Detail != "pip install pandas" || p.Calls[5].Detail != "read the docs" {
		t.Errorf("reasons: %q %q", p.Calls[6].Detail, p.Calls[5].Detail)
	}
	if p.Summary.Total != 1 || p.Summary.Chats != 1 {
		t.Errorf("summary counts internet entries: %+v", p.Summary)
	}
	all, _ := e.st.ListActivity(ctx, store.ActivityFilter{Kind: store.KindAll})
	plat, _ := e.st.ListActivity(ctx, store.ActivityFilter{})
	if len(all.Calls) != 8 || all.Calls[0].Kind != store.KindPlatform || len(plat.Calls) != 1 || plat.Calls[0].Outcome != "ok" {
		t.Fatalf("all %d (first %+v), platform %d", len(all.Calls), all.Calls[0], len(plat.Calls))
	}
}

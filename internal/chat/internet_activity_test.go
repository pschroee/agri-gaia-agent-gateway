// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"fmt"
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
// approval is not logged twice, and the platform figures stay untouched. Since issue #175 every end of a grant is an
// entry of its own (action end, via gateway) next to the switch that caused it.
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
	if code := post("/internet", `{"kind":"search","reason":"pip install pandas"}`, "tc-exp"); code != 200 {
		t.Fatalf("expired request: %d", code)
	}
	// 2. Request, approved by the user.
	events, cancel := e.m.Subscribe(c.ID)
	defer cancel()
	done := make(chan int, 1)
	go func() { done <- post("/internet", `{"kind":"search","reason":"read the docs"}`, "tc-ok") }()
	ap := waitEvent(t, events, "approval", "").Data.(store.Approval)
	if _, err := e.m.Decide(ctx, ap.ID, true); err != nil {
		t.Fatal(err)
	}
	if code := <-done; code != 200 {
		t.Fatalf("approved request: %d", code)
	}
	// 3. Asked again while on: no approval.
	post("/internet", `{"kind":"search","reason":"again"}`, "tc-again")
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
		{"switch", "user", "off", "user", ""}, // the user's end of the grant is summarised in this entry (issue #185)
		{"switch", "user", "already_on", "user", ""},
		{"switch", "user", "on", "user", ""},
		{"off", "agent", "off", "cli", ""},
		{"end", "agent", "agent", "gateway", ""},
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
	if p.Calls[7].Detail != "pip install pandas" || p.Calls[6].Detail != "read the docs" {
		t.Errorf("reasons: %q %q", p.Calls[7].Detail, p.Calls[6].Detail)
	}
	if p.Summary.Total != 1 || p.Summary.Chats != 1 {
		t.Errorf("summary counts internet entries: %+v", p.Summary)
	}
	all, _ := e.st.ListActivity(ctx, store.ActivityFilter{Kind: store.KindAll})
	plat, _ := e.st.ListActivity(ctx, store.ActivityFilter{})
	if len(all.Calls) != 9 || all.Calls[0].Kind != store.KindPlatform || len(plat.Calls) != 1 || plat.Calls[0].Outcome != "ok" {
		t.Fatalf("all %d (first %+v), platform %d", len(all.Calls), all.Calls[0], len(plat.Calls))
	}
}

// Issue #185: the user's switch-off carries the IDs of the grants it ended in its detail.
func TestUserSwitchOffLogsEndedGrants(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	off := false
	c, _ := e.m.Create(ctx, NewChat{Internet: &off})
	if _, err := e.m.SetInternetByUser(ctx, c.ID, true); err != nil {
		t.Fatal(err)
	}
	// A second grant next to the full one (a domain grant of the agent's request).
	g2, err := e.m.startUserFullGrant(ctx, c.ID, store.GrantByUser, ViaUser)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.SetInternetByUser(ctx, c.ID, false); err != nil {
		t.Fatal(err)
	}
	p, err := e.st.ListActivity(ctx, store.ActivityFilter{Kind: store.KindInternet})
	if err != nil {
		t.Fatal(err)
	}
	var sw *store.ActivityCall
	for i := range p.Calls {
		if p.Calls[i].Op == store.OpInternetUser && p.Calls[i].Result == "off" {
			sw = &p.Calls[i]
		}
	}
	if sw == nil || !strings.HasPrefix(sw.Detail, store.EndedGrantsPrefix) || !strings.Contains(sw.Detail, fmt.Sprint(g2.ID)) {
		t.Fatalf("switch-off entry: %+v", sw)
	}
	if len(sw.Ended) != len(strings.Split(strings.TrimPrefix(sw.Detail, store.EndedGrantsPrefix), ",")) || len(sw.Ended) == 0 {
		t.Errorf("ended grants of the entry: %+v for detail %q", sw.Ended, sw.Detail)
	}
}

// Issue #186: the user's switch-on next to another active grant starts a full grant and is logged as "on", not as
// "already on"; a second switch-on is "already on".
func TestUserSwitchOnBesideSearchGrant(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	chatID, _ := newChatNoNet(t, e)
	startTestGrant(t, e, chatID, "search", nil, 10, 10)
	if _, err := e.m.SetInternetByUser(ctx, chatID, true); err != nil {
		t.Fatal(err)
	}
	p, err := e.st.ListActivity(ctx, store.ActivityFilter{Kind: store.KindInternet})
	if err != nil {
		t.Fatal(err)
	}
	var sw []store.ActivityCall
	for _, c := range p.Calls {
		if c.Op == store.OpInternetUser {
			sw = append(sw, c)
		}
	}
	if len(sw) != 1 || sw[0].Internet.Result != "on" || sw[0].Grant == nil || sw[0].Grant.Kind != "full" ||
		!strings.HasSuffix(sw[0].Detail, " full") || !strings.HasPrefix(sw[0].Detail, "grant ") {
		t.Fatalf("switch-on beside a search grant: %+v", sw)
	}
	if _, err := e.m.SetInternetByUser(ctx, chatID, true); err != nil {
		t.Fatal(err)
	}
	p, _ = e.st.ListActivity(ctx, store.ActivityFilter{Kind: store.KindInternet})
	if p.Calls[0].Internet.Result != "already_on" || p.Calls[0].Detail != "" {
		t.Fatalf("second switch-on: %+v", p.Calls[0])
	}
}

// Issue #186: switching off with nothing active is "already off" and ends nothing.
func TestUserSwitchOffNothingActive(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	chatID, _ := newChatNoNet(t, e)
	if _, err := e.m.SetInternetByUser(ctx, chatID, false); err != nil {
		t.Fatal(err)
	}
	p, err := e.st.ListActivity(ctx, store.ActivityFilter{Kind: store.KindInternet})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Calls) != 1 || p.Calls[0].Internet.Result != "already_off" || len(p.Calls[0].Ended) != 0 {
		t.Fatalf("switch-off with nothing active: %+v", p.Calls)
	}
}

// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package api

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agw/internal/oidc/oidctest"
	"agw/internal/store"
)

type acrossPage struct {
	Plans   []store.PlanListItem `json:"plans"`
	Total   int                  `json:"total"`
	Running int                  `json:"running"`
}

func acrossMake(t *testing.T, st *store.Store, title, owner string) store.Chat {
	t.Helper()
	c, err := st.CreateChat(context.Background(), store.NewChat{Title: title, Model: "p/m", Variant: "cli", Owner: owner})
	if err != nil {
		t.Fatal(err)
	}
	_ = st.SetState(context.Background(), c.ID, store.StateDormant)
	return c
}

func acrossPlan(t *testing.T, st *store.Store, chatID, title, mode string, approve bool) store.Plan {
	t.Helper()
	ctx := context.Background()
	p := store.Plan{ChatID: chatID, Title: title, Session: "main", Via: "cli", Mode: mode,
		Steps: []store.PlanStep{{Idx: 1, Method: "GET", Path: "/models/1", Effect: "read"}}}
	if mode == "autonomous" {
		p.Budget = store.PlanBudget{MaxDurationS: 3600, MaxLLMCost: 1, MaxAutoTurns: 10}
		p.Grant = json.RawMessage(`{"rules":[]}`)
	}
	p, err := st.CreatePlan(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if approve {
		exp := time.Now().Add(time.Hour)
		if ok, err := st.DecidePlan(ctx, p.ID, "approved", &exp); err != nil || !ok {
			t.Fatalf("decide: %v %v", ok, err)
		}
	}
	return p
}

func TestPlansAcrossParamValidation(t *testing.T) {
	m, p, cat, _ := ownershipEnv(t)
	srv, is := oidcServer(t, m, p, cat, nil)
	a := loggedIn(t, srv, is, anna)
	for q, want := range map[string]string{
		"state=bogus":     "state: one of proposed, approved, paused, completed, failed, cancelled, rejected, expired, superseded",
		"mode=both":       "mode: interactive or autonomous",
		"chat=x":          "chat: a chat id (uuid)",
		"limit=0":         "limit: 1 to 100",
		"limit=101":       "limit: 1 to 100",
		"offset=-1":       "offset: 0 or more",
		"state=paused,zz": "state: one of",
	} {
		code, body := call(t, a, "GET", srv.URL+"/api/plans?"+q, "")
		if code != 400 || !strings.Contains(body, want) {
			t.Errorf("%s: %d %s, want 400 %q", q, code, body, want)
		}
	}
	anon := oidctest.NewBrowser(t)
	if code, _ := call(t, anon, "GET", srv.URL+"/api/plans", ""); code != 401 {
		t.Errorf("without login: %d, want 401", code)
	}
	if code, _ := call(t, anon, "GET", srv.URL+"/api/plans/events", ""); code != 401 {
		t.Errorf("events without login: %d, want 401", code)
	}
}

func TestPlansAcrossRedactsAndUsage(t *testing.T) {
	m, p, cat, st := ownershipEnv(t)
	srv, is := oidcServer(t, m, p, cat, nil)
	a := loggedIn(t, srv, is, anna)
	ctx := context.Background()
	c := acrossMake(t, st, "Anna chat", anna.Sub)
	auto := acrossPlan(t, st, c.ID, "Auto", "autonomous", true)
	inter := acrossPlan(t, st, c.ID, "Inter", "interactive", true)
	token := "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJhIn0.c2lnbmF0dXJl"
	if ok, err := st.FinishPlanStep(ctx, inter.ID, 1, "open", "failed", "denied: "+token, "", ""); err != nil {
		t.Fatal(err)
	} else if !ok {
		if ok, err := st.ClaimPlanStep(ctx, inter.ID, 1); err != nil || !ok {
			t.Fatal(err)
		}
		if _, err := st.FinishPlanStep(ctx, inter.ID, 1, "running", "failed", "denied: "+token, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	code, body := call(t, a, "GET", srv.URL+"/api/plans", "")
	if code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	if strings.Contains(body, token) {
		t.Fatalf("token in the answer: %s", body)
	}
	var pg acrossPage
	if err := json.Unmarshal([]byte(body), &pg); err != nil {
		t.Fatal(err)
	}
	if pg.Total != 2 || pg.Running != 1 || len(pg.Plans) != 2 {
		t.Fatalf("page: %+v", pg)
	}
	for _, it := range pg.Plans {
		if it.ChatTitle != "Anna chat" {
			t.Errorf("chat_title: %q", it.ChatTitle)
		}
		if (it.ID == auto.ID) != (it.Usage != nil) {
			t.Errorf("usage of %s (%s): %+v", it.Title, it.Mode, it.Usage)
		}
	}
}

func TestPlansAcrossOwnerAndFilters(t *testing.T) {
	m, p, cat, st := ownershipEnv(t)
	srv, is := oidcServer(t, m, p, cat, nil)
	a := loggedIn(t, srv, is, anna)
	b := loggedIn(t, srv, is, bert)
	ca, cb := acrossMake(t, st, "Anna", anna.Sub), acrossMake(t, st, "Bert", bert.Sub)
	acrossPlan(t, st, ca.ID, "A1", "interactive", false)
	acrossPlan(t, st, ca.ID, "A2", "autonomous", true)
	acrossPlan(t, st, cb.ID, "B1", "interactive", false)
	get := func(br *oidctest.Browser, q string) acrossPage {
		code, body := call(t, br, "GET", srv.URL+"/api/plans"+q, "")
		var pg acrossPage
		if code != 200 || json.Unmarshal([]byte(body), &pg) != nil {
			t.Fatalf("%s: %d %s", q, code, body)
		}
		return pg
	}
	if pg := get(a, ""); pg.Total != 2 || pg.Plans[0].Title != "A2" {
		t.Errorf("Anna: %+v", pg)
	}
	if pg := get(b, ""); pg.Total != 1 || pg.Plans[0].Title != "B1" {
		t.Errorf("Bert: %+v", pg)
	}
	if pg := get(a, "?mode=interactive"); pg.Total != 1 || pg.Plans[0].Title != "A1" {
		t.Errorf("mode: %+v", pg)
	}
	if pg := get(a, "?state=approved"); pg.Total != 1 || pg.Plans[0].Title != "A2" {
		t.Errorf("state: %+v", pg)
	}
	if pg := get(a, "?chat="+cb.ID); pg.Total != 0 || len(pg.Plans) != 0 {
		t.Errorf("foreign chat: %+v", pg)
	}
}

// openPlanEvents connects br to GET /api/plans/events and returns its data events.
func openPlanEvents(t *testing.T, br *oidctest.Browser, srv *httptest.Server) <-chan streamEvent {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/plans/events", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	resp, err := br.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		cancel()
		t.Fatalf("plan events: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	out := make(chan streamEvent, 64)
	go func() {
		defer close(out)
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if v, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
				var ev streamEvent
				if json.Unmarshal([]byte(v), &ev) == nil {
					out <- ev
				}
			}
		}
	}()
	t.Cleanup(func() { cancel(); resp.Body.Close() })
	return out
}

func TestPlanEventsOwnerOnly(t *testing.T) {
	m, p, cat, st := ownershipEnv(t)
	srv, is := oidcServer(t, m, p, cat, nil)
	a := loggedIn(t, srv, is, anna)
	ctx := context.Background()
	mine, foreign, trashed := acrossMake(t, st, "Anna chat", anna.Sub), acrossMake(t, st, "Bert chat", bert.Sub), acrossMake(t, st, "Anna trashed", anna.Sub)
	pf := acrossPlan(t, st, foreign.ID, "Foreign", "autonomous", true)
	pt := acrossPlan(t, st, trashed.ID, "Trashed", "autonomous", true)
	pm := acrossPlan(t, st, mine.ID, "Mine", "autonomous", true)
	if _, err := st.MoveToTrash(ctx, trashed.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	ev := openPlanEvents(t, a, srv)
	time.Sleep(300 * time.Millisecond) // the subscription starts with the handler

	// CancelPlan publishes the plan event of its chat.
	for _, x := range []struct{ chat, plan string }{{foreign.ID, pf.ID}, {trashed.ID, pt.ID}, {mine.ID, pm.ID}} {
		if _, err := m.CancelPlan(ctx, x.chat, x.plan); err != nil {
			t.Fatal(err)
		}
	}
	got := nextEvent(t, ev, "own plan")
	var it store.PlanListItem
	if err := json.Unmarshal(got.Data, &it); err != nil {
		t.Fatal(err)
	}
	if got.Kind != "plan" || it.ID != pm.ID || it.ChatTitle != "Anna chat" || it.State != "cancelled" {
		t.Fatalf("event: %s %+v", got.Kind, it)
	}
	select {
	case extra := <-ev:
		t.Fatalf("event of a foreign or trashed chat: %s %s", extra.Kind, extra.Data)
	case <-time.After(500 * time.Millisecond):
	}
}

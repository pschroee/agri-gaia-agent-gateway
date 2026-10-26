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

	"agw/internal/chat"
	"agw/internal/oidc/oidctest"
	"agw/internal/store"
)

// Issue #222: every bad parameter of GET /api/experiments gets its own 400 text.
func TestExperimentsRouteValidation(t *testing.T) {
	m, p, cat, _ := ownershipEnv(t)
	srv, is := oidcServer(t, m, p, cat, nil)
	a := loggedIn(t, srv, is, anna)
	for q, want := range map[string]string{
		"state=bogus":         "state: one of running, waiting, done, failed, stopped",
		"state=running,bogus": "state: one of running, waiting, done, failed, stopped",
		"kind=search":         "kind: one of training_search, training_run, notebook_run",
		"chat=not-a-uuid":     "chat: a chat id (uuid)",
		"since=yesterday":     "since: expected an RFC 3339 time such as 2026-10-06T00:00:00Z",
		"until=2026-10-06":    "until: expected an RFC 3339 time such as 2026-10-06T00:00:00Z",
		"sort=speed":          "sort: one of started, duration, title, state, chat",
		"order=up":            "order: asc or desc",
		"offset=-1":           "offset: 0 or more",
		"offset=x":            "offset: 0 or more",
		"limit=0":             "limit: 1 to 100",
		"limit=101":           "limit: 1 to 100",
	} {
		code, body := call(t, a, "GET", srv.URL+"/api/experiments?"+q, "")
		if code != 400 || !strings.Contains(body, want) {
			t.Errorf("%s: %d %s, want 400 %q", q, code, body, want)
		}
	}
	if code, body := call(t, a, "GET", srv.URL+"/api/experiments?state=running,done&kind=training_run&sort=duration&order=desc&limit=100", ""); code != 200 {
		t.Errorf("valid query: %d %s", code, body)
	}
}

func TestParseOverviewQueryOrderDefaults(t *testing.T) {
	for sort, want := range map[string]string{"started": "desc", "duration": "desc", "title": "asc", "state": "asc", "": ""} {
		o, bad := parseOverviewQuery(url.Values{"sort": {sort}}, chat.RunSorts)
		if bad != "" || o.Order != want || o.Limit != 25 {
			t.Errorf("sort %q: %+v %q", sort, o, bad)
		}
	}
}

// The user sees only the runs of their own chats; without a login there is 401; another user's chat in ?chat= gives
// an empty page, like an unknown one.
func TestExperimentsRouteOwner(t *testing.T) {
	m, p, cat, st := ownershipEnv(t)
	srv, is := oidcServer(t, m, p, cat, nil)
	a := loggedIn(t, srv, is, anna)
	b := loggedIn(t, srv, is, bert)
	ctx := context.Background()
	mine, err := st.CreateChat(ctx, store.NewChat{Title: "Anna's", Model: "p/m", Variant: "cli", Owner: anna.Sub})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DeclareTrial(ctx, store.Trial{ChatID: mine.ID, Path: store.SearchPathTemplate}); err != nil {
		t.Fatal(err)
	}
	page := func(br *oidctest.Browser, q string) (int, chat.RunPage, string) {
		t.Helper()
		code, body := call(t, br, "GET", srv.URL+"/api/experiments"+q, "")
		var pg chat.RunPage
		_ = json.Unmarshal([]byte(body), &pg)
		return code, pg, body
	}
	code, pg, body := page(a, "")
	if code != 200 || pg.Total != 1 || pg.Runs[0].ChatTitle != "Anna's" || pg.Runs[0].Kind != chat.RunKindTraining {
		t.Fatalf("Anna: %d %s", code, body)
	}
	if code, pg, body = page(b, ""); code != 200 || pg.Total != 0 || len(pg.Runs) != 0 {
		t.Fatalf("Bert sees nothing: %d %s", code, body)
	}
	if code, pg, body = page(b, "?chat="+mine.ID); code != 200 || pg.Total != 0 {
		t.Fatalf("Bert asks for Anna's chat: %d %s", code, body)
	}
	if code, _, _ = page(oidctest.NewBrowser(t), ""); code != 401 {
		t.Errorf("without a login: %d", code)
	}
}

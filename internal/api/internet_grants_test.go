// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package api

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"agw/internal/store"
)

// Issue #175: the grants of a chat are listed and ended through the API, only by the chat's owner; the connections
// across chats are the user's own, with filters and a cursor.
func TestInternetGrantsRoutes(t *testing.T) {
	m, p, cat, st := ownershipEnv(t)
	srv, is := oidcServer(t, m, p, cat, nil)
	a := loggedIn(t, srv, is, anna)
	b := loggedIn(t, srv, is, bert)
	ctx := context.Background()
	mk := func(title, owner string) store.Chat {
		c, err := st.CreateChat(ctx, store.NewChat{Title: title, Model: "p/m", Variant: "cli", Owner: owner})
		if err != nil {
			t.Fatal(err)
		}
		_ = st.SetState(ctx, c.ID, store.StateDormant)
		return c
	}
	chatA, chatB := mk("by Anna", anna.Sub), mk("by Bert", bert.Sub)
	now := time.Now()
	exp := now.Add(30 * time.Minute)
	g, err := st.CreateGrant(ctx, store.Grant{ChatID: chatA.ID, Kind: "domains", Origin: store.GrantByAgent, Domains: []string{"a.example.org"}, State: store.GrantActive,
		MaxMinutes: 30, MaxBytes: 100 << 20, StartedAt: &now, ExpiresAt: &exp})
	if err != nil {
		t.Fatal(err)
	}
	_ = st.SetInternet(ctx, chatA.ID, true) // the flag follows the grants (the manager does this when it starts one)
	if err := m.LoadGrants(ctx); err != nil {
		t.Fatal(err)
	}
	base := srv.URL + "/api/chats/" + chatA.ID

	// The chat shows its active grants.
	if code, body := call(t, a, "GET", base, ""); code != 200 || !strings.Contains(body, `"internet_grants":[{"id":`+strconv.FormatInt(g.ID, 10)) || !strings.Contains(body, `"internet":true`) {
		t.Fatalf("chat: %d %s", code, body)
	}
	// The list of all grants.
	code, body := call(t, a, "GET", base+"/internet/grants", "")
	var list []store.Grant
	if code != 200 || json.Unmarshal([]byte(body), &list) != nil || len(list) != 1 || list[0].ID != g.ID || list[0].Domains[0] != "a.example.org" || list[0].State != store.GrantActive {
		t.Fatalf("grants: %d %s", code, body)
	}
	// Bert reaches neither the list nor the end of Anna's grant.
	for _, c := range []struct{ method, path string }{
		{"GET", "/internet/grants"}, {"POST", "/internet/grants/" + strconv.FormatInt(g.ID, 10) + "/end"}, {"GET", "/web_requests?grant=" + strconv.FormatInt(g.ID, 10)},
	} {
		if code, body := call(t, b, c.method, base+c.path, ""); code != 404 {
			t.Errorf("Bert %s %s: %d %s", c.method, c.path, code, body)
		}
	}
	// A grant of another chat is not found under this chat's path.
	if code, _ := call(t, a, "POST", base+"/internet/grants/999999/end", ""); code != 404 {
		t.Fatalf("unknown grant: %d", code)
	}
	if code, _ := call(t, a, "POST", base+"/internet/grants/x/end", ""); code != 400 {
		t.Fatalf("bad id: %d", code)
	}

	// Connections: Anna's own across chats, filters, cursor.
	add := func(chat string, grant int64, host, denied string, at time.Time) {
		if _, err := st.AddWebRequest(ctx, store.WebRequest{ChatID: chat, Method: "CONNECT", Host: host, Port: 443, Status: 200, Denied: denied, StartedAt: at, GrantID: grant, Origin: "pi"}); err != nil {
			t.Fatal(err)
		}
	}
	add(chatA.ID, g.ID, "a.example.org", "", now)
	add(chatA.ID, g.ID, "x.example.org", "blocked: x", now.Add(time.Second))
	add(chatB.ID, 0, "b.example.org", "", now)
	type connPage struct {
		Connections []store.WebRequest            `json:"connections"`
		Chats       map[string]store.ActivityChat `json:"chats"`
		NextBefore  int64                         `json:"next_before"`
	}
	conns := func(_ any, q string) (int, connPage) {
		var out connPage
		code, body := call(t, a, "GET", srv.URL+"/api/activity/connections"+q, "")
		_ = json.Unmarshal([]byte(body), &out)
		return code, out
	}
	code, out := conns(nil, "")
	if code != 200 || len(out.Connections) != 2 || out.Connections[0].Host != "x.example.org" || out.Chats[chatA.ID].Title != "by Anna" || len(out.Chats) != 1 {
		t.Fatalf("connections: %d %+v", code, out)
	}
	if _, out = conns(nil, "?result=blocked"); len(out.Connections) != 1 || out.Connections[0].Host != "x.example.org" {
		t.Fatalf("blocked: %+v", out)
	}
	if _, out = conns(nil, "?grant="+strconv.FormatInt(g.ID, 10)+"&origin=pi&result=allowed"); len(out.Connections) != 1 || out.Connections[0].Host != "a.example.org" {
		t.Fatalf("grant filter: %+v", out)
	}
	if _, out = conns(nil, "?limit=1"); len(out.Connections) != 1 || out.NextBefore != out.Connections[0].ID {
		t.Fatalf("cursor: %+v", out)
	}
	if _, out = conns(nil, "?limit=1&before="+strconv.FormatInt(out.NextBefore, 10)); len(out.Connections) != 1 || out.Connections[0].Host != "a.example.org" || out.NextBefore != 0 {
		t.Fatalf("second page: %+v", out)
	}
	for _, q := range []string{"?result=maybe", "?origin=net", "?grant=0", "?grant=x", "?since=yesterday", "?limit=0", "?before=-1"} {
		if code, _ := conns(nil, q); code != 400 {
			t.Errorf("%s: %d", q, code)
		}
	}
	// The grant filter of the chat's own list.
	if code, body := call(t, a, "GET", base+"/web_requests?grant="+strconv.FormatInt(g.ID, 10), ""); code != 200 || strings.Count(body, `"host"`) != 2 {
		t.Fatalf("web_requests?grant: %d %s", code, body)
	}
	if code, _ := call(t, a, "GET", base+"/web_requests?grant=x", ""); code != 400 {
		t.Fatalf("bad grant filter: %d", code)
	}

	// Ending: 200 with the grant, then 409.
	code, body = call(t, a, "POST", base+"/internet/grants/"+strconv.FormatInt(g.ID, 10)+"/end", "")
	var ended store.Grant
	if code != 200 || json.Unmarshal([]byte(body), &ended) != nil || ended.State != store.GrantEnded || ended.EndedReason != store.EndUser {
		t.Fatalf("end: %d %s", code, body)
	}
	if code, body := call(t, a, "POST", base+"/internet/grants/"+strconv.FormatInt(g.ID, 10)+"/end", ""); code != 409 {
		t.Fatalf("second end: %d %s", code, body)
	}
}

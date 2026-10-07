// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package api

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"agw/internal/oidc/oidctest"
	"agw/internal/store"
)

type streamEvent struct {
	Kind string          `json:"kind"`
	Data json.RawMessage `json:"data"`
}

// openEvents connects br to GET /api/events and returns its data events; stop ends the connection.
func openEvents(t *testing.T, br *oidctest.Browser, url string) (events <-chan streamEvent, retry <-chan string, stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", url+"/api/events", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	resp, err := br.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		cancel()
		t.Fatalf("events: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	out := make(chan streamEvent, 64)
	rt := make(chan string, 1)
	go func() {
		defer close(out)
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			line := sc.Text()
			if v, ok := strings.CutPrefix(line, "retry: "); ok {
				rt <- v
			}
			if v, ok := strings.CutPrefix(line, "data: "); ok {
				var ev streamEvent
				if json.Unmarshal([]byte(v), &ev) == nil {
					out <- ev
				}
			}
		}
	}()
	stop = func() { cancel(); resp.Body.Close() }
	t.Cleanup(stop)
	return out, rt, stop
}

func nextEvent(t *testing.T, ch <-chan streamEvent, what string) streamEvent {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatalf("%s: stream closed", what)
		}
		return ev
	case <-time.After(5 * time.Second):
		t.Fatalf("%s: no event", what)
	}
	return streamEvent{}
}

func approvalOf(t *testing.T, ev streamEvent) store.Approval {
	t.Helper()
	if ev.Kind != "approval" {
		t.Fatalf("expected an approval event, got %s %s", ev.Kind, ev.Data)
	}
	var a store.Approval
	if err := json.Unmarshal(ev.Data, &a); err != nil {
		t.Fatal(err)
	}
	return a
}

func snapshotOf(t *testing.T, ev streamEvent) []store.Approval {
	t.Helper()
	if ev.Kind != "approvals" {
		t.Fatalf("expected the approvals snapshot first, got %s %s", ev.Kind, ev.Data)
	}
	var as []store.Approval
	if err := json.Unmarshal(ev.Data, &as); err != nil {
		t.Fatal(err)
	}
	if as == nil {
		t.Fatalf("snapshot must be a list, got %s", ev.Data)
	}
	return as
}

// The stream across chats (issue #32): the snapshot and every new or decided approval of the user's own
// chats, never another user's, and a fresh snapshot after reconnecting.
func TestEventsAcrossChats(t *testing.T) {
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
	chatA1, chatA2, chatB := mk("Anna 1", anna.Sub), mk("Anna 2", anna.Sub), mk("Bert", bert.Sub)
	old, err := st.CreateApproval(ctx, store.Approval{ChatID: chatA1.ID, Kind: "platform_write", Via: "cli", Name: "POST /datasets", PendingKey: "-"})
	if err != nil {
		t.Fatal(err)
	}
	oldB, err := st.CreateApproval(ctx, store.Approval{ChatID: chatB.ID, Kind: "platform_write", Via: "cli", Name: "DELETE /models/1", PendingKey: "-"})
	if err != nil {
		t.Fatal(err)
	}

	evA, retry, stopA := openEvents(t, a, srv.URL)
	evB, _, _ := openEvents(t, b, srv.URL)
	select {
	case v := <-retry:
		if v != "2000" {
			t.Fatalf("retry: %q", v)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no retry line")
	}
	if s := snapshotOf(t, nextEvent(t, evA, "Anna's snapshot")); len(s) != 1 || s[0].ID != old.ID {
		t.Fatalf("Anna's snapshot: %+v", s)
	}
	if s := snapshotOf(t, nextEvent(t, evB, "Bert's snapshot")); len(s) != 1 || s[0].ID != oldB.ID {
		t.Fatalf("Bert's snapshot: %+v", s)
	}

	// The agent asks in Anna's second chat (not the one with the old approval). Without a waiting time
	// the request expires at once: created, then resolved.
	if _, err := m.RequestInternet(ctx, chatA2.ID, "", "cli", "pip install"); err != nil {
		t.Fatal(err)
	}
	created := approvalOf(t, nextEvent(t, evA, "created"))
	if created.ChatID != chatA2.ID || created.State != store.ApprovalPending || created.Kind != "internet_access" {
		t.Fatalf("created: %+v", created)
	}
	if ex := approvalOf(t, nextEvent(t, evA, "expired")); ex.ID != created.ID || ex.State != store.ApprovalExpired {
		t.Fatalf("expired: %+v", ex)
	}

	// Bert's own request reaches Bert; Anna's decision reaches Anna. Bert never sees Anna's events and
	// Anna never Bert's: the next event on each stream is the user's own.
	if _, err := m.RequestInternet(ctx, chatB.ID, "", "cli", "curl"); err != nil {
		t.Fatal(err)
	}
	if code, body := call(t, a, "POST", srv.URL+"/api/approvals/"+old.ID, `{"approve":true}`); code != 200 {
		t.Fatalf("decide: %d %s", code, body)
	}
	if d := approvalOf(t, nextEvent(t, evA, "decided")); d.ID != old.ID || d.State != store.ApprovalApproved {
		t.Fatalf("Anna's next event is not her decision: %+v", d)
	}
	for i, want := range []string{store.ApprovalPending, store.ApprovalExpired} {
		got := approvalOf(t, nextEvent(t, evB, "Bert's event"))
		if got.ChatID != chatB.ID || got.State != want {
			t.Fatalf("Bert's event %d: %+v", i, got)
		}
	}
	select {
	case ev := <-evB:
		t.Fatalf("Bert got a foreign event: %s %s", ev.Kind, ev.Data)
	case <-time.After(200 * time.Millisecond):
	}

	// Disconnect: the subscription goes away, publishing goes on. After reconnecting the snapshot is
	// the state now (the old approval decided, nothing pending).
	stopA()
	if _, err := m.RequestInternet(ctx, chatA1.ID, "", "cli", "while away"); err != nil {
		t.Fatal(err)
	}
	evA2, _, _ := openEvents(t, a, srv.URL)
	if s := snapshotOf(t, nextEvent(t, evA2, "snapshot after reconnect")); len(s) != 0 {
		t.Fatalf("snapshot after reconnect: %+v", s)
	}
	still, err := st.CreateApproval(ctx, store.Approval{ChatID: chatA1.ID, Kind: "platform_write", Via: "cli", Name: "PATCH /datasets/3", PendingKey: "-"})
	if err != nil {
		t.Fatal(err)
	}
	evA3, _, _ := openEvents(t, a, srv.URL)
	if s := snapshotOf(t, nextEvent(t, evA3, "third snapshot")); len(s) != 1 || s[0].ID != still.ID {
		t.Fatalf("third snapshot: %+v", s)
	}
}

// Without a session the stream answers 401 like every other route.
func TestEventsNeedLogin(t *testing.T) {
	srv, _ := oidcServer(t, nil, nil, nil, nil)
	if code, _ := call(t, oidctest.NewBrowser(t), "GET", srv.URL+"/api/events", ""); code != 401 {
		t.Fatalf("without session: %d", code)
	}
}

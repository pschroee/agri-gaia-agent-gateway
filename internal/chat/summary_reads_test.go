// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"agw/internal/platform"
	"agw/internal/store"
)

// summaryReadEntries returns the summary_read entries of a chat.
func summaryReadEntries(t *testing.T, e *env, chatID string) []store.SocketCall {
	t.Helper()
	calls, err := e.st.ListSocketCalls(context.Background(), chatID)
	if err != nil {
		t.Fatal(err)
	}
	var out []store.SocketCall
	for _, sc := range calls {
		if sc.Op == "summary_read" {
			out = append(out, sc)
		}
	}
	return out
}

// The reads the gateway makes to write a summary are one activity entry of the system, not one per GET, and they
// name the approval they belong to (DL2.5).
func TestSummaryReadsLoggedOncePerSummary(t *testing.T) {
	e := setup(t)
	withNamedPlatform(t, e)
	ctx := context.Background()
	c, _ := e.m.Create(ctx, NewChat{})
	slot := e.m.live[c.ID].slot.ID
	req, err := platform.Normalize(platform.Request{Method: "PATCH", Path: "/datasets/16", Body: json.RawMessage(`{"name":"renamed"}`)})
	if err != nil {
		t.Fatal(err)
	}
	go e.m.PlatformCall(ctx, c.ID, slot, "cli", req)
	ap := pendingApproval(t, e, c.ID)
	reads := summaryReadEntries(t, e, c.ID)
	if len(reads) != 1 {
		t.Fatalf("summary_read entries: %+v", reads)
	}
	sc := reads[0]
	if sc.Via != "orchestrator" || sc.Result != "ok" {
		t.Fatalf("entry: %+v", sc)
	}
	if !strings.HasPrefix(sc.Detail, "for approval "+ap.ID[:8]+" (proposed): ") || !strings.Contains(sc.Detail, "GET /datasets/16") {
		t.Fatalf("detail: %q", sc.Detail)
	}
	if strings.Count(sc.Detail, "GET ") != 1 {
		t.Fatalf("one entry per read instead of per summary: %q", sc.Detail)
	}
	// The figures measure the agent: a summary read is not one of its platform calls.
	page, err := e.m.Activity(ctx, store.ActivityFilter{ChatID: c.ID})
	if err != nil {
		t.Fatal(err)
	}
	if page.Summary.Total != 0 {
		t.Fatalf("the figures count the system's reads: %+v", page.Summary)
	}
	if len(page.Calls) != 1 || page.Calls[0].Op != "summary_read" || page.Calls[0].Kind != store.KindPlatform {
		t.Fatalf("the entry is missing from the platform kind: %+v", page.Calls)
	}
	if _, err := e.m.Decide(ctx, ap.ID, false); err != nil {
		t.Fatal(err)
	}
}

// A read that fails makes the entry an error and names how many of the reads failed (DL2.3, DL2.5).
func TestSummaryReadsErrorResult(t *testing.T) {
	e := setup(t)
	var mu sync.Mutex
	answer := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			fmt.Fprint(w, `{"access_token":"t","expires_in":3600}`)
			return
		}
		mu.Lock()
		code := answer
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/datasets/16" {
			if code != http.StatusOK {
				w.WriteHeader(code)
				fmt.Fprint(w, `{"detail":"broken"}`)
				return
			}
			fmt.Fprint(w, `{"id":16,"name":"mnist-mini"}`)
			return
		}
		fmt.Fprint(w, `{}`)
	}))
	t.Cleanup(srv.Close)
	c1, err := platform.New(platform.Config{APIURL: srv.URL, TokenURL: srv.URL + "/token", User: "u", Password: "p"})
	if err != nil {
		t.Fatal(err)
	}
	e.m.opt.Platform = c1
	mu.Lock()
	answer = http.StatusInternalServerError
	mu.Unlock()

	ctx := context.Background()
	c, _ := e.m.Create(ctx, NewChat{})
	slot := e.m.live[c.ID].slot.ID
	req, _ := platform.Normalize(platform.Request{Method: "PATCH", Path: "/datasets/16", Body: json.RawMessage(`{"name":"renamed"}`)})
	go e.m.PlatformCall(ctx, c.ID, slot, "cli", req)
	ap := pendingApproval(t, e, c.ID)
	if strings.Contains(ap.Summary, "mnist-mini") {
		t.Fatalf("the summary names a value it could not read: %q", ap.Summary)
	}
	reads := summaryReadEntries(t, e, c.ID)
	if len(reads) != 1 || reads[0].Result != "error: 1 of 1 not readable" {
		t.Fatalf("entries: %+v", reads)
	}
	if _, err := e.m.Decide(ctx, ap.ID, false); err != nil {
		t.Fatal(err)
	}
}

// A summary that reads nothing leaves no entry at all.
func TestNoEntryWithoutReads(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, _ := e.m.Create(ctx, NewChat{})
	e.m.logSummaryReads(ctx, c.ID, "12345678-0000-0000-0000-000000000000", "proposed", &readLog{})
	if calls, err := e.st.ListSocketCalls(ctx, c.ID); err != nil || len(calls) != 0 {
		t.Fatalf("a summary without reads left an entry: %+v %v", calls, err)
	}
}

// The detail names at most 20 paths and counts the rest (DL2.5): a plan with many steps stays one readable line.
func TestSummaryReadsDetailCapped(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, _ := e.m.Create(ctx, NewChat{})
	rl := &readLog{}
	for i := 1; i <= 25; i++ {
		rl.paths = append(rl.paths, fmt.Sprintf("GET /datasets/%d", i))
	}
	e.m.logSummaryReads(ctx, c.ID, "12345678-0000-0000-0000-000000000000", "approval", rl)
	calls, err := e.st.ListSocketCalls(ctx, c.ID)
	if err != nil || len(calls) != 1 {
		t.Fatalf("entries: %+v %v", calls, err)
	}
	d := calls[0].Detail
	if !strings.HasPrefix(d, "for approval 12345678 (approval): GET /datasets/1, ") || !strings.HasSuffix(d, ", … and 5 more") {
		t.Fatalf("detail: %q", d)
	}
	if n := strings.Count(d, "GET /datasets/"); n != 20 {
		t.Fatalf("paths in the detail: %d (%q)", n, d)
	}
}

// A failed read does not stop the summary: the entry carries the count, the approval stays usable (DL2.3).
func TestSummaryReadsCountFailedReads(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, _ := e.m.Create(ctx, NewChat{})
	rl := &readLog{}
	rl.paths = append(rl.paths, "GET /datasets/16", "GET /models/3")
	rl.failed = 1
	e.m.logSummaryReads(ctx, c.ID, "12345678-0000-0000-0000-000000000000", "proposed", rl)
	calls, err := e.st.ListSocketCalls(ctx, c.ID)
	if err != nil || len(calls) != 1 {
		t.Fatalf("entries: %+v %v", calls, err)
	}
	if calls[0].Result != "error: 1 of 2 not readable" || !strings.Contains(calls[0].Detail, "GET /models/3") {
		t.Fatalf("entry: %+v", calls[0])
	}
}

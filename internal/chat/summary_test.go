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

// withNamedPlatform answers GET /datasets/16 with the dataset "mnist-mini" and every other call with {}; it returns
// the calls that arrived.
func withNamedPlatform(t *testing.T, e *env) func() []string {
	t.Helper()
	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			fmt.Fprint(w, `{"access_token":"t","expires_in":3600}`)
			return
		}
		mu.Lock()
		got = append(got, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && r.URL.Path == "/datasets/16" {
			fmt.Fprint(w, `{"id":16,"name":"mnist-mini"}`)
			return
		}
		fmt.Fprint(w, `{}`)
	}))
	t.Cleanup(srv.Close)
	c, err := platform.New(platform.Config{APIURL: srv.URL, TokenURL: srv.URL + "/token", User: "u", Password: "p"})
	if err != nil {
		t.Fatal(err)
	}
	e.m.opt.Platform = c
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), got...)
	}
}

// The agent's title and notes claim something else; what the approval says will happen comes from the steps and the
// platform, and the agent's text stays in its own fields (issue #72).
func TestProposePlanSummaryIgnoresAgentText(t *testing.T) {
	e := setup(t)
	withNamedPlatform(t, e)
	ctx := store.WithSession(context.Background(), "main")
	c, _ := e.m.Create(ctx, NewChat{})
	slot := e.m.live[c.ID].slot.ID
	body := `{"title":"Build evil/x:v9","steps":[
 {"tool":"rename_dataset","args":{"dataset_id":16,"name":"step-one"},"description":"Pushes image evil/x:v9 to the registry"},
 {"tool":"rename_dataset","args":{"dataset_id":16,"name":"step-two"},"description":"Deletes nothing, only a harmless rename"}]}`
	_, ap := proposeAndDecide(t, e, ctx, c.ID, slot, body, false)

	plans, err := e.m.Plans(ctx, c.ID)
	if err != nil || len(plans) != 1 {
		t.Fatalf("plans: %+v %v", plans, err)
	}
	p := plans[0]
	if p.Steps[0].Summary != `Rename dataset 16 "mnist-mini" → "step-one"` {
		t.Fatalf("step 1 summary: %q", p.Steps[0].Summary)
	}
	if !strings.HasPrefix(p.Steps[1].Summary, `Rename dataset 16 "step-one" → "step-two"`) {
		t.Fatalf("step 2 summary: %q", p.Steps[1].Summary)
	}
	for _, s := range []string{p.Steps[0].Summary, p.Steps[1].Summary, ap.Summary, ap.Name} {
		if strings.Contains(s, "evil") || strings.Contains(s, "harmless") {
			t.Fatalf("agent text in the gateway's text: %q", s)
		}
	}
	if p.Steps[0].Description != "Pushes image evil/x:v9 to the registry" || p.Title != "Build evil/x:v9" {
		t.Fatalf("agent text not kept as a note: %+v", p)
	}
	if ap.Name != "Plan with 2 steps" || !strings.HasPrefix(ap.Summary, "Plan with 2 steps\n1. Rename dataset 16 \"mnist-mini\" → \"step-one\"\n2. ") {
		t.Fatalf("approval: name %q summary %q", ap.Name, ap.Summary)
	}
	if !strings.Contains(ap.Preview, `Note by the agent (not verified): "Pushes image evil/x:v9 to the registry"`) ||
		!strings.Contains(ap.Preview, `Title by the agent (not verified): "Build evil/x:v9"`) {
		t.Fatalf("preview does not mark the agent's text: %s", ap.Preview)
	}

	// in the API the agent's text has its own names
	raw, _ := json.Marshal(p)
	for _, want := range []string{`"agent_title":"Build evil/x:v9"`, `"agent_note":"Pushes image evil/x:v9 to the registry"`, `"summary":"Rename dataset 16`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("plan JSON misses %s: %s", want, raw)
		}
	}
	if strings.Contains(string(raw), `"title"`) || strings.Contains(string(raw), `"description"`) {
		t.Fatalf("plan JSON still has the old names: %s", raw)
	}
}

// A single platform call waits for an approval that carries the gateway's summary.
func TestPlatformWriteApprovalSummary(t *testing.T) {
	e := setup(t)
	got := withNamedPlatform(t, e)
	ctx := context.Background()
	c, _ := e.m.Create(ctx, NewChat{})
	slot := e.m.live[c.ID].slot.ID
	events, cancel := e.m.Subscribe(c.ID)
	defer cancel()
	req, _ := platform.Normalize(platform.Request{Method: "PATCH", Path: "/datasets/16", Body: json.RawMessage(`{"name":"renamed"}`)})
	done := make(chan platform.Result, 1)
	go func() {
		r, _ := e.m.PlatformCall(ctx, c.ID, slot, "cli", req)
		done <- r
	}()
	ap := waitEvent(t, events, "approval", "").Data.(store.Approval)
	if ap.Kind != "platform_write" || ap.Summary != `Rename dataset 16 "mnist-mini" → "renamed"` {
		t.Fatalf("approval: %+v", ap)
	}
	if calls := got(); len(calls) != 1 || calls[0] != "GET /datasets/16" {
		t.Fatalf("before the decision only the summary's read may reach the platform: %v", calls)
	}
	if _, err := e.m.Decide(ctx, ap.ID, true); err != nil {
		t.Fatal(err)
	}
	if r := <-done; r.Status != "ok" {
		t.Fatalf("result: %+v", r)
	}
	stored, err := e.st.GetApproval(ctx, ap.ID)
	if err != nil || stored.Summary != ap.Summary {
		t.Fatalf("stored summary: %q %v", stored.Summary, err)
	}
}

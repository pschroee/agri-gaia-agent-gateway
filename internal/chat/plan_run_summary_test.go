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
	"sync"
	"testing"
	"time"

	"agw/internal/platform"
	"agw/internal/store"
)

// withRunSummaryPlatform answers GET /train/containers/7/model with model 42, GET /models/42 with a named model
// (or 500 while failRead is set) and every other call with {}.
func withRunSummaryPlatform(t *testing.T, e *env, failRead *bool) {
	t.Helper()
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			fmt.Fprint(w, `{"access_token":"t","expires_in":3600}`)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/train/containers/7/model":
			fmt.Fprint(w, `{"id":42}`)
		case r.Method == http.MethodGet && r.URL.Path == "/models/42":
			if failRead != nil && *failRead {
				w.WriteHeader(http.StatusInternalServerError)
				fmt.Fprint(w, `{"detail":"broken"}`)
				return
			}
			fmt.Fprint(w, `{"id":42,"name":"flowers-mini","format":"onnx"}`)
		default:
			fmt.Fprint(w, `{}`)
		}
	}))
	t.Cleanup(srv.Close)
	c, err := platform.New(platform.Config{APIURL: srv.URL, TokenURL: srv.URL + "/token", User: "u", Password: "p"})
	if err != nil {
		t.Fatal(err)
	}
	e.m.opt.Platform = c
}

const renameStepBody = `{"output_name":"a","name":"pigs v1","format":"onnx","input_name":"i","input_datatype":"float32","input_shape":[1,3],"input_semantics":"NCHW","output_datatype":"float32","output_shape":[1,2],"output_labels":["0","1"]}`

// runPlanCover runs planCover to the end: step 1 creates model 42, step 2 patches /models/42 (the placeholder
// resolved). It returns the plan afterwards.
func runPlanCover(t *testing.T, e *env, ctx context.Context, chatID, slot string) store.Plan {
	t.Helper()
	if r, _ := proposeAndDecide(t, e, ctx, chatID, slot, planCover, true); r.Status != "ok" {
		t.Fatalf("plan: %+v", r)
	}
	if r, err := e.m.PlatformCall(ctx, chatID, slot, "cli", liveCall(t, "GET", "/train/containers/7/model", "")); err != nil || r.Status != "ok" {
		t.Fatalf("step 1: %+v %v", r, err)
	}
	if r, err := e.m.PlatformCall(ctx, chatID, slot, "cli", liveCall(t, "PATCH", "/models/42", renameStepBody)); err != nil || r.Status != "ok" {
		t.Fatalf("step 2: %+v %v", r, err)
	}
	return onlyPlan(t, e, ctx, chatID)
}

// A covered step whose placeholder was resolved keeps the approved text and gets the text of the call as sent,
// with the activity entry of its reads and a plan event.
func TestRunSummaryCoveredStep(t *testing.T) {
	e := setup(t)
	withRunSummaryPlatform(t, e, nil)
	ctx := store.WithSession(context.Background(), "main")
	c, _ := e.m.Create(ctx, NewChat{})
	slot := e.m.live[c.ID].slot.ID
	events, cancel := e.m.Subscribe(c.ID)
	defer cancel()
	p := runPlanCover(t, e, ctx, c.ID, slot)
	s := p.Steps[1]
	if s.SummaryRun == "" || !strings.Contains(s.SummaryRun, "flowers-mini") || !strings.Contains(s.SummaryRun, "42") {
		t.Fatalf("summary_run: %q", s.SummaryRun)
	}
	if s.Summary == "" || strings.Contains(s.Summary, "flowers-mini") || s.Summary == s.SummaryRun {
		t.Fatalf("the approved summary changed: %q", s.Summary)
	}
	found := false
	for _, r := range summaryReadEntries(t, e, c.ID) {
		if strings.Contains(r.Detail, "step 2 (run)") && strings.Contains(r.Detail, "GET /models/42") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no summary_read entry for step 2 (run): %+v", summaryReadEntries(t, e, c.ID))
	}
	deadline := time.After(3 * time.Second)
	for {
		select {
		case ev := <-events:
			if pl, ok := ev.Data.(store.Plan); ok && ev.Kind == "plan" && len(pl.Steps) == 2 && pl.Steps[1].SummaryRun != "" {
				return
			}
		case <-deadline:
			t.Fatal("no plan event carried the run summary")
		}
	}
}

// A step that goes out as stored needs no new text and no reads.
func TestRunSummaryUnchangedStep(t *testing.T) {
	e := setup(t)
	withRunSummaryPlatform(t, e, nil)
	ctx := store.WithSession(context.Background(), "main")
	c, _ := e.m.Create(ctx, NewChat{})
	slot := e.m.live[c.ID].slot.ID
	if r, _ := proposeAndDecide(t, e, ctx, c.ID, slot, planCover, true); r.Status != "ok" {
		t.Fatalf("plan: %+v", r)
	}
	before := len(summaryReadEntries(t, e, c.ID))
	if r, err := e.m.PlatformCall(ctx, c.ID, slot, "cli", liveCall(t, "GET", "/train/containers/7/model", "")); err != nil || r.Status != "ok" {
		t.Fatalf("step 1: %+v %v", r, err)
	}
	p := onlyPlan(t, e, ctx, c.ID)
	if p.Steps[0].SummaryRun != "" {
		t.Fatalf("summary_run of an unchanged step: %q", p.Steps[0].SummaryRun)
	}
	if n := len(summaryReadEntries(t, e, c.ID)); n != before {
		t.Fatalf("reads for an unchanged step: %d new entries", n-before)
	}
}

// A read that fails stores no text: the step keeps the proposed one.
func TestRunSummaryReadFailure(t *testing.T) {
	e := setup(t)
	fail := true
	withRunSummaryPlatform(t, e, &fail)
	ctx := store.WithSession(context.Background(), "main")
	c, _ := e.m.Create(ctx, NewChat{})
	slot := e.m.live[c.ID].slot.ID
	p := runPlanCover(t, e, ctx, c.ID, slot)
	if p.Steps[1].SummaryRun != "" {
		t.Fatalf("summary_run after a failed read: %q", p.Steps[1].SummaryRun)
	}
}

// A step that is not covered asks once; the run summary is the text of that approval, and no second read follows.
func TestRunSummaryUncoveredReusesApproval(t *testing.T) {
	e := setup(t)
	withRunSummaryPlatform(t, e, nil)
	ctx := store.WithSession(context.Background(), "main")
	c, _ := e.m.Create(ctx, NewChat{})
	slot := e.m.live[c.ID].slot.ID
	if r, _ := proposeAndDecide(t, e, ctx, c.ID, slot, planDelete, true); r.Status != "ok" {
		t.Fatalf("plan: %+v", r)
	}
	if r, err := e.m.PlatformCall(ctx, c.ID, slot, "cli", liveCall(t, "GET", "/train/containers/7/model", "")); err != nil || r.Status != "ok" {
		t.Fatalf("step 1: %+v %v", r, err)
	}
	events, cancel := e.m.Subscribe(c.ID)
	defer cancel()
	done := make(chan platform.Result, 1)
	go func() {
		r, _ := e.m.PlatformCall(ctx, c.ID, slot, "cli", liveCall(t, "DELETE", "/models/42", ""))
		done <- r
	}()
	ap := waitEvent(t, events, "approval", "").Data.(store.Approval)
	for ap.State != store.ApprovalPending {
		ap = waitEvent(t, events, "approval", "").Data.(store.Approval)
	}
	if _, err := e.m.Decide(ctx, ap.ID, true); err != nil {
		t.Fatal(err)
	}
	if r := <-done; r.Status != "ok" {
		t.Fatalf("step 2: %+v", r)
	}
	p := onlyPlan(t, e, ctx, c.ID)
	if p.Steps[1].SummaryRun == "" || p.Steps[1].SummaryRun != ap.Summary {
		t.Fatalf("summary_run %q, approval %q", p.Steps[1].SummaryRun, ap.Summary)
	}
	for _, r := range summaryReadEntries(t, e, c.ID) {
		if strings.Contains(r.Detail, "(run)") {
			t.Fatalf("a second read for the run: %+v", r)
		}
	}
}

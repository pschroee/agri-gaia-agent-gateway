// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"agw/internal/platform"
	"agw/internal/store"
	"agw/internal/watch"
)

// addJobHooks registers hooks for one test and removes them afterwards.
func addJobHooks(t *testing.T, progress, ended jobHook) {
	t.Helper()
	nProgress, nEnded := len(jobProgressHooks), len(jobEndedHooks)
	if progress != nil {
		registerJobProgress(progress)
	}
	if ended != nil {
		registerJobEnded(ended)
	}
	t.Cleanup(func() { jobProgressHooks, jobEndedHooks = jobProgressHooks[:nProgress], jobEndedHooks[:nEnded] })
}

// The job hooks of the training search see every poll and the end of a job, also outside of a plan (#168).
func TestJobHooksSeeProgressAndEnd(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	f := newFakeJobs(t, e, "/tasks/5")
	c, err := e.m.Create(ctx, NewChat{})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var progress, ended []string
	addJobHooks(t,
		func(_ context.Context, _ *Manager, j store.WatchJob, p watch.Progress) {
			mu.Lock()
			defer mu.Unlock()
			progress = append(progress, j.Ref+":"+p.State+":"+j.State)
		},
		func(_ context.Context, _ *Manager, j store.WatchJob, p watch.Progress) {
			mu.Lock()
			defer mu.Unlock()
			ended = append(ended, j.Ref+":"+p.State)
		})
	jobID, err := e.st.AddWatchJob(ctx, store.WatchJob{ChatID: c.ID, Kind: "platform_task", Ref: "task:5"})
	if err != nil || jobID == "" {
		t.Fatalf("job: %q %v", jobID, err)
	}
	f.set("GET /tasks/5", `{"status":"inprogress","completion_percentage":0.5,"message":"half"}`)
	runWatchersOnce(t, e)
	mu.Lock()
	if len(progress) != 1 || progress[0] != "task:5:running:running" || len(ended) != 0 {
		t.Fatalf("while running: progress=%v ended=%v", progress, ended)
	}
	mu.Unlock()
	f.set("GET /tasks/5", `{"status":"completed","completion_percentage":1.0,"message":"done"}`)
	runWatchersOnce(t, e)
	mu.Lock()
	defer mu.Unlock()
	if len(ended) != 1 || ended[0] != "task:5:done" {
		t.Fatalf("at the end: progress=%v ended=%v", progress, ended)
	}
}

// Without a search the seams change nothing: no claim, nothing recorded, the call runs as before (#168).
func TestPlatformCallSearchSeamNil(t *testing.T) {
	e := setup(t)
	got := countPlatform(t, e)
	ctx := context.Background()
	c, err := e.m.Create(ctx, NewChat{})
	if err != nil {
		t.Fatal(err)
	}
	slot := e.m.live[c.ID].slot.ID
	req := platform.Request{Method: "DELETE", Path: "/datasets/9", Body: json.RawMessage(`{"key":"-","csrftoken":"-","sessionid":"-"}`)}
	if claim := e.m.searchMatch(ctx, c.ID, req); claim != nil {
		t.Fatalf("the seam claimed a call: %+v", claim)
	}
	if (*trialClaim)(nil).ApprovalData() != nil {
		t.Error("a nil claim carries approval data")
	}
	// The call still asks the user, and the approval carries no search data.
	done := startCall(e, c.ID, slot, req)
	ap := pendingApproval(t, e, c.ID)
	if len(ap.Search) != 0 {
		t.Errorf("an ordinary approval carries search data: %s", ap.Search)
	}
	if _, err := e.m.Decide(ctx, ap.ID, true); err != nil {
		t.Fatal(err)
	}
	if r := <-done; r.Status != "ok" {
		t.Fatalf("call: %+v", r)
	}
	if n := got(); len(n) != 1 || n[0] != "DELETE /datasets/9" {
		t.Fatalf("platform calls: %v", n)
	}
}

// A proposal of a search is answered by the gateway; without the search service it is "unavailable", asks nobody
// and never reaches the platform (#168).
func TestProposeSearchUnavailableWithoutService(t *testing.T) {
	e := setup(t)
	got := countPlatform(t, e)
	ctx := context.Background()
	c, err := e.m.Create(ctx, NewChat{})
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.m.PlatformCall(ctx, c.ID, e.m.live[c.ID].slot.ID, "cli", platform.Request{Method: "POST", Path: platform.SearchPrefix,
		Form: map[string][]string{"title": {"t"}, "path": {"template"}}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != platform.StatusUnavailable || res.Decision != platform.DecisionAllowed || res.Reason != "virtual" {
		t.Fatalf("answer: %+v", res)
	}
	if aps, _ := e.st.ListApprovals(ctx, store.ApprovalPending, c.ID); len(aps) != 0 {
		t.Fatalf("a proposal without a service asked the user: %+v", aps)
	}
	if n := got(); len(n) != 0 {
		t.Fatalf("the proposal reached the platform: %v", n)
	}
	// The paths below the prefix are answered "not configured" as every virtual prefix without a doer.
	res, err = e.m.PlatformCall(ctx, c.ID, e.m.live[c.ID].slot.ID, "cli", platform.Request{Method: "GET", Path: platform.SearchPrefix + "/s0123456789ab"})
	if err != nil || res.Status != platform.StatusUnavailable {
		t.Fatalf("status path: %+v %v", res, err)
	}
}

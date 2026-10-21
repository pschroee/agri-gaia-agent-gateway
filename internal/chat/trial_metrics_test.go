// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"agw/internal/store"
)

func trialMetricsBody(epochs int, saved bool) string {
	lines := ""
	for i := 0; i < epochs; i++ {
		if i > 0 {
			lines += ","
		}
		lines += `{"epoch":` + itoa(i) + `,"epochs":4,"val_acc":` + itoa(i+1) + `0e-2,"at":"2026-10-20T10:00:0` + itoa(i) + `Z"}`
	}
	if saved && epochs > 0 {
		lines += `,{"epoch":` + itoa(epochs-1) + `,"epochs":4,"val_acc":` + itoa(epochs) + `0e-2,"at":"2026-10-20T10:00:09Z","saved":true}`
	}
	return `{"supported":true,"gpu_available":true,"epochs":[` + lines + `],` +
		`"gpu":[{"at":"2026-10-20T10:00:01Z","util":70,"mem_used_mb":500,"mem_total_mb":4000}]}`
}

func itoa(i int) string { return string(rune('0' + i)) }

func singleRunSetup(t *testing.T) (*env, ChatView, *fakeJobs, string) {
	t.Helper()
	e := setup(t)
	ctx := context.Background()
	f := newFakeJobs(t, e, "/tasks/5")
	c, err := e.m.Create(ctx, NewChat{})
	if err != nil {
		t.Fatal(err)
	}
	f.set("GET /train/containers/9/status", `{"status":"running","score":null}`)
	f.set("GET /train/containers/9/logs?tail=100", `{"logs":""}`)
	f.set("GET /train/containers/9/config", `{"train":{"values":{"lr":0.01,"epochs":4},"schema":{}}}`)
	jobID, err := e.st.AddWatchJob(ctx, store.WatchJob{ChatID: c.ID, Kind: trialKindTraining, Ref: "9"})
	if err != nil {
		t.Fatal(err)
	}
	return e, c, f, jobID
}

// Issue #168: a training run that no search declared becomes a single trial; the figures of its epochs, the GPU
// sample and the score of the default objective land in the tables, and a second poll stores only what is new.
func TestSingleRunBecomesTrial(t *testing.T) {
	e, c, f, jobID := singleRunSetup(t)
	ctx := context.Background()
	f.set("GET /train/containers/9/metrics", trialMetricsBody(2, false))
	runWatchersOnce(t, e)

	tr, err := e.st.TrialByJob(ctx, jobID)
	if err != nil {
		t.Fatalf("no trial for the job: %v", err)
	}
	if tr.SearchID != "" || tr.Idx != 0 || tr.State != store.TrialRunning || tr.Path != store.SearchPathTemplate || tr.Ref != "9" {
		t.Fatalf("trial: %+v", tr)
	}
	// jsonb normalises key order and spacing, so compare the values.
	var gotParams map[string]float64
	if err := json.Unmarshal(tr.Params, &gotParams); err != nil || len(gotParams) != 2 || gotParams["epochs"] != 4 || gotParams["lr"] != 0.01 {
		t.Errorf("params: %s", tr.Params)
	}
	if !isTrialJob(jobID) {
		t.Error("the job of a trial is read faster")
	}
	epochs, gpu, _ := e.st.TrialSeries(ctx, tr.ID)
	if len(epochs) != 2 || len(gpu) != 1 {
		t.Fatalf("series: %d epochs, %d gpu", len(epochs), len(gpu))
	}
	tr, _ = e.st.GetTrial(ctx, c.ID, tr.ID)
	if tr.Epoch == nil || *tr.Epoch != 1 || tr.Epochs == nil || *tr.Epochs != 4 || tr.Score == nil || *tr.Score != 0.2 {
		t.Errorf("progress: epoch %v epochs %v score %v", tr.Epoch, tr.Epochs, tr.Score)
	}

	// Two more epochs and the saved line: the score is the one of the saved line.
	f.set("GET /train/containers/9/metrics", trialMetricsBody(4, true))
	f.set("GET /train/containers/9/status", `{"status":"exited","score":0.4}`)
	runWatchersOnce(t, e)
	epochs, _, _ = e.st.TrialSeries(ctx, tr.ID)
	if len(epochs) != 4 {
		t.Fatalf("epochs after the second poll: %d", len(epochs))
	}
	tr, _ = e.st.GetTrial(ctx, c.ID, tr.ID)
	if tr.State != store.TrialDone || tr.Score == nil || *tr.Score != 0.4 {
		t.Errorf("after the end: %+v", tr)
	}
	if n, _ := e.st.ListTrials(ctx, c.ID); len(n) != 1 {
		t.Errorf("one run, one trial: %d", len(n))
	}
	if isTrialJob(jobID) {
		t.Error("the job of an ended trial is forgotten")
	}
}

// The events of a trial leave at most every progressThrottle; what comes between is gathered and sent, the last one
// at once at the end of the job.
func TestRecordTrialProgressThrottled(t *testing.T) {
	old := progressThrottle
	progressThrottle = 150 * time.Millisecond
	t.Cleanup(func() { progressThrottle = old })

	e, c, f, _ := singleRunSetup(t)
	events, cancel := e.m.Subscribe(c.ID)
	defer cancel()
	count := func(wait time.Duration) (n, points int) {
		deadline := time.After(wait)
		for {
			select {
			case ev := <-events:
				if ev.Kind == "trial_progress" {
					n++
					points += len(ev.Data.(TrialProgressEvent).Points)
				}
			case <-deadline:
				return
			}
		}
	}
	f.set("GET /train/containers/9/metrics", trialMetricsBody(1, false))
	runWatchersOnce(t, e)
	f.set("GET /train/containers/9/metrics", trialMetricsBody(2, false))
	runWatchersOnce(t, e)
	f.set("GET /train/containers/9/metrics", trialMetricsBody(3, false))
	runWatchersOnce(t, e)
	// The first went out at once, the others were gathered into one.
	n, points := count(600 * time.Millisecond)
	if n != 2 || points != 3 {
		t.Fatalf("events %d, points %d (want 2 events with 3 points)", n, points)
	}
	// The end of the job flushes at once, however recent the last event was.
	f.set("GET /train/containers/9/metrics", trialMetricsBody(4, true))
	f.set("GET /train/containers/9/status", `{"status":"exited","score":0.4}`)
	runWatchersOnce(t, e)
	if n, points := count(50 * time.Millisecond); n != 1 || points != 1 {
		t.Errorf("final event: %d events, %d points", n, points)
	}
}

// A run for which a trial was declared keeps that trial: the hook links the job to it and stores the figures.
func TestDeclaredTrialTakesTheJob(t *testing.T) {
	e, c, f, jobID := singleRunSetup(t)
	ctx := context.Background()
	decl, err := e.st.DeclareTrial(ctx, store.Trial{ChatID: c.ID, Path: store.SearchPathTemplate, Ref: "9", Params: []byte(`{"lr":0.5}`)})
	if err != nil {
		t.Fatal(err)
	}
	f.set("GET /train/containers/9/metrics", trialMetricsBody(2, false))
	f.set("GET /train/containers/9/status", `{"status":"exited","score":0.4}`)
	runWatchersOnce(t, e)
	tr, err := e.st.GetTrial(ctx, c.ID, decl.ID)
	if err != nil || tr.JobID != jobID || tr.Epoch == nil || *tr.Epoch != 1 {
		t.Fatalf("declared trial: %+v %v", tr, err)
	}
	if n, _ := e.st.ListTrials(ctx, c.ID); len(n) != 1 {
		t.Errorf("no second trial for the same run: %d", len(n))
	}
}

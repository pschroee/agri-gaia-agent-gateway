// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"agw/internal/platform"
	"agw/internal/store"
)

// withSpacing replaces the clock and the wait of the spacing; the returned slice collects the waits.
func withSpacing(t *testing.T, now *time.Time) *[]time.Duration {
	t.Helper()
	oldNow, oldSleep := observedNow, observedSleep
	waits := []time.Duration{}
	observedNow = func() time.Time { return *now }
	observedSleep = func(ctx context.Context, d time.Duration) error {
		waits = append(waits, d)
		return ctx.Err()
	}
	t.Cleanup(func() { observedNow, observedSleep = oldNow, oldSleep })
	return &waits
}

func addRunningJob(t *testing.T, e *env, chatID, kind, ref string) string {
	t.Helper()
	id, err := e.st.AddWatchJob(context.Background(), store.WatchJob{ChatID: chatID, Kind: kind, Ref: ref})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// #212 W1: a running observed job keeps its chat awake like a background task, an ended one does not.
func TestKeepAliveWithRunningJob(t *testing.T) {
	e := setup(t)
	withOptions(e, func(o *Options) { o.IdleTimeout = 150 * time.Millisecond; o.BgKeepAlive = 2500 * time.Millisecond })
	id, _ := settledChat(t, e)
	jobID := addRunningJob(t, e, id, "platform_task", "task:9")
	time.Sleep(400 * time.Millisecond)
	if v, _ := e.m.View(context.Background(), id); v.State != store.StateActive {
		t.Fatalf("idle despite a running job: %s", v.State)
	}
	if _, err := e.st.FinishWatchJob(context.Background(), jobID, store.JobDone); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "idling after the job ended", func() bool {
		v, _ := e.m.View(context.Background(), id)
		return v.State == store.StateDormant
	})
}

// #212 W2: the answer of a call that started a job outside a plan says the gateway watches it; the end reaches
// the agent as a note of the type job.
func TestOutsidePlanJobNote(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	f := newFakeJobs(t, e, "/tasks/7")
	c, err := e.m.Create(ctx, NewChat{})
	if err != nil {
		t.Fatal(err)
	}
	slot := e.m.live[c.ID].slot.ID
	res := callWithApproval(t, e, ctx, c.ID, slot, platform.Request{Method: http.MethodPost, Path: "/train/config"})
	jobs, err := e.m.Jobs(ctx, c.ID)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs: %+v %v", jobs, err)
	}
	if !strings.Contains(res.Note, jobs[0].ID) || !strings.Contains(res.Note, "end your turn") || !strings.Contains(res.Note, "tells you when it ends") {
		t.Fatalf("the answer does not tell the agent: %+v", res)
	}
	f.set("GET /tasks/7", `{"status":"completed","completion_percentage":1.0}`)
	runWatchersOnce(t, e)
	waitFor(t, func() bool {
		for _, p := range e.agent(0).prompts() {
			if strings.Contains(p, "Platform job "+jobs[0].ID) {
				return true
			}
		}
		return false
	})
}

// The trials of a search have notes of their own: no sentence in the answer, no note of the type job at the end.
func TestOutsidePlanTrialNoJobNote(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	f := newFakeJobs(t, e, "/tasks/7")
	c, err := e.m.Create(ctx, NewChat{})
	if err != nil {
		t.Fatal(err)
	}
	slot := e.m.live[c.ID].slot.ID
	callWithApproval(t, e, ctx, c.ID, slot, platform.Request{Method: http.MethodPost, Path: "/train/config"})
	jobs, err := e.m.Jobs(ctx, c.ID)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs: %+v %v", jobs, err)
	}
	// The row of the trial names the job (#252); the mark of the hook is not what decides.
	s, err := runningSearchIn(t, e, c)
	if err != nil {
		t.Fatal(err)
	}
	tr, err := e.st.DeclareTrial(ctx, store.Trial{ChatID: c.ID, SearchID: s.ID, Path: s.Path, Params: json.RawMessage(`{"lr":0.1}`)})
	if err != nil {
		t.Fatal(err)
	}
	// SetTrialJob only touches a running trial (the hook runs after the start)
	if ok, err := e.st.ClaimTrialStart(ctx, tr.ID, ""); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	if err := e.st.SetTrialJob(ctx, tr.ID, "7", jobs[0].ID); err != nil {
		t.Fatal(err)
	}
	f.set("GET /tasks/7", `{"status":"completed","completion_percentage":1.0}`)
	runWatchersOnce(t, e)
	if notes := queueNotes(t, e, c.ID, store.NoteJob); len(notes) != 0 {
		t.Fatalf("a note of the type job for a trial: %+v", notes)
	}
	for _, p := range e.agent(0).prompts() {
		if strings.Contains(p, "Platform job") {
			t.Fatalf("the agent was told about a trial's job: %q", p)
		}
	}
}

func TestObservedReadNote(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	f := newFakeJobs(t, e, "/tasks/7")
	f.set("GET /tasks/128", `{"status":"inprogress"}`)
	f.set("GET /tasks/129", `{"status":"inprogress"}`)
	c, err := e.m.Create(ctx, NewChat{})
	if err != nil {
		t.Fatal(err)
	}
	slot := e.m.live[c.ID].slot.ID
	now := time.Now()
	withSpacing(t, &now)
	jobID := addRunningJob(t, e, c.ID, "platform_task", "task:128|config:5")
	trial := addRunningJob(t, e, c.ID, "platform_task", "task:131")
	trialJobs.Store(trial, struct{}{})
	t.Cleanup(func() { trialJobs.Delete(trial) })
	read := func(path string) platform.Result {
		req, err := platform.Normalize(platform.Request{Method: http.MethodGet, Path: path})
		if err != nil {
			t.Fatal(err)
		}
		r, err := e.m.PlatformCall(ctx, c.ID, slot, "cli", req)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	if r := read("/tasks/128"); !strings.Contains(r.Note, jobID) || !strings.Contains(r.Note, "do not poll it") || !strings.Contains(r.Text(), "end your turn") {
		t.Fatalf("no note on the read of an observed job: %+v", r)
	}
	if r := read("/tasks/129"); r.Note != "" {
		t.Fatalf("note for a task nobody observes: %+v", r)
	}
	if r := read("/tasks/131"); r.Note != "" {
		t.Fatalf("note for a trial's job: %+v", r)
	}
	if _, err := e.st.FinishWatchJob(ctx, jobID, store.JobDone); err != nil {
		t.Fatal(err)
	}
	if r := read("/tasks/128"); r.Note != "" {
		t.Fatalf("note for a job that ended: %+v", r)
	}
}

func TestObservedReadSpacing(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	f := newFakeJobs(t, e, "/tasks/7")
	f.set("GET /tasks/128", `{"status":"inprogress"}`)
	f.set("GET /tasks/140", `{"status":"inprogress"}`)
	c, err := e.m.Create(ctx, NewChat{})
	if err != nil {
		t.Fatal(err)
	}
	slot := e.m.live[c.ID].slot.ID
	now := time.Now()
	waits := withSpacing(t, &now)
	addRunningJob(t, e, c.ID, "platform_task", "task:128")
	addRunningJob(t, e, c.ID, "platform_task", "task:140")
	read := func(path string) {
		req, err := platform.Normalize(platform.Request{Method: http.MethodGet, Path: path})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.m.PlatformCall(ctx, c.ID, slot, "cli", req); err != nil {
			t.Fatal(err)
		}
	}
	read("/tasks/128")
	if len(*waits) != 0 {
		t.Fatalf("the first read waited: %v", *waits)
	}
	now = now.Add(3 * time.Second)
	read("/tasks/128")
	if len(*waits) != 1 || (*waits)[0] != 12*time.Second {
		t.Fatalf("the second read after 3 s must wait 12 s: %v", *waits)
	}
	read("/tasks/140") // another job: no wait
	if len(*waits) != 1 {
		t.Fatalf("another job waited: %v", *waits)
	}
	now = now.Add(time.Minute)
	read("/tasks/128") // long enough ago
	if len(*waits) != 1 {
		t.Fatalf("a read after the spacing waited: %v", *waits)
	}
}

// The wait ends with the context.
func TestObservedReadSpacingCancelled(t *testing.T) {
	e := setup(t)
	now := time.Now()
	oldNow := observedNow
	observedNow = func() time.Time { return now }
	t.Cleanup(func() { observedNow = oldNow })
	req := platform.Request{Method: http.MethodGet, Path: "/tasks/1"}
	if err := e.m.spaceObservedRead(context.Background(), "c", "j", req); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := e.m.spaceObservedRead(ctx, "c", "j", req); err == nil {
		t.Fatal("a cancelled context must end the wait")
	}
}

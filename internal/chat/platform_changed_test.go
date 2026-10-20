// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"agw/internal/delegation"
	"agw/internal/platform"
	"agw/internal/store"
	"agw/internal/watch"
)

// changedWait: how long these tests wait for a platform_changed event.
const changedWait = 3 * time.Second

// nextChanged reads the next platform_changed event of a subscription, or nothing within the wait.
func nextChanged(ch <-chan ChatEvent, wait time.Duration) (PlatformChangedEvent, bool) {
	deadline := time.After(wait)
	for {
		select {
		case ev := <-ch:
			if ev.Kind != KindPlatformChanged {
				continue
			}
			d, ok := ev.Data.(PlatformChangedEvent)
			if !ok {
				return PlatformChangedEvent{}, false
			}
			return d, true
		case <-deadline:
			return PlatformChangedEvent{}, false
		}
	}
}

// statusPlatform answers every call with the given status and {}.
func statusPlatform(t *testing.T, e *env, status int) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_, _ = w.Write([]byte(`{"access_token":"t","expires_in":3600}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	c, err := platform.New(platform.Config{APIURL: srv.URL, TokenURL: srv.URL + "/token", User: "u", Password: "p"})
	if err != nil {
		t.Fatal(err)
	}
	e.m.opt.Platform = c
}

const confirmNoneDelegation = `{"rules":[{"action":"update","resource":"dataset","ids":["*"]},{"action":"read","resource":"dataset"}],"confirm":"none"}`

func TestPlatformChangedAfterWrite(t *testing.T) {
	e := setup(t)
	statusPlatform(t, e, 200)
	ctx := context.Background()
	c, err := e.m.Create(ctx, NewChat{Delegation: json.RawMessage(confirmNoneDelegation)})
	if err != nil {
		t.Fatal(err)
	}
	all, cancel := e.m.SubscribeAll()
	defer cancel()
	r, err := e.m.PlatformCall(ctx, c.ID, e.m.live[c.ID].slot.ID, "cli", liveCall(t, "PATCH", "/datasets/5", `{"name":"x"}`))
	if err != nil || r.Status != "ok" {
		t.Fatalf("call: %+v %v", r, err)
	}
	ev, ok := nextChanged(all, changedWait)
	if !ok {
		t.Fatal("no platform_changed event")
	}
	if ev.ChatID != c.ID || ev.Source != "call" || ev.Path != "/datasets/5" ||
		len(ev.Changes) != 1 || ev.Changes[0] != (delegation.Change{Resource: "dataset", Action: "update", ID: "5"}) {
		t.Fatalf("event: %+v", ev)
	}
	if _, again := nextChanged(all, 200*time.Millisecond); again {
		t.Fatal("more than one event for one call")
	}
}

func TestNoPlatformChanged(t *testing.T) {
	e := setup(t)
	statusPlatform(t, e, 500)
	ctx := context.Background()
	c, err := e.m.Create(ctx, NewChat{Delegation: json.RawMessage(confirmNoneDelegation)})
	if err != nil {
		t.Fatal(err)
	}
	slot := e.m.live[c.ID].slot.ID
	all, cancel := e.m.SubscribeAll()
	defer cancel()
	// The platform fails.
	_, _ = e.m.PlatformCall(ctx, c.ID, slot, "cli", liveCall(t, "PATCH", "/datasets/5", `{"name":"x"}`))
	// The delegation refuses (no rule for models).
	_, _ = e.m.PlatformCall(ctx, c.ID, slot, "cli", liveCall(t, "PATCH", "/models/5", `{"name":"x"}`))
	// A field rule refuses.
	_, _ = e.m.PlatformCall(ctx, c.ID, slot, "cli", liveCall(t, "PATCH", "/datasets/5", `{"bucket_name":"x"}`))
	// A read.
	statusPlatform(t, e, 200)
	_, _ = e.m.PlatformCall(ctx, c.ID, slot, "cli", liveCall(t, "GET", "/datasets", ""))
	if ev, ok := nextChanged(all, 300*time.Millisecond); ok {
		t.Fatalf("unexpected event: %+v", ev)
	}
	// A rejected approval: without a delegation the write asks, and the user says no.
	c2, err := e.m.Create(ctx, NewChat{})
	if err != nil {
		t.Fatal(err)
	}
	done := startCall(e, c2.ID, e.m.live[c2.ID].slot.ID, liveCall(t, "PATCH", "/datasets/5", `{"name":"x"}`))
	ap := pendingApproval(t, e, c2.ID)
	if _, err := e.m.Decide(ctx, ap.ID, false); err != nil {
		t.Fatal(err)
	}
	<-done
	if ev, ok := nextChanged(all, 300*time.Millisecond); ok {
		t.Fatalf("unexpected event after a rejected approval: %+v", ev)
	}
}

func TestPlatformChangedOnJobNextAndEnd(t *testing.T) {
	e := setup(t)
	newFakeJobs(t, e, "/tasks/5")
	ctx := context.Background()
	c, err := e.m.Create(ctx, NewChat{})
	if err != nil {
		t.Fatal(err)
	}
	phase := 0
	watch.Register(watch.Kind{Name: "pc_second", Resource: delegation.Model, Poll: func(context.Context, watch.Job, watch.Reader) (watch.Progress, error) {
		phase++
		if phase == 1 {
			return watch.Progress{State: watch.StateRunning}, nil // progress: no event
		}
		return watch.Progress{State: watch.StateDone, Outputs: []watch.Output{{Resource: delegation.Model, ID: "42"}}}, nil
	}})
	watch.Register(watch.Kind{Name: "pc_first", Resource: delegation.Task, Poll: func(context.Context, watch.Job, watch.Reader) (watch.Progress, error) {
		return watch.Progress{State: watch.StateRunning, Next: &watch.Job{Kind: "pc_second", Ref: "x"}}, nil
	}})
	jobID, err := e.st.AddWatchJob(ctx, store.WatchJob{ChatID: c.ID, Kind: "pc_first", Ref: "x"})
	if err != nil {
		t.Fatal(err)
	}
	all, cancel := e.m.SubscribeAll()
	defer cancel()
	runWatchersOnce(t, e)
	ev, ok := nextChanged(all, changedWait)
	if !ok || ev.Source != "job" || ev.JobID != jobID || ev.Path != "" ||
		len(ev.Changes) != 1 || ev.Changes[0] != (delegation.Change{Resource: "model", Action: "run"}) {
		t.Fatalf("next: %+v %v", ev, ok)
	}
	runWatchersOnce(t, e) // progress only
	if ev, ok := nextChanged(all, 300*time.Millisecond); ok {
		t.Fatalf("progress sent an event: %+v", ev)
	}
	runWatchersOnce(t, e) // ended
	ev, ok = nextChanged(all, changedWait)
	if !ok || len(ev.Changes) != 2 || ev.Changes[0] != (delegation.Change{Resource: "model", Action: "update"}) ||
		ev.Changes[1] != (delegation.Change{Resource: "model", Action: "create", ID: "42"}) {
		t.Fatalf("end: %+v %v", ev, ok)
	}
}

func TestPlatformChangedOnUserStop(t *testing.T) {
	e := setup(t)
	newFakeJobs(t, e, "/tasks/5")
	ctx := context.Background()
	c, err := e.m.Create(ctx, NewChat{})
	if err != nil {
		t.Fatal(err)
	}
	watch.Register(watch.Kind{Name: "pc_stoppable", Resource: delegation.Training,
		Poll: func(context.Context, watch.Job, watch.Reader) (watch.Progress, error) {
			return watch.Progress{State: watch.StateRunning}, nil
		},
		Stop: func(watch.Job) (platform.Request, bool) {
			return platform.Request{Method: "POST", Path: "/train/containers/7/stop"}, true
		}})
	jobID, err := e.st.AddWatchJob(ctx, store.WatchJob{ChatID: c.ID, Kind: "pc_stoppable", Ref: "7"})
	if err != nil {
		t.Fatal(err)
	}
	all, cancel := e.m.SubscribeAll()
	defer cancel()
	if _, err := e.m.StopJob(ctx, c.ID, jobID); err != nil {
		t.Fatal(err)
	}
	ev, ok := nextChanged(all, changedWait)
	if !ok || ev.Source != "stop" || ev.JobID != jobID ||
		len(ev.Changes) != 1 || ev.Changes[0] != (delegation.Change{Resource: "training", Action: "run"}) {
		t.Fatalf("stop: %+v %v", ev, ok)
	}
}

func TestPublishPlatformChangedEmptyAndShape(t *testing.T) {
	m := NewManager(nil, nil, nil, nil, nil, Options{})
	all, cancel := m.SubscribeAll()
	defer cancel()
	m.PublishPlatformChanged("c1", PlatformChangedEvent{Source: "call"})
	if _, ok := nextChanged(all, 100*time.Millisecond); ok {
		t.Fatal("an empty change list must send nothing")
	}
	m.PublishPlatformChanged("c1", PlatformChangedEvent{Source: "call", Path: "/datasets/5",
		Changes: []delegation.Change{{Resource: "dataset", Action: "update", ID: "5"}}})
	ev, ok := nextChanged(all, changedWait)
	if !ok {
		t.Fatal("no event")
	}
	b, _ := json.Marshal(Event{Kind: KindPlatformChanged, Data: ev})
	var back struct {
		Kind string `json:"kind"`
		Data struct {
			ChatID  string `json:"chat_id"`
			Source  string `json:"source"`
			Path    string `json:"path"`
			JobID   string `json:"job_id"`
			At      string `json:"at"`
			Changes []struct {
				Resource, Action, ID string
			} `json:"changes"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &back); err != nil || back.Kind != "platform_changed" || back.Data.ChatID != "c1" || back.Data.At == "" {
		t.Fatalf("shape: %s %v", b, err)
	}
}

// #173: a call that started a platform task lists it in the event.
func TestPlatformChangedCarriesTask(t *testing.T) {
	e := setup(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_, _ = w.Write([]byte(`{"access_token":"t","expires_in":3600}`))
			return
		}
		w.Header().Set("Location", "/tasks/42")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	c, err := platform.New(platform.Config{APIURL: srv.URL, TokenURL: srv.URL + "/token", User: "u", Password: "p"})
	if err != nil {
		t.Fatal(err)
	}
	e.m.opt.Platform = c
	ctx := context.Background()
	ch, err := e.m.Create(ctx, NewChat{Delegation: json.RawMessage(`{"rules":[{"action":"create","resource":"container_image","ids":["*"]}],"confirm":"none"}`)})
	if err != nil {
		t.Fatal(err)
	}
	all, cancel := e.m.SubscribeAll()
	defer cancel()
	r, err := e.m.PlatformCall(ctx, ch.ID, e.m.live[ch.ID].slot.ID, "cli",
		liveCall(t, "POST", "/container-images/build", `{"repository":"u/i","tag":"v1","model_id":1}`))
	if err != nil || r.Status != "ok" {
		t.Fatalf("call: %+v %v", r, err)
	}
	ev, ok := nextChanged(all, changedWait)
	if !ok {
		t.Fatal("no platform_changed event")
	}
	found := false
	for _, x := range ev.Changes {
		if x == (delegation.Change{Resource: "task", Action: "create", ID: "42"}) {
			found = true
		}
	}
	if !found {
		t.Fatalf("task 42 missing in %+v", ev.Changes)
	}
}

// #173: a task the main entry already names is not listed twice.
func TestWithTasksNoDuplicate(t *testing.T) {
	ch := []delegation.Change{{Resource: "training", Action: "create", ID: "9"}, {Resource: "task", Action: "create", ID: "5"}}
	got := withTasks(ch, []string{"5", "6", "6"})
	if len(got) != 3 || got[2] != (delegation.Change{Resource: "task", Action: "create", ID: "6"}) {
		t.Fatalf("got %+v", got)
	}
	if got := withTasks(nil, nil); len(got) != 0 {
		t.Fatalf("empty: %+v", got)
	}
}

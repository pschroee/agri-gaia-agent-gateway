// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"agw/internal/agwclient"
)

func ok(body string) agwclient.ExpCallResult {
	return agwclient.ExpCallResult{Status: "ok", HTTPStatus: 200, Body: body}
}

// BM11: a run that ended at a limit leaves its image build running. The cleanup waits until the task has
// ended, waits for the registry to list the image and deletes it, so that it cannot appear in the next run.
func TestCleanupWaitsForBuildAndDeletesItsImage(t *testing.T) {
	api := newFakeExpAPI()
	api.jobs["c1"] = []agwclient.WatchJob{{ID: "j1", ChatID: "c1", Kind: "image_build", State: "stopped", Ref: "task:41|image:agent-exp/garage-fahrzeug:v1"}}
	taskReads, listed := 0, 0
	api.callScript = func(req agwclient.ExpCallRequest) (agwclient.ExpCallResult, bool) {
		switch {
		case req.Method == "GET" && req.Path == "/tasks/41":
			taskReads++
			if taskReads < 4 {
				return ok(`{"status":"inprogress"}`), true
			}
			return ok(`{"status":"completed"}`), true
		case req.Method == "GET" && req.Path == "/container-images/agent-exp/garage-fahrzeug:v1":
			listed++
			if listed < 3 { // the registry's webhook is a little late
				return agwclient.ExpCallResult{Status: "error", HTTPStatus: 404}, true
			}
			return ok(`[{"id":167}]`), true
		}
		return agwclient.ExpCallResult{}, false
	}
	r, _, errw := newTestRunner(t, api)
	r.buildWait, r.registryWait = time.Second, time.Second
	if err := r.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if taskReads < 4 || listed < 3 {
		t.Fatalf("the cleanup did not wait: task read %d times, registry %d", taskReads, listed)
	}
	var images []string
	for _, p := range api.deletes() {
		if strings.HasPrefix(p, "/container-images/") {
			images = append(images, p)
		}
	}
	if len(images) != 1 || images[0] != "/container-images/agent-exp/garage-fahrzeug:v1" {
		t.Fatalf("image deletes: %v", images)
	}
	if !strings.Contains(errw.String(), "waited for the image build") {
		t.Fatalf("the wait should be reported: %s", errw.String())
	}
	// The next run only starts after the wait: the end of the run is recorded after the last task read.
	if len(api.ends) == 0 {
		t.Fatal("run not ended")
	}
}

// A build that failed leaves no image: nothing is awaited in the registry and nothing deleted.
func TestCleanupFailedBuildLeavesNothingToDelete(t *testing.T) {
	api := newFakeExpAPI()
	api.jobs["c1"] = []agwclient.WatchJob{{ID: "j1", ChatID: "c1", Kind: "image_build", State: "failed", Ref: "task:41|image:agent-exp/x:v1"}}
	api.callScript = func(req agwclient.ExpCallRequest) (agwclient.ExpCallResult, bool) {
		if req.Method == "GET" && req.Path == "/tasks/41" {
			return ok(`{"status":"failed"}`), true
		}
		return agwclient.ExpCallResult{}, false
	}
	r, _, _ := newTestRunner(t, api)
	if err := r.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, p := range api.order {
		if strings.Contains(p, "/container-images/") {
			t.Fatalf("a failed build has no image, but %q was called", p)
		}
	}
}

// A build that does not end within buildWait stops the plan after the run is recorded.
func TestCleanupStuckBuildStopsThePlan(t *testing.T) {
	api := newFakeExpAPI()
	api.jobs["c1"] = []agwclient.WatchJob{{ID: "j1", ChatID: "c1", Kind: "image_build", State: "stopped", Ref: "task:41|image:agent-exp/x:v1"}}
	api.callScript = func(req agwclient.ExpCallRequest) (agwclient.ExpCallResult, bool) {
		if req.Method == "GET" && req.Path == "/tasks/41" {
			return ok(`{"status":"inprogress"}`), true
		}
		return agwclient.ExpCallResult{}, false
	}
	r, _, _ := newTestRunner(t, api)
	r.buildWait = 20 * time.Millisecond
	err := r.run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "image build still running") {
		t.Fatalf("want the plan to stop, got %v", err)
	}
	if len(api.ends) != 1 {
		t.Fatalf("the run is still recorded: %d ends", len(api.ends))
	}
}

// Only a chat's own build jobs count; a registry reference (task ended, row awaited) is followed as well.
func TestSettleBuildsJobKinds(t *testing.T) {
	api := newFakeExpAPI()
	api.jobs["c1"] = []agwclient.WatchJob{
		{Kind: "platform_task", Ref: "task:7|config:3"},
		{Kind: "image_build", Ref: "registry:agent-exp/y:v2@1700000000"},
		{Kind: "image_build", Ref: "garbage"},
	}
	api.answers["/container-images/agent-exp/y:v2"] = `[]`
	api.callScript = func(req agwclient.ExpCallRequest) (agwclient.ExpCallResult, bool) {
		if req.Path == "/container-images/agent-exp/y:v2" {
			return ok(`[{"id":9}]`), true
		}
		return agwclient.ExpCallResult{}, false
	}
	r, _, _ := newTestRunner(t, api)
	built, err := r.settleBuilds(context.Background(), "c1")
	if err != nil || len(built) != 1 || built[0] != "agent-exp/y:v2" {
		t.Fatalf("built %v, err %v", built, err)
	}
}

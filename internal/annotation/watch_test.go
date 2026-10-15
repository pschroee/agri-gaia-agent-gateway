// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package annotation

import (
	"context"
	"testing"

	"agw/internal/delegation"
	"agw/internal/platform"
	"agw/internal/store"
	"agw/internal/watch"
)

// The kinds of a started run are registered with internal/watch: the route of a run answers the run id
// from its answer, and a CVAT status becomes the state of the run (G1.7).
func TestAnnotationRunWatchKind(t *testing.T) {
	e := newTestEnv(t)
	chatID := readyDataset(t, e)
	e.svc.RegisterWatch()

	call := platform.Request{Method: "POST", Path: platform.AnnotationPrefix + "/runs"}
	res := e.do(t, chatID, "POST", platform.AnnotationPrefix+"/runs",
		`{"dataset_id":5,"function":"agw-test-m1-abcd1234","min_confidence":50,"mapping":{"class_0":"0"},"existing":"keep"}`)
	kind, ref, ok := watch.For(delegation.Classify(call), call, res)
	if !ok || kind != WatchKindName || ref != "run-1" {
		t.Fatalf("route: %q %q %v", kind, ref, ok)
	}
	// An answer without a run id carries no reference.
	if _, _, ok := watch.For(delegation.Classify(call), call, platform.Result{Status: "ok", Body: `{}`}); ok {
		t.Error("a run without an id gave a reference")
	}

	// CVAT's statuses become the states of the gateway.
	for status, want := range map[string]string{
		"queued":    store.AnnotationQueued,
		"deferred":  store.AnnotationQueued,
		"scheduled": store.AnnotationQueued,
		"started":   store.AnnotationStarted,
		"finished":  store.AnnotationFinished,
		"failed":    store.AnnotationFailed,
		"canceled":  store.AnnotationFailed,
		"":          store.AnnotationQueued,
	} {
		if got := runState(status); got != want {
			t.Errorf("state of %q: %q, want %q", status, got, want)
		}
	}
}

// The observer reads the state at CVAT, keeps the row up to date and reports the progress; a run CVAT
// no longer knows becomes expired (E2).
func TestPollRun(t *testing.T) {
	e := newTestEnv(t)
	chatID := e.chat(t)
	ctx := context.Background()
	if _, err := e.st.AddAnnotationRun(ctx, store.AnnotationRun{
		ID: "run-1", ChatID: chatID, DatasetID: 5, TaskID: 7, Function: "f", MinConfidence: 50, Existing: "keep"}); err != nil {
		t.Fatal(err)
	}
	job := watch.Job{ID: "1", ChatID: chatID, Kind: WatchKindName, Ref: "run-1"}

	e.fc.setRun("run-1", `{"id":"run-1","status":"started","progress":30}`)
	got, err := e.svc.poll(ctx, job, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "running" || got.Percent == nil || *got.Percent != 30 {
		t.Fatalf("running: %+v", got)
	}
	if row, _ := e.st.AnnotationRun(ctx, chatID, "run-1"); row.State != store.AnnotationStarted {
		t.Errorf("row: %+v", row)
	}

	// A failed run carries the first line of the traceback, shortened.
	e.fc.setRun("run-1", `{"id":"run-1","status":"failed","progress":100,"exc_info":"Traceback (most recent call last):\nboom"}`)
	got, err = e.svc.poll(ctx, job, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "failed" || got.Message != "failed: Traceback (most recent call last):" {
		t.Errorf("failed: %+v", got)
	}

	// A finished run is done; the row is final, so further polls answer from it without CVAT.
	e.fc.setRun("run-1", `{"id":"run-1","status":"finished","progress":100}`)
	if got, err = e.svc.poll(ctx, job, nil); err != nil || got.State != "done" {
		t.Fatalf("finished: %+v %v", got, err)
	}
	before := len(e.fc.saw())
	e.fc.setRun("run-1", `{"id":"run-1","status":"queued"}`)
	if got, err = e.svc.poll(ctx, job, nil); err != nil || got.State != "done" {
		t.Fatalf("after the end: %+v %v", got, err)
	}
	if len(e.fc.saw()) != before {
		t.Errorf("CVAT was asked again for a finished run: %v", e.fc.saw())
	}

	// A second run CVAT does not know any more becomes expired.
	if _, err := e.st.AddAnnotationRun(ctx, store.AnnotationRun{
		ID: "run-2", ChatID: chatID, DatasetID: 5, TaskID: 7, Function: "f", MinConfidence: 50, Existing: "keep"}); err != nil {
		t.Fatal(err)
	}
	got, err = e.svc.poll(ctx, watch.Job{ChatID: chatID, Kind: WatchKindName, Ref: "run-2"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "done" || got.Message == "" {
		t.Fatalf("expired: %+v", got)
	}
	if row, _ := e.st.AnnotationRun(ctx, chatID, "run-2"); row.State != store.AnnotationExpired {
		t.Errorf("row of the expired run: %+v", row)
	}
}

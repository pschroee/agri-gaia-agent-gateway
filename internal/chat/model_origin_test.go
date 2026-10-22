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

	"agw/internal/platform"
	"agw/internal/store"
)

const (
	modelKey = "GET /train/containers/84/model"
	listKey  = "GET /train/containers?limit=1000"
)

// callVia runs a writing platform call over the named binding and approves it.
func callVia(t *testing.T, e *env, ctx context.Context, chatID, slot, via string, req platform.Request) platform.Result {
	t.Helper()
	events, cancel := e.m.Subscribe(chatID)
	defer cancel()
	norm, err := platform.Normalize(req)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan platform.Result, 1)
	go func() {
		r, _ := e.m.PlatformCall(ctx, chatID, slot, via, norm)
		done <- r
	}()
	ap := waitEvent(t, events, "approval", "").Data.(store.Approval)
	for ap.State != store.ApprovalPending {
		ap = waitEvent(t, events, "approval", "").Data.(store.Approval)
	}
	if _, err := e.m.Decide(ctx, ap.ID, true); err != nil {
		t.Fatal(err)
	}
	return <-done
}

func originEnv(t *testing.T) (*env, context.Context, string, string, *fakeJobs) {
	t.Helper()
	e := setup(t)
	ctx := context.Background()
	f := newFakeJobs(t, e, "/tasks/7")
	c, err := e.m.Create(ctx, NewChat{})
	if err != nil {
		t.Fatal(err)
	}
	return e, ctx, c.ID, e.m.live[c.ID].slot.ID, f
}

// Issue #211: the origin is written whichever binding made the call, with no tool involved.
func TestModelOriginFromRawCall(t *testing.T) {
	for _, via := range []string{"api", "cli", "mcp"} { // api: platform_http; cli, mcp: the raw tool request
		t.Run(via, func(t *testing.T) {
			e, ctx, chatID, slot, f := originEnv(t)
			f.set(modelKey, `{"id":31,"name":"m"}`)
			f.set(listKey, `[{"id":83,"dataset_id":9},{"id":84,"dataset_id":141}]`)
			res := callVia(t, e, ctx, chatID, slot, via, platform.Request{Method: http.MethodGet, Path: "/train/containers/84/model"})
			if res.Status != "ok" || !strings.Contains(res.Note, "model 31") || !strings.Contains(res.Note, "training container 84, dataset 141") {
				t.Fatalf("answer: %+v", res)
			}
			o, err := e.st.ModelOriginOf(ctx, chatID, 31)
			if err != nil || o.Source != store.OriginTraining || o.TrainContainerID != 84 || len(o.SeenDatasetIDs) != 1 || o.SeenDatasetIDs[0] != 141 {
				t.Fatalf("origin: %+v %v", o, err)
			}
		})
	}
}

// The tool creates the model through the same call: one row, the note once.
func TestModelOriginViaTool(t *testing.T) {
	e, ctx, chatID, slot, f := originEnv(t)
	f.set("GET /train/containers/84/status", `{"status":"exited"}`)
	f.set(listKey, `[{"id":84,"dataset_id":141}]`)
	f.set("GET /datasets/141", `{"id":141}`)
	f.set(modelKey, `{"id":31,"name":"m","format":"pytorch"}`)
	tool, ok := platform.Lookup("create_model_from_training")
	if !ok {
		t.Fatal("tool missing")
	}
	events, cancel := e.m.Subscribe(chatID)
	defer cancel()
	do := func(c context.Context, r platform.Request) (platform.Result, error) {
		return e.m.PlatformCall(c, chatID, slot, "mcp", r)
	}
	done := make(chan platform.Result, 1)
	go func() {
		r, _, _ := tool.Run(ctx, json.RawMessage(`{"train_container_id":84}`), do)
		done <- r
	}()
	ap := waitEvent(t, events, "approval", "").Data.(store.Approval)
	for ap.State != store.ApprovalPending {
		ap = waitEvent(t, events, "approval", "").Data.(store.Approval)
	}
	if _, err := e.m.Decide(ctx, ap.ID, true); err != nil {
		t.Fatal(err)
	}
	res := <-done
	if res.Status != "ok" || strings.Count(res.Note, "recorded where model 31 came from") != 1 {
		t.Fatalf("answer: %+v", res)
	}
	if o, err := e.st.ModelOriginOf(ctx, chatID, 31); err != nil || o.TrainContainerID != 84 {
		t.Fatalf("origin: %+v %v", o, err)
	}
}

// No origin without a model, a dataset or a successful call of the right path.
func TestModelOriginNotRecorded(t *testing.T) {
	cases := []struct {
		name  string
		setup func(f *fakeJobs)
		path  string
	}{
		{"platform error", func(f *fakeJobs) { f.failRead(modelKey, 500); f.set(listKey, `[{"id":84,"dataset_id":141}]`) }, "/train/containers/84/model"},
		{"not in the list", func(f *fakeJobs) { f.set(modelKey, `{"id":31}`); f.set(listKey, `[{"id":83,"dataset_id":9}]`) }, "/train/containers/84/model"},
		{"no dataset", func(f *fakeJobs) { f.set(modelKey, `{"id":31}`); f.set(listKey, `[{"id":84,"dataset_id":0}]`) }, "/train/containers/84/model"},
		{"no model id", func(f *fakeJobs) { f.set(modelKey, `{}`); f.set(listKey, `[{"id":84,"dataset_id":141}]`) }, "/train/containers/84/model"},
		{"other path", func(f *fakeJobs) {
			f.set("GET /train/containers/84/status", `{"id":31}`)
			f.set(listKey, `[{"id":84,"dataset_id":141}]`)
		}, "/train/containers/84/status"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, ctx, chatID, slot, f := originEnv(t)
			c.setup(f)
			var res platform.Result
			if c.path == "/train/containers/84/model" {
				res = callVia(t, e, ctx, chatID, slot, "api", platform.Request{Method: http.MethodGet, Path: c.path})
			} else {
				res, _ = e.m.PlatformCall(ctx, chatID, slot, "api", platform.Request{Method: http.MethodGet, Path: c.path})
			}
			if strings.Contains(res.Note, "recorded where model") {
				t.Fatalf("note without an origin: %+v", res)
			}
			if o, err := e.st.ModelOriginOf(ctx, chatID, 31); err == nil {
				t.Fatalf("origin recorded: %+v", o)
			}
		})
	}
}

// A check on a model taken over by a raw call knows where it came from (no "origin unknown").
func TestAssessAfterRESTModel(t *testing.T) {
	e, ctx, chatID, slot, f := originEnv(t)
	f.set(modelKey, `{"id":31,"name":"m"}`)
	f.set(listKey, `[{"id":84,"dataset_id":141}]`)
	callVia(t, e, ctx, chatID, slot, "api", platform.Request{Method: http.MethodGet, Path: "/train/containers/84/model"})
	o, err := e.st.ModelOriginOf(ctx, chatID, 31)
	if err != nil || o.Source != store.OriginTraining || len(o.SeenDatasetIDs) == 0 {
		t.Fatalf("the check reads this row; it must name the datasets the model saw: %+v %v", o, err)
	}
}

func TestJoinNote(t *testing.T) {
	if got := joinNote("", "a", " ", "b"); got != "a b" {
		t.Errorf("got %q", got)
	}
}

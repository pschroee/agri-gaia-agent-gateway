// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package watch

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"agw/internal/delegation"
	"agw/internal/platform"
)

// recorded answers the calls of a kind from a table of recorded platform answers.
func recorded(t *testing.T, answers map[string]string) (Reader, func() []string) {
	t.Helper()
	var got []string
	read := func(_ context.Context, r platform.Request) (platform.Result, error) {
		key := r.String() // "GET /train/containers/9/logs?tail=20"
		got = append(got, key)
		body, ok := answers[key]
		if !ok {
			return platform.Result{Status: "error", HTTPStatus: 404, Message: "platform responds 404"}, nil
		}
		return platform.Result{Status: "ok", HTTPStatus: http.StatusOK, Body: body}, nil
	}
	return read, func() []string { return append([]string(nil), got...) }
}

// A-E2: the states of a platform task. completion_percentage comes as a fraction and becomes the job's
// percent; created and inprogress mean running, completed done, failed failed.
func TestTaskStates(t *testing.T) {
	cases := []struct {
		answer string
		state  string
		pcnt   *float64
		msg    string
	}{
		{`{"status":"created","completion_percentage":0.0,"message":null}`, StateRunning, nil, ""},
		{`{"status":"inprogress","completion_percentage":0.4,"message":"building"}`, StateRunning, floatPtr(40), "building"},
		{`{"status":"completed","completion_percentage":1.0,"message":"done"}`, StateDone, floatPtr(100), "done"},
		{`{"status":"failed","completion_percentage":0.2,"message":"no space left"}`, StateFailed, floatPtr(20), "no space left"},
	}
	for _, c := range cases {
		read, calls := recorded(t, map[string]string{"GET /tasks/5": c.answer})
		p, err := pollPlatformTask(context.Background(), Job{ID: "w1", Kind: kindPlatformTask, Ref: "task:5"}, read)
		if err != nil {
			t.Fatalf("%s: %v", c.answer, err)
		}
		if p.State != c.state || p.Message != c.msg {
			t.Errorf("%s: %+v", c.answer, p)
		}
		if (p.Percent == nil) != (c.pcnt == nil) || (p.Percent != nil && *p.Percent != *c.pcnt) {
			t.Errorf("%s: percent %v, want %v", c.answer, p.Percent, c.pcnt)
		}
		if p.Next != nil {
			t.Errorf("%s: an unchained task has no next job: %+v", c.answer, p.Next)
		}
		if len(calls()) != 1 {
			t.Errorf("%s: %v", c.answer, calls())
		}
	}
	// A task that is not readable is an error of the reading call, not a state.
	read, _ := recorded(t, nil)
	if _, err := pollPlatformTask(context.Background(), Job{ID: "w1", Ref: "task:5"}, read); err == nil {
		t.Error("a 404 must be an error")
	}
	// A job without a task reference cannot be polled.
	if _, err := pollPlatformTask(context.Background(), Job{ID: "w1", Ref: ""}, read); err == nil {
		t.Error("a job without a reference must be an error")
	}
}

// A-E2: which calls start a job of which kind, and which do not.
func TestForUnknownRouteNoWatch(t *testing.T) {
	cases := []struct {
		method, path, location string
		kind, ref              string
	}{
		{"POST", "/train/config", "/tasks/5", kindPlatformTask, "task:5"},
		{"POST", "/container-images/build", "/tasks/7", kindImageBuild, "task:7|image:u/i:v1"},
		{"POST", "/train/containers/9/run", "/tasks/11", kindPlatformTask, "task:11|train:9"},
	}
	for _, c := range cases {
		req := platform.Request{Method: c.method, Path: c.path}
		if c.kind == kindImageBuild {
			req.Body = json.RawMessage(`{"repository":"u/i","tag":"v1"}`)
		}
		res := platform.Result{Status: "ok", HTTPStatus: 202, Location: c.location}
		kind, ref, ok := For(delegation.Access{}, req, res)
		if !ok || kind != c.kind || ref != c.ref {
			t.Errorf("%s %s: %q %q %v, want %q %q", c.method, c.path, kind, ref, ok, c.kind, c.ref)
		}
	}
	// Nothing registered: no job.
	for _, req := range []platform.Request{
		{Method: "POST", Path: "/datasets"},
		{Method: "GET", Path: "/train/config"},
		{Method: "POST", Path: "/train/containers/9/stop"},
	} {
		if kind, _, ok := For(delegation.Access{}, req, platform.Result{Status: "ok", HTTPStatus: 202, Location: "/tasks/1"}); ok {
			t.Errorf("%s %s must not start a job (%s)", req.Method, req.Path, kind)
		}
	}
	// The route matches, but the answer names no task: no job either (an answer without Location).
	if _, _, ok := For(delegation.Access{}, platform.Request{Method: "POST", Path: "/train/config"}, platform.Result{Status: "ok", HTTPStatus: 200}); ok {
		t.Error("without a Location there is no task to observe")
	}
}

// A-E2: the reference of an image build names the image the build produced, so that a card can show it
// and the outputs carry it.
func TestImageBuildCarriesItsImage(t *testing.T) {
	req := platform.Request{Method: http.MethodPost, Path: "/container-images/build",
		Body: json.RawMessage(`{"repository":"u/mnist-triton","tag":"v1","model_id":3}`)}
	res := platform.Result{Status: "ok", HTTPStatus: 202, Location: "/tasks/8"}
	kind, ref, ok := For(delegation.Access{}, req, res)
	if !ok || kind != kindImageBuild || ref != "task:8|image:u/mnist-triton:v1" {
		t.Fatalf("For = %q %q %v", kind, ref, ok)
	}
	read, _ := recorded(t, map[string]string{"GET /tasks/8": `{"status":"completed","completion_percentage":1.0}`})
	p, err := pollImageBuild(context.Background(), Job{ID: "w1", Kind: kindImageBuild, Ref: ref}, read)
	if err != nil || p.State != StateDone {
		t.Fatalf("poll: %+v %v", p, err)
	}
	if len(p.Outputs) != 1 || p.Outputs[0].Resource != "container_image" || p.Outputs[0].ID != "u/mnist-triton:v1" {
		t.Fatalf("outputs: %+v", p.Outputs)
	}
	// A build without a repository or tag in its body still starts a task; only the image is unnamed.
	req.Body = json.RawMessage(`{"model_id":3}`)
	if _, ref, ok := For(delegation.Access{}, req, res); ok {
		t.Fatalf("a build without an image name must not be observed: %q", ref)
	}
}

// floatPtr is the pointer the tests compare against.
func floatPtr(f float64) *float64 { return &f }

// #173: the tasks a call started, from Location and from the watched reference.
func TestTaskIDsFromLocationAndRef(t *testing.T) {
	ok202 := func(loc string) platform.Result {
		return platform.Result{Status: "ok", HTTPStatus: 202, Location: loc}
	}
	cases := []struct {
		name string
		req  platform.Request
		res  platform.Result
		want []string
	}{
		{"training config", platform.Request{Method: "POST", Path: "/train/config", Body: json.RawMessage(`{"dataset_id":2}`)}, ok202("/tasks/5"), []string{"5"}},
		{"run with chain", platform.Request{Method: "POST", Path: "/train/containers/7/run"}, ok202("/tasks/11"), []string{"11"}},
		{"stop without watch", platform.Request{Method: "POST", Path: "/train/containers/7/stop"}, ok202("/tasks/12"), []string{"12"}},
		{"no task", platform.Request{Method: "PATCH", Path: "/datasets/5"}, platform.Result{Status: "ok", HTTPStatus: 200}, nil},
		{"other location", platform.Request{Method: "POST", Path: "/datasets"}, ok202("/datasets/3"), nil},
	}
	for _, c := range cases {
		got := TaskIDs(delegation.Access{}, c.req, c.res)
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
	// The same id from Location and from the reference counts once.
	got := TaskIDs(delegation.Access{}, platform.Request{Method: "POST", Path: "/train/config"}, ok202("/tasks/5"))
	if len(got) != 1 || got[0] != "5" {
		t.Errorf("duplicate id: %v", got)
	}
}

// #173: a task named in the body of a virtual call (the annotation build) comes from the watched
// reference, not from Location.
func TestTaskIDsFromBodyReference(t *testing.T) {
	Register(Kind{Name: "test_taskids_body"})
	RegisterRoute(http.MethodPost, regexp.MustCompile(`^/test-taskids-body$`), "test_taskids_body",
		func(_ platform.Request, res platform.Result) (string, bool) {
			return "task:" + strings.TrimSpace(res.Body), true
		})
	got := TaskIDs(delegation.Access{}, platform.Request{Method: "POST", Path: "/test-taskids-body"},
		platform.Result{Status: "ok", HTTPStatus: 200, Body: "33"})
	if len(got) != 1 || got[0] != "33" {
		t.Errorf("body reference: %v", got)
	}
}

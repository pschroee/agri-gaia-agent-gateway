// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package watch

import (
	"context"
	"regexp"
	"testing"

	"agw/internal/delegation"
	"agw/internal/platform"
)

// A kind registers its route and For finds it again for a matching call; calls of another kind or
// without a reference stay unseen (G1.7).
func TestWatchForRegisteredRoute(t *testing.T) {
	Register(Kind{Name: "seam_run"})
	t.Cleanup(func() { mu.Lock(); delete(kinds, "seam_run"); mu.Unlock() })
	RegisterRoute("POST", regexp.MustCompile(`^/seam/jobs/[^/]+/run$`), "seam_run", func(_ platform.Request, res platform.Result) (string, bool) {
		m := regexp.MustCompile(`^/tasks/([0-9]+)$`).FindStringSubmatch(res.Location)
		if m == nil {
			return "", false
		}
		return m[1], true
	})
	t.Cleanup(func() { mu.Lock(); routes = routes[:len(routes)-1]; mu.Unlock() })

	req := platform.Request{Method: "POST", Path: "/seam/jobs/7/run"}
	res := platform.Result{Status: "ok", HTTPStatus: 202, Location: "/tasks/12"}
	kind, ref, ok := For(delegation.Access{Action: delegation.Run, Resource: delegation.Training}, req, res)
	if !ok || kind != "seam_run" || ref != "12" {
		t.Fatalf("For = %q %q %v", kind, ref, ok)
	}
	// Another method or path is not this route.
	if _, _, ok := For(delegation.Access{}, platform.Request{Method: "GET", Path: "/seam/jobs/7/run"}, res); ok {
		t.Error("GET matched the route of POST")
	}
	if _, _, ok := For(delegation.Access{}, platform.Request{Method: "POST", Path: "/seam/jobs/7/stop"}, res); ok {
		t.Error("another path matched")
	}
	// The route matched, but the answer names no reference.
	if _, _, ok := For(delegation.Access{}, req, platform.Result{Status: "ok", HTTPStatus: 202}); ok {
		t.Error("without a reference For must report false")
	}
}

// The kind keeps what it was registered with, so that the loop (A-E2) can poll and stop it.
func TestRegisteredKindKeepsItsFunctions(t *testing.T) {
	called := ""
	Register(Kind{Name: "seam_poll", Poll: func(_ context.Context, _ Job, _ Reader) (Progress, error) {
		called = "poll"
		return Progress{State: "running"}, nil
	}, Stop: func(Job) (platform.Request, bool) {
		return platform.Request{Method: "POST", Path: "/seam/stop"}, true
	}})
	t.Cleanup(func() { mu.Lock(); delete(kinds, "seam_poll"); mu.Unlock() })

	mu.Lock()
	k, ok := kinds["seam_poll"]
	mu.Unlock()
	if !ok || k.Poll == nil || k.Stop == nil {
		t.Fatalf("kind not registered: %+v", k)
	}
	if p, err := k.Poll(t.Context(), Job{ID: "w1", Kind: "seam_poll"}, nil); err != nil || p.State != "running" || called != "poll" {
		t.Fatalf("poll: %+v %v %q", p, err, called)
	}
	if r, ok := k.Stop(Job{ID: "w1"}); !ok || r.Path != "/seam/stop" {
		t.Fatalf("stop: %+v %v", r, ok)
	}
}

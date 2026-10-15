// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"agw/internal/delegation"
	"agw/internal/platform"
	"agw/internal/store"
)

// countPlatform attaches a fake platform that counts every call except the token call; the test
// seams must show that a call never got there.
func countPlatform(t *testing.T, e *env) func() []string {
	t.Helper()
	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			fmt.Fprint(w, `{"access_token":"t","expires_in":3600}`)
			return
		}
		mu.Lock()
		got = append(got, r.Method+" "+r.URL.Path)
		mu.Unlock()
		fmt.Fprint(w, `[]`)
	}))
	t.Cleanup(srv.Close)
	c, err := platform.New(platform.Config{APIURL: srv.URL, TokenURL: srv.URL + "/token", User: "u", Password: "p"})
	if err != nil {
		t.Fatal(err)
	}
	e.m.opt.Platform = c
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), got...)
	}
}

// addGuard registers a guard for one test and removes it afterwards.
func addGuard(t *testing.T, g guard) {
	t.Helper()
	registerGuard(g)
	t.Cleanup(func() { guards = guards[:len(guards)-1] })
}

// A guard refuses a call the delegation allows, before the compute limit, the plan match and the
// approval: nobody is asked and the platform is never reached (G1.4).
func TestGuardRefusesBeforeApproval(t *testing.T) {
	e := setup(t)
	got := countPlatform(t, e)
	ctx := context.Background()
	var seen delegation.Access
	addGuard(t, func(_ context.Context, _ *Manager, _ string, req platform.Request, a delegation.Access) (string, error) {
		seen = a
		if strings.HasPrefix(req.Path, "/edge-x/") {
			return "edge-x is out of scope in this chat", nil
		}
		return "", nil
	})
	c, err := e.m.Create(ctx, NewChat{Delegation: json.RawMessage(`{"rules":[{"action":"delete","resource":"dataset","ids":["*"]}],"confirm":"none"}`)})
	if err != nil {
		t.Fatal(err)
	}
	slot := e.m.live[c.ID].slot.ID
	res, err := e.m.PlatformCall(ctx, c.ID, slot, "cli", platform.Request{Method: "DELETE", Path: "/edge-x/1"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "denied" || res.Message != "refused: edge-x is out of scope in this chat" {
		t.Fatalf("refusal: %+v", res)
	}
	if seen.Effect != delegation.EffectIrreversible || seen.ConfirmEach {
		t.Errorf("guard saw %+v", seen)
	}
	if aps, _ := e.st.ListApprovals(ctx, store.ApprovalPending, c.ID); len(aps) != 0 {
		t.Fatalf("a refused call asked the user: %+v", aps)
	}
	if n := got(); len(n) != 0 {
		t.Fatalf("a refused call reached the platform: %v", n)
	}
	// A path the guard lets through runs as before (here: the irreversible call asks).
	done := startCall(e, c.ID, slot, platform.Request{Method: "DELETE", Path: "/datasets/9", Body: json.RawMessage(`{"key":"-","csrftoken":"-","sessionid":"-"}`)})
	ap := pendingApproval(t, e, c.ID)
	if _, err := e.m.Decide(ctx, ap.ID, true); err != nil {
		t.Fatal(err)
	}
	if r := <-done; r.Status != "ok" {
		t.Fatalf("allowed call: %+v", r)
	}
}

// A guard that returns an error fails closed: the call does not go out and the caller sees the error
// (G1.4).
func TestGuardErrorFailsClosed(t *testing.T) {
	e := setup(t)
	got := countPlatform(t, e)
	ctx := context.Background()
	addGuard(t, func(_ context.Context, _ *Manager, _ string, req platform.Request, _ delegation.Access) (string, error) {
		if strings.HasPrefix(req.Path, "/edge-x/") {
			return "", errors.New("state of the edge device not readable")
		}
		return "", nil
	})
	c, err := e.m.Create(ctx, NewChat{Delegation: json.RawMessage(`{"rules":[{"action":"read","resource":"dataset","ids":["*"]}],"confirm":"none"}`)})
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.m.PlatformCall(ctx, c.ID, e.m.live[c.ID].slot.ID, "cli", platform.Request{Method: "GET", Path: "/edge-x/1"})
	if err == nil || !strings.Contains(err.Error(), "state of the edge device not readable") {
		t.Fatalf("error: %v %+v", err, res)
	}
	if n := got(); len(n) != 0 {
		t.Fatalf("a call with a failing guard reached the platform: %v", n)
	}
}

// The guards run in PlatformPrecheck as well, also without a delegation (G1.4): a refused upload
// never reads its files (Review 5, M7).
func TestPrecheckRunsGuards(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	addGuard(t, func(_ context.Context, _ *Manager, _ string, req platform.Request, _ delegation.Access) (string, error) {
		if strings.HasPrefix(req.Path, "/edge-x/") {
			return "edge-x is out of scope in this chat", nil
		}
		if strings.HasPrefix(req.Path, "/edge-fail/") {
			return "", errors.New("edge state unreadable")
		}
		return "", nil
	})
	c, err := e.m.Create(ctx, NewChat{})
	if err != nil {
		t.Fatal(err)
	}
	res, allowed := e.m.PlatformPrecheck(ctx, c.ID, platform.Request{Method: "POST", Path: "/edge-x/upload", Files: []platform.File{{Field: "files", Path: "/workspace/a.png"}}})
	if allowed || res.Status != "denied" || res.Message != "refused: edge-x is out of scope in this chat" {
		t.Fatalf("precheck: %+v allowed=%v", res, allowed)
	}
	res, allowed = e.m.PlatformPrecheck(ctx, c.ID, platform.Request{Method: "POST", Path: "/edge-fail/upload", Files: []platform.File{{Field: "files", Path: "/workspace/a.png"}}})
	if allowed || res.Status != "error" || !strings.Contains(res.Message, "edge state unreadable") {
		t.Fatalf("precheck with a failing guard: %+v allowed=%v", res, allowed)
	}
	// Without a delegation a call that no guard refuses passes the precheck.
	if res, allowed = e.m.PlatformPrecheck(ctx, c.ID, platform.Request{Method: "POST", Path: "/datasets"}); !allowed || res.Status != "" {
		t.Fatalf("other call: %+v allowed=%v", res, allowed)
	}
}

// seamDoer plays a virtual path (G1.5) and records what it got.
type seamDoer struct {
	mu    sync.Mutex
	calls []string
	res   platform.Result
}

func (d *seamDoer) Do(_ context.Context, chatID string, req platform.Request) (platform.Result, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, chatID+": "+req.Method+" "+req.Path)
	return d.res, nil
}

func (d *seamDoer) got() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.calls...)
}

// A registered prefix answers its own calls; the platform is not touched, and another doer does not
// see them (G1.5).
func TestVirtualDispatch(t *testing.T) {
	e := setup(t)
	got := countPlatform(t, e)
	ctx := context.Background()
	seam := &seamDoer{res: platform.Result{Status: "ok", HTTPStatus: 200, Body: `{"seam":true}`}}
	other := &seamDoer{res: platform.Result{Status: "ok", HTTPStatus: 200, Body: `{"other":true}`}}
	c, err := e.m.Create(ctx, NewChat{})
	if err != nil {
		t.Fatal(err)
	}
	e.m.RegisterVirtual("/_agw/seam", seam)
	e.m.RegisterVirtual("/_agw/other", other)
	e.m.RegisterVirtual("/_agw/seam/deep", other) // the longer prefix wins
	slot := e.m.live[c.ID].slot.ID

	res, err := e.m.PlatformCall(ctx, c.ID, slot, "cli", platform.Request{Method: "GET", Path: "/_agw/seam/status"})
	if err != nil || res.Body != `{"seam":true}` {
		t.Fatalf("dispatch: %+v %v", res, err)
	}
	if calls := seam.got(); len(calls) != 1 || !strings.Contains(calls[0], "GET /_agw/seam/status") {
		t.Fatalf("the doer saw %v", calls)
	}
	if calls := other.got(); len(calls) != 0 {
		t.Fatalf("another doer got the call: %v", calls)
	}
	if n := got(); len(n) != 0 {
		t.Fatalf("a virtual path reached the platform: %v", n)
	}
	// The exact prefix itself belongs to its doer, the longer prefix to the other one.
	if _, err := e.m.PlatformCall(ctx, c.ID, slot, "cli", platform.Request{Method: "GET", Path: "/_agw/seam"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.PlatformCall(ctx, c.ID, slot, "cli", platform.Request{Method: "GET", Path: "/_agw/seam/deep/1"}); err != nil {
		t.Fatal(err)
	}
	if calls := seam.got(); len(calls) != 2 || other.got()[0] != c.ID+": GET /_agw/seam/deep/1" {
		t.Fatalf("prefixes: seam=%v other=%v", calls, other.got())
	}
}

// A /_agw/ path nobody answers is answered by the gateway, not sent to the platform (G1.5, G1.13):
// the answer names the capability, says what to do instead and points at capabilities.
func TestUnregisteredVirtualPrefix(t *testing.T) {
	e := setup(t)
	got := countPlatform(t, e)
	ctx := context.Background()
	c, err := e.m.Create(ctx, NewChat{})
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.m.PlatformCall(ctx, c.ID, e.m.live[c.ID].slot.ID, "cli", platform.Request{Method: "POST", Path: "/_agw/notebook/runs", Body: json.RawMessage(`{"code":"print(1)"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != platform.StatusUnavailable {
		t.Fatalf("status: %+v", res)
	}
	for _, want := range []string{
		"notebook is not available in this setup:",
		"/_agw/notebook is not configured on this instance",
		"capabilities shows what this setup offers.",
	} {
		if !strings.Contains(res.Message, want) {
			t.Errorf("message without %q: %s", want, res.Message)
		}
	}
	if n := got(); len(n) != 0 {
		t.Fatalf("an unregistered /_agw/ path reached the platform: %v", n)
	}
	if aps, _ := e.st.ListApprovals(ctx, store.ApprovalPending, c.ID); len(aps) != 0 {
		t.Fatalf("an unavailable call asked the user: %+v", aps)
	}
}

// An unavailable call costs no compute call (G1.13): the answer comes before anything is reserved, and
// it is not an error the agent would repeat.
func TestUnavailableUsesNoCompute(t *testing.T) {
	e := setup(t)
	got := countPlatform(t, e)
	e.m.opt.ComputeMaxPerChat = 1
	ctx := context.Background()
	c, err := e.m.Create(ctx, NewChat{})
	if err != nil {
		t.Fatal(err)
	}
	slot := e.m.live[c.ID].slot.ID
	for _, path := range []string{"/_agw/notebook/runs", "/_agw/annotation/start", "/_agw/nothing"} {
		res, err := e.m.PlatformCall(ctx, c.ID, slot, "cli", platform.Request{Method: "POST", Path: path, Body: json.RawMessage(`{}`)})
		if err != nil || res.Status != platform.StatusUnavailable {
			t.Fatalf("%s: %+v %v", path, res, err)
		}
	}
	if n, _ := e.st.ComputeCalls(ctx, c.ID); n != 0 {
		t.Fatalf("compute calls used: %d", n)
	}
	// The chat can still spend its one compute call afterwards.
	if ok, err := e.st.ReserveCompute(ctx, c.ID, 1); err != nil || !ok {
		t.Fatalf("the limit is still free: %v %v", ok, err)
	}
	if aps, _ := e.st.ListApprovals(ctx, store.ApprovalPending, c.ID); len(aps) != 0 {
		t.Fatalf("somebody was asked: %+v", aps)
	}
	if n := got(); len(n) != 0 {
		t.Fatalf("the platform was called: %v", n)
	}
}

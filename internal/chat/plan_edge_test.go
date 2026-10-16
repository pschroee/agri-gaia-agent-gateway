// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"agw/internal/delegation"
	"agw/internal/platform"
	"agw/internal/store"
)

// withEdgeDeployPlatform answers the calls of a whole deployment: the device list, the image, the
// creation and the start. It returns the calls that arrived.
func withEdgeDeployPlatform(t *testing.T, e *env) func() []string {
	t.Helper()
	device := edgeDeviceStateJSON("sim-edge-01", "amd64", []string{"simulated"}, edgeHeartbeatNow(), true)
	var mu sync.Mutex
	var got []string
	// started: the deploy call arrived, so the device is on its way up. Before that the deployment is
	// "created" (the tool refuses to start an already deployed one), after it "deployed" — that is the state
	// the watcher of the edge kind waits for (edge D-E2).
	started := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			// The guard of D-E1 reads the user out of the chat's token and compares it with the namespace
			// of the image, so the fake has to hand out a token that carries the name.
			fmt.Fprintf(w, `{"access_token":%q,"expires_in":3600}`, edgeJWT("agent-test"))
			return
		}
		mu.Lock()
		got = append(got, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/edge-devices":
			fmt.Fprint(w, "["+device+"]")
		case r.Method == http.MethodGet && r.URL.Path == "/container-images/agent-test/mnist-triton:v1":
			fmt.Fprint(w, `[{"id":5,"repository":"agent-test/mnist-triton","tag":"v1","platform":"linux/amd64","exposed_ports":[]}]`)
		case r.Method == http.MethodGet && r.URL.Path == "/container-images":
			// The guard of D-E1 (edge.go) reads the list and looks the image up by its identifier.
			fmt.Fprint(w, `[{"id":5,"repository":"agent-test/mnist-triton","tag":"v1","platform":"linux/amd64","exposed_ports":[]}]`)
		case r.Method == http.MethodPost && r.URL.Path == "/container-deployments":
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":9,"name":"agw-mnist","status":"created","edge_device_id":1,"container_image_id":5}`)
		case r.Method == http.MethodGet && r.URL.Path == "/container-deployments/9":
			mu.Lock()
			up := started
			mu.Unlock()
			status := "created"
			if up {
				status = "deployed"
			}
			fmt.Fprintf(w, `{"id":9,"name":"agw-mnist","status":%q,"edge_device_id":1,"container_image_id":5}`, status)
		case r.Method == http.MethodGet && r.URL.Path == "/tasks/5":
			mu.Lock()
			up := started
			mu.Unlock()
			if up {
				fmt.Fprint(w, `{"id":5,"status":"completed","completion_percentage":1.0,"message":"started"}`)
				return
			}
			fmt.Fprint(w, `{"id":5,"status":"inprogress","completion_percentage":0.5}`)
		case r.Method == http.MethodPut && r.URL.Path == "/container-deployments/9/deploy":
			mu.Lock()
			started = true
			mu.Unlock()
			w.Header().Set("Location", "/tasks/5")
			w.WriteHeader(http.StatusAccepted)
		default:
			fmt.Fprint(w, `{}`)
		}
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

// edgeHeartbeatNow is a fresh naive UTC heartbeat, well inside the 5 minute window.
func edgeHeartbeatNow() string {
	return time.Now().UTC().Format(edgeHeartbeatLayout)
}

// A plan of two steps brings an image onto a device: the plan's approval covers the creation, the
// start asks on its own (confirm each) and counts against the compute limit (edge D2.1, D1.4).
func TestPlanCreateAndStartDeployment(t *testing.T) {
	e := setup(t)
	got := withEdgeDeployPlatform(t, e)
	ctx := store.WithSession(context.Background(), "main")
	c, err := e.m.Create(ctx, NewChat{})
	if err != nil {
		t.Fatal(err)
	}
	slot := e.m.live[c.ID].slot.ID

	const createArgs = `{"device_id":1,"repository":"agent-test/mnist-triton","tag":"v1","name":"mnist","host_port":18100,"image_id":5}`
	planBody := `{"title":"Deploy mnist","steps":[
	 {"tool":"create_deployment","args":{"device_id":1,"repository":"agent-test/mnist-triton","tag":"v1","name":"mnist","host_port":18100,"image_id":5}},
	 {"tool":"start_deployment","args":{"deployment_id":"{{step1.id}}"}}]}`
	if r, _ := proposeAndDecide(t, e, ctx, c.ID, slot, planBody, true); r.Status != "ok" {
		t.Fatalf("plan: %+v", r)
	}

	// Step 1: the creation is covered by the plan approval, so no new approval is asked.
	res, err := invokePlatformTool(e, ctx, c.ID, slot, "create_deployment", createArgs)
	if err != nil || res.Status != "ok" {
		t.Fatalf("create: %+v %v", res, err)
	}
	if kinds := approvalKinds(t, e, ctx, c.ID); len(kinds) != 1 || kinds[0] != "plan" {
		t.Fatalf("approvals after the create: %v, want the plan only", kinds)
	}
	if !reached(got(), "POST /container-deployments") {
		t.Fatalf("the creation did not reach the platform: %v", got())
	}

	// Step 2: the start is compute and asks for every single call, also inside a plan.
	done := make(chan platform.Result, 1)
	go func() {
		r, _ := invokePlatformTool(e, store.WithSession(context.Background(), "main"), c.ID, slot, "start_deployment", `{"deployment_id":9}`)
		done <- r
	}()
	ap := pendingApproval(t, e, c.ID)
	if ap.Kind != "platform_write" || ap.Effect != string(delegation.EffectCompute) {
		t.Fatalf("the start asked as %q/%q", ap.Kind, ap.Effect)
	}
	if _, err := e.m.Decide(ctx, ap.ID, true); err != nil {
		t.Fatal(err)
	}
	if r := <-done; r.Status != "ok" || !strings.Contains(r.Body, `"task":"/tasks/5"`) {
		t.Fatalf("start: %+v", r)
	}
	if !reached(got(), "PUT /container-deployments/9/deploy") {
		t.Fatalf("the start did not reach the platform: %v", got())
	}
	if n, _ := e.st.ComputeCalls(ctx, c.ID); n != 1 {
		t.Fatalf("compute calls: %d", n)
	}
	// The start is a platform job of its own (A-E2): the step waits for it instead of being done at once,
	// and the plan is through once the device reports the deployment up (edge D-E2).
	if p := onlyPlan(t, e, ctx, c.ID); p.Steps[1].State != "waiting" || p.Steps[1].WatchJob == "" {
		t.Fatalf("the start step does not wait for its job: %+v", p.Steps[1])
	}
	e.m.watchOnce(ctx, make(chan struct{}, watchParallel))
	if p := onlyPlan(t, e, ctx, c.ID); p.State != "completed" || p.Steps[0].State != "done" || p.Steps[1].State != "done" {
		t.Fatalf("plan %s, steps %+v", p.State, p.Steps)
	}
}

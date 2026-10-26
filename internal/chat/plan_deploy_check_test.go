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

	"agw/internal/platform"
	"agw/internal/store"
)

// The deployment of another user on device 1: agw-demo-mittwoch, deployed, host port 18100 (#231).
const occupiedDeployment6 = `{"id":6,"name":"agw-demo-mittwoch","status":"deployed","edge_device_id":1,"container_image_id":3,` +
	`"port_bindings":[{"host_port":"18100","container_port":"8000","protocol":"tcp"}],"docker_container_id":"abc"}`

// withOccupancyPlatform answers the reads of the deployment check: the device with its deployments and
// GET /edge-devices/1/containers with the given status and body. It returns the calls that arrived.
func withOccupancyPlatform(t *testing.T, e *env, deployments string, containersStatus int, containers string) func() []string {
	t.Helper()
	device := fmt.Sprintf(`{"id":1,"name":"sim-edge-01","arch":"amd64","tags":["simulated"],"registered":true,"last_heartbeat":%q,"container_deployments":[%s]}`,
		edgeHeartbeatNow(), deployments)
	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
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
		case r.Method == http.MethodGet && r.URL.Path == "/edge-devices/1/containers":
			w.WriteHeader(containersStatus)
			fmt.Fprint(w, containers)
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

// createDeploymentStep is a raw create-deployment step on device 1 for image 87.
func createDeploymentStep(name string, hostPorts ...int) string {
	var bindings []string
	for i, p := range hostPorts {
		bindings = append(bindings, fmt.Sprintf(`{"host_port":"%d","container_port":"%d","protocol":"tcp"}`, p, 8000+i))
	}
	return fmt.Sprintf(`{"method":"POST","path":"/container-deployments","body":{"edge_device_id":1,"container_image_id":87,"name":%q,"port_bindings":[%s]}}`,
		name, strings.Join(bindings, ","))
}

func occupancyPlan(steps ...string) string {
	return `{"title":"Deploy","steps":[` + strings.Join(steps, ",") + `]}`
}

// proposeRefused proposes a plan that must be refused in the answer, without an approval.
func proposeRefused(t *testing.T, e *env, body string) (platform.Result, string) {
	t.Helper()
	ctx := store.WithSession(context.Background(), "main")
	c, err := e.m.Create(ctx, NewChat{})
	if err != nil {
		t.Fatal(err)
	}
	slot := e.m.live[c.ID].slot.ID
	req, err := platform.Normalize(platform.Request{Method: "POST", Path: platform.PlanPath, Body: []byte(body)})
	if err != nil {
		t.Fatal(err)
	}
	res, _ := e.m.PlatformCall(ctx, c.ID, slot, "cli", req)
	if kinds := approvalKinds(t, e, ctx, c.ID); len(kinds) != 0 {
		t.Fatalf("a refused proposal created approvals: %v", kinds)
	}
	return res, c.ID
}

func TestProposePlanRefusesTakenPort(t *testing.T) {
	e := setup(t)
	withOccupancyPlatform(t, e, occupiedDeployment6, http.StatusOK, `[]`)
	res, _ := proposeRefused(t, e, occupancyPlan(
		createDeploymentStep("agw-keep", 18110),
		createDeploymentStep("agw-dd231", 18100)))
	want := `step 2: host port 18100 is used by deployment 6 (agw-demo-mittwoch) on device 1 "sim-edge-01"; free host ports: 18101, 18102, 18103`
	if res.Status != "error" || !strings.Contains(res.Message, want) {
		t.Fatalf("res=%+v\nwant %q", res, want)
	}
}

func TestProposePlanRefusesTakenContainerName(t *testing.T) {
	e := setup(t)
	withOccupancyPlatform(t, e, ``, http.StatusOK, `[{"name":"agw-x","state":"exited","host_ports":[]}]`)
	res, _ := proposeRefused(t, e, occupancyPlan(createDeploymentStep("agw-x", 18100)))
	if res.Status != "error" || !strings.Contains(res.Message, `step 1: a container named agw-x is on device 1 "sim-edge-01" (state exited)`) ||
		!strings.Contains(res.Message, `name "x-2"`) {
		t.Fatalf("res=%+v", res)
	}
}

func TestProposePlanTwoCreatesSamePort(t *testing.T) {
	e := setup(t)
	withOccupancyPlatform(t, e, ``, http.StatusOK, `[]`)
	res, _ := proposeRefused(t, e, occupancyPlan(
		createDeploymentStep("agw-a", 18100),
		createDeploymentStep("agw-b", 18100)))
	if res.Status != "error" || !strings.Contains(res.Message, "step 2: host port 18100 is also used by step 1 of this plan on the same device; free host ports: 18101, 18102, 18103") {
		t.Fatalf("res=%+v", res)
	}
	res, _ = proposeRefused(t, e, occupancyPlan(
		createDeploymentStep("agw-a", 18100),
		createDeploymentStep("agw-a", 18101)))
	if res.Status != "error" || !strings.Contains(res.Message, `step 2: name agw-a is also given to step 1 of this plan on the same device; use a free name, e.g. name "a-2"`) {
		t.Fatalf("res=%+v", res)
	}
}

// An earlier undeploy step with a literal id gives back the port of its entry; a delete step the name too.
func TestProposePlanUndeployReleasesPort(t *testing.T) {
	e := setup(t)
	withOccupancyPlatform(t, e, occupiedDeployment6, http.StatusOK, `[{"name":"agw-demo-mittwoch","state":"running","host_ports":[18100]}]`)
	undeploy := `{"method":"PUT","path":"/container-deployments/6/undeploy"}`
	del := `{"method":"DELETE","path":"/container-deployments/6"}`
	ctx := store.WithSession(context.Background(), "main")
	c, err := e.m.Create(ctx, NewChat{})
	if err != nil {
		t.Fatal(err)
	}
	slot := e.m.live[c.ID].slot.ID
	if r, _ := proposeAndDecide(t, e, ctx, c.ID, slot, occupancyPlan(undeploy, createDeploymentStep("agw-new", 18100)), false); strings.Contains(r.Message, "18100 is used") {
		t.Fatalf("the undeploy did not free the port: %+v", r)
	}
	// The name stays taken after an undeploy, and is free after a delete.
	res, _ := proposeRefused(t, e, occupancyPlan(undeploy, createDeploymentStep("agw-demo-mittwoch", 18100)))
	if res.Status != "error" || !strings.Contains(res.Message, "name agw-demo-mittwoch is taken on") {
		t.Fatalf("res=%+v", res)
	}
	if r, _ := proposeAndDecide(t, e, ctx, c.ID, slot, occupancyPlan(undeploy, del, createDeploymentStep("agw-demo-mittwoch", 18100)), false); strings.Contains(r.Message, "is taken on") {
		t.Fatalf("the delete did not free the name: %+v", r)
	}
}

// A device whose containers cannot be read does not stop the proposal (E7).
func TestProposePlanOccupancyUnreadableProceeds(t *testing.T) {
	e := setup(t)
	got := withOccupancyPlatform(t, e, ``, http.StatusBadGateway, `{"detail":"Portainer: down"}`)
	ctx := store.WithSession(context.Background(), "main")
	c, err := e.m.Create(ctx, NewChat{})
	if err != nil {
		t.Fatal(err)
	}
	slot := e.m.live[c.ID].slot.ID
	r, _ := proposeAndDecide(t, e, ctx, c.ID, slot, occupancyPlan(createDeploymentStep("agw-x", 18100)), true)
	if r.Status != "ok" {
		t.Fatalf("plan: %+v", r)
	}
	if !reached(got(), "GET /edge-devices/1/containers") {
		t.Fatalf("the containers were not read: %v", got())
	}
}

// The occupancy problems come in the same answer as the other problems of the proposal (#218).
func TestProposePlanJoinsOccupancyWithOtherProblems(t *testing.T) {
	e := setup(t)
	withOccupancyPlatform(t, e, occupiedDeployment6, http.StatusOK, `[]`)
	res, _ := proposeRefused(t, e, occupancyPlan(
		`{"tool":"nonsense_tool","args":{}}`,
		createDeploymentStep("agw-dd231", 18100)))
	for _, want := range []string{"plan refused, 2 problems", `step 1: unknown tool "nonsense_tool"`, "step 2: host port 18100 is used by deployment 6"} {
		if res.Status != "error" || !strings.Contains(res.Message, want) {
			t.Fatalf("missing %q in %+v", want, res)
		}
	}
}

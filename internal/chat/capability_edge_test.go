// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"agw/internal/platform"
	"agw/internal/store"
)

// edgeHeartbeatLayout is the naive UTC timestamp of the platform (F8).
const edgeHeartbeatLayout = "2006-01-02T15:04:05.000000"

// edgeDeviceStateJSON builds one device as GET /edge-devices returns it.
func edgeDeviceStateJSON(name, arch string, tags []string, heartbeat string, registered bool) string {
	tagJSON, _ := json.Marshal(tags)
	return fmt.Sprintf(`{"id":1,"name":%q,"arch":%q,"tags":%s,"registered":%t,"last_heartbeat":%q}`,
		name, arch, tagJSON, registered, heartbeat)
}

// withEdgeDevicePlatform attaches a fake platform that answers GET /edge-devices with body and the
// deployment read with a created deployment; it returns the calls that arrived.
func withEdgeDevicePlatform(t *testing.T, e *env, body string) func() []string {
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
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/edge-devices":
			fmt.Fprint(w, body)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/container-deployments/"):
			fmt.Fprint(w, `{"id":9,"name":"agw-mnist","status":"created","edge_device_id":1,"container_image_id":5}`)
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

// The entry edge follows the platform's device list: no device, an offline device and an online
// simulated device each answer on their own (edge D2.3, G1.11).
func TestEdgeCapability(t *testing.T) {
	e := setup(t)
	ctx := context.Background()

	none := withEdgeDevicePlatform(t, e, `[]`)
	cap := byName(e.m.Capabilities(ctx, "edge-none"))["edge"]
	if cap.Available || cap.Detail != "no edge device is registered" || cap.Without != platform.EdgeWithout {
		t.Fatalf("no device: %+v", cap)
	}
	if calls := none(); len(calls) != 1 || calls[0] != "GET /edge-devices" {
		t.Fatalf("reads: %v", calls)
	}

	offline := edgeDeviceStateJSON("sim-edge-01", "amd64", []string{"simulated"}, time.Now().UTC().Add(-9*time.Minute).Format(edgeHeartbeatLayout), true)
	withEdgeDevicePlatform(t, e, "["+offline+"]")
	cap = byName(e.m.Capabilities(ctx, "edge-offline"))["edge"]
	if cap.Available || !strings.Contains(cap.Detail, "no edge device online (last heartbeat") || cap.Without != platform.EdgeWithout {
		t.Fatalf("offline: %+v", cap)
	}

	online := edgeDeviceStateJSON("sim-edge-01", "amd64", []string{"simulated"}, time.Now().UTC().Add(-2*time.Minute).Format(edgeHeartbeatLayout), true)
	withEdgeDevicePlatform(t, e, "["+online+"]")
	cap = byName(e.m.Capabilities(ctx, "edge-online"))["edge"]
	if !cap.Available || cap.Detail != "1 device online: sim-edge-01 (simulated, amd64)" {
		t.Fatalf("online: %+v", cap)
	}
	// An unregistered device with a fresh heartbeat does not count (D2.3).
	unregistered := edgeDeviceStateJSON("sim-edge-01", "amd64", nil, time.Now().UTC().Add(-time.Minute).Format(edgeHeartbeatLayout), false)
	withEdgeDevicePlatform(t, e, "["+unregistered+"]")
	if cap := byName(e.m.Capabilities(ctx, "edge-unregistered"))["edge"]; cap.Available {
		t.Fatalf("unregistered device counts as online: %+v", cap)
	}
}

// invokePlatformTool runs a tool the way the socket does: with the invocation (plan match, issue #70)
// and every call through PlatformCall (delegation, guard, approval, compute limit).
func invokePlatformTool(e *env, ctx context.Context, chatID, slot, name, args string) (platform.Result, error) {
	tool, ok := platform.Lookup(name)
	if !ok {
		return platform.Result{}, fmt.Errorf("tool %s is not in the table", name)
	}
	raw := json.RawMessage(args)
	cctx := platform.WithInvocation(ctx, name, raw)
	res, _, err := tool.Run(cctx, raw, func(c context.Context, r platform.Request) (platform.Result, error) {
		n, nerr := platform.Normalize(r)
		if nerr != nil {
			return platform.Result{Status: "error", Message: nerr.Error()}, nil
		}
		return e.m.PlatformCall(c, chatID, slot, "cli", n)
	})
	return res, err
}

// Without an online device the two deploy tools answer unavailable: no approval, no POST, no PUT and
// no compute call (edge D2, G1.13).
func TestDeployToolsUnavailable(t *testing.T) {
	e := setup(t)
	got := withEdgeDevicePlatform(t, e, `[]`)
	ctx := context.Background()
	c, err := e.m.Create(ctx, NewChat{})
	if err != nil {
		t.Fatal(err)
	}
	slot := e.m.live[c.ID].slot.ID

	res, err := invokePlatformTool(e, ctx, c.ID, slot, "create_deployment",
		`{"device_id":1,"repository":"agent-test/mnist-triton","tag":"v1","name":"mnist","host_port":18100}`)
	if err != nil || res.Status != platform.StatusUnavailable || !strings.Contains(res.Message, "edge is not available in this setup") {
		t.Fatalf("create_deployment: %+v %v", res, err)
	}
	res, err = invokePlatformTool(e, ctx, c.ID, slot, "start_deployment", `{"deployment_id":9}`)
	if err != nil || res.Status != platform.StatusUnavailable {
		t.Fatalf("start_deployment: %+v %v", res, err)
	}
	if aps, _ := e.st.ListApprovals(ctx, store.ApprovalPending, c.ID); len(aps) != 0 {
		t.Fatalf("unavailable asked the user: %+v", aps)
	}
	if n, _ := e.st.ComputeCalls(ctx, c.ID); n != 0 {
		t.Fatalf("unavailable used a compute call: %d", n)
	}
	for _, call := range got() {
		if strings.HasPrefix(call, "POST /container-deployments") || strings.HasPrefix(call, "PUT /container-deployments") {
			t.Fatalf("a deploy call reached the platform: %v", call)
		}
	}
}

// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"strings"
	"testing"
	"time"

	"agw/internal/delegation"
	"agw/internal/platform"
	"agw/internal/store"
)

// onlineEdgePlatform gives the manager a platform whose device list carries one online simulated device
// (D-E0): without a device the edge tools answer unavailable and never reach the guard or their service.
func onlineEdgePlatform(t *testing.T, e *env) {
	t.Helper()
	online := edgeDeviceStateJSON("sim-edge-01", "amd64", []string{"simulated"},
		time.Now().UTC().Add(-2*time.Minute).Format(edgeHeartbeatLayout), true)
	withEdgeDevicePlatform(t, e, "["+online+"]")
}

// assessStub answers the check paths for the tests of this package: the check itself is tested in
// internal/assess, here only the way to it counts (delegation, guard, approval, compute limit).
type assessStub struct {
	calls int
}

func (s *assessStub) Do(_ context.Context, _ string, req platform.Request) (platform.Result, error) {
	s.calls++
	return platform.Result{Status: "ok", HTTPStatus: 200,
		Body: `{"check_id":"11111111-2222-3333-4444-555555555555","state":"finished"}`}, nil
}

// The deployment check is a reading call (R3.1, P7): the user approves nothing, the call counts against
// no compute call, and only a deployment of this chat is reached.
func TestClassifyDeploymentCheck(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	onlineEdgePlatform(t, e)
	stub := &assessStub{}
	e.m.RegisterVirtual(platform.AssessPrefix, stub)
	c, err := e.m.Create(ctx, NewChat{})
	if err != nil {
		t.Fatal(err)
	}
	slot := e.m.live[c.ID].slot.ID
	// The deployment was created in this chat (edge D1.7: the register carries it in every chat).
	if err := e.st.AddDelegationObject(ctx, c.ID, delegation.ContainerDeployment, "9"); err != nil {
		t.Fatal(err)
	}

	res, err := invokePlatformTool(e, ctx, c.ID, slot, "deployment_check", `{"deployment_id":9,"model_id":5}`)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "ok" || stub.calls != 1 {
		t.Fatalf("the check did not reach its service: %+v (calls %d)", res, stub.calls)
	}
	if aps, _ := e.st.ListApprovals(ctx, store.ApprovalPending, c.ID); len(aps) != 0 {
		t.Fatalf("the user was asked for a reading check: %+v", aps)
	}
	if n, _ := e.st.ComputeCalls(ctx, c.ID); n != 0 {
		t.Fatalf("the check used a compute call: %d", n)
	}
}

// A deployment this chat did not create is out of reach: the guard of the edge feature refuses the check
// before anything reaches its service (edge D-E1).
func TestEdgeCheckForeignDeploymentRefused(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	onlineEdgePlatform(t, e)
	stub := &assessStub{}
	e.m.RegisterVirtual(platform.AssessPrefix, stub)
	c, err := e.m.Create(ctx, NewChat{})
	if err != nil {
		t.Fatal(err)
	}
	slot := e.m.live[c.ID].slot.ID

	res, err := invokePlatformTool(e, ctx, c.ID, slot, "deployment_check", `{"deployment_id":9,"model_id":5}`)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "denied" || !strings.Contains(res.Message, "deployment 9") ||
		!strings.Contains(res.Message, "was not created in this chat") {
		t.Fatalf("a foreign deployment was not refused: %+v", res)
	}
	if stub.calls != 0 {
		t.Fatalf("the refused check reached its service %d times", stub.calls)
	}
}

// refusingStub answers like a handler that refuses the call itself before doing anything (#184).
type refusingStub struct{ reason string }

func (s refusingStub) Do(_ context.Context, _ string, _ platform.Request) (platform.Result, error) {
	return platform.Result{Status: "error", Message: platform.RefusedPrefix + "not now",
		Decision: platform.DecisionError, Reason: s.reason}, nil
}

// A handler that refuses a call with its own reason keeps decision and reason in the result the manager
// records; a result without a reason still gets the decision of the executed call.
func TestHandlerErrorReasonKept(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	onlineEdgePlatform(t, e)
	e.m.RegisterVirtual(platform.AssessPrefix, refusingStub{reason: "preprocess_differs_from_run"})
	c, err := e.m.Create(ctx, NewChat{})
	if err != nil {
		t.Fatal(err)
	}
	slot := e.m.live[c.ID].slot.ID
	if err := e.st.AddDelegationObject(ctx, c.ID, delegation.ContainerDeployment, "9"); err != nil {
		t.Fatal(err)
	}
	res, err := invokePlatformTool(e, ctx, c.ID, slot, "deployment_check", `{"deployment_id":9,"model_id":5}`)
	if err != nil {
		t.Fatal(err)
	}
	if res.Decision != platform.DecisionError || res.Reason != "preprocess_differs_from_run" {
		t.Fatalf("the handler's decision and reason were overwritten: %q / %q", res.Decision, res.Reason)
	}
}

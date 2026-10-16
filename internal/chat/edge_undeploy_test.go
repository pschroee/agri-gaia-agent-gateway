// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"agw/internal/delegation"
	"agw/internal/plan"
	"agw/internal/platform"
	"agw/internal/store"
)

// undeployCall is the removal of a deployment by the tool (edge D-E3).
func undeployCall(id string) platform.Request {
	return platform.Request{Method: http.MethodPut, Path: "/container-deployments/" + id + "/undeploy"}
}

// Taking a deployment back goes through the same guard as starting it (edge D1.8), and that holds
// without a delegation too: a deployment of this chat is removed after the user's approval (write), a
// foreign one is refused without asking anybody and without reaching the platform.
func TestUndeployForeignRefusedByGuard(t *testing.T) {
	e := setup(t)
	got := withEdgePlatform(t, e, `[]`, "test")
	ctx := context.Background()
	c, err := e.m.Create(ctx, NewChat{}) // no delegation at all
	if err != nil {
		t.Fatal(err)
	}
	slot := e.m.live[c.ID].slot.ID

	res, err := e.m.PlatformCall(ctx, c.ID, slot, "cli", undeployCall("8"))
	if err != nil {
		t.Fatal(err)
	}
	want := platform.RefusedPrefix + fmt.Sprintf(msgForeignDeployment, "8")
	if res.Status != "denied" || res.Message != want {
		t.Fatalf("foreign undeploy: %+v", res)
	}
	if aps, _ := e.st.ListApprovals(ctx, store.ApprovalPending, c.ID); len(aps) != 0 {
		t.Fatalf("a foreign deployment asked the user: %+v", aps)
	}
	if calls := got(); len(calls) != 0 {
		t.Fatalf("a foreign deployment reached the platform: %v", calls)
	}

	// A deployment of this chat: the removal is a write and asks once.
	if err := e.st.AddDelegationObject(ctx, c.ID, delegation.ContainerDeployment, "9"); err != nil {
		t.Fatal(err)
	}
	done := startCall(e, c.ID, slot, undeployCall("9"))
	ap := pendingApproval(t, e, c.ID)
	if ap.Kind != "platform_write" || ap.Effect != string(delegation.EffectWrite) {
		t.Fatalf("the removal asked as %q/%q", ap.Kind, ap.Effect)
	}
	if _, err := e.m.Decide(ctx, ap.ID, true); err != nil {
		t.Fatal(err)
	}
	if r := <-done; r.Status != "ok" {
		t.Fatalf("undeploy of an own deployment: %+v", r)
	}
	if calls := got(); !reached(calls, "PUT /container-deployments/9/undeploy") {
		t.Fatalf("the removal did not reach the platform: %v", calls)
	}
}

// Deleting a deployment is irreversible: the approved plan shows the step but does not cover it, so
// the call asks the user on its own and the plan runs on afterwards (edge D-E3, issue #70).
func TestDeleteDeploymentIrreversibleInPlan(t *testing.T) {
	e := setup(t)
	withPlanPlatform(t, e)
	ctx := store.WithSession(context.Background(), "main")
	c, err := e.m.Create(ctx, NewChat{})
	if err != nil {
		t.Fatal(err)
	}
	slot := e.m.live[c.ID].slot.ID
	// The guard of D-E1 (edge.go) lets the agent touch only the deployments of this chat, so the test
	// records the deployment as created here.
	if err := e.st.AddDelegationObject(ctx, c.ID, delegation.ContainerDeployment, "9"); err != nil {
		t.Fatal(err)
	}

	const proposal = `{"title":"Delete the deployment","steps":[{"tool":"delete_deployment","args":{"deployment_id":9}}]}`
	if r, _ := proposeAndDecide(t, e, ctx, c.ID, slot, proposal, true); r.Status != "ok" {
		t.Fatalf("plan: %+v", r)
	}
	p := onlyPlan(t, e, ctx, c.ID)
	if len(p.Steps) != 1 || p.Steps[0].Effect != plan.EffectIrreversible || p.Steps[0].Covered {
		t.Fatalf("the plan covers the delete: %+v", p.Steps)
	}

	done := make(chan platform.Result, 1)
	go func() {
		r, _ := invokePlatformTool(e, ctx, c.ID, slot, "delete_deployment", `{"deployment_id":9}`)
		done <- r
	}()
	ap := pendingApproval(t, e, c.ID)
	if ap.Kind != "platform_write" || ap.Effect != string(delegation.EffectIrreversible) {
		t.Fatalf("the delete asked as %q/%q", ap.Kind, ap.Effect)
	}
	if _, err := e.m.Decide(ctx, ap.ID, true); err != nil {
		t.Fatal(err)
	}
	if r := <-done; r.Status != "ok" {
		t.Fatalf("delete: %+v", r)
	}
	if p := onlyPlan(t, e, ctx, c.ID); p.State != "completed" || p.Steps[0].State != plan.StepDone {
		t.Fatalf("after the approved delete: plan %s, step %+v", p.State, p.Steps[0])
	}
	if kinds := approvalKinds(t, e, ctx, c.ID); len(kinds) != 2 || kinds[0] != "plan" || kinds[1] != "platform_write" {
		t.Fatalf("approvals: %v", kinds)
	}
}

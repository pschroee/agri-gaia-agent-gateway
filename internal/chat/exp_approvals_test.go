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
	"time"

	"agw/internal/netgrant"
	"agw/internal/platform"
	"agw/internal/sock"
	"agw/internal/store"
	"agw/internal/webproxy"
)

// BM2: in an experiment chat the profile decides every kind of approval, right after the card is written and
// without waiting. Like the other chat tests these need AGW_TEST_DATABASE_URL.

// experimentWith creates an experiment chat whose task has the given internet field ("", "off" or "request").
func experimentWith(t *testing.T, e *env, internet string) (ChatView, string) {
	t.Helper()
	c, err := e.m.Create(context.Background(), NewChat{Title: "bm2",
		Experiment: &store.Experiment{Protection: "none", Cell: "cli", Run: "r1", Seed: 7, Internet: internet},
		Delegation: json.RawMessage(experimentDelegation), Owner: "sub-1", OwnerName: experimentUser})
	if err != nil {
		t.Fatal(err)
	}
	return c, e.m.live[c.ID].slot.ID
}

// returnsWithin runs f and fails if it does not return in time: a decision of the profile never waits.
func returnsWithin[T any](t *testing.T, d time.Duration, f func() T) T {
	t.Helper()
	ch := make(chan T, 1)
	go func() { ch <- f() }()
	select {
	case v := <-ch:
		return v
	case <-time.After(d):
		t.Fatal("the call waited for a decision")
		panic("unreachable")
	}
}

func TestExperimentDecidesPlanApproval(t *testing.T) {
	e := setup(t)
	allowExperiment(e)
	withPlanPlatform(t, e)
	// Long enough that a wait would be noticed by returnsWithin, not by an expired approval.
	e.m.opt.ApprovalTimeout = time.Minute
	ctx := context.Background()
	c, slot := experimentWith(t, e, "")
	req, err := platform.Normalize(platform.Request{Method: "POST", Path: platform.PlanPath, Body: json.RawMessage(planTwo)})
	if err != nil {
		t.Fatal(err)
	}
	res := returnsWithin(t, 10*time.Second, func() platform.Result {
		r, _ := e.m.PlatformCall(ctx, c.ID, slot, "cli", req)
		return r
	})
	if res.Status != "ok" || !strings.Contains(res.Body, `"state":"approved"`) {
		t.Fatalf("plan result: %+v", res)
	}
	aps, err := e.st.ListApprovals(ctx, "", c.ID)
	if err != nil || len(aps) != 1 {
		t.Fatalf("approvals: %+v %v", aps, err)
	}
	if aps[0].Kind != "plan" || aps[0].State != store.ApprovalApproved || aps[0].DecidedBy != store.DecidedByExperiment || aps[0].DecidedAt == nil {
		t.Fatalf("plan approval: %+v", aps[0])
	}
	plans, err := e.m.Plans(ctx, c.ID)
	if err != nil || len(plans) != 1 || plans[0].State != "approved" || plans[0].ExpiresAt == nil {
		t.Fatalf("stored plan: %+v %v", plans, err)
	}
}

// The search shares the rule: the approval of a search is approved by the profile with decided_by experiment.
func TestExperimentDecidesSearchApproval(t *testing.T) {
	e := setup(t)
	allowExperiment(e)
	ctx := context.Background()
	c, _ := experimentWith(t, e, "")
	ap, err := e.st.CreateApproval(ctx, store.Approval{ChatID: c.ID, Kind: "search", Via: "cli", Name: "Training search: t",
		ContentType: "application/json", PendingKey: "-", Preview: "p", Summary: "p"})
	if err != nil {
		t.Fatal(err)
	}
	decided, ok, err := e.m.experimentDecides(ctx, c.ID, ap)
	if err != nil || !ok {
		t.Fatalf("decides: ok=%v err=%v", ok, err)
	}
	if decided.State != store.ApprovalApproved || decided.DecidedBy != store.DecidedByExperiment {
		t.Fatalf("search approval: %+v", decided)
	}
	if stored, _ := e.st.GetApproval(ctx, ap.ID); stored.State != store.ApprovalApproved || stored.DecidedBy != store.DecidedByExperiment {
		t.Fatalf("stored: %+v", stored)
	}
}

// Without internet "request" in the task the request is refused with the reason and no grant starts.
func TestExperimentInternetRefusedWithoutRequest(t *testing.T) {
	for _, field := range []string{"", "off"} {
		e := setup(t)
		allowExperiment(e)
		e.m.opt.ApprovalTimeout = time.Minute
		ctx := context.Background()
		c, slot := experimentWith(t, e, field)
		spec := netgrant.Spec{Kind: netgrant.Domains, Reason: "pypi", Domains: []string{"pypi.org"}}
		res := returnsWithin(t, 10*time.Second, func() sock.UploadResult {
			r, _ := e.m.RequestInternet(ctx, c.ID, slot, "cli", spec)
			return r
		})
		if res.Status != "rejected" || !strings.Contains(res.Message, ReasonExperimentNoInternet) {
			t.Fatalf("internet %q: %+v", field, res)
		}
		if gs, _ := e.st.ActiveGrants(ctx, c.ID); len(gs) != 0 {
			t.Fatalf("grants: %+v", gs)
		}
		aps, _ := e.st.ListApprovals(ctx, "", c.ID)
		if len(aps) != 1 || aps[0].Kind != "internet_access" || aps[0].State != store.ApprovalRejected || aps[0].DecidedBy != store.DecidedByExperiment {
			t.Fatalf("approvals: %+v", aps)
		}
	}
}

func TestExperimentInternetGrantedWithRequest(t *testing.T) {
	e := setup(t)
	allowExperiment(e)
	e.m.opt.ApprovalTimeout = time.Minute
	ctx := context.Background()
	c, slot := experimentWith(t, e, "request")
	if c.Internet {
		t.Fatal("a task with internet \"request\" starts without a grant")
	}
	if c.Variant != "exp-net:"+e.m.opt.Toolsets.Key() {
		t.Fatalf("variant %q", c.Variant)
	}
	spec := netgrant.Spec{Kind: netgrant.Domains, Reason: "pypi", Domains: []string{"pypi.org"}}
	status := returnsWithin(t, 10*time.Second, func() string {
		r, _ := e.m.RequestInternet(ctx, c.ID, slot, "cli", spec)
		return r.Status
	})
	if status != "approved" {
		t.Fatalf("status %q", status)
	}
	gs, _ := e.st.ActiveGrants(ctx, c.ID)
	if len(gs) != 1 || gs[0].Kind != "domains" {
		t.Fatalf("grants: %+v", gs)
	}
	aps, _ := e.st.ListApprovals(ctx, "", c.ID)
	if len(aps) != 1 || aps[0].State != store.ApprovalApproved || aps[0].DecidedBy != store.DecidedByExperiment {
		t.Fatalf("approvals: %+v", aps)
	}
	// The grant counts what goes through the proxy, as in a product chat.
	id := e.m.RecordWeb(webproxy.Request{ChatID: c.ID, Method: "CONNECT", Host: "pypi.org", Port: 443, Status: 200, StartedAt: time.Now(), GrantID: gs[0].ID, Origin: "exec"})
	if id == 0 {
		t.Fatal("web request not stored")
	}
	rows, _ := e.m.GrantWebRequests(ctx, c.ID, gs[0].ID)
	if len(rows) != 1 || rows[0].Host != "pypi.org" {
		t.Fatalf("web requests: %+v", rows)
	}
}

// The switch stays closed to an experiment chat, also in a task with internet "request".
func TestExperimentInternetSwitchStaysClosed(t *testing.T) {
	e := setup(t)
	allowExperiment(e)
	c, _ := experimentWith(t, e, "request")
	if _, err := e.m.SetInternetByUser(context.Background(), c.ID, true); err == nil {
		t.Fatal("the user's switch worked in an experiment chat")
	}
	if gs, _ := e.st.ActiveGrants(context.Background(), c.ID); len(gs) != 0 {
		t.Fatalf("grants: %+v", gs)
	}
}

// A product chat still waits for the user: internet and plan expire without a click, decided_by is not experiment.
func TestProductChatStillWaits(t *testing.T) {
	e := setup(t)
	withPlanPlatform(t, e)
	e.m.opt.ApprovalTimeout = 100 * time.Millisecond
	ctx := context.Background()
	off := false
	c, err := e.m.Create(ctx, NewChat{Internet: &off})
	if err != nil {
		t.Fatal(err)
	}
	slot := e.m.live[c.ID].slot.ID
	r, _ := e.m.RequestInternet(ctx, c.ID, slot, "cli", netgrant.Spec{Kind: netgrant.Domains, Reason: "pypi", Domains: []string{"pypi.org"}})
	if r.Status != "rejected" || r.Message != "no decision within the waiting time" {
		t.Fatalf("internet: %+v", r)
	}
	req, err := platform.Normalize(platform.Request{Method: "POST", Path: platform.PlanPath, Body: json.RawMessage(planTwo)})
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.m.PlatformCall(ctx, c.ID, slot, "cli", req)
	if err != nil || res.Status != "rejected" || !strings.Contains(res.Message, "no decision on the plan") {
		t.Fatalf("plan: %+v %v", res, err)
	}
	aps, _ := e.st.ListApprovals(ctx, "", c.ID)
	if len(aps) != 2 {
		t.Fatalf("approvals: %+v", aps)
	}
	for _, ap := range aps {
		if ap.State != store.ApprovalExpired || ap.DecidedBy == store.DecidedByExperiment {
			t.Errorf("approval %+v", ap)
		}
	}
}

// The case of X0.9 is unchanged by the shared function: a platform write is approved as DecisionApprovedAuto.
func TestPlatformCallExperimentUnchanged(t *testing.T) {
	e := setup(t)
	allowExperiment(e)
	e.m.opt.ApprovalTimeout = 50 * time.Millisecond
	got := withPlatform(t, e)
	ctx := context.Background()
	c := createExperiment(t, e, "none", NewChat{})
	slot := e.m.live[c.ID].slot.ID
	res, err := e.m.PlatformCall(ctx, c.ID, slot, "cli", platform.Request{Method: http.MethodDelete, Path: "/datasets/3"})
	if err != nil || res.Status != "ok" {
		t.Fatalf("call: %+v %v", res, err)
	}
	if res.Decision != platform.DecisionApprovedAuto || res.ApprovalDecision != store.DecidedByExperiment || res.ApprovalID == "" {
		t.Fatalf("decision: %+v", res)
	}
	if calls := got(); len(calls) != 1 || calls[0] != "DELETE /datasets/3" {
		t.Fatalf("platform calls: %v", calls)
	}
}

// A package install asks for the package sources only; a task without internet "request" may still use it.
func TestExperimentPackagesApprovedWithoutInternetRequest(t *testing.T) {
	e := setup(t)
	allowExperiment(e)
	e.m.opt.ApprovalTimeout = time.Minute
	ctx := context.Background()
	c, slot := experimentWith(t, e, "")
	spec := netgrant.Spec{Kind: netgrant.Packages, Reason: "install", Packages: []string{"requests"}}
	res := returnsWithin(t, 10*time.Second, func() sock.UploadResult {
		r, _ := e.m.RequestInternet(ctx, c.ID, slot, "cli", spec)
		return r
	})
	if res.Status != "approved" {
		t.Fatalf("packages result: %+v", res)
	}
	aps, _ := e.st.ListApprovals(ctx, "", c.ID)
	if len(aps) != 1 || aps[0].State != store.ApprovalApproved || aps[0].DecidedBy != store.DecidedByExperiment {
		t.Fatalf("approvals: %+v", aps)
	}
}

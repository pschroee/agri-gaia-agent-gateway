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

	"agw/internal/plan"
	"agw/internal/platform"
	"agw/internal/store"
)

// planTwo: a GET that creates model 42 (the fake platform answers so), then a rename of it.
const planTwo = `{"title":"Model and rename","steps":[
 {"method":"GET","path":"/train/containers/7/model"},
 {"method":"PATCH","path":"/models/{{step1.id}}","body":{"name":"pigs v1"}}]}`

// withPlanPlatform answers GET /train/containers/7/model with model 42 and every other call with {}.
func withPlanPlatform(t *testing.T, e *env) {
	t.Helper()
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			fmt.Fprint(w, `{"access_token":"t","expires_in":3600}`)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && r.URL.Path == "/train/containers/7/model" {
			fmt.Fprint(w, `{"id":42}`)
			return
		}
		fmt.Fprint(w, `{}`)
	}))
	t.Cleanup(srv.Close)
	c, err := platform.New(platform.Config{APIURL: srv.URL, TokenURL: srv.URL + "/token", User: "u", Password: "p"})
	if err != nil {
		t.Fatal(err)
	}
	e.m.opt.Platform = c
}

// proposeAndDecide sends a plan proposal, waits for its approval and decides it.
func proposeAndDecide(t *testing.T, e *env, ctx context.Context, chatID, slot, body string, approve bool) (platform.Result, store.Approval) {
	t.Helper()
	events, cancel := e.m.Subscribe(chatID)
	defer cancel()
	req, err := platform.Normalize(platform.Request{Method: "POST", Path: platform.PlanPath, Body: json.RawMessage(body)})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan platform.Result, 1)
	go func() {
		r, _ := e.m.PlatformCall(ctx, chatID, slot, "cli", req)
		done <- r
	}()
	ap := waitEvent(t, events, "approval", "").Data.(store.Approval)
	for ap.State != store.ApprovalPending {
		ap = waitEvent(t, events, "approval", "").Data.(store.Approval)
	}
	if ap.Kind != "plan" {
		t.Fatalf("approval kind %q", ap.Kind)
	}
	if _, err := e.m.Decide(ctx, ap.ID, approve); err != nil {
		t.Fatal(err)
	}
	return <-done, ap
}

// approvedPlan stores a plan of the session as approved for 30 minutes, without the approval flow.
func approvedPlan(t *testing.T, e *env, ctx context.Context, chatID, body string) store.Plan {
	t.Helper()
	p, err := plan.ParseProposal([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	steps, err := plan.Build(p, planClassify)
	if err != nil {
		t.Fatal(err)
	}
	pl, err := e.st.CreatePlan(ctx, store.Plan{ChatID: chatID, Title: p.Title, Session: store.SessionFrom(ctx), Via: "cli", Steps: steps})
	if err != nil {
		t.Fatal(err)
	}
	exp := planNow().Add(plan.ValidFor)
	if ok, err := e.st.DecidePlan(ctx, pl.ID, plan.StateApproved, &exp); err != nil || !ok {
		t.Fatalf("approve: ok=%v err=%v", ok, err)
	}
	return pl
}

func liveCall(t *testing.T, method, path, body string) platform.Request {
	t.Helper()
	r := platform.Request{Method: method, Path: path}
	if body != "" {
		r.Body = json.RawMessage(body)
	}
	r, err := platform.Normalize(r)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestProposePlanApproveAndReject(t *testing.T) {
	e := setup(t)
	withPlanPlatform(t, e)
	ctx := store.WithSession(context.Background(), "main")
	c, _ := e.m.Create(ctx, NewChat{})
	slot := e.m.live[c.ID].slot.ID

	r, ap := proposeAndDecide(t, e, ctx, c.ID, slot, planTwo, true)
	if !strings.Contains(ap.Preview, "1. WRITE · GET /train/containers/7/model") {
		t.Fatalf("preview lists no step: %s", ap.Preview)
	}
	if r.Status != "ok" || r.HTTPStatus != 200 || !strings.Contains(r.Body, `"plan_id"`) || !strings.Contains(r.Body, `"state":"approved"`) {
		t.Fatalf("approved: %+v", r)
	}
	plans, err := e.m.Plans(ctx, c.ID)
	if err != nil || len(plans) != 1 || plans[0].State != "approved" || plans[0].ExpiresAt == nil {
		t.Fatalf("stored plan: %+v %v", plans, err)
	}
	if left := time.Until(*plans[0].ExpiresAt); left < 29*time.Minute || left > 31*time.Minute {
		t.Fatalf("validity %v, want about 30 minutes", left)
	}

	r, _ = proposeAndDecide(t, e, ctx, c.ID, slot, planTwo, false)
	if r.Status != "rejected" || r.Message != "plan not approved by the user; nothing executed. Ask the user what to change and propose a new plan" {
		t.Fatalf("rejected: %+v", r)
	}
}

func TestProposePlanRefusesReadStep(t *testing.T) {
	e := setup(t)
	withPlanPlatform(t, e)
	ctx := store.WithSession(context.Background(), "main")
	c, _ := e.m.Create(ctx, NewChat{})
	slot := e.m.live[c.ID].slot.ID
	req := liveCall(t, "POST", platform.PlanPath, `{"title":"t","steps":[{"method":"GET","path":"/datasets"}]}`)
	r, err := e.m.PlatformCall(ctx, c.ID, slot, "cli", req)
	if err != nil || r.Status != "error" || !strings.Contains(r.Message, "reading calls") {
		t.Fatalf("read step: %+v %v", r, err)
	}
	if aps, _ := e.st.ListApprovals(ctx, "", c.ID); len(aps) != 0 {
		t.Fatalf("a refused plan must not ask: %+v", aps)
	}
}

func TestPlanMatchClaimsNextStepOnce(t *testing.T) {
	e := setup(t)
	ctx := store.WithSession(context.Background(), "main")
	c, _ := e.m.Create(ctx, NewChat{})
	approvedPlan(t, e, ctx, c.ID, planTwo)
	call := liveCall(t, "GET", "/train/containers/7/model", "")
	if st := e.m.planMatch(ctx, c.ID, call); st == nil || st.Idx != 1 {
		t.Fatalf("first match: %+v", st)
	}
	if st := e.m.planMatch(ctx, c.ID, call); st != nil {
		t.Fatalf("step claimed twice: %+v", st)
	}
}

func TestPlanMatchOrder(t *testing.T) {
	e := setup(t)
	ctx := store.WithSession(context.Background(), "main")
	c, _ := e.m.Create(ctx, NewChat{})
	pl := approvedPlan(t, e, ctx, c.ID, planTwo)
	if st := e.m.planMatch(ctx, c.ID, liveCall(t, "PATCH", "/models/42", `{"name":"pigs v1"}`)); st != nil {
		t.Fatalf("second step matched before the first: %+v", st)
	}
	if p, _ := e.st.GetPlan(ctx, pl.ID); p.Steps[0].State != plan.StepOpen {
		t.Fatalf("first step touched: %+v", p.Steps[0])
	}
}

func TestPlanMatchPlaceholderOwnObjectsOnly(t *testing.T) {
	e := setup(t)
	ctx := store.WithSession(context.Background(), "main")
	c, _ := e.m.Create(ctx, NewChat{})
	pl := approvedPlan(t, e, ctx, c.ID, planTwo)
	if ok, err := e.st.ClaimPlanStep(ctx, pl.ID, 1); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	if ok, err := e.st.FinishPlanStep(ctx, pl.ID, 1, plan.StepRunning, plan.StepDone, "ok", "model", "42"); err != nil || !ok {
		t.Fatalf("finish: %v %v", ok, err)
	}
	rename := liveCall(t, "PATCH", "/models/42", `{"name":"pigs v1"}`)
	if st := e.m.planMatch(ctx, c.ID, rename); st != nil {
		t.Fatalf("id not in the register matched: %+v", st)
	}
	if err := e.st.AddDelegationObject(ctx, c.ID, "model", "42"); err != nil {
		t.Fatal(err)
	}
	if st := e.m.planMatch(ctx, c.ID, rename); st == nil || st.Idx != 2 {
		t.Fatalf("own object does not match: %+v", st)
	}
}

func TestPlanMatchExpired(t *testing.T) {
	e := setup(t)
	ctx := store.WithSession(context.Background(), "main")
	c, _ := e.m.Create(ctx, NewChat{})
	pl := approvedPlan(t, e, ctx, c.ID, planTwo)
	old := planNow
	planNow = func() time.Time { return time.Now().Add(31 * time.Minute) }
	t.Cleanup(func() { planNow = old })
	if st := e.m.planMatch(ctx, c.ID, liveCall(t, "GET", "/train/containers/7/model", "")); st != nil {
		t.Fatalf("expired plan matched: %+v", st)
	}
	if p, _ := e.st.GetPlan(ctx, pl.ID); p.State != "expired" {
		t.Fatalf("state after expiry: %s", p.State)
	}
}

func TestPlanSuperseded(t *testing.T) {
	e := setup(t)
	withPlanPlatform(t, e)
	ctx := store.WithSession(context.Background(), "main")
	c, _ := e.m.Create(ctx, NewChat{})
	slot := e.m.live[c.ID].slot.ID
	if r, _ := proposeAndDecide(t, e, ctx, c.ID, slot, planTwo, true); r.Status != "ok" {
		t.Fatalf("first plan: %+v", r)
	}
	if r, _ := proposeAndDecide(t, e, ctx, c.ID, slot, planTwo, true); r.Status != "ok" {
		t.Fatalf("second plan: %+v", r)
	}
	plans, _ := e.m.Plans(ctx, c.ID)
	if len(plans) != 2 || plans[0].State != "superseded" || plans[1].State != "approved" {
		t.Fatalf("states after the second approval: %+v", plans)
	}
}

func TestPlanRecover(t *testing.T) {
	e := setup(t)
	ctx := store.WithSession(context.Background(), "main")
	c, _ := e.m.Create(ctx, NewChat{})
	proposed, err := e.st.CreatePlan(ctx, store.Plan{ChatID: c.ID, Title: "p", Session: "main", Via: "cli", Steps: []store.PlanStep{
		{Idx: 1, Method: "DELETE", Path: "/models/5", Effect: plan.EffectIrreversible}}})
	if err != nil {
		t.Fatal(err)
	}
	running := approvedPlan(t, e, ctx, c.ID, planTwo)
	if ok, err := e.st.ClaimPlanStep(ctx, running.ID, 1); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	if err := e.m.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if p, _ := e.st.GetPlan(ctx, proposed.ID); p.State != "expired" {
		t.Fatalf("proposal after restart: %s", p.State)
	}
	p, _ := e.st.GetPlan(ctx, running.ID)
	if p.State != "failed" || p.Steps[0].State != plan.StepFailed {
		t.Fatalf("running plan after restart: %+v", p)
	}
}

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
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"agw/internal/plan"
	"agw/internal/platform"
	"agw/internal/store"
)

// planTwo: a GET that creates model 42 (the fake platform answers so), then a rename of it.
const planTwo = `{"title":"Model and rename","steps":[
 {"method":"GET","path":"/train/containers/7/model"},
 {"method":"PATCH","path":"/models/{{step1.id}}","body":{"name":"pigs v1","format":"onnx","input_name":"i","input_datatype":"float32","input_shape":[1,3],"input_semantics":"NCHW","output_name":"o","output_datatype":"float32","output_shape":[1,2],"output_labels":["0","1"]}}]}`

// withPlanPlatform answers GET /train/containers/7/model with model 42 and every other call with {}.
func withPlanPlatform(t *testing.T, e *env) { withPlanPlatformPatch(t, e, http.StatusOK) }

// withPlanPlatformPatch answers like withPlanPlatform, but PATCH with the given status (a failing step).
func withPlanPlatformPatch(t *testing.T, e *env, patch int) {
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
		if r.Method == http.MethodPatch && patch != http.StatusOK {
			w.WriteHeader(patch)
			fmt.Fprint(w, `{"detail":"failed"}`)
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

// proposeAndDecide sends a plan proposal over the CLI, waits for its approval and decides it.
func proposeAndDecide(t *testing.T, e *env, ctx context.Context, chatID, slot, body string, approve bool) (platform.Result, store.Approval) {
	t.Helper()
	return proposeAndDecideVia(t, e, ctx, chatID, slot, "cli", body, approve)
}

// proposeAndDecideVia is proposeAndDecide for a binding (cli, mcp or api).
func proposeAndDecideVia(t *testing.T, e *env, ctx context.Context, chatID, slot, via, body string, approve bool) (platform.Result, store.Approval) {
	t.Helper()
	events, cancel := e.m.Subscribe(chatID)
	defer cancel()
	req, err := platform.Normalize(platform.Request{Method: "POST", Path: platform.PlanPath, Body: json.RawMessage(body)})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan platform.Result, 1)
	go func() {
		r, _ := e.m.PlatformCall(ctx, chatID, slot, via, req)
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
	p, err := plan.ParseProposal([]byte(body), plan.Limits{MaxDuration: 8 * time.Hour, MaxLLMCost: 5})
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
	if !strings.Contains(ap.Preview, "1. WRITE · Create a new model from training container 7") {
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
	if st := e.m.planMatch(ctx, c.ID, liveCall(t, "PATCH", "/models/42", `{"name":"pigs v1","format":"onnx","input_name":"i","input_datatype":"float32","input_shape":[1,3],"input_semantics":"NCHW","output_name":"o","output_datatype":"float32","output_shape":[1,2],"output_labels":["0","1"]}`)); st != nil {
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
	rename := liveCall(t, "PATCH", "/models/42", `{"name":"pigs v1","format":"onnx","input_name":"i","input_datatype":"float32","input_shape":[1,3],"input_semantics":"NCHW","output_name":"o","output_datatype":"float32","output_shape":[1,2],"output_labels":["0","1"]}`)
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

// planCover: a GET that creates model 42, then a PATCH whose body has two keys (the call may order them differently).
const planCover = `{"title":"Create and tag","steps":[
 {"method":"GET","path":"/train/containers/7/model"},
 {"method":"PATCH","path":"/models/{{step1.id}}","body":{"name":"pigs v1","format":"onnx","input_name":"i","input_datatype":"float32","input_shape":[1,3],"input_semantics":"NCHW","output_name":"a","output_datatype":"float32","output_shape":[1,2],"output_labels":["0","1"]}}]}`

// planDelete: a GET that creates model 42, then an irreversible DELETE of it.
const planDelete = `{"title":"Create and delete","steps":[
 {"method":"GET","path":"/train/containers/7/model"},
 {"method":"DELETE","path":"/models/{{step1.id}}"}]}`

// planThree: a GET that creates model 42, then two renames; the second one follows a failed step.
const planThree = `{"title":"Three steps","steps":[
 {"method":"GET","path":"/train/containers/7/model"},
 {"method":"PATCH","path":"/models/{{step1.id}}","body":{"name":"pigs v1","format":"onnx","input_name":"i","input_datatype":"float32","input_shape":[1,3],"input_semantics":"NCHW","output_name":"o","output_datatype":"float32","output_shape":[1,2],"output_labels":["0","1"]}},
 {"method":"PATCH","path":"/models/{{step1.id}}","body":{"name":"pigs v2","format":"onnx","input_name":"i","input_datatype":"float32","input_shape":[1,3],"input_semantics":"NCHW","output_name":"o","output_datatype":"float32","output_shape":[1,2],"output_labels":["0","1"]}}]}`

// onlyPlan returns the single plan of a chat.
func onlyPlan(t *testing.T, e *env, ctx context.Context, chatID string) store.Plan {
	t.Helper()
	plans, err := e.m.Plans(ctx, chatID)
	if err != nil || len(plans) != 1 {
		t.Fatalf("plans: %+v %v", plans, err)
	}
	return plans[0]
}

// approvalKinds lists the kinds of all approvals of a chat.
func approvalKinds(t *testing.T, e *env, ctx context.Context, chatID string) []string {
	t.Helper()
	aps, err := e.st.ListApprovals(ctx, "", chatID)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, a := range aps {
		kinds = append(kinds, a.Kind)
	}
	return kinds
}

// Covered steps run without a new approval: one approval for the plan, then both calls go straight through.
// The second call has its keys in another order and still matches its step.
func TestPlanCoversSteps(t *testing.T) {
	e := setup(t)
	withPlanPlatform(t, e)
	ctx := store.WithSession(context.Background(), "main")
	c, _ := e.m.Create(ctx, NewChat{})
	slot := e.m.live[c.ID].slot.ID
	if r, _ := proposeAndDecide(t, e, ctx, c.ID, slot, planCover, true); r.Status != "ok" {
		t.Fatalf("plan: %+v", r)
	}
	if r, err := e.m.PlatformCall(ctx, c.ID, slot, "cli", liveCall(t, "GET", "/train/containers/7/model", "")); err != nil || r.Status != "ok" {
		t.Fatalf("step 1: %+v %v", r, err)
	}
	if r, err := e.m.PlatformCall(ctx, c.ID, slot, "cli", liveCall(t, "PATCH", "/models/42", `{"output_name":"a","name":"pigs v1","format":"onnx","input_name":"i","input_datatype":"float32","input_shape":[1,3],"input_semantics":"NCHW","output_datatype":"float32","output_shape":[1,2],"output_labels":["0","1"]}`)); err != nil || r.Status != "ok" {
		t.Fatalf("step 2: %+v %v", r, err)
	}
	if kinds := approvalKinds(t, e, ctx, c.ID); len(kinds) != 1 || kinds[0] != "plan" {
		t.Fatalf("approvals %v, want the plan only", kinds)
	}
	if p := onlyPlan(t, e, ctx, c.ID); p.State != "completed" {
		t.Fatalf("plan state %s, want completed", p.State)
	}
}

// A call that differs from the next step is not covered: it asks the user, and the step stays open.
func TestPlanChangedCallNeedsApproval(t *testing.T) {
	e := setup(t)
	withPlanPlatform(t, e)
	ctx := store.WithSession(context.Background(), "main")
	c, _ := e.m.Create(ctx, NewChat{})
	slot := e.m.live[c.ID].slot.ID
	if r, _ := proposeAndDecide(t, e, ctx, c.ID, slot, planTwo, true); r.Status != "ok" {
		t.Fatalf("plan: %+v", r)
	}
	if r, err := e.m.PlatformCall(ctx, c.ID, slot, "cli", liveCall(t, "GET", "/train/containers/7/model", "")); err != nil || r.Status != "ok" {
		t.Fatalf("step 1: %+v %v", r, err)
	}
	changed := liveCall(t, "PATCH", "/models/42", `{"name":"other","format":"onnx","input_name":"i","input_datatype":"float32","input_shape":[1,3],"input_semantics":"NCHW","output_name":"o","output_datatype":"float32","output_shape":[1,2],"output_labels":["0","1"]}`)
	done := make(chan platform.Result, 1)
	go func() {
		r, _ := e.m.PlatformCall(ctx, c.ID, slot, "cli", changed)
		done <- r
	}()
	ap := pendingApproval(t, e, c.ID)
	if ap.Kind != "platform_write" {
		t.Fatalf("changed call asked as %q", ap.Kind)
	}
	if _, err := e.m.Decide(ctx, ap.ID, false); err != nil {
		t.Fatal(err)
	}
	if r := <-done; r.Status != "rejected" {
		t.Fatalf("changed call: %+v", r)
	}
	if p := onlyPlan(t, e, ctx, c.ID); p.Steps[1].State != plan.StepOpen {
		t.Fatalf("step 2 after a changed call: %+v", p.Steps[1])
	}
}

// An irreversible step is shown in the plan but keeps its own approval, even inside an approved plan.
func TestPlanIrreversibleStepOwnApproval(t *testing.T) {
	e := setup(t)
	withPlanPlatform(t, e)
	ctx := store.WithSession(context.Background(), "main")
	c, _ := e.m.Create(ctx, NewChat{})
	slot := e.m.live[c.ID].slot.ID
	if r, _ := proposeAndDecide(t, e, ctx, c.ID, slot, planDelete, true); r.Status != "ok" {
		t.Fatalf("plan: %+v", r)
	}
	if r, err := e.m.PlatformCall(ctx, c.ID, slot, "cli", liveCall(t, "GET", "/train/containers/7/model", "")); err != nil || r.Status != "ok" {
		t.Fatalf("step 1: %+v %v", r, err)
	}
	del := liveCall(t, "DELETE", "/models/42", "")
	done := make(chan platform.Result, 1)
	go func() {
		r, _ := e.m.PlatformCall(ctx, c.ID, slot, "cli", del)
		done <- r
	}()
	ap := pendingApproval(t, e, c.ID)
	if ap.Kind != "platform_write" {
		t.Fatalf("irreversible step asked as %q", ap.Kind)
	}
	if _, err := e.m.Decide(ctx, ap.ID, true); err != nil {
		t.Fatal(err)
	}
	if r := <-done; r.Status != "ok" {
		t.Fatalf("delete: %+v", r)
	}
	if p := onlyPlan(t, e, ctx, c.ID); p.Steps[1].State != plan.StepDone || p.State != "completed" {
		t.Fatalf("after the approved delete: plan %s, step %+v", p.State, p.Steps[1])
	}
}

// The same plan works over every binding: each proposal asks once, and the steps ask nothing more.
func TestPlanSameOverCLIMCPREST(t *testing.T) {
	e := setup(t)
	withPlanPlatform(t, e)
	for _, via := range []string{"cli", "mcp", "api"} {
		ctx := store.WithSession(context.Background(), "main")
		c, _ := e.m.Create(ctx, NewChat{})
		slot := e.m.live[c.ID].slot.ID
		if r, _ := proposeAndDecideVia(t, e, ctx, c.ID, slot, via, planCover, true); r.Status != "ok" {
			t.Fatalf("%s: plan %+v", via, r)
		}
		if r, err := e.m.PlatformCall(ctx, c.ID, slot, via, liveCall(t, "GET", "/train/containers/7/model", "")); err != nil || r.Status != "ok" {
			t.Fatalf("%s: step 1 %+v %v", via, r, err)
		}
		if r, err := e.m.PlatformCall(ctx, c.ID, slot, via, liveCall(t, "PATCH", "/models/42", `{"output_name":"a","name":"pigs v1","format":"onnx","input_name":"i","input_datatype":"float32","input_shape":[1,3],"input_semantics":"NCHW","output_datatype":"float32","output_shape":[1,2],"output_labels":["0","1"]}`)); err != nil || r.Status != "ok" {
			t.Fatalf("%s: step 2 %+v %v", via, r, err)
		}
		if kinds := approvalKinds(t, e, ctx, c.ID); len(kinds) != 1 || kinds[0] != "plan" {
			t.Fatalf("%s: approvals %v, want the plan only", via, kinds)
		}
	}
}

// A failed step ends the plan; the step after it is no longer covered and asks the user again.
func TestPlanFailedStepEndsPlan(t *testing.T) {
	e := setup(t)
	withPlanPlatformPatch(t, e, http.StatusInternalServerError)
	ctx := store.WithSession(context.Background(), "main")
	c, _ := e.m.Create(ctx, NewChat{})
	slot := e.m.live[c.ID].slot.ID
	if r, _ := proposeAndDecide(t, e, ctx, c.ID, slot, planThree, true); r.Status != "ok" {
		t.Fatalf("plan: %+v", r)
	}
	if r, err := e.m.PlatformCall(ctx, c.ID, slot, "cli", liveCall(t, "GET", "/train/containers/7/model", "")); err != nil || r.Status != "ok" {
		t.Fatalf("step 1: %+v %v", r, err)
	}
	if r, err := e.m.PlatformCall(ctx, c.ID, slot, "cli", liveCall(t, "PATCH", "/models/42", `{"name":"pigs v1","format":"onnx","input_name":"i","input_datatype":"float32","input_shape":[1,3],"input_semantics":"NCHW","output_name":"o","output_datatype":"float32","output_shape":[1,2],"output_labels":["0","1"]}`)); err != nil || r.Status == "ok" {
		t.Fatalf("step 2 should fail: %+v %v", r, err)
	}
	p := onlyPlan(t, e, ctx, c.ID)
	if p.State != "failed" || p.Steps[1].State != plan.StepFailed || p.Steps[2].State != plan.StepOpen {
		t.Fatalf("after the failed step: plan %s, steps %+v", p.State, p.Steps)
	}
	next := liveCall(t, "PATCH", "/models/42", `{"name":"pigs v2","format":"onnx","input_name":"i","input_datatype":"float32","input_shape":[1,3],"input_semantics":"NCHW","output_name":"o","output_datatype":"float32","output_shape":[1,2],"output_labels":["0","1"]}`)
	done := make(chan platform.Result, 1)
	go func() {
		r, _ := e.m.PlatformCall(ctx, c.ID, slot, "cli", next)
		done <- r
	}()
	ap := pendingApproval(t, e, c.ID)
	if ap.Kind != "platform_write" {
		t.Fatalf("step after a failed plan asked as %q", ap.Kind)
	}
	if _, err := e.m.Decide(ctx, ap.ID, false); err != nil {
		t.Fatal(err)
	}
	if r := <-done; r.Status != "rejected" {
		t.Fatalf("next call: %+v", r)
	}
}

// failComputeReserve makes every update of chats.compute_calls fail in the chat's test schema, so that
// ReserveCompute errors after the plan step was claimed.
func failComputeReserve(t *testing.T, chatID string) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, os.Getenv("AGW_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	rows, err := conn.Query(ctx, `SELECT table_schema FROM information_schema.tables WHERE table_name='chats' AND table_schema LIKE 'chattest\_%'`)
	if err != nil {
		t.Fatal(err)
	}
	var schemas []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		schemas = append(schemas, s)
	}
	rows.Close()
	for _, s := range schemas {
		var n int
		if err := conn.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM %s.chats WHERE id=$1`, pgx.Identifier{s}.Sanitize()), chatID).Scan(&n); err != nil || n == 0 {
			continue
		}
		q := pgx.Identifier{s}.Sanitize()
		if _, err := conn.Exec(ctx, fmt.Sprintf(`CREATE FUNCTION %s.fail_reserve() RETURNS trigger AS $$ BEGIN RAISE EXCEPTION 'reserve refused'; END $$ LANGUAGE plpgsql`, q)); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Exec(ctx, fmt.Sprintf(`CREATE TRIGGER fail_reserve BEFORE UPDATE OF compute_calls ON %s.chats FOR EACH ROW EXECUTE FUNCTION %s.fail_reserve()`, q, q)); err != nil {
			t.Fatal(err)
		}
		return
	}
	t.Fatal("test schema of the chat not found")
}

// A claimed plan step that returns before the call to the platform (here: ReserveCompute fails) must not
// stay running: the step fails and so does the plan.
func TestPlanClaimedStepFailsOnEarlyReturn(t *testing.T) {
	e := setup(t)
	withPlanPlatform(t, e)
	ctx := store.WithSession(context.Background(), "main")
	c, _ := e.m.Create(ctx, NewChat{})
	slot := e.m.live[c.ID].slot.ID
	const body = `{"title":"Run","steps":[{"method":"POST","path":"/train/containers/7/run"}]}`
	if r, _ := proposeAndDecide(t, e, ctx, c.ID, slot, body, true); r.Status != "ok" {
		t.Fatalf("plan: %+v", r)
	}
	failComputeReserve(t, c.ID)
	if _, err := e.m.PlatformCall(ctx, c.ID, slot, "cli", liveCall(t, "POST", "/train/containers/7/run", "")); err == nil {
		t.Fatal("expected the reservation error")
	}
	p := onlyPlan(t, e, ctx, c.ID)
	if p.State != "failed" || p.Steps[0].State != plan.StepFailed {
		t.Fatalf("after the early return: plan %s, step %+v", p.State, p.Steps[0])
	}
}

// planBuild: one tool step that builds an image; the tool puts the owner into the repository itself.
const planBuild = `{"title":"Build","steps":[
 {"tool":"build_inference_image","args":{"template_id":1,"model_id":5,"name":"img","tag":"v1","architecture":"amd64"}}]}`

// buildCall is the final call build_inference_image sends for planBuild (the CLI passes numbers as text).
func buildCall(t *testing.T, tag string) platform.Request {
	return liveCall(t, "POST", "/container-images/build",
		`{"architecture":"amd64","container_template_id":1,"model_id":5,"repository":"agent-test/img","tag":"`+tag+`"}`)
}

func invCtx(ctx context.Context, tag string) context.Context {
	return platform.WithInvocation(ctx, "build_inference_image",
		json.RawMessage(`{"template_id":"1","model_id":"5","name":"img","tag":"`+tag+`","architecture":"amd64"}`))
}

// A tool step is matched on the tool and its arguments: the call the tool builds (owner in the repository)
// runs without a new approval.
func TestPlanToolStepCovered(t *testing.T) {
	e := setup(t)
	withPlanPlatform(t, e)
	ctx := store.WithSession(context.Background(), "main")
	c, _ := e.m.Create(ctx, NewChat{})
	slot := e.m.live[c.ID].slot.ID
	if r, _ := proposeAndDecide(t, e, ctx, c.ID, slot, planBuild, true); r.Status != "ok" {
		t.Fatalf("plan: %+v", r)
	}
	if r, err := e.m.PlatformCall(invCtx(ctx, "v1"), c.ID, slot, "cli", buildCall(t, "v1")); err != nil || r.Status != "ok" {
		t.Fatalf("covered tool step: %+v %v", r, err)
	}
	if kinds := approvalKinds(t, e, ctx, c.ID); len(kinds) != 1 || kinds[0] != "plan" {
		t.Fatalf("approvals %v, want the plan only", kinds)
	}
	if p := onlyPlan(t, e, ctx, c.ID); p.State != "completed" || p.Steps[0].State != "done" {
		t.Fatalf("plan %s, step %+v", p.State, p.Steps[0])
	}
}

// A changed argument of the tool asks again.
func TestPlanToolStepChangedArgumentAsks(t *testing.T) {
	e := setup(t)
	withPlanPlatform(t, e)
	ctx := store.WithSession(context.Background(), "main")
	c, _ := e.m.Create(ctx, NewChat{})
	slot := e.m.live[c.ID].slot.ID
	if r, _ := proposeAndDecide(t, e, ctx, c.ID, slot, planBuild, true); r.Status != "ok" {
		t.Fatalf("plan: %+v", r)
	}
	done := make(chan platform.Result, 1)
	go func() {
		r, _ := e.m.PlatformCall(invCtx(ctx, "v2"), c.ID, slot, "cli", buildCall(t, "v2"))
		done <- r
	}()
	ap := pendingApproval(t, e, c.ID)
	if ap.Kind != "platform_write" {
		t.Fatalf("changed argument asked as %q", ap.Kind)
	}
	if _, err := e.m.Decide(ctx, ap.ID, false); err != nil {
		t.Fatal(err)
	}
	if r := <-done; r.Status != "rejected" {
		t.Fatalf("changed call: %+v", r)
	}
	if p := onlyPlan(t, e, ctx, c.ID); p.Steps[0].State != "open" {
		t.Fatalf("step touched by a changed call: %+v", p.Steps[0])
	}
}

// A raw request with the same target is not covered by a tool step.
func TestPlanToolStepNotCoveredByRawRequest(t *testing.T) {
	e := setup(t)
	withPlanPlatform(t, e)
	ctx := store.WithSession(context.Background(), "main")
	c, _ := e.m.Create(ctx, NewChat{})
	slot := e.m.live[c.ID].slot.ID
	if r, _ := proposeAndDecide(t, e, ctx, c.ID, slot, planBuild, true); r.Status != "ok" {
		t.Fatalf("plan: %+v", r)
	}
	done := make(chan platform.Result, 1)
	go func() {
		r, _ := e.m.PlatformCall(ctx, c.ID, slot, "api", buildCall(t, "v1")) // no invocation: a raw request
		done <- r
	}()
	ap := pendingApproval(t, e, c.ID)
	if ap.Kind != "platform_write" {
		t.Fatalf("raw request asked as %q", ap.Kind)
	}
	if _, err := e.m.Decide(ctx, ap.ID, false); err != nil {
		t.Fatal(err)
	}
	if r := <-done; r.Status != "rejected" {
		t.Fatalf("raw call: %+v", r)
	}
	if p := onlyPlan(t, e, ctx, c.ID); p.Steps[0].State != "open" {
		t.Fatalf("step claimed by a raw request: %+v", p.Steps[0])
	}
}

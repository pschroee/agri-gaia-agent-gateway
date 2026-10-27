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

// countedPlatform attaches a fake platform that counts every call and answers an empty JSON object, so
// that the gateway's summaries still work while a test checks which calls went out (A-E1).
func countedPlatform(t *testing.T, e *env) func() []string {
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
		fmt.Fprint(w, `{}`)
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

// modelBody is a complete body for PATCH /models/{id} (issue #65): the platform clears every field it
// does not find in the body, so the gateway requires all of platform.ModelPatchFields. A step of a plan
// is validated like the call it will execute, so it has to carry the full body as well.
const modelBody = `{"name":"x","format":"onnx","input_name":"i","input_datatype":"float32","input_shape":[1,3],"input_semantics":"NCHW","output_name":"o","output_datatype":"float32","output_shape":[1,2],"output_labels":["0","1"]}`

// autoPlanBody is an autonomous proposal with a create step and a covered rename step.
const autoPlanBody = `{"title":"Autonom","mode":"autonomous","budget":{"max_duration_minutes":60,"max_llm_cost_usd":1,"max_auto_turns":5},
 "steps":[{"method":"POST","path":"/datasets"},{"method":"PATCH","path":"/models/5","body":` + modelBody + `}]}`

// autonomousChat creates a chat and approves the given autonomous body over the CLI.
func autonomousChat(t *testing.T, e *env, ctx context.Context, body string) (ChatView, string, store.Plan) {
	t.Helper()
	c, err := e.m.Create(ctx, NewChat{})
	if err != nil {
		t.Fatal(err)
	}
	slot := e.m.live[c.ID].slot.ID
	r, ap := proposeAndDecide(t, e, ctx, c.ID, slot, body, true)
	if r.Status != "ok" || r.HTTPStatus != 200 {
		t.Fatalf("autonomous plan not approved: %+v (%s)", r, ap.Preview)
	}
	plans, err := e.m.Plans(ctx, c.ID)
	if err != nil || len(plans) == 0 {
		t.Fatalf("plans: %+v %v", plans, err)
	}
	return c, slot, plans[len(plans)-1]
}

// A-E1: an autonomous proposal is stored with its mode, budget and grant, and the preview and the answer
// carry the budget block.
func TestProposeAutonomousStoresGrantAndBudget(t *testing.T) {
	e := setup(t)
	withPlanPlatform(t, e)
	ctx := store.WithSession(context.Background(), "main")
	c, _, p := autonomousChat(t, e, ctx, autoPlanBody)
	if p.Mode != plan.ModeAutonomous || p.Budget.MaxDurationS != 3600 || p.Budget.MaxLLMCost != 1 || p.Budget.MaxAutoTurns != 5 {
		t.Fatalf("stored plan: %+v", p)
	}
	if len(p.Grant) == 0 {
		t.Fatalf("grant missing: %+v", p)
	}
	if p.ExpiresAt == nil {
		t.Fatalf("expiry missing")
	}
	if left := time.Until(*p.ExpiresAt); left < 59*time.Minute || left > 61*time.Minute {
		t.Fatalf("validity %v, want about 60 minutes", left)
	}
	// The active autonomous plan is found independent of the session.
	ap, err := e.st.ActiveAutonomousPlan(ctx, c.ID)
	if err != nil || ap.ID != p.ID {
		t.Fatalf("active autonomous plan: %+v %v", ap, err)
	}
}

// A-E1: the preview carries the budget block and the rights line.
func TestProposeAutonomousPreviewBlock(t *testing.T) {
	e := setup(t)
	withPlanPlatform(t, e)
	ctx := store.WithSession(context.Background(), "main")
	c, err := e.m.Create(ctx, NewChat{})
	if err != nil {
		t.Fatal(err)
	}
	slot := e.m.live[c.ID].slot.ID
	_, ap := proposeAndDecide(t, e, ctx, c.ID, slot, autoPlanBody, true)
	for _, want := range []string{
		"Autonomous: runs without asking you for up to 1h 0m",
		"Budget: at most 0 compute calls",
		"Rights while it runs: create dataset; update model 5; read all;",
	} {
		if !strings.Contains(ap.Preview, want) {
			t.Errorf("preview misses %q in:\n%s", want, ap.Preview)
		}
	}
}

// A-E1 (D1.7): a plan whose Σ compute × max_attempts exceeds the chat's remaining quota is refused before
// any approval is raised.
func TestProposeAutonomousRefusesOverComputeLimit(t *testing.T) {
	e := setup(t)
	withPlanPlatform(t, e)
	e.m.opt.ComputeMaxPerChat = 1
	ctx := store.WithSession(context.Background(), "main")
	c, err := e.m.Create(ctx, NewChat{})
	if err != nil {
		t.Fatal(err)
	}
	slot := e.m.live[c.ID].slot.ID
	body := `{"title":"Autonom","mode":"autonomous","steps":[
      {"method":"POST","path":"/train/config","body":{"dataset_id":1}},
      {"method":"POST","path":"/train/containers/7/run"}]}`
	r, err := e.m.PlatformCall(ctx, c.ID, slot, "cli", liveCall(t, "POST", platform.PlanPath, body))
	if err != nil || r.Status != "error" || !strings.Contains(r.Message, "compute calls") {
		t.Fatalf("over the compute limit: %+v %v", r, err)
	}
	if aps, _ := e.st.ListApprovals(ctx, store.ApprovalPending, c.ID); len(aps) != 0 {
		t.Fatalf("a refused plan must not ask: %+v", aps)
	}
}

// A-E1: a step outside the chat's delegated rights refuses the whole proposal.
func TestProposeAutonomousRefusesStepOutsideChatDelegation(t *testing.T) {
	e := setup(t)
	withPlanPlatform(t, e)
	ctx := store.WithSession(context.Background(), "main")
	c, err := e.m.Create(ctx, NewChat{Delegation: json.RawMessage(`{"rules":[{"action":"create","resource":"dataset"}],"confirm":"none"}`)})
	if err != nil {
		t.Fatal(err)
	}
	slot := e.m.live[c.ID].slot.ID
	body := `{"title":"Autonom","mode":"autonomous","steps":[{"method":"PATCH","path":"/models/5","body":` + modelBody + `}]}`
	r, err := e.m.PlatformCall(ctx, c.ID, slot, "cli", liveCall(t, "POST", platform.PlanPath, body))
	if err != nil || r.Status != "error" || !strings.Contains(r.Message, "outside the chat's delegated rights") {
		t.Fatalf("step outside the chat delegation: %+v %v", r, err)
	}
}

// A-E1 (D1.12): proposing a plan with "replaces" supersedes the named running plan once the new one is
// approved; an unknown or foreign "replaces" is refused.
func TestReplacesSupersedesOldPlan(t *testing.T) {
	e := setup(t)
	withPlanPlatform(t, e)
	ctx := store.WithSession(context.Background(), "main")
	c, slot, old := autonomousChat(t, e, ctx, autoPlanBody)
	body := strings.Replace(autoPlanBody, `"title":"Autonom"`, `"title":"Autonom 2","replaces":"`+old.ID+`"`, 1)
	r, _ := proposeAndDecide(t, e, ctx, c.ID, slot, body, true)
	if r.Status != "ok" {
		t.Fatalf("replacement not approved: %+v", r)
	}
	got, err := e.st.GetPlan(ctx, old.ID)
	if err != nil || got.State != plan.StateSuperseded {
		t.Fatalf("old plan must be superseded: %+v %v", got, err)
	}
	// An unknown replaces is refused.
	bad := strings.Replace(autoPlanBody, `"title":"Autonom"`, `"title":"Autonom 3","replaces":"00000000-0000-0000-0000-000000000001"`, 1)
	res, err := e.m.PlatformCall(ctx, c.ID, slot, "cli", liveCall(t, "POST", platform.PlanPath, bad))
	if err != nil || res.Status != "error" || !strings.Contains(res.Message, "replaces: no running plan") {
		t.Fatalf("unknown replaces: %+v %v", res, err)
	}
}

// A-E1 (D1.4): a call outside the running plan's grant is refused without asking, from any session and any
// binding.
func TestAutonomousGrantRefusesOutsideWithoutApproval(t *testing.T) {
	for _, via := range []string{"cli", "mcp", "api"} {
		t.Run(via, func(t *testing.T) {
			e := setup(t)
			withPlanPlatform(t, e)
			ctx := store.WithSession(context.Background(), "main")
			c, slot, _ := autonomousChat(t, e, ctx, autoPlanBody)
			got := countedPlatform(t, e)
			req := liveCall(t, "PATCH", "/datasets/16", `{"name":"x"}`)
			// A foreign session: the grant holds for every session (D1.11).
			r, err := e.m.PlatformCall(store.WithSession(context.Background(), "abc#1"), c.ID, slot, via, req)
			if err != nil || r.Status != "denied" {
				t.Fatalf("outside the grant: %+v %v", r, err)
			}
			if !strings.Contains(r.Message, "outside the running plan") || !strings.Contains(r.Message, "replaces") {
				t.Fatalf("the refusal must name the plan and replaces: %s", r.Message)
			}
			if aps, _ := e.st.ListApprovals(ctx, store.ApprovalPending, c.ID); len(aps) != 0 {
				t.Fatalf("a refused call must not ask: %+v", aps)
			}
			if n := got(); len(n) != 0 {
				t.Fatalf("a refused call reached the platform: %v", n)
			}
		})
	}
}

// A-E1 (D1.10): a paused plan refuses writes inside the grant, but reads stay open.
func TestPausedPlanRefusesWritesAllowsReads(t *testing.T) {
	e := setup(t)
	withPlanPlatform(t, e)
	ctx := store.WithSession(context.Background(), "main")
	c, slot, p := autonomousChat(t, e, ctx, autoPlanBody)
	got := countedPlatform(t, e)
	if _, err := e.m.PausePlan(ctx, c.ID, p.ID, "user"); err != nil {
		t.Fatal(err)
	}
	// A write that is the plan's own step is inside the grant, but the plan is paused.
	r, err := e.m.PlatformCall(ctx, c.ID, slot, "cli", liveCall(t, "PATCH", "/models/5", modelBody))
	if err != nil || r.Status != "denied" || !strings.Contains(r.Message, "paused") {
		t.Fatalf("paused write: %+v %v", r, err)
	}
	// A read stays open and runs.
	if r, err := e.m.PlatformCall(ctx, c.ID, slot, "cli", liveCall(t, "GET", "/datasets", "")); err != nil || r.Status != "ok" {
		t.Fatalf("paused read: %+v %v", r, err)
	}
	if aps, _ := e.st.ListApprovals(ctx, store.ApprovalPending, c.ID); len(aps) != 0 {
		t.Fatalf("nobody must be asked: %+v", aps)
	}
	if calls := got(); len(calls) != 1 || !strings.HasPrefix(calls[0], "GET") {
		t.Fatalf("only the read should have gone out: %v", calls)
	}
}

// A-E1 (D1.7): a failed step with attempts left goes back to open and the plan stays approved; the next
// failure fails the plan.
func TestRetryUpToMaxAttempts(t *testing.T) {
	e := setup(t)
	withPlanPlatformPatch(t, e, http.StatusInternalServerError)
	ctx := store.WithSession(context.Background(), "main")
	body := `{"title":"Autonom","mode":"autonomous","steps":[{"method":"PATCH","path":"/models/5","body":` + modelBody + `,"max_attempts":2}]}`
	c, slot, p := autonomousChat(t, e, ctx, body)
	call := func() platform.Result {
		r, err := e.m.PlatformCall(ctx, c.ID, slot, "cli", liveCall(t, "PATCH", "/models/5", modelBody))
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	call()
	got, err := e.st.GetPlan(ctx, p.ID)
	if err != nil || got.State != plan.StateApproved || got.Steps[0].State != plan.StepOpen || got.Steps[0].Attempts != 1 {
		t.Fatalf("after the first failure: %+v %v", got, err)
	}
	call()
	got, err = e.st.GetPlan(ctx, p.ID)
	if err != nil || got.State != plan.StateFailed || got.Steps[0].State != plan.StepFailed || got.Steps[0].Attempts != 2 {
		t.Fatalf("after the second failure: %+v %v", got, err)
	}
}

// A-E1 (fail closed): an unreadable grant grants nothing.
func TestUnreadableGrantFailsClosed(t *testing.T) {
	e := setup(t)
	got := countPlatform(t, e)
	ctx := store.WithSession(context.Background(), "main")
	c, err := e.m.Create(ctx, NewChat{})
	if err != nil {
		t.Fatal(err)
	}
	slot := e.m.live[c.ID].slot.ID
	p, err := e.st.CreatePlan(ctx, store.Plan{ChatID: c.ID, Title: "broken", Session: "main", Via: "cli", Mode: plan.ModeAutonomous,
		Budget: store.PlanBudget{MaxDurationS: 3600, MaxLLMCost: 1, MaxAutoTurns: 5},
		Grant:  json.RawMessage("{not json}"),
		Steps:  []store.PlanStep{{Idx: 1, Method: "PATCH", Path: "/models/5", Effect: plan.EffectWrite, Covered: true, MaxAttempts: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	exp := time.Now().Add(time.Hour)
	if ok, err := e.st.DecidePlan(ctx, p.ID, plan.StateApproved, &exp); err != nil || !ok {
		t.Fatalf("decide: %v %v", ok, err)
	}
	r, err := e.m.PlatformCall(ctx, c.ID, slot, "cli", liveCall(t, "GET", "/datasets", ""))
	if err != nil || r.Status != "denied" || !strings.Contains(r.Message, "unreadable") {
		t.Fatalf("unreadable grant: %+v %v", r, err)
	}
	if n := got(); len(n) != 0 {
		t.Fatalf("the platform was reached: %v", n)
	}
}

// A-E1: an interactive plan keeps the former behaviour; a call outside it still asks.
func TestInteractivePlansUnchanged(t *testing.T) {
	e := setup(t)
	withPlanPlatform(t, e)
	ctx := store.WithSession(context.Background(), "main")
	c, err := e.m.Create(ctx, NewChat{})
	if err != nil {
		t.Fatal(err)
	}
	slot := e.m.live[c.ID].slot.ID
	body := `{"title":"Interaktiv","steps":[{"method":"PATCH","path":"/models/5","body":` + modelBody + `}]}`
	proposeAndDecide(t, e, ctx, c.ID, slot, body, true)
	p, err := e.st.ActiveAutonomousPlan(ctx, c.ID)
	if err == nil && p.ID != "" {
		t.Fatalf("an interactive plan must not be an autonomous one: %+v", p)
	}
	// A write outside the plan is asked, not refused.
	done := startCall(e, c.ID, slot, liveCall(t, "PATCH", "/datasets/16", `{"name":"x"}`))
	ap := pendingApproval(t, e, c.ID)
	if _, err := e.m.Decide(ctx, ap.ID, true); err != nil {
		t.Fatal(err)
	}
	if r := <-done; r.Status != "ok" {
		t.Fatalf("the approved call should run: %+v", r)
	}
}

// A-E1 (D1.15): a step the plan does not cover (confirm each or irreversible) is inside the grant and asks
// for its own approval instead of being refused. No route carries confirm-each yet (G1 built the seam), so
// the test stores such a step directly, which is what the flow produces for it.
func TestAutonomousPlanConfirmEachStepAsks(t *testing.T) {
	e := setup(t)
	withPlanPlatform(t, e)
	ctx := store.WithSession(context.Background(), "main")
	c, err := e.m.Create(ctx, NewChat{})
	if err != nil {
		t.Fatal(err)
	}
	slot := e.m.live[c.ID].slot.ID
	p, err := e.st.CreatePlan(ctx, store.Plan{ChatID: c.ID, Title: "each", Session: "main", Via: "cli", Mode: plan.ModeAutonomous,
		Budget: store.PlanBudget{MaxDurationS: 3600, MaxLLMCost: 1, MaxAutoTurns: 5},
		Grant:  json.RawMessage(`{"rules":[{"action":"update","resource":"model","ids":["5"]}],"enforce":true}`),
		Steps:  []store.PlanStep{{Idx: 1, Method: "PATCH", Path: "/models/5", Effect: plan.EffectWrite, Covered: false, MaxAttempts: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	exp := time.Now().Add(time.Hour)
	e.st.DecidePlan(ctx, p.ID, plan.StateApproved, &exp)
	done := startCall(e, c.ID, slot, liveCall(t, "PATCH", "/models/5", modelBody))
	ap := pendingApproval(t, e, c.ID)
	if ap.Kind != "platform_write" {
		t.Fatalf("a single approval is expected: %+v", ap)
	}
	if _, err := e.m.Decide(ctx, ap.ID, true); err != nil {
		t.Fatal(err)
	}
	if r := <-done; r.Status != "ok" {
		t.Fatalf("the confirmed call should run: %+v", r)
	}
}

// A-E1 (D1.9): the model-cost budget is enforced at the proxy; over budget the plan is paused.
func TestCostOverrunPausesPlan(t *testing.T) {
	e := setup(t)
	withPlanPlatform(t, e)
	ctx := store.WithSession(context.Background(), "main")
	c, _, p := autonomousChat(t, e, ctx, `{"title":"Autonom","mode":"autonomous","budget":{"max_llm_cost_usd":0.5},"steps":[{"method":"POST","path":"/datasets"}]}`)
	if err := e.m.planBudget(c.ID); err != nil {
		t.Fatalf("under budget: %v", err)
	}
	if _, err := e.st.AddLLMCall(ctx, store.LLMCall{ChatID: c.ID, SlotID: "s", SourceIP: "1.2.3.4", Model: "p/m", StartedAt: time.Now(), Cost: 0.7}); err != nil {
		t.Fatal(err)
	}
	err := e.m.planBudget(c.ID)
	if err == nil || !strings.Contains(err.Error(), "used up") {
		t.Fatalf("over budget: %v", err)
	}
	got, err := e.st.GetPlan(ctx, p.ID)
	if err != nil || got.State != plan.StatePaused || got.PauseReason != "budget_cost" {
		t.Fatalf("plan after the overrun: %+v %v", got, err)
	}
}

// A-E1 (D1.10): the stop button pauses a running autonomous plan.
func TestAbortPausesAutonomousPlan(t *testing.T) {
	e := setup(t)
	withPlanPlatform(t, e)
	ctx := store.WithSession(context.Background(), "main")
	c, _, p := autonomousChat(t, e, ctx, autoPlanBody)
	if _, err := e.m.Abort(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	got, err := e.st.GetPlan(ctx, p.ID)
	if err != nil || got.State != plan.StatePaused || got.PauseReason != "abort" {
		t.Fatalf("plan after the abort: %+v %v", got, err)
	}
}

// A-E1: cancel ends the plan, skips its open steps and queues a note; resume queues a note too.
func TestCancelAndResumePlan(t *testing.T) {
	e := setup(t)
	withPlanPlatform(t, e)
	ctx := store.WithSession(context.Background(), "main")
	c, _, p := autonomousChat(t, e, ctx, autoPlanBody)
	if _, err := e.m.CancelPlan(ctx, c.ID, p.ID); err != nil {
		t.Fatal(err)
	}
	got, err := e.st.GetPlan(ctx, p.ID)
	if err != nil || got.State != plan.StateCancelled {
		t.Fatalf("cancelled plan: %+v %v", got, err)
	}
	for _, st := range got.Steps {
		if st.State != plan.StepSkipped {
			t.Fatalf("open steps must be skipped: %+v", got.Steps)
		}
	}
	q, err := e.st.ListQueue(ctx, c.ID)
	if err != nil || len(q) != 1 || q[0].Note != store.NotePlan {
		t.Fatalf("cancel note: %+v %v", q, err)
	}
}

// A-E1: resume queues a note and moves the plan back to approved.
func TestResumeQueuesNote(t *testing.T) {
	e := setup(t)
	withPlanPlatform(t, e)
	ctx := store.WithSession(context.Background(), "main")
	c, _, p := autonomousChat(t, e, ctx, autoPlanBody)
	if _, err := e.m.PausePlan(ctx, c.ID, p.ID, "user"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.ResumePlan(ctx, c.ID, p.ID); err != nil {
		t.Fatal(err)
	}
	got, err := e.st.GetPlan(ctx, p.ID)
	if err != nil || got.State != plan.StateApproved || got.PauseReason != "" {
		t.Fatalf("resumed plan: %+v %v", got, err)
	}
	q, err := e.st.ListQueue(ctx, c.ID)
	if err != nil || len(q) != 1 || q[0].Note != store.NotePlan || !strings.Contains(q[0].Text, "resumed plan") {
		t.Fatalf("resume note: %+v %v", q, err)
	}
}

// A-E1: the controls refuse a plan that is not in the required state (the API answers 409).
func TestPlanControlsWrongState(t *testing.T) {
	e := setup(t)
	withPlanPlatform(t, e)
	ctx := store.WithSession(context.Background(), "main")
	c, _, p := autonomousChat(t, e, ctx, autoPlanBody)
	if _, err := e.m.ResumePlan(ctx, c.ID, p.ID); err == nil {
		t.Fatal("resuming an approved plan must fail")
	} else if _, ok := err.(*PlanStateError); !ok {
		t.Fatalf("error type: %T %v", err, err)
	}
	if _, err := e.m.CancelPlan(ctx, c.ID, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.PausePlan(ctx, c.ID, p.ID, "user"); err == nil {
		t.Fatal("pausing a cancelled plan must fail")
	}
	// A foreign plan id is not found.
	if _, err := e.m.PausePlan(ctx, c.ID, "00000000-0000-0000-0000-000000000001", "user"); err != store.ErrNotFound {
		t.Fatalf("unknown plan: %v", err)
	}
}

// #251 (B3): a waiting call that is aborted ends its approval as cancelled; the card is gone and a later click
// decides nothing (no rejection nobody receives).
func TestAbortedCallCancelsApproval(t *testing.T) {
	e := setup(t)
	withPlanPlatform(t, e)
	ctx := store.WithSession(context.Background(), "main")
	c, err := e.m.Create(ctx, NewChat{})
	if err != nil {
		t.Fatal(err)
	}
	slot := e.m.live[c.ID].slot.ID
	cctx, cancel := context.WithCancel(ctx)
	done := make(chan platform.Result, 1)
	go func() {
		r, _ := e.m.PlatformCall(cctx, c.ID, slot, "mcp", liveCall(t, "PATCH", "/datasets/16", `{"name":"x"}`))
		done <- r
	}()
	ap := pendingApproval(t, e, c.ID)
	cancel() // pi aborted the call
	<-done
	got, err := e.st.GetApproval(ctx, ap.ID)
	if err != nil || got.State != store.ApprovalCancelled {
		t.Fatalf("approval after the abort: %+v %v", got, err)
	}
	if aps, _ := e.st.ListApprovals(ctx, store.ApprovalPending, c.ID); len(aps) != 0 {
		t.Fatalf("the card must be gone: %+v", aps)
	}
	if d, err := e.m.Decide(ctx, ap.ID, false); err == nil && d.State != store.ApprovalCancelled {
		t.Fatalf("a later click must not turn it into a rejection: %+v", d)
	}
}

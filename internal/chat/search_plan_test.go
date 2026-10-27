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
	"testing"
	"time"

	"agw/internal/delegation"
	"agw/internal/platform"
	"agw/internal/search"
	"agw/internal/store"
	"agw/internal/watch"
)

// searchPlanPlatform answers what a template search reads about container 7 and the takeover of container 63.
func searchPlanPlatform(t *testing.T, e *env) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/token":
			fmt.Fprint(w, `{"access_token":"t","expires_in":3600}`)
		case r.URL.Path == "/train/containers/7/metrics":
			fmt.Fprint(w, `{"supported":true,"epochs":[],"gpu":[]}`)
		case r.URL.Path == "/train/containers/7/config":
			fmt.Fprint(w, `{"config":{"train":{"values":{"lr":0.1,"epochs":5},"schema":{"properties":{"lr":{"type":"number"},"epochs":{"type":"integer"}}}}},`+
				`"provider":"Torchvision","architecture":{"name":"EfficientNet","category":"classification"},"dataset_id":16}`)
		case r.URL.Path == "/datasets/16":
			fmt.Fprint(w, `{"name":"mnist","filecount":100}`)
		case r.Method == http.MethodGet && r.URL.Path == "/train/containers/63/model":
			fmt.Fprint(w, `{"id":42}`)
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
	e.m.RegisterVirtual(platform.SearchPrefix, e.m.SearchDoer())
}

const planSearchArgs = `{"title":"t","path":"template","base_container_id":7,` +
	`"space":{"lr":{"type":"float","min":0.0001,"max":0.5,"scale":"log"}},"budget":{"max_runs":1,"max_compute_minutes":10}}`

// planWithSearch: the search step, then the takeover of the winner's container through the placeholder.
const planWithSearch = `{"title":"Search and take over","steps":[
 {"tool":"propose_search","args":` + planSearchArgs + `},
 {"method":"GET","path":"/train/containers/{{step1.id}}/model"}]}`

func searchCall(t *testing.T, args string) platform.Request {
	t.Helper()
	tool, ok := platform.Lookup("propose_search")
	if !ok {
		t.Fatal("no propose_search")
	}
	req, err := tool.Build(json.RawMessage(args))
	if err != nil {
		t.Fatal(err)
	}
	req, err = platform.Normalize(req)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

// approvedSearchPlan makes a chat that owns container 7 and an approved plan with a search step.
func approvedSearchPlan(t *testing.T, e *env) (context.Context, ChatView, string) {
	t.Helper()
	searchPlanPlatform(t, e)
	ctx := store.WithSession(context.Background(), "main")
	c, err := e.m.Create(ctx, NewChat{})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.st.AddDelegationObject(ctx, c.ID, delegation.Training, "7"); err != nil {
		t.Fatal(err)
	}
	slot := e.m.live[c.ID].slot.ID
	if r, _ := proposeAndDecide(t, e, ctx, c.ID, slot, planWithSearch, true); r.Status != "ok" {
		t.Fatalf("plan: %+v", r)
	}
	return ctx, c, slot
}

// finishFirstTrial runs the one trial of a one-run search to its end with the container 63 as the winner.
func finishFirstTrial(t *testing.T, e *env, c ChatView, s store.Search) {
	t.Helper()
	ctx := context.Background()
	tr, err := e.st.DeclareTrial(ctx, store.Trial{ChatID: c.ID, SearchID: s.ID, Path: s.Path, Params: json.RawMessage(`{"lr":0.1}`)})
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := e.st.ClaimTrialStart(ctx, tr.ID, ""); !ok {
		t.Fatal("claim")
	}
	jobID, err := e.st.AddWatchJob(ctx, store.WatchJob{ChatID: c.ID, Kind: "training_run", Ref: "63"})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.st.SetTrialJob(ctx, tr.ID, "63", jobID); err != nil {
		t.Fatal(err)
	}
	score, epoch, epochs := 0.93, 5, 5
	if err := e.st.SetTrialProgress(ctx, tr.ID, &epoch, &epochs, &score, &epoch, nil); err != nil {
		t.Fatal(err)
	}
	searchTrialEnded(ctx, e.m, store.WatchJob{ID: jobID, ChatID: c.ID, Kind: "training_run", Ref: "63"}, watch.Progress{State: watch.StateDone})
}

// The search is the next covered step of the approved plan: no card, the search runs, its approval is the plan's,
// and the step waits for the end of the search (issue #244, E2).
func TestProposeSearchCoveredByPlan(t *testing.T) {
	e := setup(t)
	ctx, c, slot := approvedSearchPlan(t, e)
	res, err := e.m.PlatformCall(ctx, c.ID, slot, "cli", searchCall(t, planSearchArgs))
	if err != nil || res.Status != "ok" || !strings.Contains(res.Body, "approved by plan") {
		t.Fatalf("search: %+v %v", res, err)
	}
	if kinds := approvalKinds(t, e, ctx, c.ID); len(kinds) != 1 || kinds[0] != "plan" {
		t.Fatalf("approvals: %v (only the plan's)", kinds)
	}
	p := onlyPlan(t, e, ctx, c.ID)
	list, err := e.st.ListSearches(ctx, c.ID)
	if err != nil || len(list) != 1 {
		t.Fatalf("searches: %+v %v", list, err)
	}
	if list[0].State != store.SearchRunning || list[0].ApprovalID != p.ApprovalID {
		t.Fatalf("search: state %q approval %q, plan approval %q", list[0].State, list[0].ApprovalID, p.ApprovalID)
	}
	if st := p.Steps[0]; st.State != "waiting" || st.SearchID != list[0].ID {
		t.Fatalf("step 1: %+v", st)
	}
	// The takeover is not yet the plan's next step: the search is still running.
	if res.PlanStep == nil || *res.PlanStep != 1 || res.PlanID != p.ID {
		t.Errorf("the answer does not name the plan step: %+v", res)
	}
}

// A search that is not exactly the step (another space) is not covered: the user is asked on its own.
func TestProposeSearchPlanMismatchAsksUser(t *testing.T) {
	e := setup(t)
	ctx, c, slot := approvedSearchPlan(t, e)
	other := strings.Replace(planSearchArgs, `"max":0.5`, `"max":0.4`, 1)
	done := startCall(e, c.ID, slot, searchCall(t, other))
	ap := pendingApproval(t, e, c.ID)
	if ap.Kind != "search" {
		t.Fatalf("approval kind %q", ap.Kind)
	}
	if st := onlyPlan(t, e, ctx, c.ID).Steps[0]; st.State != "open" {
		t.Fatalf("the step was claimed by a call that is not the step: %+v", st)
	}
	if _, err := e.m.Decide(ctx, ap.ID, false); err != nil {
		t.Fatal(err)
	}
	<-done
}

// When the search ends with a winner, the step is done with the winner's container as its object, and the next
// step names it by {{step1.id}} and is covered (issue #244, E3).
func TestPlanSearchStepDoneWithWinner(t *testing.T) {
	e := setup(t)
	ctx, c, slot := approvedSearchPlan(t, e)
	if res, _ := e.m.PlatformCall(ctx, c.ID, slot, "cli", searchCall(t, planSearchArgs)); res.Status != "ok" {
		t.Fatalf("search: %+v", res)
	}
	list, _ := e.st.ListSearches(ctx, c.ID)
	finishFirstTrial(t, e, c, list[0])
	p := onlyPlan(t, e, ctx, c.ID)
	if st := p.Steps[0]; st.State != "done" || st.CreatedResource != delegation.Training || st.CreatedID != "63" {
		t.Fatalf("step 1: %+v", st)
	}
	res, err := e.m.PlatformCall(ctx, c.ID, slot, "cli", liveCall(t, "GET", "/train/containers/63/model", ""))
	if err != nil || res.Status != "ok" || res.PlanStep == nil || *res.PlanStep != 2 {
		t.Fatalf("step 2: %+v %v", res, err)
	}
}

// Without a winner the step fails and the plan with it.
func TestPlanSearchStepFailsWithoutWinner(t *testing.T) {
	e := setup(t)
	ctx, c, slot := approvedSearchPlan(t, e)
	if res, _ := e.m.PlatformCall(ctx, c.ID, slot, "cli", searchCall(t, planSearchArgs)); res.Status != "ok" {
		t.Fatalf("search: %+v", res)
	}
	list, _ := e.st.ListSearches(ctx, c.ID)
	if ended, _ := e.m.finishSearch(ctx, list[0], endByAgent, nil, "no trial"); !ended {
		t.Fatal("search not ended")
	}
	p := onlyPlan(t, e, ctx, c.ID)
	if p.Steps[0].State != "failed" || p.State != "failed" {
		t.Fatalf("plan %q, step 1 %+v", p.State, p.Steps[0])
	}
}

// An interactive plan with a search step is valid for the usual time plus the search's compute minutes, at most as
// long as a search may run (issue #244, E5).
func TestInteractivePlanWithSearchExpiry(t *testing.T) {
	m := &Manager{}
	m.opt.SearchMaxHours = 2
	step := func(minutes int) store.PlanStep {
		return store.PlanStep{Idx: 1, Tool: "propose_search",
			Form: json.RawMessage(fmt.Sprintf(`{"budget":["{\"max_runs\":3,\"max_compute_minutes\":%d}"]}`, minutes))}
	}
	if got := m.planExpiry(nil); got != 30*time.Minute {
		t.Errorf("without a search: %v", got)
	}
	if got := m.planExpiry([]store.PlanStep{step(60)}); got != 90*time.Minute {
		t.Errorf("60 minutes: %v", got)
	}
	if got := m.planExpiry([]store.PlanStep{step(600)}); got != 2*time.Hour {
		t.Errorf("capped: %v", got)
	}
}

// The search covers the model takeover of the winner's container, once, and nothing else (issue #244, E6).
func TestSearchCoversWinnerModelTakeover(t *testing.T) {
	e := setup(t)
	searchPlanPlatform(t, e)
	ctx := context.Background()
	c, s := runningSearch(t, e, `{"max_runs":1}`)
	if claim := e.m.searchMatch(ctx, c.ID, liveCall(t, "GET", "/train/containers/63/model", "")); claim != nil {
		t.Fatalf("covered while the search runs: %+v", claim)
	}
	finishFirstTrial(t, e, c, s)
	for _, other := range []string{"64", "7"} {
		if claim := e.m.searchMatch(ctx, c.ID, liveCall(t, "GET", "/train/containers/"+other+"/model", "")); claim != nil {
			t.Errorf("container %s covered", other)
		}
	}
	req := liveCall(t, "GET", "/train/containers/63/model", "")
	claim := e.m.searchMatch(ctx, c.ID, req)
	if claim == nil || !claim.Covered || claim.Kind != claimTakeover {
		t.Fatalf("takeover of the winner: %+v", claim)
	}
	e.m.searchFinish(ctx, c.ID, claim, delegation.Classify(req), req, platform.Result{Status: "ok", HTTPStatus: 200, Body: `{"id":42}`}, nil, "")
	if again := e.m.searchMatch(ctx, c.ID, req); again != nil {
		t.Errorf("a second takeover is covered: %+v", again)
	}
}

// The end text points to ONE plan for the rest of the route and says what the search covers.
func TestSearchEndTextNamesPlan(t *testing.T) {
	m := &Manager{}
	score := 0.9
	w := &store.Trial{Idx: 1, Score: &score, Ref: "63"}
	got := m.endText(store.Search{Path: store.SearchPathTemplate}, search.Objective{Metric: "val_acc", Mode: "max"}, "budget_runs", w,
		SearchHoldout{Available: true, FinalState: "pending", EvalDataset: &SearchNamed{ID: 12}})
	for _, want := range []string{"propose the remaining steps as ONE plan", "{{tool:propose_plan}}", "covered by this search"} {
		if !strings.Contains(got, want) {
			t.Errorf("end text lacks %q: %s", want, got)
		}
	}
}

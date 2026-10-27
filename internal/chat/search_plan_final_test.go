// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"agw/internal/platform"
	"agw/internal/store"
)

// planWithSearchAndFinal: the search, the takeover of the winner's container and the final test of the model as
// steps of one plan (B29, Demo 12).
const planWithSearchAndFinal = `{"title":"Search, take over, final test","steps":[
 {"tool":"propose_search","args":` + planSearchArgs + `},
 {"method":"GET","path":"/train/containers/{{step1.id}}/model"},
 {"method":"POST","path":"/_agw/assess/checks","body":{"eval_dataset_id":16,"final":true,"model_id":"{{step2.id}}"}}]}`

// pendingHoldout gives an ended search the held-back set 16 with the final test still to run.
func pendingHoldout(t *testing.T, e *env, searchID string) {
	t.Helper()
	ho, _ := json.Marshal(SearchHoldout{Available: true, FinalState: "pending", Images: 90, EvalDataset: &SearchNamed{ID: 16}})
	if err := e.st.SetSearchHoldout(context.Background(), searchID, ho); err != nil {
		t.Fatal(err)
	}
}

func holdoutOf(t *testing.T, e *env, chatID string) SearchHoldout {
	t.Helper()
	list, err := e.m.Searches(context.Background(), chatID)
	if err != nil || len(list) != 1 {
		t.Fatalf("searches: %+v %v", list, err)
	}
	var ho SearchHoldout
	if err := json.Unmarshal(list[0].Holdout, &ho); err != nil {
		t.Fatal(err)
	}
	return ho
}

func searchEventKinds(t *testing.T, e *env, searchID string) map[string]int {
	t.Helper()
	events, err := e.st.SearchEvents(context.Background(), searchID)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int{}
	for _, ev := range events {
		kinds[ev.Kind]++
	}
	return kinds
}

// The final test that runs as a plan step (not as a call the search covers) is the search's final test all the
// same: the search names the check and is no longer pending (B29). The takeover as a plan step is recorded too.
func TestPlanStepFinalCheckLinksSearch(t *testing.T) {
	e := setup(t)
	searchPlanPlatform(t, e)
	e.m.RegisterVirtual(platform.AssessPrefix, &assessStub{})
	ctx := store.WithSession(context.Background(), "main")
	c, err := e.m.Create(ctx, NewChat{})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.st.AddDelegationObject(ctx, c.ID, "training", "7"); err != nil {
		t.Fatal(err)
	}
	slot := e.m.live[c.ID].slot.ID
	if r, _ := proposeAndDecide(t, e, ctx, c.ID, slot, planWithSearchAndFinal, true); r.Status != "ok" {
		t.Fatalf("plan: %+v", r)
	}
	if res, _ := e.m.PlatformCall(ctx, c.ID, slot, "cli", searchCall(t, planSearchArgs)); res.Status != "ok" {
		t.Fatalf("search: %+v", res)
	}
	list, _ := e.st.ListSearches(ctx, c.ID)
	finishFirstTrial(t, e, c, list[0])
	pendingHoldout(t, e, list[0].ID)
	res, err := e.m.PlatformCall(ctx, c.ID, slot, "cli", liveCall(t, http.MethodGet, "/train/containers/63/model", ""))
	if err != nil || res.Status != "ok" || res.PlanStep == nil || *res.PlanStep != 2 {
		t.Fatalf("step 2: %+v %v", res, err)
	}
	// The fake platform names no dataset of container 63, so the takeover recorded no origin; live it does.
	if err := e.st.RecordModelOrigin(ctx, store.ModelOrigin{ModelID: 42, ChatID: c.ID, Source: store.OriginTraining, TrainContainerID: 63}); err != nil {
		t.Fatal(err)
	}
	res, err = e.m.PlatformCall(ctx, c.ID, slot, "cli", liveCall(t, http.MethodPost, platform.AssessCheckPath,
		`{"eval_dataset_id":16,"final":true,"model_id":42}`))
	if err != nil || res.Status != "ok" || res.PlanStep == nil || *res.PlanStep != 3 {
		t.Fatalf("step 3: %+v %v", res, err)
	}
	ho := holdoutOf(t, e, c.ID)
	if ho.FinalState != "started" || ho.CheckID != "11111111-2222-3333-4444-555555555555" {
		t.Fatalf("holdout after the final test as a plan step: %+v", ho)
	}
	kinds := searchEventKinds(t, e, list[0].ID)
	if kinds["final"] != 1 || kinds["takeover"] != 1 {
		t.Errorf("search events: %v", kinds)
	}
}

// A search whose final test ran as a plan step before B29 still says "pending": reading it links the check of the
// winner's model on the held-back set, once, and fills the result (Demo 12). Checks of another model, of another
// set, interim checks and a check that did not run the held-back images are not linked.
func TestSearchReadLinksFinalCheckOfWinner(t *testing.T) {
	e := setup(t)
	searchPlanPlatform(t, e)
	ctx := context.Background()
	c, s := runningSearch(t, e, `{"max_runs":1}`)
	finishFirstTrial(t, e, c, s)
	pendingHoldout(t, e, s.ID)
	for id, container := range map[int64]int64{42: 63, 43: 64} {
		if err := e.st.RecordModelOrigin(ctx, store.ModelOrigin{ModelID: id, ChatID: c.ID, Source: store.OriginTraining, TrainContainerID: container}); err != nil {
			t.Fatal(err)
		}
	}
	report := json.RawMessage(`{"verdict":"usable","measured_images":90,"figures":{"overall":{"accuracy":0.85}}}`)
	add := func(a store.Assessment, state string) store.Assessment {
		t.Helper()
		a.ChatID, a.Kind = c.ID, "training_model"
		row, err := e.st.CreateAssessment(ctx, a)
		if err != nil {
			t.Fatal(err)
		}
		if state != "" {
			if err := e.st.FinishAssessment(ctx, c.ID, row.ID, state, report, ""); err != nil {
				t.Fatal(err)
			}
		}
		return row
	}
	other := add(store.Assessment{Subject: "model:43", EvalDatasetID: 16, Final: true}, store.AssessFinished) // another model
	if ho := holdoutOf(t, e, c.ID); ho.FinalState != "pending" || ho.CheckID != "" {
		t.Fatalf("linked the final check of another model: %+v", ho)
	}
	// One measured final check per set and chat (assessments_final_once2): the other model's check gives way.
	if err := e.st.MarkHoldoutNotRun(ctx, other.ID); err != nil {
		t.Fatal(err)
	}
	add(store.Assessment{Subject: "model:42", EvalDatasetID: 17, Final: true}, store.AssessFinished) // another set
	add(store.Assessment{Subject: "model:42", EvalDatasetID: 16}, store.AssessFinished)              // interim check
	notRun := add(store.Assessment{Subject: "model:42", EvalDatasetID: 16, Final: true}, store.AssessFinished)
	if err := e.st.MarkHoldoutNotRun(ctx, notRun.ID); err != nil {
		t.Fatal(err)
	}
	if ho := holdoutOf(t, e, c.ID); ho.FinalState != "pending" || ho.CheckID != "" {
		t.Fatalf("linked a check that is not the final test of the winner: %+v", ho)
	}
	final := add(store.Assessment{Subject: "model:42", EvalDatasetID: 16, Final: true}, store.AssessFinished)
	ho := holdoutOf(t, e, c.ID)
	if ho.FinalState != "started" || ho.CheckID != final.ID {
		t.Fatalf("holdout after reading: %+v", ho)
	}
	var res FinalResult
	if err := json.Unmarshal(ho.Result, &res); err != nil || res.State != store.AssessFinished || res.Accuracy == nil ||
		*res.Accuracy != 0.85 || res.Images != 90 || res.Verdict != "usable" {
		t.Fatalf("result: %s %v", ho.Result, err)
	}
	// stored: a second read links nothing more
	_ = holdoutOf(t, e, c.ID)
	if kinds := searchEventKinds(t, e, s.ID); kinds["final"] != 1 {
		t.Errorf("final events: %v", kinds)
	}
}

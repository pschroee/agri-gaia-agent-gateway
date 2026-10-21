// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"agw/internal/search"
	"agw/internal/store"
)

// A final check that ended without the hold-out run puts the search back to pending, clears the check and
// counts the retry (#181, NT7); a check that measured keeps its result.
func TestSearchFinalRetry(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, s := runningSearch(t, e, `{"max_runs":3}`)
	check, err := e.st.CreateAssessment(ctx, store.Assessment{ChatID: c.ID, Kind: "own_model", Subject: "sha:a", EvalDatasetID: 9, Final: true})
	if err != nil {
		t.Fatal(err)
	}
	ho, _ := json.Marshal(SearchHoldout{Available: true, FinalState: "started", CheckID: check.ID, EvalDataset: &SearchNamed{ID: 9}})
	if err := e.st.SetSearchHoldout(ctx, s.ID, ho); err != nil {
		t.Fatal(err)
	}
	report := `{"verdict":"not_measured","holdout_not_run":true,"measured_images":0}`
	if err := e.st.FinishAssessment(ctx, c.ID, check.ID, store.AssessFinished, json.RawMessage(report), ""); err != nil {
		t.Fatal(err)
	}
	list, err := e.m.Searches(ctx, c.ID)
	if err != nil || len(list) != 1 {
		t.Fatalf("searches: %v %v", list, err)
	}
	var got SearchHoldout
	_ = json.Unmarshal(list[0].Holdout, &got)
	if got.FinalState != "pending" || got.CheckID != "" || got.FinalRetries != 1 || len(got.Result) != 0 {
		t.Fatalf("holdout after a final check without hold-out run: %+v", got)
	}
	// stored: a second read counts nothing more
	list, _ = e.m.Searches(ctx, c.ID)
	_ = json.Unmarshal(list[0].Holdout, &got)
	if got.FinalRetries != 1 {
		t.Errorf("retries counted twice: %d", got.FinalRetries)
	}
}

// matchFinal covers at most maxFinalRetries repeats of the final test.
func TestMatchFinalStopsAfterRetries(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, s := runningSearch(t, e, `{"max_runs":3}`)
	winner := 1
	if ended, _ := e.m.finishSearch(ctx, s, endByAgent, &winner, "done"); !ended {
		t.Fatal("search not ended")
	}
	ho, _ := json.Marshal(SearchHoldout{Available: true, FinalState: "pending", EvalDataset: &SearchNamed{ID: 9}, FinalRetries: maxFinalRetries})
	if err := e.st.SetSearchHoldout(ctx, s.ID, ho); err != nil {
		t.Fatal(err)
	}
	if claim := e.m.matchFinal(ctx, c.ID, finalCheckBody{ModelID: 5}); claim != nil {
		t.Errorf("a fourth repeat is covered: %+v", claim)
	}
}

func TestEndTextNotebookModelMeta(t *testing.T) {
	m := &Manager{}
	score := 0.91
	w := &store.Trial{Idx: 2, Score: &score, Ref: "r2026"}
	obj := search.Objective{Metric: "val_acc", Mode: "max"}
	ho := SearchHoldout{Available: true, FinalState: "pending", EvalDataset: &SearchNamed{ID: 12}}
	got := m.endText(store.Search{Path: store.SearchPathNotebook}, obj, "budget_runs", w, ho)
	if !strings.Contains(got, "The check takes class names and preprocessing from outputs/model.json of that run.") {
		t.Errorf("notebook end text: %s", got)
	}
	got = m.endText(store.Search{Path: store.SearchPathTemplate}, obj, "budget_runs", w, ho)
	if strings.Contains(got, "outputs/model.json") {
		t.Errorf("template end text names model.json: %s", got)
	}
}

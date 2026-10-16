// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// The rows of a check: create, read, another chat sees nothing, the report of a finished check, the
// count without failures (R0.16, R0.20).
func TestAssessments(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c, err := s.CreateChat(ctx, NewChat{Title: "checks", Model: "m", Variant: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.CreateChat(ctx, NewChat{Title: "other", Model: "m", Variant: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.CreateAssessment(ctx, Assessment{ChatID: c.ID, Kind: "training_model", Subject: "model:1",
		EvalDatasetID: 7, Final: true, ToolCallID: "call-1"})
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == "" || a.State != AssessQueued || a.StartedAt != nil || a.EndedAt != nil || a.CreatedAt.IsZero() {
		t.Fatalf("new check: %+v", a)
	}
	got, err := s.Assessment(ctx, c.ID, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != "training_model" || got.Subject != "model:1" || got.EvalDatasetID != 7 || !got.Final || got.ToolCallID != "call-1" {
		t.Fatalf("read: %+v", got)
	}
	// A check of another chat is not found, and neither is an unknown or misshapen id.
	if _, err := s.Assessment(ctx, other.ID, a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another chat: %v", err)
	}
	if _, err := s.Assessment(ctx, c.ID, "11111111-2222-3333-4444-555555555555"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown check: %v", err)
	}
	if _, err := s.Assessment(ctx, c.ID, "not-a-uuid"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("misshapen id: %v", err)
	}
	// Starting it sets started_at, the progress is kept while it runs, and the end writes state, report
	// and error.
	if err := s.SetAssessmentState(ctx, c.ID, a.ID, AssessRunning); err != nil {
		t.Fatal(err)
	}
	running, err := s.Assessment(ctx, c.ID, a.ID)
	if err != nil || running.StartedAt == nil {
		t.Fatalf("running: %+v %v", running, err)
	}
	if err := s.SetAssessmentProgress(ctx, a.ID, json.RawMessage(`{"set":"holdout","images":10,"note":"10 of 40"}`)); err != nil {
		t.Fatal(err)
	}
	if row, _ := s.Assessment(ctx, c.ID, a.ID); len(row.Progress) == 0 {
		t.Error("the progress is lost")
	}
	if err := s.FinishAssessment(ctx, c.ID, a.ID, AssessFinished, json.RawMessage(`{"header":"Result: usable"}`), ""); err != nil {
		t.Fatal(err)
	}
	done, _ := s.Assessment(ctx, c.ID, a.ID)
	var report struct {
		Header string `json:"header"`
	}
	if err := json.Unmarshal(done.Report, &report); err != nil {
		t.Fatalf("report: %s (%v)", done.Report, err)
	}
	if done.State != AssessFinished || done.EndedAt == nil || report.Header != "Result: usable" {
		t.Fatalf("finished: %+v", done)
	}
	// The count of a chat leaves failed checks out.
	if n, err := s.CountAssessments(ctx, c.ID); err != nil || n != 1 {
		t.Fatalf("count: %d %v", n, err)
	}
	if n, _ := s.CountAssessments(ctx, other.ID); n != 0 {
		t.Errorf("another chat counts %d checks", n)
	}
	// The list is oldest first.
	if _, err := s.CreateAssessment(ctx, Assessment{ChatID: c.ID, Kind: "own_model", Subject: "sha:abc"}); err != nil {
		t.Fatal(err)
	}
	list, err := s.Assessments(ctx, c.ID)
	if err != nil || len(list) != 2 || list[0].ID != a.ID {
		t.Fatalf("list: %+v %v", list, err)
	}
}

// A check that was queued or running when the gateway stopped has no container any more: the recovery
// ends it as failed (R0.22).
func TestRecoverAssessments(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c, err := s.CreateChat(ctx, NewChat{Title: "recover", Model: "m", Variant: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	queued, err := s.CreateAssessment(ctx, Assessment{ChatID: c.ID, Kind: "training_model", Subject: "model:1"})
	if err != nil {
		t.Fatal(err)
	}
	running, err := s.CreateAssessment(ctx, Assessment{ChatID: c.ID, Kind: "own_model", Subject: "sha:abc"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetAssessmentState(ctx, c.ID, running.ID, AssessRunning); err != nil {
		t.Fatal(err)
	}
	finished, err := s.CreateAssessment(ctx, Assessment{ChatID: c.ID, Kind: "deployment", Subject: "deployment:1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishAssessment(ctx, c.ID, finished.ID, AssessFinished, json.RawMessage(`{"header":"Result: usable"}`), ""); err != nil {
		t.Fatal(err)
	}
	n, err := s.RecoverAssessments(ctx)
	if err != nil || n != 2 {
		t.Fatalf("recovered: %d %v", n, err)
	}
	for _, id := range []string{queued.ID, running.ID} {
		row, err := s.Assessment(ctx, c.ID, id)
		if err != nil || row.State != AssessFailed || row.Error != "gateway restarted" || row.EndedAt == nil {
			t.Errorf("check %s: %+v %v", id, row, err)
		}
	}
	// The finished check keeps its report.
	row, _ := s.Assessment(ctx, c.ID, finished.ID)
	if row.State != AssessFinished || row.Error != "" {
		t.Errorf("the finished check was touched: %+v", row)
	}
}

// A final check of one dataset happens once per chat: the unique partial index holds the rule, and a
// failed one frees the dataset again (P4).
func TestAssessmentsFinalOnce(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c, err := s.CreateChat(ctx, NewChat{Title: "final", Model: "m", Variant: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.CreateAssessment(ctx, Assessment{ChatID: c.ID, Kind: "training_model", Subject: "model:1", EvalDatasetID: 3, Final: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateAssessment(ctx, Assessment{ChatID: c.ID, Kind: "training_model", Subject: "model:2", EvalDatasetID: 3, Final: true}); err == nil {
		t.Fatal("a second final check of the same dataset is let through")
	}
	// An interim check of the same dataset is fine.
	if _, err := s.CreateAssessment(ctx, Assessment{ChatID: c.ID, Kind: "training_model", Subject: "model:2", EvalDatasetID: 3}); err != nil {
		t.Fatalf("an interim check: %v", err)
	}
	// The same for another dataset or another chat.
	if _, err := s.CreateAssessment(ctx, Assessment{ChatID: c.ID, Kind: "training_model", Subject: "model:2", EvalDatasetID: 4, Final: true}); err != nil {
		t.Fatalf("another dataset: %v", err)
	}
	if ok, err := s.FinalUsed(ctx, c.ID, 3); err != nil || !ok {
		t.Fatalf("FinalUsed: %v %v", ok, err)
	}
	if ok, _ := s.FinalUsed(ctx, c.ID, 5); ok {
		t.Error("a dataset without a final check reports one")
	}
	if ok, _ := s.FinalUsed(ctx, c.ID, 0); ok {
		t.Error("a check without a dataset reports a final one")
	}
	// Failed: the dataset is free again.
	if err := s.FinishAssessment(ctx, c.ID, first.ID, AssessFailed, nil, "gateway restarted"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.FinalUsed(ctx, c.ID, 3); ok {
		t.Error("a failed check counts as the final one")
	}
	if _, err := s.CreateAssessment(ctx, Assessment{ChatID: c.ID, Kind: "training_model", Subject: "model:3", EvalDatasetID: 3, Final: true}); err != nil {
		t.Fatalf("after a failed final check: %v", err)
	}
}

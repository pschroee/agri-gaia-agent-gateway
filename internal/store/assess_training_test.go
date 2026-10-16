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

// Where a model came from is what the gateway saw, keyed by the model; a second entry for the same model
// replaces the first, and another chat sees nothing (R1.6, P2).
func TestModelOriginRecorded(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c := testChatRow(t, s, "origins")
	other := testChatRow(t, s, "other")
	if _, err := s.ModelOriginOf(ctx, c.ID, 21); !errors.Is(err, ErrNotFound) {
		t.Fatalf("without a row: %v", err)
	}
	if err := s.RecordModelOrigin(ctx, ModelOrigin{ModelID: 21, ChatID: c.ID, Source: OriginTraining,
		TrainContainerID: 9, SeenDatasetIDs: []int64{7, 8}}); err != nil {
		t.Fatal(err)
	}
	got, err := s.ModelOriginOf(ctx, c.ID, 21)
	if err != nil {
		t.Fatal(err)
	}
	if got.Source != OriginTraining || got.TrainContainerID != 9 || len(got.SeenDatasetIDs) != 2 ||
		got.SeenDatasetIDs[0] != 7 || got.SeenDatasetIDs[1] != 8 || got.CreatedAt.IsZero() {
		t.Fatalf("origin: %+v", got)
	}
	if _, err := s.ModelOriginOf(ctx, other.ID, 21); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another chat sees the row: %v", err)
	}
	// The newest entry wins (a model is created once, but a later statement about it may be better).
	if err := s.RecordModelOrigin(ctx, ModelOrigin{ModelID: 21, ChatID: c.ID, Source: OriginNotebook,
		SeenDatasetIDs: []int64{5}}); err != nil {
		t.Fatal(err)
	}
	got, err = s.ModelOriginOf(ctx, c.ID, 21)
	if err != nil || got.Source != OriginNotebook || got.TrainContainerID != 0 || len(got.SeenDatasetIDs) != 1 {
		t.Fatalf("after the second entry: %+v %v", got, err)
	}
	// A model without datasets is allowed (an empty list, not null).
	if err := s.RecordModelOrigin(ctx, ModelOrigin{ModelID: 22, ChatID: c.ID, Source: OriginNotebook}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ModelOriginOf(ctx, c.ID, 22); err != nil || got.SeenDatasetIDs == nil || len(got.SeenDatasetIDs) != 0 {
		t.Fatalf("without datasets: %+v %v", got, err)
	}
}

// The split is looked up from both sides: the training dataset knows its source, the source its newest
// training dataset (R1.5).
func TestRecordSplitAndLookup(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c := testChatRow(t, s, "splits")
	other := testChatRow(t, s, "other")
	if err := s.RecordSplit(ctx, Split{ChatID: c.ID, SourceDatasetID: 8, TrainDatasetID: 42,
		TrainImages: 84, KeptImages: 36}); err != nil {
		t.Fatal(err)
	}
	sp, err := s.SplitOfTrainDataset(ctx, c.ID, 42)
	if err != nil {
		t.Fatal(err)
	}
	if sp.SourceDatasetID != 8 || sp.TrainImages != 84 || sp.KeptImages != 36 {
		t.Fatalf("split: %+v", sp)
	}
	if sp, err := s.SplitOfSourceDataset(ctx, c.ID, 8); err != nil || sp.TrainDatasetID != 42 {
		t.Fatalf("from the source: %+v %v", sp, err)
	}
	if _, err := s.SplitOfTrainDataset(ctx, other.ID, 42); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another chat sees the split: %v", err)
	}
	if _, err := s.SplitOfTrainDataset(ctx, c.ID, 43); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an unknown training dataset: %v", err)
	}
	// A second split of the same source: the newest is the one the chat trains on now, and the training
	// dataset is the key (recording it twice changes nothing).
	if err := s.RecordSplit(ctx, Split{ChatID: c.ID, SourceDatasetID: 8, TrainDatasetID: 43,
		TrainImages: 70, KeptImages: 50}); err != nil {
		t.Fatal(err)
	}
	if sp, err := s.SplitOfSourceDataset(ctx, c.ID, 8); err != nil || sp.TrainDatasetID != 43 {
		t.Fatalf("the newest split: %+v %v", sp, err)
	}
	if err := s.RecordSplit(ctx, Split{ChatID: c.ID, SourceDatasetID: 8, TrainDatasetID: 43,
		TrainImages: 71, KeptImages: 49}); err != nil {
		t.Fatal(err)
	}
	if sp, _ := s.SplitOfTrainDataset(ctx, c.ID, 43); sp.TrainImages != 71 || sp.KeptImages != 49 {
		t.Fatalf("the second entry did not replace the first: %+v", sp)
	}
	// A misshapen chat id is not found instead of an error.
	if _, err := s.SplitOfTrainDataset(ctx, "not-a-uuid", 42); !errors.Is(err, ErrNotFound) {
		t.Fatalf("misshapen chat: %v", err)
	}
}

// The previous check of a dataset is the newest finished interim check of another model (R1.12).
func TestPreviousInterimCheck(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c := testChatRow(t, s, "previous")
	report := json.RawMessage(`{"verdict":"questionable","figures":{"model":{"id":2,"name":"old"},"overall":{"accuracy":0.62,"macro_f1":0.6}}}`)
	first, err := s.CreateAssessment(ctx, Assessment{ChatID: c.ID, Kind: "training_model", Subject: "model:2",
		EvalDatasetID: 7, Final: false})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.PreviousInterimCheck(ctx, c.ID, 7, "model:3"); err != nil || ok {
		t.Fatalf("a running check counts: %v %v", ok, err)
	}
	if err := s.FinishAssessment(ctx, c.ID, first.ID, AssessFinished, report, ""); err != nil {
		t.Fatal(err)
	}
	prev, ok, err := s.PreviousInterimCheck(ctx, c.ID, 7, "model:3")
	if err != nil || !ok {
		t.Fatalf("previous: %v %v", ok, err)
	}
	if prev.Subject != "model:2" || len(prev.Report) == 0 {
		t.Fatalf("previous: %+v", prev)
	}
	// The same model is no previous check, another dataset neither.
	if _, ok, _ := s.PreviousInterimCheck(ctx, c.ID, 7, "model:2"); ok {
		t.Error("the check of the same model counts as a previous one")
	}
	if _, ok, _ := s.PreviousInterimCheck(ctx, c.ID, 9, "model:3"); ok {
		t.Error("a check of another dataset counts as a previous one")
	}
	// A final check is no interim check.
	second, err := s.CreateAssessment(ctx, Assessment{ChatID: c.ID, Kind: "training_model", Subject: "model:4",
		EvalDatasetID: 7, Final: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishAssessment(ctx, c.ID, second.ID, AssessFinished, report, ""); err != nil {
		t.Fatal(err)
	}
	if prev, ok, _ := s.PreviousInterimCheck(ctx, c.ID, 7, "model:3"); !ok || prev.Subject != "model:2" {
		t.Fatalf("the final check was taken as the previous one: %+v", prev)
	}
	// The newest interim check wins.
	third, err := s.CreateAssessment(ctx, Assessment{ChatID: c.ID, Kind: "training_model", Subject: "model:5",
		EvalDatasetID: 7})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishAssessment(ctx, c.ID, third.ID, AssessFinished, report, ""); err != nil {
		t.Fatal(err)
	}
	if prev, _, _ := s.PreviousInterimCheck(ctx, c.ID, 7, "model:3"); prev.Subject != "model:5" {
		t.Fatalf("not the newest: %+v", prev)
	}
}

// The last finished check of a model is what an approval of an image build names; a running one does not
// count, and a failed one neither (R1.14, W4).
func TestLastFinishedCheck(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c := testChatRow(t, s, "last check")
	if _, found, err := s.LastFinishedCheck(ctx, c.ID, "model:3"); err != nil || found {
		t.Fatalf("without a check: %v %v", found, err)
	}
	a, err := s.CreateAssessment(ctx, Assessment{ChatID: c.ID, Kind: "training_model", Subject: "model:3",
		EvalDatasetID: 7, Final: false})
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := s.LastFinishedCheck(ctx, c.ID, "model:3"); err != nil || found {
		t.Fatalf("a queued check counts: %v %v", found, err)
	}
	report := json.RawMessage(`{"verdict":"usable","measured_images":24,"reasons":[],"figures":{"overall":{"accuracy":0.92}}}`)
	if err := s.FinishAssessment(ctx, c.ID, a.ID, AssessFinished, report, ""); err != nil {
		t.Fatal(err)
	}
	got, found, err := s.LastFinishedCheck(ctx, c.ID, "model:3")
	if err != nil || !found || got.Final || got.Subject != "model:3" {
		t.Fatalf("interim: %+v %v %v", got, found, err)
	}
	// A failed check is no answer, and a check of another model neither.
	if err := s.FinishAssessment(ctx, c.ID, a.ID, AssessFailed, nil, "boom"); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := s.LastFinishedCheck(ctx, c.ID, "model:3"); found {
		t.Error("a failed check counts as the last one")
	}
	if _, found, _ := s.LastFinishedCheck(ctx, c.ID, "model:4"); found {
		t.Error("a check of another model counts")
	}
}

// The last **final** check of a subject is what the lines of the approvals rest on (W4): an interim check
// that ran afterwards does not hide the verdict, and without a final one the answer stays empty.
func TestLastFinishedFinalCheck(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c := testChatRow(t, s, "final check")
	report := json.RawMessage(`{"verdict":"usable","measured_images":24,"figures":{"overall":{"accuracy":0.92}}}`)
	if _, found, err := s.LastFinishedFinalCheck(ctx, c.ID, "sha:x"); err != nil || found {
		t.Fatalf("without a check: %v %v", found, err)
	}
	interim, err := s.CreateAssessment(ctx, Assessment{ChatID: c.ID, Kind: "own_model", Subject: "sha:x", Final: false})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishAssessment(ctx, c.ID, interim.ID, AssessFinished, report, ""); err != nil {
		t.Fatal(err)
	}
	if _, found, err := s.LastFinishedFinalCheck(ctx, c.ID, "sha:x"); err != nil || found {
		t.Fatalf("an interim check is no final one: %v %v", found, err)
	}
	final, err := s.CreateAssessment(ctx, Assessment{ChatID: c.ID, Kind: "own_model", Subject: "sha:x",
		EvalDatasetID: 7, Final: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishAssessment(ctx, c.ID, final.ID, AssessFinished, report, ""); err != nil {
		t.Fatal(err)
	}
	got, found, err := s.LastFinishedFinalCheck(ctx, c.ID, "sha:x")
	if err != nil || !found || !got.Final || got.ID != final.ID {
		t.Fatalf("the final check: %+v %v %v", got, found, err)
	}
	// A second interim check afterwards does not change what the line names (W4).
	again, err := s.CreateAssessment(ctx, Assessment{ChatID: c.ID, Kind: "own_model", Subject: "sha:x", Final: false})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishAssessment(ctx, c.ID, again.ID, AssessFinished, report, ""); err != nil {
		t.Fatal(err)
	}
	if got, found, _ := s.LastFinishedFinalCheck(ctx, c.ID, "sha:x"); !found || got.ID != final.ID {
		t.Errorf("the newer interim check hides the final one: %+v", got)
	}
	// A final check of another subject and of another chat is none of this one.
	if _, found, _ := s.LastFinishedFinalCheck(ctx, c.ID, "sha:y"); found {
		t.Error("another subject counts")
	}
	other := testChatRow(t, s, "elsewhere")
	if _, found, _ := s.LastFinishedFinalCheck(ctx, other.ID, "sha:x"); found {
		t.Error("another chat counts")
	}
}

// testChatRow creates a chat for the tests of this file.
func testChatRow(t *testing.T, s *Store, title string) Chat {
	t.Helper()
	c, err := s.CreateChat(context.Background(), NewChat{Title: title, Model: "m", Variant: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

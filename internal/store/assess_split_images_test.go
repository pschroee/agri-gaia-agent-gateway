// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"testing"
)

// #260: the division of the kept-back images survives a round trip; a row written before the columns
// existed reads as 0 (unknown).
func TestRecordSplitKeepsCheckSets(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c := testChatRow(t, s, "check sets")
	if err := s.RecordSplit(ctx, Split{ChatID: c.ID, SourceDatasetID: 8, TrainDatasetID: 50,
		TrainImages: 210, KeptImages: 90, HoldoutImages: 60, ValidationImages: 30}); err != nil {
		t.Fatal(err)
	}
	sp, err := s.SplitOfTrainDataset(ctx, c.ID, 50)
	if err != nil {
		t.Fatal(err)
	}
	if sp.HoldoutImages != 60 || sp.ValidationImages != 30 || sp.KeptImages != 90 {
		t.Fatalf("round trip: %+v", sp)
	}
	// An old row: inserted without the two columns, they take their default.
	if _, err := s.pool.Exec(ctx, `INSERT INTO assess_splits (chat_id, source_dataset_id, train_dataset_id, train_images, kept_images)
VALUES ($1, 9, 51, 70, 30)`, c.ID); err != nil {
		t.Fatal(err)
	}
	old, err := s.SplitOfTrainDataset(ctx, c.ID, 51)
	if err != nil {
		t.Fatal(err)
	}
	if old.HoldoutImages != 0 || old.ValidationImages != 0 || old.KeptImages != 30 {
		t.Fatalf("old row: %+v", old)
	}
	// Recording again replaces the division together with the counts.
	if err := s.RecordSplit(ctx, Split{ChatID: c.ID, SourceDatasetID: 8, TrainDatasetID: 50,
		TrainImages: 200, KeptImages: 100, HoldoutImages: 66, ValidationImages: 34}); err != nil {
		t.Fatal(err)
	}
	if sp, _ := s.SplitOfTrainDataset(ctx, c.ID, 50); sp.HoldoutImages != 66 || sp.ValidationImages != 34 {
		t.Fatalf("replaced: %+v", sp)
	}
}

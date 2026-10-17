// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"testing"
)

// A fixture is recorded before it is used and removed by its row id; the removed row stays as
// evidence (X4.5, issue #129).
func TestFixtureLifecycle(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	f, err := s.AddFixture(ctx, NewFixture{
		Plan: "pilot", Cell: "cli-none-no-main", ManifestID: "dataset-1", Kind: FixtureDataset,
		Owner: FixturePrimary, ObjectID: "42",
		FieldSHA256: map[string]string{"name": "abc", "description": "def"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if f.ID == "" || f.ObjectID != "42" || f.RemovedAt != nil || len(f.FieldSHA256) != 2 {
		t.Fatalf("new fixture: %+v", f)
	}
	if f.FieldSHA256["description"] != "def" {
		t.Fatalf("field hashes: %+v", f.FieldSHA256)
	}
	list, err := s.ListFixtures(ctx, "pilot", "cli-none-no-main")
	if err != nil || len(list) != 1 {
		t.Fatalf("ListFixtures: %v %v", list, err)
	}
	if err := s.MarkFixtureRemoved(ctx, f.ID, "deleted"); err != nil {
		t.Fatal(err)
	}
	list, err = s.ListFixtures(ctx, "pilot", "cli-none-no-main")
	if err != nil || len(list) != 1 {
		t.Fatalf("ListFixtures after removal: %v %v", list, err)
	}
	if list[0].RemovedAt == nil || list[0].RemoveResult != "deleted" {
		t.Fatalf("removed row: %+v", list[0])
	}
	// A second removal does not touch the first record.
	if err := s.MarkFixtureRemoved(ctx, f.ID, "again"); err != ErrNotFound {
		t.Fatalf("second removal: %v", err)
	}
	// Another cell does not see the row.
	if list, err := s.ListFixtures(ctx, "pilot", "other"); err != nil || len(list) != 0 {
		t.Fatalf("other cell: %v %v", list, err)
	}
}

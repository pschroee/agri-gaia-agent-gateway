// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"errors"
	"testing"
)

// The runs of V1 belong to one chat: another chat sees nothing, and an update touches only state,
// error and ended_at (E8).
func TestAnnotationRuns(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c, err := s.CreateChat(ctx, NewChat{Title: "a", Model: "m", Variant: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.CreateChat(ctx, NewChat{Title: "b", Model: "m", Variant: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.AddAnnotationRun(ctx, AnnotationRun{
		ID: "run-1", ChatID: c.ID, DatasetID: 5, TaskID: 7, Function: "agw-test-m1-abcd1234",
		MinConfidence: 65, Mapping: `{"class_0":"0"}`, Existing: "keep"})
	if err != nil {
		t.Fatal(err)
	}
	if r.State != AnnotationQueued || r.CreatedAt.IsZero() || r.EndedAt != nil {
		t.Fatalf("new run: %+v", r)
	}
	got, err := s.AnnotationRun(ctx, c.ID, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.DatasetID != 5 || got.TaskID != 7 || got.MinConfidence != 65 || got.Mapping != `{"class_0":"0"}` || got.Existing != "keep" {
		t.Fatalf("read: %+v", got)
	}
	// A run of another chat is not found.
	if _, err := s.AnnotationRun(ctx, other.ID, "run-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another chat: %v", err)
	}
	if _, err := s.AnnotationRun(ctx, c.ID, "unknown"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown run: %v", err)
	}

	// An update writes state and error and sets ended_at with a final state.
	if err := s.UpdateAnnotationRun(ctx, c.ID, "run-1", AnnotationFailed, "boom"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.AnnotationRun(ctx, c.ID, "run-1")
	if got.State != AnnotationFailed || got.Error != "boom" || got.EndedAt == nil {
		t.Fatalf("after the update: %+v", got)
	}
	if got.Existing != "keep" || got.MinConfidence != 65 {
		t.Errorf("the update changed the start of the run: %+v", got)
	}
	// A running state leaves ended_at alone.
	if err := s.UpdateAnnotationRun(ctx, c.ID, "run-1", AnnotationStarted, ""); err != nil {
		t.Fatal(err)
	}
	got, _ = s.AnnotationRun(ctx, c.ID, "run-1")
	if got.State != AnnotationStarted || got.EndedAt == nil {
		t.Errorf("a non-final state removed ended_at: %+v", got)
	}
	// Another chat cannot update the row.
	if err := s.UpdateAnnotationRun(ctx, other.ID, "run-1", AnnotationFailed, "x"); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.AnnotationRun(ctx, c.ID, "run-1"); got.State != AnnotationStarted {
		t.Errorf("a foreign chat changed the row: %+v", got)
	}
	// Deleting the chat takes the runs with it (the table cascades).
	if _, err := s.DeleteChatAnonymised(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AnnotationRun(ctx, c.ID, "run-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after deleting the chat: %v", err)
	}
	// The final states are the ones that end a run.
	for state, want := range map[string]bool{
		AnnotationQueued: false, AnnotationStarted: false, AnnotationFinished: true,
		AnnotationFailed: true, AnnotationExpired: true,
	} {
		if got := AnnotationRunFinal(state); got != want {
			t.Errorf("%s: %v", state, got)
		}
	}
}

// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"errors"
	"testing"
)

// The jobs of V3 belong to one chat: another chat sees nothing, and progress and end are written into
// the row of this chat only (E8).
func TestAnnotationJobs(t *testing.T) {
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
	row, err := s.AddAnnotationJob(ctx, AnnotationJob{ChatID: c.ID, Kind: "calibrate", DatasetID: 5, TaskID: 7,
		Function: "agw-test-m1-abcd1234", Params: `{"mode":"calibrate"}`})
	if err != nil {
		t.Fatal(err)
	}
	if row.ID == "" || row.State != AnnotationJobQueued || row.CreatedAt.IsZero() || row.EndedAt != nil {
		t.Fatalf("new job: %+v", row)
	}
	got, err := s.AnnotationJob(ctx, c.ID, row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != "calibrate" || got.DatasetID != 5 || got.TaskID != 7 || got.Params != `{"mode":"calibrate"}` {
		t.Fatalf("read: %+v", got)
	}
	// A job of another chat is not found.
	if _, err := s.AnnotationJob(ctx, other.ID, row.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another chat: %v", err)
	}

	// The progress of the job; an end carries the result and the error.
	if err := s.SetAnnotationJobProgress(ctx, c.ID, row.ID, 10, 50); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.AnnotationJob(ctx, c.ID, row.ID); got.Done != 10 || got.Total != 50 || got.State != AnnotationJobQueued {
		t.Fatalf("progress: %+v", got)
	}
	if err := s.SetAnnotationJobState(ctx, c.ID, row.ID, AnnotationJobRunning); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.AnnotationJob(ctx, c.ID, row.ID); got.State != AnnotationJobRunning || got.EndedAt != nil {
		t.Fatalf("running: %+v", got)
	}
	if err := s.FinishAnnotationJob(ctx, c.ID, row.ID, AnnotationJobFinished, `{"images":50}`, ""); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.AnnotationJob(ctx, c.ID, row.ID); got.State != AnnotationJobFinished || got.Result != `{"images":50}` || got.EndedAt == nil {
		t.Fatalf("finished: %+v", got)
	}
	// A job that is over keeps its progress.
	if err := s.SetAnnotationJobProgress(ctx, c.ID, row.ID, 99, 99); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.AnnotationJob(ctx, c.ID, row.ID); got.Done != 10 || got.Total != 50 {
		t.Errorf("a finished job changed its progress: %+v", got)
	}
	// Another chat cannot change the row.
	if err := s.FinishAnnotationJob(ctx, other.ID, row.ID, AnnotationJobFailed, "", "x"); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.AnnotationJob(ctx, c.ID, row.ID); got.State != AnnotationJobFinished {
		t.Errorf("a foreign chat changed the row: %+v", got)
	}
	// Deleting the chat takes its jobs with it (the table cascades).
	if _, err := s.DeleteChatAnonymised(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AnnotationJob(ctx, c.ID, row.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after deleting the chat: %v", err)
	}
	for state, want := range map[string]bool{
		AnnotationJobQueued: false, AnnotationJobRunning: false,
		AnnotationJobFinished: true, AnnotationJobFailed: true,
	} {
		if got := AnnotationJobFinal(state); got != want {
			t.Errorf("%s: %v", state, got)
		}
	}
}

// A restart ends the jobs of the last process: their goroutine is gone, so a job that was queued or
// running can never end by itself and the agent would wait for it forever (V3).
func TestJobsRecover(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c, err := s.CreateChat(ctx, NewChat{Title: "a", Model: "m", Variant: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	mk := func(kind, state string) AnnotationJob {
		row, err := s.AddAnnotationJob(ctx, AnnotationJob{ChatID: c.ID, Kind: kind, DatasetID: 5, TaskID: 7, Function: "f"})
		if err != nil {
			t.Fatal(err)
		}
		if state != AnnotationJobQueued {
			if err := s.SetAnnotationJobState(ctx, c.ID, row.ID, state); err != nil {
				t.Fatal(err)
			}
		}
		return row
	}
	queued := mk("calibrate", AnnotationJobQueued)
	running := mk("fill", AnnotationJobRunning)
	finished := mk("uncertain", AnnotationJobFinished)

	n, err := s.RecoverAnnotationJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("recovered: %d, want 2", n)
	}
	for _, row := range []AnnotationJob{queued, running} {
		got, err := s.AnnotationJob(ctx, c.ID, row.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.State != AnnotationJobFailed || got.Error != AnnotationJobRecovered || got.EndedAt == nil {
			t.Errorf("job %s: %+v", row.Kind, got)
		}
	}
	if got, _ := s.AnnotationJob(ctx, c.ID, finished.ID); got.State != AnnotationJobFinished || got.Error != "" {
		t.Errorf("a finished job was touched: %+v", got)
	}
	// Nothing is left to recover afterwards.
	if n, err = s.RecoverAnnotationJobs(ctx); err != nil || n != 0 {
		t.Errorf("second run: %d %v", n, err)
	}
}

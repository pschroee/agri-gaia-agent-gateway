// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"testing"

	"agw/internal/store"
)

// The background jobs of pre-annotation (V3) ran in a goroutine of the last process: a restart ends
// every job that was queued or running, so that the agent does not wait for one forever. The row says
// why it ended.
func TestRecoverEndsAnnotationJobs(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, err := e.m.Create(ctx, NewChat{})
	if err != nil {
		t.Fatal(err)
	}
	row, err := e.st.AddAnnotationJob(ctx, store.AnnotationJob{ChatID: c.ID, Kind: "fill", DatasetID: 5, TaskID: 7, Function: "f"})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.st.SetAnnotationJobState(ctx, c.ID, row.ID, store.AnnotationJobRunning); err != nil {
		t.Fatal(err)
	}
	if err := e.m.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := e.st.AnnotationJob(ctx, c.ID, row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.AnnotationJobFailed || got.Error != store.AnnotationJobRecovered || got.EndedAt == nil {
		t.Fatalf("after the restart: %+v", got)
	}
}

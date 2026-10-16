// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"errors"
	"testing"
)

// splitRule is the rule of internal/annotation/PartForIndex, repeated here as a plain counter: the store
// keeps what the caller decides and must not know the rule itself.
func splitRule(assigned int) string {
	switch assigned % 5 {
	case 0, 1:
		return SplitCalibration
	case 2:
		return SplitValidation
	}
	return SplitHoldout
}

// The three sets of a task are recorded when a frame is first seen and never recomputed (V3b, E22): a
// frame that is annotated later joins its set without moving the frames that were already measured on.
func TestSplitAddsLaterFrames(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c, err := s.CreateChat(ctx, NewChat{Title: "a", Model: "m", Variant: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.SplitFor(ctx, c.ID, 7, []int{0, 1, 2}, splitRule)
	if err != nil {
		t.Fatal(err)
	}
	if first[0] != SplitCalibration || first[1] != SplitCalibration || first[2] != SplitValidation {
		t.Fatalf("first assignment: %v", first)
	}
	// Frames 3 and 4 are annotated later: they follow the rule at their position, and 0 to 2 keep theirs.
	second, err := s.SplitFor(ctx, c.ID, 7, []int{0, 1, 2, 3, 4, 5}, splitRule)
	if err != nil {
		t.Fatal(err)
	}
	for frame, part := range first {
		if second[frame] != part {
			t.Errorf("frame %d changed its set: %s became %s", frame, part, second[frame])
		}
	}
	if second[3] != SplitHoldout || second[4] != SplitHoldout || second[5] != SplitCalibration {
		t.Fatalf("later frames: %v", second)
	}
	// The parts can be read back one by one, sorted by frame number.
	frames, err := s.FramesWithPart(ctx, 7, SplitCalibration)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 3 || frames[0] != 0 || frames[1] != 1 || frames[2] != 5 {
		t.Errorf("frames of the calibration set: %v", frames)
	}
	// The rows belong to the chat: a deleted chat takes them with it.
	if _, err := s.pool.Exec(ctx, `DELETE FROM chats WHERE id=$1`, c.ID); err != nil {
		t.Fatal(err)
	}
	if frames, err := s.FramesWithPart(ctx, 7, SplitCalibration); err != nil || len(frames) != 0 {
		t.Errorf("the rows survived their chat: %v %v", frames, err)
	}
}

// The register keeps what a run or a fill wrote, only for its own chat and source, and the first source
// that registered an object stays the one it belongs to (V3b, E21).
func TestAnnotationObjectRegister(t *testing.T) {
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
	objs := []AnnotationObject{
		{SourceID: "run-1", ChatID: c.ID, TaskID: 7, Kind: ObjectTag, CVATID: 11, Frame: 0, LabelID: 1, Fingerprint: "a"},
		{SourceID: "run-1", ChatID: c.ID, TaskID: 7, Kind: ObjectShape, CVATID: 12, Frame: 1, LabelID: 2, Fingerprint: "b"},
	}
	if err := s.AddAnnotationObjects(ctx, objs); err != nil {
		t.Fatal(err)
	}
	// The same object registered again under another source keeps its first row: what a fill wrote stays
	// the fill's business.
	again := []AnnotationObject{{SourceID: "job-2", ChatID: c.ID, TaskID: 7, Kind: ObjectTag, CVATID: 11,
		Frame: 0, LabelID: 1, Fingerprint: "changed"}}
	if err := s.AddAnnotationObjects(ctx, again); err != nil {
		t.Fatal(err)
	}
	got, err := s.AnnotationObjects(ctx, c.ID, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Kind != ObjectTag || got[0].Fingerprint != "a" {
		t.Fatalf("register of run-1: %+v", got)
	}
	if left, err := s.AnnotationObjects(ctx, c.ID, "job-2"); err != nil || len(left) != 0 {
		t.Errorf("a second source took the object over: %+v %v", left, err)
	}
	// Another chat sees nothing (E8).
	if left, err := s.AnnotationObjects(ctx, other.ID, "run-1"); err != nil || len(left) != 0 {
		t.Errorf("the register is not bound to the chat: %+v %v", left, err)
	}
	// Taking one back removes exactly that row.
	if err := s.DeleteAnnotationObjects(ctx, c.ID, []ObjectRef{{TaskID: 7, Kind: ObjectTag, CVATID: 11}}); err != nil {
		t.Fatal(err)
	}
	got, err = s.AnnotationObjects(ctx, c.ID, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].CVATID != 12 {
		t.Fatalf("after the rollback: %+v", got)
	}
	// A rollback from another chat deletes nothing of this one.
	if err := s.DeleteAnnotationObjects(ctx, other.ID, []ObjectRef{{TaskID: 7, Kind: ObjectShape, CVATID: 12}}); err != nil {
		t.Fatal(err)
	}
	if got, err = s.AnnotationObjects(ctx, c.ID, "run-1"); err != nil || len(got) != 1 {
		t.Errorf("another chat deleted a row: %+v %v", got, err)
	}
}

// The held-out images of a task are used at most once in a chat (V3b, E22): the mark is set on the first
// call and every later one says so; another chat has its own mark.
func TestMarkFinalVerified(t *testing.T) {
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
	used, err := s.MarkFinalVerified(ctx, c.ID, 7)
	if err != nil {
		t.Fatal(err)
	}
	if used {
		t.Fatal("the first final verdict counts as already used")
	}
	if used, err = s.MarkFinalVerified(ctx, c.ID, 7); err != nil || !used {
		t.Fatalf("second final verdict: %v %v", used, err)
	}
	// Another task of the same chat is free, and so is another chat.
	if used, err = s.MarkFinalVerified(ctx, c.ID, 8); err != nil || used {
		t.Errorf("another task of the same chat: %v %v", used, err)
	}
	if used, err = s.MarkFinalVerified(ctx, other.ID, 7); err != nil || used {
		t.Errorf("another chat: %v %v", used, err)
	}
}

// LastVerifyResult reads the newest finished verification of a source; without one it says so (V3b).
func TestLastVerifyResult(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c, err := s.CreateChat(ctx, NewChat{Title: "a", Model: "m", Variant: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.LastVerifyResult(ctx, c.ID, "job-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("without a verification: %v", err)
	}
	params := `{"kind":"verify","source_id":"job-1"}`
	first, err := s.AddAnnotationJob(ctx, AnnotationJob{ChatID: c.ID, Kind: AnnotationJobVerify,
		DatasetID: 5, TaskID: 7, Function: "f", Params: params, State: AnnotationJobRunning})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishAnnotationJob(ctx, c.ID, first.ID, AnnotationJobFinished, `{"suspicious":[{"frame":3}]}`, ""); err != nil {
		t.Fatal(err)
	}
	second, err := s.AddAnnotationJob(ctx, AnnotationJob{ChatID: c.ID, Kind: AnnotationJobVerify,
		DatasetID: 5, TaskID: 7, Function: "f", Params: params, State: AnnotationJobRunning})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishAnnotationJob(ctx, c.ID, second.ID, AnnotationJobFinished, `{"suspicious":[{"frame":9}]}`, ""); err != nil {
		t.Fatal(err)
	}
	// A verification that is still running is not the answer, even when it is the newest row.
	if _, err := s.AddAnnotationJob(ctx, AnnotationJob{ChatID: c.ID, Kind: AnnotationJobVerify,
		DatasetID: 5, TaskID: 7, Function: "f", Params: params, State: AnnotationJobRunning}); err != nil {
		t.Fatal(err)
	}
	got, err := s.LastVerifyResult(ctx, c.ID, "job-1")
	if err != nil {
		t.Fatal(err)
	}
	if got != `{"suspicious":[{"frame":9}]}` {
		t.Errorf("the newest finished verification is not the answer: %s", got)
	}
	// Another source has none.
	if _, err := s.LastVerifyResult(ctx, c.ID, "job-2"); !errors.Is(err, ErrNotFound) {
		t.Errorf("another source: %v", err)
	}
	// A deleted chat takes its jobs with it.
	if _, err := s.pool.Exec(ctx, `DELETE FROM chats WHERE id=$1`, c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LastVerifyResult(ctx, c.ID, "job-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("after the chat was deleted: %v", err)
	}
}

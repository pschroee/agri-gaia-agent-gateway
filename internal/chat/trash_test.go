// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"agw/internal/store"
)

// trashChat makes an idle chat with one reply (issue #76 tests).
func trashChat(t *testing.T, e *env) string {
	t.Helper()
	c, err := e.m.Create(context.Background(), NewChat{Message: "Remember 42"})
	if err != nil {
		t.Fatal(err)
	}
	waitSettled(t, e, c.ID)
	return c.ID
}

// TestTrashStopsEverything: the chat leaves the sandbox, open approvals are rejected, plans expire, the queue
// is emptied and the sandbox returns to the pool with the reason chat_trashed.
func TestTrashStopsEverything(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	id := trashChat(t, e)
	if _, err := e.m.Resume(ctx, id); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { v, _ := e.m.View(ctx, id); return v.SlotID != "" })
	ap, err := e.st.CreateApproval(ctx, store.Approval{ChatID: id, Kind: "internet_access", Via: "cli", Name: "why", ContentType: "text/plain", PendingKey: "-"})
	if err != nil {
		t.Fatal(err)
	}
	v, err := e.m.Trash(ctx, id)
	if err != nil {
		t.Fatalf("Trash: %v", err)
	}
	if v.TrashedAt == nil || v.State != store.StateDormant || v.SlotID != "" || v.Running {
		t.Fatalf("after Trash: %+v", v)
	}
	if got, _ := e.st.GetApproval(ctx, ap.ID); got.State != store.ApprovalRejected {
		t.Fatalf("approval still %s", got.State)
	}
	if list, _ := e.m.List(ctx); len(list) != 0 {
		t.Fatalf("trashed chat in the list: %d", len(list))
	}
	if again, err := e.m.Trash(ctx, id); err != nil || again.TrashedAt == nil {
		t.Fatalf("second Trash: %+v %v", again, err)
	}
	if n := slotsOf(e, id); n != 0 {
		t.Fatalf("%d sandboxes still assigned to the chat", n)
	}
	// The session is saved: the restore brings the conversation back.
	if sess, err := e.st.LoadSession(ctx, id); err != nil || len(sess) == 0 {
		t.Fatalf("session not saved: %d bytes, %v", len(sess), err)
	}
}

// TestTrashedChatRefusesWork: no message, no resume, no flush of the queue and no upload (the agent's refusal) for a
// chat in the trash; nothing takes a slot.
func TestTrashedChatRefusesWork(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	id := trashChat(t, e)
	if _, err := e.m.Trash(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.Send(ctx, id, "hello"); !errors.Is(err, ErrTrashed) {
		t.Fatalf("Send: %v", err)
	}
	if _, err := e.m.FlushQueue(ctx, id); !errors.Is(err, ErrTrashed) {
		t.Fatalf("FlushQueue: %v", err)
	}
	if _, err := e.m.Resume(ctx, id); !errors.Is(err, ErrTrashed) {
		t.Fatalf("Resume: %v", err)
	}
	if _, _, err := e.m.ensureLive(ctx, id); !errors.Is(err, ErrTrashed) {
		t.Fatalf("ensureLive: %v", err)
	}
	res, err := e.m.Upload(ctx, id, "", "cli", "out.txt", 2, "", strings.NewReader("hi"))
	if err != nil || res.Status != "rejected" || !strings.Contains(res.Message, "chat is in the trash") {
		t.Fatalf("Upload of a trashed chat: %+v %v", res, err)
	}
	inet, err := e.m.RequestInternet(ctx, id, "", "cli", "why")
	if err != nil || inet.Status != "rejected" {
		t.Fatalf("RequestInternet of a trashed chat: %+v %v", inet, err)
	}
	if n := slotsOf(e, id); n != 0 {
		t.Fatalf("a trashed chat took %d sandboxes", n)
	}
}

// TestRestoreKeepsHistoryWithoutWake: after the restore the history is whole, the chat is dormant, and no sandbox
// starts (the UI resumes it when it is opened).
func TestRestoreKeepsHistoryWithoutWake(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	id := trashChat(t, e)
	before, _ := e.st.Messages(ctx, id)
	if _, err := e.m.Trash(ctx, id); err != nil {
		t.Fatal(err)
	}
	v, err := e.m.Restore(ctx, id)
	if err != nil || v.TrashedAt != nil || v.State != store.StateDormant || v.SlotID != "" {
		t.Fatalf("Restore: %+v %v", v, err)
	}
	after, _ := e.st.Messages(ctx, id)
	if len(after) != len(before) {
		t.Fatalf("messages %d -> %d", len(before), len(after))
	}
	if _, err := e.m.Restore(ctx, id); !errors.Is(err, ErrNotInTrash) {
		t.Fatalf("second Restore: %v", err)
	}
}

// TestDeleteOnlyFromTrash: a chat that is not in the trash cannot be deleted for good; from the trash it can, and
// its blobs go with it.
func TestDeleteOnlyFromTrash(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	id := trashChat(t, e)
	if err := e.m.DeletePermanently(ctx, id); !errors.Is(err, ErrNotInTrash) {
		t.Fatalf("delete outside the trash: %v", err)
	}
	if _, err := e.m.Trash(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := e.m.DeletePermanently(ctx, id); err != nil {
		t.Fatalf("DeletePermanently: %v", err)
	}
	if _, err := e.st.GetChat(ctx, id); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("chat still there: %v", err)
	}
}

// TestDeleteRemovesBlobs: the artifact of a deleted chat is gone from the object store; a failing delete is only
// counted, not an error.
func TestDeleteRemovesBlobs(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	id := trashChat(t, e)
	key := objectKey(id, store.KindOutput, "report.txt")
	if err := e.blobs.Put(ctx, key, strings.NewReader("data"), 4, "text/plain"); err != nil {
		t.Fatal(err)
	}
	if err := e.st.PutArtifact(ctx, store.Artifact{ChatID: id, Kind: store.KindOutput, Name: "report.txt", Size: 4,
		SHA256: "x", Via: "cli", ObjectKey: key}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.Trash(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := e.m.DeletePermanently(ctx, id); err != nil {
		t.Fatal(err)
	}
	for _, k := range e.blobs.keys() {
		if k == key {
			t.Fatal("artifact blob still in the object store")
		}
	}
}

// TestEmptyTrashOnlyTrashed: the trash is emptied, the chats in the history stay.
func TestEmptyTrashOnlyTrashed(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	a := trashChat(t, e)
	b := trashChat(t, e)
	keep := trashChat(t, e)
	for _, id := range []string{a, b} {
		if _, err := e.m.Trash(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	n, err := e.m.EmptyTrash(ctx, nil)
	if err != nil || n != 2 {
		t.Fatalf("EmptyTrash: %d %v", n, err)
	}
	if items, _ := e.m.TrashList(ctx, nil); len(items) != 0 {
		t.Fatalf("trash not empty: %d", len(items))
	}
	if _, err := e.st.GetChat(ctx, keep); err != nil {
		t.Fatalf("chat in the history deleted: %v", err)
	}
}

// TestTrashRetention: with a retention, only the chats that were in the trash longer than the retention go; with 0
// nothing is ever deleted.
func TestTrashRetention(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	old := trashChat(t, e)
	recent := trashChat(t, e)
	if _, err := e.st.MoveToTrash(ctx, old, time.Now().Add(-72*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.MoveToTrash(ctx, recent, time.Now()); err != nil {
		t.Fatal(err)
	}
	e.m.opt.TrashRetentionDays = 0
	e.m.RunTrashRetention(ctx) // off: returns at once
	if _, err := e.st.GetChat(ctx, old); err != nil {
		t.Fatalf("retention off deleted a chat: %v", err)
	}
	e.m.opt.TrashRetentionDays = 2
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); e.m.RunTrashRetention(rctx) }()
	waitFor(t, func() bool { _, err := e.st.GetChat(ctx, old); return errors.Is(err, store.ErrNotFound) })
	cancel()
	<-done
	if _, err := e.st.GetChat(ctx, recent); err != nil {
		t.Fatalf("recent chat deleted by the retention: %v", err)
	}
}

// TestTrashStopsRunningTurnTaskAndPlan: a chat that is working (a turn in progress, a background task, a plan
// waiting for a decision) stops in every part when it moves to the trash (issue #76, phase B).
func TestTrashStopsRunningTurnTaskAndPlan(t *testing.T) {
	e := setup(t)
	ctx := store.WithSession(context.Background(), "main")
	id, a, release := busyChat(t, e)
	defer release()
	if v, _ := e.m.View(ctx, id); !v.Running {
		t.Fatalf("the turn is not running: %+v", v)
	}
	task, err := e.st.CreateBackgroundTask(ctx, store.BackgroundTask{ChatID: id, SlotID: "s1", Command: "sleep 600"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	proposed, err := e.st.CreatePlan(ctx, store.Plan{ChatID: id, Title: "p", Session: "main", Via: "cli", Steps: []store.PlanStep{
		{Idx: 1, Method: "DELETE", Path: "/models/5", Effect: "irreversible"}}})
	if err != nil {
		t.Fatal(err)
	}
	approved := approvedPlan(t, e, ctx, id, planTwo)
	v, err := e.m.Trash(ctx, id)
	if err != nil {
		t.Fatalf("Trash: %v", err)
	}
	if v.TrashedAt == nil || v.Running || v.SlotID != "" {
		t.Fatalf("after Trash: %+v", v)
	}
	a.mu.Lock()
	aborted := strings.Contains(fmt.Sprint(a.cmds), "abort")
	a.mu.Unlock()
	if !aborted {
		t.Fatal("the running turn was not aborted")
	}
	got, err := e.st.GetBackgroundTask(ctx, id, task.Seq)
	if err != nil || got.State != store.BgStopped {
		t.Fatalf("background task after Trash: %+v %v", got, err)
	}
	for _, pid := range []string{proposed.ID, approved.ID} {
		if p, _ := e.st.GetPlan(ctx, pid); p.State != "expired" {
			t.Fatalf("plan %s after Trash: %s", pid, p.State)
		}
	}
	if n := slotsOf(e, id); n != 0 {
		t.Fatalf("%d sandboxes still assigned", n)
	}
}

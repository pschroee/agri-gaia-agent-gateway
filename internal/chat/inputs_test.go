// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"errors"
	"testing"

	"agw/internal/store"
)

// sentAt reads an input's sent_at (nil while the file is only staged in the field).
func sentAt(t *testing.T, e *env, chatID, name string) *string {
	t.Helper()
	a, err := e.st.GetArtifact(context.Background(), chatID, store.KindInput, name)
	if err != nil {
		t.Fatalf("input %q: %v", name, err)
	}
	if a.SentAt == nil {
		return nil
	}
	s := a.SentAt.Format("2006-01-02T15:04:05Z07:00")
	return &s
}

// L4/#121: taking back an input removes the row, the file in the object store and the mirror in the sandbox, and
// the chat's subscribers get artifact_removed.
func TestRemoveInputDeletesBlobRowAndFile(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, _ := e.m.Create(ctx, NewChat{})
	events, cancel := e.m.Subscribe(c.ID)
	defer cancel()
	if _, err := e.m.AddInput(ctx, c.ID, "data.csv", []byte("x,y\n")); err != nil {
		t.Fatal(err)
	}
	a := e.agent(0)
	if string(a.file(inputsDir+"/data.csv")) != "x,y\n" {
		t.Fatalf("input not mirrored: %v", a.files)
	}
	if len(e.blobs.keys()) != 1 {
		t.Fatalf("upload not stored: %v", e.blobs.keys())
	}
	if err := e.m.RemoveInput(ctx, c.ID, "data.csv"); err != nil {
		t.Fatal(err)
	}
	if arts, _ := e.st.ListArtifacts(ctx, c.ID); len(arts) != 0 {
		t.Fatalf("row still there: %+v", arts)
	}
	if len(e.blobs.keys()) != 0 {
		t.Fatalf("file still in the object store: %v", e.blobs.keys())
	}
	if got := a.file(inputsDir + "/data.csv"); got != nil {
		t.Fatalf("mirror still in the sandbox: %q", got)
	}
	ev := waitEvent(t, events, "artifact_removed", "")
	d, ok := ev.Data.(map[string]string)
	if !ok || d["kind"] != store.KindInput || d["name"] != "data.csv" {
		t.Fatalf("artifact_removed: %+v", ev.Data)
	}
	// an unknown name and a name that is no plain file name are refused
	if err := e.m.RemoveInput(ctx, c.ID, "gone.csv"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown input: %v", err)
	}
	if err := e.m.RemoveInput(ctx, c.ID, "../data.csv"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid name: %v", err)
	}
	// and nothing happened to the other inputs
	if _, err := e.m.AddInput(ctx, c.ID, "keep.csv", []byte("k\n")); err != nil {
		t.Fatal(err)
	}
	if err := e.m.RemoveInput(ctx, c.ID, "gone.csv"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown input after a second upload: %v", err)
	}
	if arts, _ := e.st.ListArtifacts(ctx, c.ID); len(arts) != 1 || arts[0].Name != "keep.csv" {
		t.Fatalf("the wrong input: %+v", arts)
	}
}

// L4/#121: a file that went with a message belongs to the history — taking it back is refused and it stays.
func TestRemoveInputSentRefused(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, _ := e.m.Create(ctx, NewChat{})
	if _, err := e.m.AddInput(ctx, c.ID, "data.csv", []byte("x,y\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.SendWithAttachments(ctx, c.ID, "Have a look", []string{"data.csv"}); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, e, c.ID)
	if sentAt(t, e, c.ID, "data.csv") == nil {
		t.Fatal("the attachment was not marked as sent")
	}
	if err := e.m.RemoveInput(ctx, c.ID, "data.csv"); !errors.Is(err, ErrAlreadySent) {
		t.Fatalf("sent input removable: %v", err)
	}
	if arts, _ := e.st.ListArtifacts(ctx, c.ID); len(arts) != 1 {
		t.Fatalf("the sent input is gone: %+v", arts)
	}
	if len(e.blobs.keys()) != 1 {
		t.Fatalf("the file is gone from the object store: %v", e.blobs.keys())
	}
}

// L4/#121: sending marks the attachments as sent (also when the message goes to the queue first).
func TestSendMarksInputsSent(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, _ := e.m.Create(ctx, NewChat{})
	if _, err := e.m.AddInput(ctx, c.ID, "data.csv", []byte("x,y\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.AddInput(ctx, c.ID, "rest.csv", []byte("z\n")); err != nil {
		t.Fatal(err)
	}
	if got := sentAt(t, e, c.ID, "data.csv"); got != nil {
		t.Fatalf("uploaded input already marked: %v", *got)
	}
	if _, err := e.m.SendWithAttachments(ctx, c.ID, "Have a look", []string{"data.csv"}); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, e, c.ID)
	if sentAt(t, e, c.ID, "data.csv") == nil {
		t.Fatal("sent attachment not marked")
	}
	// the file that did not go along stays removable
	if got := sentAt(t, e, c.ID, "rest.csv"); got != nil {
		t.Fatalf("unattached input marked: %v", *got)
	}
	if err := e.m.RemoveInput(ctx, c.ID, "rest.csv"); err != nil {
		t.Fatalf("unattached input not removable: %v", err)
	}
}

// L4/#121: a queued message counts as sent — it is handed to pi later, and the file must not disappear meanwhile.
func TestQueuedMarksInputsSent(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	id, _, release := busyChat(t, e)
	if _, err := e.m.AddInput(ctx, id, "data.csv", []byte("x\n")); err != nil {
		t.Fatal(err)
	}
	r, err := e.m.SendWithAttachments(ctx, id, "two", []string{"data.csv"})
	if err != nil || !r.Queued {
		t.Fatalf("not queued: %+v %v", r, err)
	}
	if sentAt(t, e, id, "data.csv") == nil {
		t.Fatal("the attachment of a queued message was not marked as sent")
	}
	if err := e.m.RemoveInput(ctx, id, "data.csv"); !errors.Is(err, ErrAlreadySent) {
		t.Fatalf("attachment of a queued message removable: %v", err)
	}
	release()
}

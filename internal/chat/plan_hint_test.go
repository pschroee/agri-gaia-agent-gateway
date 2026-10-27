// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"strings"
	"testing"
	"time"

	"agw/internal/platform"
	"agw/internal/store"
)

// singleApproved sends one writing call and approves its card; it returns the result of the call.
func singleApproved(t *testing.T, e *env, ctx context.Context, chatID, slot string) platform.Result {
	t.Helper()
	events, cancel := e.m.Subscribe(chatID)
	defer cancel()
	done := make(chan platform.Result, 1)
	go func() {
		r, _ := e.m.PlatformCall(ctx, chatID, slot, "cli", liveCall(t, "PATCH", "/models/42", renameStepBody))
		done <- r
	}()
	ap := waitEvent(t, events, "approval", "").Data.(store.Approval)
	for ap.State != store.ApprovalPending {
		ap = waitEvent(t, events, "approval", "").Data.(store.Approval)
	}
	if ap.Kind != "platform_write" {
		t.Fatalf("approval kind %q", ap.Kind)
	}
	if _, err := e.m.Decide(ctx, ap.ID, true); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("call did not finish")
	}
	return platform.Result{}
}

// The first single approval gets no hint, the second does, the third within 30 minutes does not (#254, E2).
func TestSecondSingleApprovalHintsPlan(t *testing.T) {
	e := setup(t)
	withRunSummaryPlatform(t, e, nil)
	ctx := store.WithSession(context.Background(), "main")
	c, _ := e.m.Create(ctx, NewChat{})
	slot := e.m.live[c.ID].slot.ID
	if r := singleApproved(t, e, ctx, c.ID, slot); r.Status != "ok" || strings.Contains(r.Note, secondApprovalNote) {
		t.Fatalf("first approval: %+v", r)
	}
	if r := singleApproved(t, e, ctx, c.ID, slot); r.Status != "ok" || !strings.Contains(r.Note, "second single approval in a row") {
		t.Fatalf("second approval: %+v", r)
	}
	if r := singleApproved(t, e, ctx, c.ID, slot); r.Status != "ok" || strings.Contains(r.Note, secondApprovalNote) {
		t.Fatalf("third approval within 30 minutes: %+v", r)
	}
}

// A step of an approved plan never gets the hint, also not after two single approvals (#254).
func TestNoPlanHintInsidePlan(t *testing.T) {
	e := setup(t)
	withRunSummaryPlatform(t, e, nil)
	ctx := store.WithSession(context.Background(), "main")
	c, _ := e.m.Create(ctx, NewChat{})
	slot := e.m.live[c.ID].slot.ID
	singleApproved(t, e, ctx, c.ID, slot)
	e.m.planHintMu.Lock()
	e.m.planHintAt = map[string]time.Time{} // the hint of a second approval is not under test here
	e.m.planHintMu.Unlock()
	if r, _ := proposeAndDecide(t, e, ctx, c.ID, slot, planCover, true); r.Status != "ok" {
		t.Fatalf("plan: %+v", r)
	}
	r, err := e.m.PlatformCall(ctx, c.ID, slot, "cli", liveCall(t, "GET", "/train/containers/7/model", ""))
	if err != nil || r.Status != "ok" || strings.Contains(r.Note, secondApprovalNote) {
		t.Fatalf("step of a plan: %+v %v", r, err)
	}
}

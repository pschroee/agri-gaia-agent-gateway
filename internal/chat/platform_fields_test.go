// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"agw/internal/platform"
	"agw/internal/store"
)

// TestFieldRulesWithoutDelegation: a chat without delegation (the case on the instance) must still refuse
// a body with a field the platform would take over (issue #65), in every binding, before any approval.
func TestFieldRulesWithoutDelegation(t *testing.T) {
	e := setup(t)
	got := withPlatform(t, e)
	ctx := context.Background()
	c, _ := e.m.Create(ctx, NewChat{})
	slot := e.m.live[c.ID].slot.ID
	events, cancel := e.m.Subscribe(c.ID)
	defer cancel()

	bad, err := platform.Normalize(platform.Request{Method: "PATCH", Path: "/datasets/1", Body: json.RawMessage(`{"bucket_name":"x"}`)})
	if err != nil {
		t.Fatal(err)
	}
	for _, via := range []string{"cli", "mcp", "api"} {
		r, err := e.m.PlatformCall(ctx, c.ID, slot, via, bad)
		if err != nil || r.Status != "denied" || !strings.Contains(r.Message, "bucket_name") {
			t.Fatalf("%s: %+v %v", via, r, err)
		}
	}
	if n := len(got()); n != 0 {
		t.Fatalf("refused call reached the platform: %v", got())
	}
	if aps, _ := e.st.ListApprovals(ctx, "", c.ID); len(aps) != 0 {
		t.Fatalf("refused call created an approval: %+v", aps)
	}

	// The allowed field still goes through the approval.
	good, err := platform.Normalize(platform.Request{Method: "PATCH", Path: "/datasets/1", Body: json.RawMessage(`{"name":"x"}`)})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan platform.Result, 1)
	go func() {
		r, _ := e.m.PlatformCall(ctx, c.ID, slot, "cli", good)
		done <- r
	}()
	ap := waitEvent(t, events, "approval", "").Data.(store.Approval)
	for ap.State != store.ApprovalPending { // skip the event of an earlier decision
		ap = waitEvent(t, events, "approval", "").Data.(store.Approval)
	}
	if ap.Kind != "platform_write" || ap.Name != "PATCH /datasets/1" {
		t.Fatalf("approval: %+v", ap)
	}
	if _, err := e.m.Decide(ctx, ap.ID, true); err != nil {
		t.Fatal(err)
	}
	if r := <-done; r.Status != "ok" || len(got()) != 1 {
		t.Fatalf("allowed field after approval: %+v %v", r, got())
	}
}

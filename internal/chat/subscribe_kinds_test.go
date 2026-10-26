// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"testing"

	"agw/internal/store"
)

// SubscribeKinds("plan") gets the plan events of every chat and nothing of crossChatKinds; SubscribeAll gets the
// approvals but no plan (issue #223).
func TestSubscribeKindsFilters(t *testing.T) {
	m := NewManager(nil, nil, nil, nil, nil, Options{})
	plans, cancelPlans := m.SubscribeKinds("plan")
	defer cancelPlans()
	all, cancelAll := m.SubscribeAll()
	defer cancelAll()

	m.publish("c1", Event{Kind: "plan", Data: store.Plan{ID: "p1", ChatID: "c1"}})
	m.publish("c2", Event{Kind: "approval", Data: store.Approval{ID: "a1", ChatID: "c2"}})
	m.publish("c1", Event{Kind: "pi", Data: "x"})

	if len(plans) != 1 {
		t.Fatalf("plan subscriber got %d events, want 1", len(plans))
	}
	if ev := <-plans; ev.ChatID != "c1" || ev.Kind != "plan" {
		t.Fatalf("plan subscriber got %+v", ev)
	}
	if len(all) != 1 {
		t.Fatalf("SubscribeAll got %d events, want only the approval", len(all))
	}
	if ev := <-all; ev.Kind != "approval" {
		t.Fatalf("SubscribeAll got %+v", ev)
	}

	cancelPlans()
	m.publish("c1", Event{Kind: "plan", Data: store.Plan{ID: "p2"}})
	if len(plans) != 0 {
		t.Fatal("event after cancel")
	}
}

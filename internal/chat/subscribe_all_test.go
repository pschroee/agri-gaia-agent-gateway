// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"testing"

	"agw/internal/store"
)

// SubscribeAll (issue #32) gets approval events of every chat with the chat's ID, nothing else, never
// blocks the publisher, and nothing more after its cancel.
func TestSubscribeAll(t *testing.T) {
	m := NewManager(nil, nil, nil, nil, nil, Options{})
	all, cancel := m.SubscribeAll()
	one, cancelOne := m.Subscribe("c1")
	defer cancelOne()

	m.publish("c1", Event{Kind: "pi", Data: "x"})
	m.publish("c1", Event{Kind: "chat", Data: "x"})
	m.publish("c2", Event{Kind: "approval", Data: store.Approval{ID: "a2", ChatID: "c2"}})
	m.publish("c1", Event{Kind: "approval", Data: store.Approval{ID: "a1", ChatID: "c1"}})
	for _, want := range []string{"c2", "c1"} {
		select {
		case ev := <-all:
			if ev.ChatID != want || ev.Kind != "approval" {
				t.Fatalf("got %+v, want approval of %s", ev, want)
			}
		default:
			t.Fatalf("no event for %s", want)
		}
	}
	select {
	case ev := <-all:
		t.Fatalf("unexpected %+v", ev)
	default:
	}
	if n := len(one); n != 3 {
		t.Fatalf("subscriber of c1 got %d events, want 3", n)
	}

	// A reader that does not read: the publisher goes on, the newest approval is kept.
	for i := 0; i < 1000; i++ {
		m.publish("c3", Event{Kind: "approval", Data: store.Approval{ID: "x"}})
	}
	m.publish("c3", Event{Kind: "approval", Data: store.Approval{ID: "last"}})
	var last ChatEvent
	for len(all) > 0 {
		last = <-all
	}
	if a, _ := last.Data.(store.Approval); a.ID != "last" {
		t.Fatalf("newest approval dropped: %+v", last)
	}

	cancel()
	m.publish("c1", Event{Kind: "approval", Data: store.Approval{ID: "after"}})
	select {
	case ev := <-all:
		t.Fatalf("event after cancel: %+v", ev)
	default:
	}
	m.mu.Lock()
	n := len(m.allSubs)
	m.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d subscribers left after cancel", n)
	}
}

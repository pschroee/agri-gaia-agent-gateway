// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"testing"
	"time"
)

// A command of an extension (/todos) is answered by pi without a run: no agent_start, no
// agent_settled. The chat must not stay "working" for ever (live bug of 12.10.2026: queued messages
// were not delivered and the abort changed nothing).
func TestExtensionCommandDoesNotLeaveChatRunning(t *testing.T) {
	old := settleDelay.Swap(int64(100 * time.Millisecond))
	defer settleDelay.Store(old)
	e := setup(t)
	ctx := context.Background()
	c, err := e.m.Create(ctx, NewChat{Title: "ext"})
	if err != nil {
		t.Fatal(err)
	}
	a := e.agent(0)
	if _, err := e.m.Send(ctx, c.ID, "/todos"); err != nil {
		t.Fatal(err)
	}
	// While pi has not answered yet the chat counts as running: this message is queued ...
	if r, err := e.m.Send(ctx, c.ID, "/skill:charts"); err != nil || !r.Queued {
		t.Fatalf("queued: %+v %v", r, err)
	}
	// ... and goes to pi once the flag is taken back.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(a.prompts()) >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := a.prompts(); len(got) != 2 || got[1] != "/skill:charts" {
		t.Fatalf("prompts: %q", got)
	}
	waitSettled(t, e, c.ID)
	if v, _ := e.m.View(ctx, c.ID); v.Running || v.Queued != 0 {
		t.Fatalf("after: %+v", v)
	}
}

// Abort on a chat whose run flag is stuck takes the flag back (the queue stays held).
func TestAbortResetsStuckRunFlag(t *testing.T) {
	old := settleDelay.Swap(int64(100 * time.Millisecond))
	defer settleDelay.Store(old)
	e := setup(t)
	ctx := context.Background()
	c, err := e.m.Create(ctx, NewChat{Title: "ext2"})
	if err != nil {
		t.Fatal(err)
	}
	e.agent(0)
	l := func() *live { e.m.mu.Lock(); defer e.m.mu.Unlock(); return e.m.live[c.ID] }
	if _, err := e.m.Send(ctx, c.ID, "hello"); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, e, c.ID)
	time.Sleep(1100 * time.Millisecond) // a start less than a second ago counts as a live run
	lv := l()
	e.m.setRunning(lv, true) // stuck: pi is idle, the orchestrator thinks it works
	if v, _ := e.m.View(ctx, c.ID); !v.Running {
		t.Fatal("precondition: running")
	}
	if _, err := e.m.Abort(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if v, _ := e.m.View(ctx, c.ID); !v.Running {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("run flag not reset after abort")
}

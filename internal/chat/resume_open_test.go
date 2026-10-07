// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"strings"
	"testing"
	"time"

	"agw/internal/store"
)

// idleChat creates a chat with one answered message and lets it idle.
func idleChat(t *testing.T, e *env) string {
	t.Helper()
	ctx := context.Background()
	c, err := e.m.Create(ctx, NewChat{Message: "Remember 42"})
	if err != nil {
		t.Fatal(err)
	}
	waitSettled(t, e, c.ID)
	if _, err := e.m.Suspend(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	return c.ID
}

// wakeSteps collects resume steps until ready or failed.
func wakeSteps(t *testing.T, events <-chan Event) []ResumeStep {
	t.Helper()
	var steps []ResumeStep
	for {
		s := waitEvent(t, events, "resume", "").Data.(ResumeStep)
		steps = append(steps, s)
		if s.Phase == PhaseReady || s.Phase == PhaseFailed {
			return steps
		}
	}
}

func stepNames(steps []ResumeStep) string {
	var out []string
	for _, s := range steps {
		out = append(out, s.Phase+":"+s.Status)
	}
	return strings.Join(out, ",")
}

// waitWoken waits until the chat is active and no longer resuming.
func waitWoken(t *testing.T, e *env, id string) ChatView {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		v, _ := e.m.View(context.Background(), id)
		if v.State == store.StateActive && v.SlotID != "" && !v.Resuming {
			return v
		}
		if time.Now().After(deadline) {
			t.Fatalf("chat not woken: %+v", v)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// takeWarmSlot holds the start of new sandboxes and gives the warm slot to another chat, so that a wake
// waits in "acquire" until the returned function lets sandboxes start.
func takeWarmSlot(t *testing.T, e *env) func() {
	t.Helper()
	waitIdleSlot(t, e)
	release := e.holdStarts(t)
	if _, err := e.m.Create(context.Background(), NewChat{}); err != nil {
		t.Fatal(err)
	}
	return release
}

func slotsOf(e *env, id string) int {
	n := 0
	for _, s := range e.p.Snapshot() {
		if s.ChatID == id {
			n++
		}
	}
	return n
}

// Issue #31: waking returns at once with resuming set, reports the steps of a resume once, and a second
// call during the wake and a call on the woken chat change nothing.
func TestResumeOnOpenTwiceResumesOnce(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	id := idleChat(t, e)
	events, cancel := e.m.Subscribe(id)
	defer cancel()
	release := takeWarmSlot(t, e) // keep the wake in "acquire" until both calls are made
	v, err := e.m.Resume(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !v.Resuming || v.State != store.StateDormant {
		t.Fatalf("first wake: %+v", v)
	}
	v2, err := e.m.Resume(ctx, id)
	if err != nil || !v2.Resuming {
		t.Fatalf("second wake: %+v %v", v2, err)
	}
	release()
	steps := wakeSteps(t, events)
	want := "acquire:running,acquire:done,session:running,session:done,settings:running,settings:done," +
		"workspace:running,workspace:done,inputs:running,inputs:done,ready:done"
	if got := stepNames(steps); got != want {
		t.Fatalf("steps:\n%s\nwant\n%s", got, want)
	}
	for _, s := range steps {
		if s.Start || s.ID != steps[0].ID {
			t.Fatalf("not one resume of an idle chat: %+v", s)
		}
	}
	waitWoken(t, e, id)
	// waking the active chat: nothing happens
	v3, err := e.m.Resume(ctx, id)
	if err != nil || v3.Resuming || v3.SlotID == "" {
		t.Fatalf("wake of an active chat: %+v %v", v3, err)
	}
	time.Sleep(100 * time.Millisecond) // negative check: no second resume
	for {
		select {
		case ev := <-events:
			if ev.Kind == "resume" {
				t.Fatalf("second resume: %+v", ev.Data)
			}
			continue
		default:
		}
		break
	}
	if n := slotsOf(e, id); n != 1 {
		t.Fatalf("chat holds %d slots", n)
	}
	// the woken chat answers and still knows its session
	if res, err := e.m.Send(ctx, id, "What was the number?"); err != nil || res.Resumed {
		t.Fatalf("send after wake: %+v %v", res, err)
	}
	waitSettled(t, e, id)
}

// Waking a chat whose agent is working changes nothing: no resume, same sandbox, still running.
func TestResumeOnOpenRunningChat(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	id, _, release := busyChat(t, e)
	before, _ := e.m.View(ctx, id)
	events, cancel := e.m.Subscribe(id)
	defer cancel()
	v, err := e.m.Resume(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !v.Running || v.Resuming || v.SlotID != before.SlotID {
		t.Fatalf("wake of a running chat: %+v (before %+v)", v, before)
	}
	time.Sleep(100 * time.Millisecond) // negative check
	for {
		select {
		case ev := <-events:
			if ev.Kind == "resume" {
				t.Fatalf("resume of a running chat: %+v", ev.Data)
			}
			continue
		default:
		}
		break
	}
	release()
	waitSettled(t, e, id)
}

// A message sent while the chat wakes waits for the wake and goes out once; the chat takes one slot.
func TestResumeOnOpenWithMessageMeanwhile(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	id := idleChat(t, e)
	events, cancel := e.m.Subscribe(id)
	defer cancel()
	release := takeWarmSlot(t, e)
	if _, err := e.m.Resume(ctx, id); err != nil {
		t.Fatal(err)
	}
	sent := make(chan error, 1)
	go func() {
		res, err := e.m.Send(ctx, id, "typed while waking")
		if err == nil && res.Queued {
			t.Errorf("message queued instead of waiting: %+v", res)
		}
		sent <- err
	}()
	time.Sleep(50 * time.Millisecond) // negative check: the message must not get through yet
	select {
	case err := <-sent:
		t.Fatalf("message went out before the sandbox was there: %v", err)
	default:
	}
	release()
	if steps := wakeSteps(t, events); steps[len(steps)-1].Phase != PhaseReady {
		t.Fatalf("wake: %s", stepNames(steps))
	}
	if err := <-sent; err != nil {
		t.Fatal(err)
	}
	waitSettled(t, e, id)
	msgs, _ := e.st.Messages(ctx, id)
	n := 0
	for _, m := range msgs {
		if m.Role == "user" {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("user messages: %d", n)
	}
	if n := slotsOf(e, id); n != 1 {
		t.Fatalf("chat holds %d slots", n)
	}
}

// A failed wake reports failed and leaves the chat idle; waking again tries again.
func TestResumeOnOpenFailsAndRetries(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	id := idleChat(t, e)
	e.mu.Lock()
	e.failSwitch = true
	for _, a := range e.agents {
		a.mu.Lock()
		a.failSwitch = true
		a.mu.Unlock()
	}
	e.mu.Unlock()
	events, cancel := e.m.Subscribe(id)
	defer cancel()
	if _, err := e.m.Resume(ctx, id); err != nil {
		t.Fatal(err)
	}
	if steps := wakeSteps(t, events); steps[len(steps)-1].Phase != PhaseFailed {
		t.Fatalf("wake should fail: %s", stepNames(steps))
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		v, _ := e.m.View(ctx, id)
		if v.State == store.StateDormant && !v.Resuming && v.SlotID == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after the failed wake: %+v", v)
		}
		time.Sleep(5 * time.Millisecond)
	}
	e.mu.Lock()
	e.failSwitch = false
	for _, a := range e.agents {
		a.mu.Lock()
		a.failSwitch = false
		a.mu.Unlock()
	}
	e.mu.Unlock()
	if _, err := e.m.Resume(ctx, id); err != nil {
		t.Fatal(err)
	}
	if steps := wakeSteps(t, events); steps[len(steps)-1].Phase != PhaseReady {
		t.Fatalf("retry: %s", stepNames(steps))
	}
	waitWoken(t, e, id)
}

func TestResumeOnOpenUnknownChat(t *testing.T) {
	e := setup(t)
	if _, err := e.m.Resume(context.Background(), "00000000-0000-0000-0000-000000000000"); err == nil {
		t.Fatal("wake of an unknown chat succeeded")
	}
}

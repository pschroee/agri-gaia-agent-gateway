// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"testing"
	"time"

	"agw/internal/store"
)

// turnReason returns the abort reason the newest stored message carries (empty when its turn has none).
func turnReason(t *testing.T, e *env, id string) string {
	t.Helper()
	msgs, err := e.st.Messages(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) == 0 {
		return ""
	}
	return msgs[len(msgs)-1].AbortReason
}

// waitRunning waits until the manager counts the chat as running (the fake agent reports its run in a goroutine).
func waitRunning(t *testing.T, e *env, id string) {
	t.Helper()
	waitFor(t, func() bool {
		v, err := e.m.View(context.Background(), id)
		return err == nil && v.Running
	})
}

// waitReported waits until pi has reported the chat's user message. The reason of an abort travels with the stored
// messages (a join on chat_turns), so a test that probes it needs that message; and an abort that tears the sandbox
// down (the trash, the time limit) would otherwise end before the message exists, leaving only the turn behind.
func waitReported(t *testing.T, e *env, id string) {
	t.Helper()
	waitFor(t, func() bool {
		msgs, err := e.st.Messages(context.Background(), id)
		return err == nil && len(msgs) > 0 && msgs[0].Role == "user"
	})
}

// The user's stop is recorded on the turn, so the UI says "stopped by you" only when the user really stopped
// (issue #120, L3, F9).
func TestAbortReasonUser(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	id, _, release := busyChat(t, e)
	waitRunning(t, e, id)
	if _, err := e.m.Abort(ctx, id); err != nil {
		t.Fatal(err)
	}
	// The reason stands on the turn right away, but the message that carries it is written once pi's stream
	// is stored — so it is waited for rather than read on the spot.
	waitFor(t, func() bool { return turnReason(t, e, id) == AbortUser })
	release()
	waitSettled(t, e, id)
	if got := turnReason(t, e, id); got != AbortUser {
		t.Fatalf("abort reason after the run: %q", got)
	}
}

// Moving a chat to the trash also ends its running turn, but not because the user stopped it (issue #120, L3).
func TestAbortReasonTrash(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	id, _, release := busyChat(t, e)
	defer release()
	waitRunning(t, e, id)
	waitReported(t, e, id)
	if _, err := e.m.Trash(ctx, id); err != nil {
		t.Fatal(err)
	}
	// The message that carries the reason is stored by now; still waited for, so the probe depends on no timing.
	waitFor(t, func() bool { return turnReason(t, e, id) == AbortTrash })
}

// A turn that runs longer than maxRunTime is aborted by the time limit (issue #120, L3). The 30 minutes are not
// waited out: the start of the run is moved back and the idle timer fires at once.
func TestAbortReasonTimeLimit(t *testing.T) {
	e := setup(t)
	id, _, release := busyChat(t, e)
	defer release()
	waitRunning(t, e, id)
	// The timer below suspends the chat right after the abort, which ends pi's stream: without the message the
	// reason could never be read back. Waited for, so the abort meets a turn pi has reported.
	waitReported(t, e, id)
	e.m.opt.IdleTimeout = time.Minute
	l := e.m.liveOf(id)
	if l == nil {
		t.Fatal("chat is not live")
	}
	e.m.mu.Lock()
	l.runningSince = time.Now().Add(-maxRunTime - time.Minute)
	e.m.mu.Unlock()
	e.m.armIdleAfter(id, l, time.Millisecond)
	waitFor(t, func() bool { return turnReason(t, e, id) == AbortTimeLimit })
	if v, _ := e.m.View(context.Background(), id); v.Running {
		t.Fatal("the chat still runs after the time limit")
	}
}

// Too many turns in a row without the user: the turn from pi is aborted and says why (issue #120, L3; Review 3, H2).
func TestAbortReasonTurnLimit(t *testing.T) {
	e := setup(t)
	e.m.opt.AutoTurnsMax = 1
	ctx := context.Background()
	c, err := e.m.Create(ctx, NewChat{Title: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.Send(ctx, c.ID, "go"); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, e, c.ID)
	a := e.agent(0)
	a.piRun("first note")
	waitSettled(t, e, c.ID)
	a.piRun("second note")
	waitFor(t, func() bool { return turnReason(t, e, c.ID) == AbortTurnLimit })
}

// The subagent limit ends the running turn and marks every run that met it (issue #120, L3, DL3.4): the steps of
// those runs say "stopped (subagent limit)" instead of "stopped by you".
func TestAbortReasonSubagentLimitMarksRuns(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	id, _, release := busyChat(t, e)
	defer release()
	waitRunning(t, e, id)
	l := e.m.liveOf(id)
	if l == nil {
		t.Fatal("chat is not live")
	}
	for _, run := range []string{"run-1", "run-2"} {
		if _, err := e.st.UpsertSubagentRun(ctx, store.SubagentRun{ChatID: id, RunID: run, Agent: "worker", PiRunID: "pi-" + run, State: "running"}); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	e.m.mu.Lock()
	l.maxSub = 1
	e.m.mu.Unlock()
	l.pollMu.Lock()
	l.track("run-1", now).State = "running"
	l.track("run-2", now).State = "running"
	l.pollMu.Unlock()
	if changed := e.m.enforceSubagentLimit(id, l, now); !changed {
		t.Fatal("the limit did not fire")
	}
	// The reason stands on the turn right away; the message that carries it is written once pi's stream is
	// stored, so it is waited for rather than read on the spot.
	waitFor(t, func() bool { return turnReason(t, e, id) == AbortSubagentLimit })
	for _, run := range []string{"run-1", "run-2"} {
		rec, err := e.st.SubagentRunOf(ctx, id, run)
		if err != nil || rec.StoppedBy != StoppedByLimit {
			t.Fatalf("run %s: stopped_by %q (%v)", run, rec.StoppedBy, err)
		}
	}
}

// An abort without a running turn records nothing: a finished turn must not look stopped, and a chat that never ran
// has no turn at all (issue #120, L3, F9).
func TestAbortWithoutTurnNoReason(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, err := e.m.Create(ctx, NewChat{Title: "t"})
	if err != nil {
		t.Fatal(err)
	}
	// never run: nothing to record
	if _, err := e.m.Abort(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if got := turnReason(t, e, c.ID); got != "" {
		t.Fatalf("reason without a turn: %q", got)
	}
	// a run that finished normally stays "not aborted", even when the user stops afterwards
	if _, err := e.m.Send(ctx, c.ID, "hallo"); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, e, c.ID)
	if _, err := e.m.Abort(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if got := turnReason(t, e, c.ID); got != "" {
		t.Fatalf("reason for a finished turn: %q", got)
	}
}

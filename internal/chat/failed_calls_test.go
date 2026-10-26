// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"errors"
	"strings"
	"testing"

	"agw/internal/plan"
	"agw/internal/platform"
	"agw/internal/store"
)

// Tests of the hard limit for failed tool calls in a row (#217). They need the test database like the rest of the
// package.

var (
	callOK     = platform.Result{Status: "ok", HTTPStatus: 200}
	callFailed = func(msg string) platform.Result { return platform.Result{Status: "error", Message: msg} }
)

// note feeds one call result to the counter of the chat.
func note(e *env, id, key string, res platform.Result) string {
	return e.m.NoteCallOutcome(context.Background(), id, "slot", key, res, nil)
}

func limits(e *env, total, same int) {
	e.m.opt.MaxFailedCalls, e.m.opt.MaxFailedSameTool = total, same
}

// Five failures of propose_plan with reads in between: the fifth call stops, with the detail naming tool and error.
func TestSameToolLimitStops(t *testing.T) {
	e := setup(t)
	limits(e, 20, 5)
	id, _, release := busyChat(t, e)
	defer release()
	waitRunning(t, e, id)
	waitReported(t, e, id)
	var last string
	for i := 1; i <= 5; i++ {
		last = note(e, id, "propose_plan", callFailed("refused: step 3 is unknown"))
		if i < 5 && strings.Contains(last, "stopped by the gateway") {
			t.Fatalf("stopped at call %d: %q", i, last)
		}
		if got := note(e, id, "list_datasets", callOK); got != "" {
			t.Fatalf("a read answered with a note: %q", got)
		}
	}
	if !strings.Contains(last, "5 failed calls of propose_plan in a row (limit 5)") {
		t.Fatalf("stop notice: %q", last)
	}
	waitFor(t, func() bool { return turnReason(t, e, id) == AbortFailedCalls })
	msgs, _ := e.st.Messages(context.Background(), id)
	d := msgs[len(msgs)-1].AbortDetail
	if !strings.Contains(d, "propose_plan") || !strings.Contains(d, "step 3 is unknown") || !strings.Contains(d, "Last error:") {
		t.Fatalf("detail: %q", d)
	}
}

// The total counter: two tools alternate without a success; the limit of 4 stops at the fourth call.
func TestTotalLimitStops(t *testing.T) {
	e := setup(t)
	limits(e, 4, 0)
	id, _, release := busyChat(t, e)
	defer release()
	waitRunning(t, e, id)
	waitReported(t, e, id)
	for i, key := range []string{"get_dataset", "get_model", "get_dataset"} {
		if got := note(e, id, key, callFailed("HTTP 404")); strings.Contains(got, "stopped") {
			t.Fatalf("stopped at call %d: %q", i+1, got)
		}
	}
	got := note(e, id, "get_model", callFailed("HTTP 404"))
	if !strings.Contains(got, "4 failed tool calls in a row (limit 4)") {
		t.Fatalf("stop notice: %q", got)
	}
	waitFor(t, func() bool { return turnReason(t, e, id) == AbortFailedCalls })
	if s := e.m.FailedCallsStopped(id); !strings.Contains(s, "4 failed tool calls in a row") || !strings.HasPrefix(s, "refused:") {
		t.Fatalf("refusal: %q", s)
	}
}

// A success resets the total, and the per-tool count only of its own tool.
func TestSuccessResetsTotalNotOtherTool(t *testing.T) {
	e := setup(t)
	limits(e, 20, 5)
	id := plainChat(t, e)
	for i := 0; i < 4; i++ {
		note(e, id, "propose_plan", callFailed("x"))
	}
	note(e, id, "list_datasets", callOK) // another tool: the total restarts, propose_plan stays at 4
	e.m.mu.Lock()
	st := e.m.failed[id]
	total, tool, other := st.total, st.perTool["propose_plan"], st.perTool["list_datasets"]
	e.m.mu.Unlock()
	if total != 0 || tool != 4 || other != 0 {
		t.Fatalf("total %d, propose_plan %d, list_datasets %d", total, tool, other)
	}
	if got := note(e, id, "propose_plan", callFailed("x")); !strings.Contains(got, "stopped by the gateway") {
		t.Fatalf("the fifth failure of the same tool should stop: %q", got)
	}
	// And a success of the tool itself clears its count.
	id2 := plainChat(t, e)
	for i := 0; i < 4; i++ {
		note(e, id2, "propose_plan", callFailed("x"))
	}
	note(e, id2, "propose_plan", callOK)
	if got := note(e, id2, "propose_plan", callFailed("x")); got != "" {
		t.Fatalf("after its own success: %q", got)
	}
}

// Rejected and pending answers neither count nor reset.
func TestRejectedIsNeutral(t *testing.T) {
	e := setup(t)
	limits(e, 20, 3)
	id := plainChat(t, e)
	note(e, id, "create_training", callFailed("x"))
	note(e, id, "create_training", callFailed("x"))
	for _, st := range []string{"rejected", platform.StatusPendingUser} {
		if got := note(e, id, "create_training", platform.Result{Status: st, Message: "no"}); got != "" {
			t.Fatalf("%s answered with a note: %q", st, got)
		}
	}
	e.m.mu.Lock()
	n := e.m.failed[id].perTool["create_training"]
	e.m.mu.Unlock()
	if n != 2 {
		t.Fatalf("count after neutral answers: %d", n)
	}
	if got := note(e, id, "create_training", callFailed("x")); !strings.Contains(got, "stopped by the gateway") {
		t.Fatalf("third failure: %q", got)
	}
}

// The warning comes once per chain: two calls before the per-tool limit, five before the total limit.
func TestWarningOnce(t *testing.T) {
	e := setup(t)
	limits(e, 10, 5)
	id := plainChat(t, e)
	var warned []int
	for i := 1; i <= 4; i++ {
		if got := note(e, id, "get_model", callFailed("x")); got != "" {
			warned = append(warned, i)
			if !strings.Contains(got, "3 failed calls of get_model in a row; at 5 the gateway stops this turn") {
				t.Fatalf("warning: %q", got)
			}
		}
	}
	if len(warned) != 1 || warned[0] != 3 {
		t.Fatalf("warned at %v", warned)
	}
	// Total: four more failures of other tools bring the total to 8... the warning is at 5 (limit 10 - 5).
	id2 := plainChat(t, e)
	var at []int
	for i := 1; i <= 6; i++ {
		key := []string{"a", "b", "c", "d", "e", "f"}[i-1]
		if got := note(e, id2, key, callFailed("x")); got != "" {
			at = append(at, i)
			if !strings.Contains(got, "5 failed tool calls in a row; at 10 the gateway stops this turn") {
				t.Fatalf("total warning: %q", got)
			}
		}
	}
	if len(at) != 1 || at[0] != 5 {
		t.Fatalf("total warning at %v", at)
	}
}

// 0 switches a limit off.
func TestZeroDisables(t *testing.T) {
	e := setup(t)
	limits(e, 0, 0)
	id := plainChat(t, e)
	for i := 0; i < 50; i++ {
		if got := note(e, id, "get_model", callFailed("x")); got != "" {
			t.Fatalf("note with both limits off: %q", got)
		}
	}
	if s := e.m.FailedCallsStopped(id); s != "" {
		t.Fatalf("stopped with both limits off: %q", s)
	}
	limits(e, 0, 3) // only the per-tool limit
	id2 := plainChat(t, e)
	note(e, id2, "a", callFailed("x"))
	note(e, id2, "a", callFailed("x"))
	if got := note(e, id2, "a", callFailed("x")); !strings.Contains(got, "stopped") {
		t.Fatalf("per-tool limit alone: %q", got)
	}
}

// An experiment chat is not counted.
func TestExperimentChatNotCounted(t *testing.T) {
	e := setup(t)
	limits(e, 2, 2)
	allowExperiment(e)
	id := createLimited(t, e, &store.RunLimits{MaxTokens: 1000000}).ID
	for i := 0; i < 5; i++ {
		if got := note(e, id, "get_model", callFailed("x")); got != "" {
			t.Fatalf("experiment chat counted: %q", got)
		}
	}
	if e.m.FailedCallsStopped(id) != "" {
		t.Fatal("experiment chat stopped")
	}
}

// A stopped chat refuses socket calls and model calls.
func TestStoppedRefusesCallsAndModel(t *testing.T) {
	e := setup(t)
	limits(e, 0, 2)
	id := plainChat(t, e)
	note(e, id, "get_model", callFailed("x"))
	note(e, id, "get_model", callFailed("x"))
	if e.m.FailedCallsStopped(id) == "" {
		t.Fatal("the chat is not stopped")
	}
	err := e.m.chatBudget(id)
	if err == nil || !strings.Contains(err.Error(), "the gateway stopped this chat") {
		t.Fatalf("model call: %v", err)
	}
	// A call that was on its way when the chat was stopped does not count or change the stop.
	if got := note(e, id, "get_model", callFailed("y")); got != "" {
		t.Fatalf("late call: %q", got)
	}
}

// A message of the user resets counters and stop; a wake-up does not.
func TestUserMessageResets(t *testing.T) {
	e := setup(t)
	limits(e, 0, 2)
	id := plainChat(t, e)
	note(e, id, "get_model", callFailed("x"))
	note(e, id, "get_model", callFailed("x"))
	if e.m.FailedCallsStopped(id) == "" {
		t.Fatal("not stopped")
	}
	waitFor(t, func() bool { v, _ := e.m.View(context.Background(), id); return v.HoldReason == HoldFailedCalls })
	if _, err := e.m.Send(context.Background(), id, "go on"); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, e, id)
	if s := e.m.FailedCallsStopped(id); s != "" {
		t.Fatalf("still stopped after the user wrote: %q", s)
	}
	e.m.mu.Lock()
	_, there := e.m.failed[id]
	e.m.mu.Unlock()
	if there {
		t.Fatal("counters survived the user's message")
	}
}

func TestWakeDoesNotReset(t *testing.T) {
	e := setup(t)
	limits(e, 0, 5)
	id := plainChat(t, e)
	note(e, id, "get_model", callFailed("x"))
	note(e, id, "get_model", callFailed("x"))
	a := e.agent(0)
	a.piRun("a note from the sandbox") // a turn without the user
	waitSettled(t, e, id)
	e.m.mu.Lock()
	n := 0
	if st := e.m.failed[id]; st != nil {
		n = st.perTool["get_model"]
	}
	e.m.mu.Unlock()
	if n != 2 {
		t.Fatalf("count after a turn without the user: %d", n)
	}
}

// Resuming a paused plan resets, too.
func TestResumePlanResets(t *testing.T) {
	e := setup(t)
	withPlanPlatform(t, e)
	limits(e, 0, 2)
	ctx := store.WithSession(context.Background(), "main")
	c, _, p := autonomousChat(t, e, ctx, autoPlanBody)
	note(e, c.ID, "get_model", callFailed("x"))
	note(e, c.ID, "get_model", callFailed("x"))
	waitFor(t, func() bool {
		got, err := e.st.GetPlan(ctx, p.ID)
		return err == nil && got.State == plan.StatePaused
	})
	if _, err := e.m.ResumePlan(ctx, c.ID, p.ID); err != nil {
		t.Fatal(err)
	}
	if s := e.m.FailedCallsStopped(c.ID); s != "" {
		t.Fatalf("still stopped after the resume: %q", s)
	}
}

// The stop holds the queue with the reason failed_calls and queues a passive note that does not wake the agent.
func TestStopHoldsQueueWithPassiveNote(t *testing.T) {
	e := setup(t)
	limits(e, 0, 2)
	id := plainChat(t, e)
	note(e, id, "get_model", callFailed("x"))
	note(e, id, "get_model", callFailed("x"))
	waitFor(t, func() bool {
		q, err := e.st.ListQueue(context.Background(), id)
		return err == nil && len(q) == 1 && q[0].Note == store.NoteFailedCalls
	})
	v, _ := e.m.View(context.Background(), id)
	if !v.QueueHeld || v.HoldReason != HoldFailedCalls {
		t.Fatalf("hold: %v %q", v.QueueHeld, v.HoldReason)
	}
	q, _ := e.st.ListQueue(context.Background(), id)
	if !onlyPassive(q) {
		t.Fatal("the note is not passive")
	}
	if !strings.Contains(q[0].Text, "The gateway stopped your last turn: 2 failed calls of get_model in a row (limit 2)") {
		t.Fatalf("note text: %q", q[0].Text)
	}
}

// An autonomous plan is paused with the reason failed_calls.
func TestStopPausesAutonomousPlan(t *testing.T) {
	e := setup(t)
	withPlanPlatform(t, e)
	limits(e, 0, 2)
	ctx := store.WithSession(context.Background(), "main")
	c, _, p := autonomousChat(t, e, ctx, autoPlanBody)
	note(e, c.ID, "get_model", callFailed("x"))
	note(e, c.ID, "get_model", callFailed("x"))
	waitFor(t, func() bool {
		got, err := e.st.GetPlan(ctx, p.ID)
		return err == nil && got.State == plan.StatePaused && got.PauseReason == "failed_calls"
	})
}

// A Go error counts as a failure and its text is cut and redacted.
func TestErrorCountsAndLastErrorIsCut(t *testing.T) {
	e := setup(t)
	limits(e, 0, 1)
	id := plainChat(t, e)
	long := strings.Repeat("x", 500)
	got := e.m.NoteCallOutcome(context.Background(), id, "slot", "get_model", platform.Result{}, errors.New(long))
	if !strings.Contains(got, "stopped by the gateway") {
		t.Fatalf("notice: %q", got)
	}
	e.m.mu.Lock()
	le := e.m.failed[id].stop.LastError
	e.m.mu.Unlock()
	if r := []rune(le); len(r) != maxLastError+1 || !strings.HasSuffix(le, "…") {
		t.Fatalf("last error: %d characters", len(r))
	}
}

// plainChat creates a chat whose sandbox is up and idle.
func plainChat(t *testing.T, e *env) string {
	t.Helper()
	ctx := context.Background()
	c, err := e.m.Create(ctx, NewChat{Title: "f"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.Send(ctx, c.ID, "go"); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, e, c.ID)
	return c.ID
}

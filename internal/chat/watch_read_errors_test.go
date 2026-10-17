// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"agw/internal/plan"
	"agw/internal/store"
)

// agentPrompts joins what went to pi in every sandbox of the test chat (a wake-up may use a fresh one).
func agentPrompts(e *env) string {
	e.mu.Lock()
	agents := append([]*fakeAgent(nil), e.agents...)
	e.mu.Unlock()
	var out []string
	for _, a := range agents {
		out = append(out, a.prompts()...)
	}
	return strings.Join(out, "\n")
}

// watchAdvance moves the watcher's clock far enough that a job whose reads fail is due again despite the
// growing backoff (issue #140). runWatchersOnce moves the shared offset by two minutes per pass, which the
// backoff outgrows after a few failures; this adds an hour on top.
func watchAdvance(t *testing.T) {
	t.Helper()
	watchOffset += time.Hour
	t.Cleanup(func() { watchOffset = 0 })
}

// Issue #140: a task the platform no longer has (404) is a decided failure. The job ends at once with the
// reason, the step and the plan follow, and the agent learns why — instead of polling it every minute for
// hours.
func TestUnreadableJobEndsFailedAndFailsThePlan(t *testing.T) {
	e := setup(t)
	ctx := store.WithSession(context.Background(), "main")
	c, slot, p, f := watchedChat(t, e, ctx, watchPlanBody)
	job := startWatchedStep(t, e, ctx, c, slot)
	f.failRead("GET /tasks/5", http.StatusNotFound)
	runWatchersOnce(t, e)

	got, err := e.st.GetWatchJob(ctx, c.ID, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.JobFailed {
		t.Fatalf("a 404 must end the job: %+v", got)
	}
	if !strings.Contains(got.Message, "no longer readable") || !strings.Contains(got.Message, "task 5") {
		t.Fatalf("the job does not say why it ended: %q", got.Message)
	}
	if got.EndedAt == nil {
		t.Fatalf("the end of the job is not recorded: %+v", got)
	}
	if steps := stepsOfPlan(t, e, p.ID); steps[0].State != plan.StepFailed {
		t.Fatalf("the step: %+v", steps[0])
	}
	pl, err := e.st.GetPlan(ctx, p.ID)
	if err != nil || pl.State != plan.StateFailed {
		t.Fatalf("the plan: %+v %v", pl, err)
	}
	// The agent learns why (D2.5): the chat is live, so the note goes to pi at once.
	waitFor(t, func() bool { return strings.Contains(agentPrompts(e), "no longer readable") })
}

// Issue #140: a transient failure (5xx, network) does not end the job. It is counted, the job keeps its
// state and the next attempt waits longer than the interval of its age.
func TestTransientReadErrorKeepsTheJob(t *testing.T) {
	e := setup(t)
	ctx := store.WithSession(context.Background(), "main")
	c, slot, p, f := watchedChat(t, e, ctx, watchPlanBody)
	job := startWatchedStep(t, e, ctx, c, slot)
	f.failRead("GET /tasks/5", http.StatusInternalServerError)
	runWatchersOnce(t, e)

	got, err := e.st.GetWatchJob(ctx, c.ID, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.JobRunning || got.FailReads != 1 {
		t.Fatalf("a 5xx must not end the job: %+v", got)
	}
	if got.CheckedAt == nil {
		t.Fatalf("the attempt is not recorded: %+v", got)
	}
	// The attempt is not repeated after the plain interval of a young job (10 s) but after twice of it.
	if watchDue(got, got.CheckedAt.Add(15*time.Second)) {
		t.Error("a failing job is polled again after 15 s")
	}
	if !watchDue(got, got.CheckedAt.Add(25*time.Second)) {
		t.Error("after the doubled interval the job is due again")
	}
	// Six failures in a row: still running, the job says why, and the step keeps waiting.
	for i := 0; i < 5; i++ {
		watchAdvance(t)
		runWatchersOnce(t, e)
	}
	got, err = e.st.GetWatchJob(ctx, c.ID, job.ID)
	if err != nil || got.State != store.JobRunning || got.FailReads < watchFailReads {
		t.Fatalf("after the failures it must still run: %+v %v", got, err)
	}
	if !strings.Contains(got.Message, "platform not readable") {
		t.Fatalf("the job does not say that it is not readable: %q", got.Message)
	}
	if steps := stepsOfPlan(t, e, p.ID); steps[0].State != plan.StepWaiting {
		t.Fatalf("the step must keep waiting: %+v", steps[0])
	}
	if notes := queueNotes(t, e, c.ID, store.NotePlanStep); len(notes) != 0 {
		t.Fatalf("a transient failure must not note the agent: %+v", notes)
	}
}

// Issue #140: a job that becomes readable again runs on — the counter of failed reads starts over and the
// job ends with its own state, not with the failure.
func TestRecoveringJobRunsOn(t *testing.T) {
	e := setup(t)
	ctx := store.WithSession(context.Background(), "main")
	c, slot, p, f := watchedChat(t, e, ctx, watchPlanBody)
	job := startWatchedStep(t, e, ctx, c, slot)
	f.failRead("GET /tasks/5", http.StatusBadGateway)
	for i := 0; i < 2; i++ {
		watchAdvance(t)
		runWatchersOnce(t, e)
	}
	if got, err := e.st.GetWatchJob(ctx, c.ID, job.ID); err != nil || got.FailReads != 2 {
		t.Fatalf("after two failures: %+v %v", got, err)
	}
	f.set("GET /tasks/5", `{"status":"inprogress","completion_percentage":0.5,"message":"building"}`)
	watchAdvance(t)
	runWatchersOnce(t, e)
	got, err := e.st.GetWatchJob(ctx, c.ID, job.ID)
	if err != nil || got.State != store.JobRunning || got.FailReads != 0 {
		t.Fatalf("after the recovery: %+v %v", got, err)
	}
	if got.Progress == nil || *got.Progress != 50 {
		t.Fatalf("the progress after the recovery: %+v", got)
	}
	f.set("GET /tasks/5", `{"status":"completed","completion_percentage":1.0,"message":"built"}`)
	watchAdvance(t)
	runWatchersOnce(t, e)
	if got, err = e.st.GetWatchJob(ctx, c.ID, job.ID); err != nil || got.State != store.JobDone {
		t.Fatalf("after the end: %+v %v", got, err)
	}
	if steps := stepsOfPlan(t, e, p.ID); steps[0].State != plan.StepDone {
		t.Fatalf("the step: %+v", steps[0])
	}
}

// The backoff of a failing job (issue #140): the interval of its age doubles with every failure in a row,
// up to the cap — and a job without failures keeps the plain interval.
func TestWatchIntervalBacksOff(t *testing.T) {
	young := 30 * time.Second
	if got := watchInterval(young, 0); got != watchIntervalA {
		t.Fatalf("without failures: %v", got)
	}
	prev := watchInterval(young, 0)
	for fails := 1; fails <= 6; fails++ {
		got := watchInterval(young, fails)
		if got < prev {
			t.Fatalf("the interval shrank at %d failures: %v after %v", fails, got, prev)
		}
		prev = got
	}
	if got := watchInterval(young, 20); got != watchMaxBackoff {
		t.Fatalf("the cap: %v, want %v", got, watchMaxBackoff)
	}
	// The same for an old job: its base interval is longer, the cap still holds.
	if got := watchInterval(2*time.Hour, 20); got != watchMaxBackoff {
		t.Fatalf("the cap for an old job: %v", got)
	}
}

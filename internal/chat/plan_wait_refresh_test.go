// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agw/internal/delegation"
	"agw/internal/plan"
	"agw/internal/platform"
	"agw/internal/store"
)

// #180: a plan that builds a training container (step 1, may be repeated by max_attempts) and starts it
// with a tool step (step 2).
func startTrainingPlan(attempts string) string {
	return `{"title":"Train","mode":"autonomous","budget":{"max_duration_minutes":60,"max_llm_cost_usd":1,"max_auto_turns":3},
 "steps":[{"method":"POST","path":"/train/config","body":{"dataset_id":59},"max_attempts":` + attempts + `},
 {"tool":"start_training","args":{"train_container_id":"{{step1.id}}"}}]}`
}

const (
	taskCompleted63 = `{"status":"completed","completion_percentage":1.0,"message":"Train Container #63 created."}`
	taskRunning     = `{"status":"running","completion_percentage":0.4,"message":"building"}`
)

func earlyRunCall(t *testing.T) platform.Request {
	t.Helper()
	req, err := platform.Normalize(platform.Request{Method: http.MethodPost, Path: "/train/containers/63/run"})
	if err != nil {
		t.Fatal(err)
	}
	return req
}

// jobEndNotes counts the notes about the end of the watched job: those still queued plus those already
// delivered to pi (a live chat that is not working gets the note at once). It waits for the first one.
func jobEndNotes(t *testing.T, e *env, chatID string) int {
	t.Helper()
	count := func() int {
		n := len(queueNotes(t, e, chatID, store.NotePlanStep))
		for _, p := range e.agent(0).prompts() {
			n += strings.Count(p, "finished (platform_task)")
		}
		return n
	}
	waitFor(t, func() bool { return count() > 0 })
	time.Sleep(300 * time.Millisecond) // a second note, if there were one, has arrived by now
	return count()
}

// guardOn runs the guards on a call without a tool invocation (as the REST binding does).
func guardOn(t *testing.T, e *env, ctx context.Context, chatID string, req platform.Request) (platform.Result, bool) {
	t.Helper()
	res, refused, err := e.m.guardRefusal(ctx, chatID, req, delegation.Classify(req))
	if err != nil {
		t.Fatal(err)
	}
	return res, refused
}

func TestPlanWaitGuardToolStepWithoutInvocation(t *testing.T) {
	e := setup(t)
	ctx := store.WithSession(context.Background(), "main")
	c, slot, _, f := watchedChat(t, e, ctx, startTrainingPlan("1"))
	f.set("GET /tasks/5", taskRunning)
	job := startConfigStep(t, e, ctx, c, slot)
	res, refused := guardOn(t, e, ctx, c.ID, earlyRunCall(t))
	if !refused || !strings.Contains(res.Message, "plan step 2 cannot run yet: step 1 still waits for platform job "+job.ID) {
		t.Fatalf("the early call must be refused with the reason: %+v", res)
	}
}

func TestEarlyCallRunsInPlanAfterRefresh(t *testing.T) {
	e := setup(t)
	ctx := store.WithSession(context.Background(), "main")
	c, slot, p, f := watchedChat(t, e, ctx, startTrainingPlan("1"))
	startConfigStep(t, e, ctx, c, slot)
	// The task is finished, but the watcher has not looked yet (no runWatchersOnce).
	f.set("GET /tasks/5", taskCompleted63)
	f.set("GET /train/containers?limit=1000", containersOfDataset59)
	if res, refused := guardOn(t, e, ctx, c.ID, earlyRunCall(t)); refused && strings.Contains(res.Message, "cannot run yet") {
		t.Fatalf("the finished job must not hold the call: %s", res.Message)
	}
	steps := stepsOfPlan(t, e, p.ID)
	if steps[0].State != plan.StepDone || steps[0].CreatedID != "63" {
		t.Fatalf("step 1 must be done with container 63: %+v", steps[0])
	}
	if st := e.m.planMatch(ctx, c.ID, earlyRunCall(t)); st == nil || st.Idx != 2 {
		t.Fatalf("the call must be claimed by step 2: %+v", st)
	}
	if n := jobEndNotes(t, e, c.ID); n != 1 {
		t.Fatalf("exactly one end-of-job note expected, got %d", n)
	}
}

func TestEarlyCallWhileJobRunsRefused(t *testing.T) {
	e := setup(t)
	ctx := store.WithSession(context.Background(), "main")
	c, slot, p, f := watchedChat(t, e, ctx, startTrainingPlan("1"))
	startConfigStep(t, e, ctx, c, slot)
	f.set("GET /tasks/5", taskRunning)
	res, refused := guardOn(t, e, ctx, c.ID, earlyRunCall(t))
	if !refused || !strings.Contains(res.Message, "still waits for platform job") {
		t.Fatalf("a running job must refuse the call: %+v", res)
	}
	if steps := stepsOfPlan(t, e, p.ID); steps[0].State != plan.StepWaiting {
		t.Fatalf("step 1 must keep waiting: %+v", steps[0])
	}
}

func TestEarlyCallAfterFailedJobRefused(t *testing.T) {
	e := setup(t)
	ctx := store.WithSession(context.Background(), "main")
	c, slot, p, f := watchedChat(t, e, ctx, startTrainingPlan("1"))
	startConfigStep(t, e, ctx, c, slot)
	f.set("GET /tasks/5", `{"status":"failed","completion_percentage":0.2,"message":"no space left"}`)
	res, refused := guardOn(t, e, ctx, c.ID, earlyRunCall(t))
	if !refused || !strings.Contains(res.Message, "plan step 2 did not run: step 1 failed") || !strings.Contains(res.Message, "Nothing was sent.") {
		t.Fatalf("a failed job must refuse the call with the reason: %+v", res)
	}
	if pl, err := e.st.GetPlan(ctx, p.ID); err != nil || pl.State != plan.StateFailed {
		t.Fatalf("the plan must have failed: %+v %v", pl, err)
	}
	if len(f.writeCalls()) != 1 {
		t.Fatalf("only the build call may have gone out: %v", f.writeCalls())
	}
}

func TestEarlyCallRetryOpenRefused(t *testing.T) {
	e := setup(t)
	ctx := store.WithSession(context.Background(), "main")
	c, slot, _, f := watchedChat(t, e, ctx, startTrainingPlan("2"))
	startConfigStep(t, e, ctx, c, slot)
	f.set("GET /tasks/5", `{"status":"failed","completion_percentage":0.2,"message":"no space left"}`)
	res, refused := guardOn(t, e, ctx, c.ID, earlyRunCall(t))
	if !refused || !strings.Contains(res.Message, "is open for another attempt") {
		t.Fatalf("a step open for a retry must refuse the call: %+v", res)
	}
}

func TestJobFinishedOnceGuardAndWatcher(t *testing.T) {
	e := setup(t)
	ctx := store.WithSession(context.Background(), "main")
	c, slot, p, f := watchedChat(t, e, ctx, startTrainingPlan("1"))
	job := startConfigStep(t, e, ctx, c, slot)
	f.set("GET /tasks/5", taskCompleted63)
	f.set("GET /train/containers?limit=1000", containersOfDataset59)
	guardOn(t, e, ctx, c.ID, earlyRunCall(t)) // the immediate read closes the job
	kind, _ := watchKind(job.Kind)
	// The watcher comes after with the row it read before: it must neither note nor finish a second time.
	e.m.pollJob(ctx, job, kind)
	runWatchersOnce(t, e)
	if n := jobEndNotes(t, e, c.ID); n != 1 {
		t.Fatalf("the end of the job must be noted once, got %d", n)
	}
	if steps := stepsOfPlan(t, e, p.ID); steps[0].State != plan.StepDone || steps[0].CreatedID != "63" {
		t.Fatalf("step 1: %+v", steps[0])
	}
}

func TestRefreshKeepsMinGap(t *testing.T) {
	e := setup(t)
	ctx := store.WithSession(context.Background(), "main")
	c, slot, _, f := watchedChat(t, e, ctx, startTrainingPlan("1"))
	startConfigStep(t, e, ctx, c, slot)
	f.set("GET /tasks/5", taskRunning)
	var reads atomic.Int32
	f.onRead = func(key string) {
		if key == "GET /tasks/5" {
			reads.Add(1)
		}
	}
	guardOn(t, e, ctx, c.ID, earlyRunCall(t))
	first := reads.Load()
	if first == 0 {
		t.Fatal("the first early call must read the job")
	}
	time.Sleep(10 * time.Millisecond)
	guardOn(t, e, ctx, c.ID, earlyRunCall(t))
	if reads.Load() != first {
		t.Fatalf("a second early call within %s must not read again: %d -> %d", planWaitMinGap, first, reads.Load())
	}
}

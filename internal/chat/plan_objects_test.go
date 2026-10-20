// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"agw/internal/delegation"
	"agw/internal/plan"
	"agw/internal/platform"
	"agw/internal/store"
	"agw/internal/watch"
)

// #169: a plan that builds a training container and uses the container in a later step.
const objectPlanBody = `{"title":"Train","mode":"autonomous","budget":{"max_duration_minutes":60,"max_llm_cost_usd":1,"max_auto_turns":3},
 "steps":[{"method":"POST","path":"/train/config","body":{"dataset_id":59},"max_attempts":2},
 {"method":"GET","path":"/train/containers/{{step1.id}}/model"}]}`

const containersOfDataset59 = `[{"id":63,"dataset_id":59}]`

// startConfigStep makes the call of step 1 (it names dataset 59), which leaves the step waiting for the build.
func startConfigStep(t *testing.T, e *env, ctx context.Context, c ChatView, slot string) store.WatchJob {
	t.Helper()
	req, err := platform.Normalize(platform.Request{Method: http.MethodPost, Path: "/train/config", Body: json.RawMessage(`{"dataset_id":59}`)})
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.m.PlatformCall(ctx, c.ID, slot, "cli", req)
	if err != nil || res.Status != "ok" {
		t.Fatalf("the step did not run: %+v %v", res, err)
	}
	steps := stepsOfPlan(t, e, planOfChat(t, e, c.ID))
	job, err := e.st.GetWatchJob(ctx, c.ID, steps[0].WatchJob)
	if err != nil {
		t.Fatal(err)
	}
	if job.Ref != "task:5|config:59" {
		t.Fatalf("the job does not carry the dataset: %+v", job)
	}
	return job
}

func TestPlanTrainingPlaceholderResolvesToContainer(t *testing.T) {
	e := setup(t)
	ctx := store.WithSession(context.Background(), "main")
	c, slot, p, f := watchedChat(t, e, ctx, objectPlanBody)
	startConfigStep(t, e, ctx, c, slot)
	f.set("GET /tasks/5", `{"status":"completed","completion_percentage":1.0,"message":"Train Container #63 created."}`)
	f.set("GET /train/containers?limit=1000", containersOfDataset59)
	runWatchersOnce(t, e)

	steps := stepsOfPlan(t, e, p.ID)
	if steps[0].State != plan.StepDone || steps[0].CreatedResource != delegation.Training || steps[0].CreatedID != "63" {
		t.Fatalf("step 1 must have created training container 63, not the task: %+v", steps[0])
	}
	if own, err := e.st.IsDelegationObject(ctx, c.ID, delegation.Training, "63"); err != nil || !own {
		t.Fatalf("the container is not in the register: %v %v", own, err)
	}
	right, _ := platform.Normalize(platform.Request{Method: http.MethodGet, Path: "/train/containers/63/model"})
	if st := e.m.planMatch(ctx, c.ID, right); st == nil || st.Idx != 2 {
		t.Fatalf("step 2 must cover the call on container 63: %+v", st)
	}
}

func TestPlanTaskIdDoesNotMatch(t *testing.T) {
	e := setup(t)
	ctx := store.WithSession(context.Background(), "main")
	c, slot, p, f := watchedChat(t, e, ctx, objectPlanBody)
	startConfigStep(t, e, ctx, c, slot)
	f.set("GET /tasks/5", `{"status":"completed","completion_percentage":1.0,"message":"Train Container #63 created."}`)
	f.set("GET /train/containers?limit=1000", containersOfDataset59)
	runWatchersOnce(t, e)
	_ = p
	wrong, _ := platform.Normalize(platform.Request{Method: http.MethodGet, Path: "/train/containers/5/model"})
	if st := e.m.planMatch(ctx, c.ID, wrong); st != nil {
		t.Fatalf("the task id must not match: %+v", st)
	}
}

func TestCreatedContainerRegisteredOutsidePlan(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	f := newFakeJobs(t, e, "/tasks/5")
	c, err := e.m.Create(ctx, NewChat{})
	if err != nil {
		t.Fatal(err)
	}
	slot := e.m.live[c.ID].slot.ID
	res := callWithApproval(t, e, ctx, c.ID, slot, platform.Request{Method: http.MethodPost, Path: "/train/config", Body: json.RawMessage(`{"dataset_id":59}`)})
	if res.Status != "ok" {
		t.Fatalf("call: %+v", res)
	}
	f.set("GET /tasks/5", `{"status":"completed","completion_percentage":1.0,"message":"Train Container #63 created."}`)
	f.set("GET /train/containers?limit=1000", containersOfDataset59)
	runWatchersOnce(t, e)
	if own, err := e.st.IsDelegationObject(ctx, c.ID, delegation.Training, "63"); err != nil || !own {
		t.Fatalf("the container is not in the register: %v %v", own, err)
	}
}

func TestStepFailsWhenJobNamesNoObject(t *testing.T) {
	e := setup(t)
	ctx := store.WithSession(context.Background(), "main")
	c, slot, p, f := watchedChat(t, e, ctx, objectPlanBody)
	startConfigStep(t, e, ctx, c, slot)
	f.set("GET /tasks/5", `{"status":"completed","completion_percentage":1.0,"message":null}`)
	runWatchersOnce(t, e)
	steps := stepsOfPlan(t, e, p.ID)
	if steps[0].State != plan.StepFailed || !strings.Contains(steps[0].Result, "named no training container it built; step 2 needs it") {
		t.Fatalf("step 1: %+v", steps[0])
	}
	if pl, err := e.st.GetPlan(ctx, p.ID); err != nil || pl.State != plan.StateFailed {
		t.Fatalf("the plan must fail without a retry although max_attempts is 2: %+v %v", pl, err)
	}
}

func TestFailedJobResultReadable(t *testing.T) {
	e := setup(t)
	ctx := store.WithSession(context.Background(), "main")
	c, slot, p, f := watchedChat(t, e, ctx, watchPlanBody)
	job := startWatchedStep(t, e, ctx, c, slot)
	f.set("GET /tasks/5", `{"status":"failed","completion_percentage":0.2,"message":"no space left"}`)
	runWatchersOnce(t, e)
	steps := stepsOfPlan(t, e, p.ID)
	if steps[0].Result != "job "+job.ID+" failed: no space left" {
		t.Fatalf("result: %q", steps[0].Result)
	}
}

func TestPlanWaitGuardRefusesNextStep(t *testing.T) {
	e := setup(t)
	ctx := store.WithSession(context.Background(), "main")
	c, slot, _, _ := watchedChat(t, e, ctx, watchPlanTwoSteps)
	job := startWatchedStep(t, e, ctx, c, slot)
	next, _ := platform.Normalize(platform.Request{Method: http.MethodPatch, Path: "/models/5", Body: json.RawMessage(modelBody)})
	res, refused, err := e.m.guardRefusal(ctx, c.ID, next, delegation.Classify(next))
	if err != nil || !refused {
		t.Fatalf("the call of the next step must be refused: %+v %v %v", res, refused, err)
	}
	if !strings.Contains(res.Message, "plan step 2 cannot run yet: step 1 still waits for platform job "+job.ID) {
		t.Fatalf("reason: %s", res.Message)
	}
	other, _ := platform.Normalize(platform.Request{Method: http.MethodPost, Path: "/tags", Body: json.RawMessage(`{"name":"x"}`)})
	if _, refused, _ := e.m.guardRefusal(ctx, c.ID, other, delegation.Classify(other)); refused {
		t.Error("an unrelated call must pass the guard")
	}
	read, _ := platform.Normalize(platform.Request{Method: http.MethodGet, Path: "/models/5"})
	if _, refused, _ := e.m.guardRefusal(ctx, c.ID, read, delegation.Classify(read)); refused {
		t.Error("a read must pass the guard")
	}
	_ = slot
}

func TestPlanValidityExtendedAfterJob(t *testing.T) {
	e := setup(t)
	ctx := store.WithSession(context.Background(), "main")
	f := newFakeJobs(t, e, "/tasks/5")
	c, err := e.m.Create(ctx, NewChat{})
	if err != nil {
		t.Fatal(err)
	}
	slot := e.m.live[c.ID].slot.ID
	r, _ := proposeAndDecide(t, e, ctx, c.ID, slot, `{"title":"Train","steps":[{"method":"POST","path":"/train/config"}]}`, true)
	if r.Status != "ok" {
		t.Fatalf("plan not approved: %+v", r)
	}
	pid := planOfChat(t, e, c.ID)
	before, _ := e.st.GetPlan(ctx, pid)
	startWatchedStep(t, e, ctx, c, slot)
	f.set("GET /tasks/5", `{"status":"completed","completion_percentage":1.0}`)
	runWatchersOnce(t, e)
	after, _ := e.st.GetPlan(ctx, pid)
	if before.ExpiresAt == nil || after.ExpiresAt == nil || !after.ExpiresAt.After(*before.ExpiresAt) {
		t.Fatalf("validity not counted again: %v -> %v", before.ExpiresAt, after.ExpiresAt)
	}
}

func TestJobNoteNamesCreatedObject(t *testing.T) {
	st := &store.PlanStep{Idx: 2, Description: "Build"}
	note := JobNote(store.WatchJob{ID: "w1", Kind: "platform_task"}, st, watch.StateDone, time.Minute,
		&watch.Output{Resource: delegation.Training, ID: "63"})
	if !strings.Contains(note.Summary, "Created training container 63; {{step2.id}} is 63.") {
		t.Fatalf("summary: %s", note.Summary)
	}
	plain := JobNote(store.WatchJob{ID: "w1", Kind: "platform_task"}, st, watch.StateDone, time.Minute, nil)
	if strings.Contains(plain.Summary, "Created") {
		t.Fatalf("no object, no sentence: %s", plain.Summary)
	}
}

func TestWatchNoteMentionsPlaceholder(t *testing.T) {
	e := setup(t)
	ctx := store.WithSession(context.Background(), "main")
	c, slot, p, _ := watchedChat(t, e, ctx, objectPlanBody)
	job := startConfigStep(t, e, ctx, c, slot)
	steps := stepsOfPlan(t, e, p.ID)
	note := e.m.watchNote(ctx, &steps[0], job.ID)
	if !strings.Contains(note, "Later steps use {{step1.id}}: the training container this job creates.") {
		t.Fatalf("note: %s", note)
	}
}

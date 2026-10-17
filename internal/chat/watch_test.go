// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"agw/internal/delegation"
	"agw/internal/plan"
	"agw/internal/platform"
	"agw/internal/store"
	"agw/internal/watch"
)

// A-E2: the watched platform of the tests. Writes answer 202 with a Location (a task), reads come from a
// table; a path without an entry answers an empty JSON object, and an entry may name another status (a
// platform that is not readable).
type fakeJobs struct {
	mu       sync.Mutex
	answers  map[string]string
	status   map[string]int
	location string
	writes   []string
}

func newFakeJobs(t *testing.T, e *env, location string) *fakeJobs {
	t.Helper()
	f := &fakeJobs{answers: map[string]string{}, status: map[string]int{}, location: location}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			fmt.Fprint(w, `{"access_token":"t","expires_in":3600}`)
			return
		}
		key := r.Method + " " + r.URL.Path
		if r.URL.RawQuery != "" {
			key += "?" + r.URL.RawQuery
		}
		f.mu.Lock()
		body, known := f.answers[key]
		st, location := f.status[key], f.location
		if r.Method != http.MethodGet {
			f.writes = append(f.writes, key)
		}
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodGet {
			w.Header().Set("Location", location)
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprint(w, `{}`)
			return
		}
		if st != 0 {
			w.WriteHeader(st)
			fmt.Fprint(w, `{"detail":"not readable"}`)
			return
		}
		if !known {
			fmt.Fprint(w, `{}`)
			return
		}
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	c, err := platform.New(platform.Config{APIURL: srv.URL, TokenURL: srv.URL + "/token", User: "u", Password: "p"})
	if err != nil {
		t.Fatal(err)
	}
	e.m.opt.Platform = c
	return f
}

// set records an answer for a reading call.
func (f *fakeJobs) set(key, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers[key] = body
	delete(f.status, key)
}

// failRead makes a reading call answer with a status (a platform that is not readable).
func (f *fakeJobs) failRead(key string, status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status[key] = status
	delete(f.answers, key)
}

// writeCalls names the writing calls that arrived at the platform.
func (f *fakeJobs) writeCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.writes...)
}

// watchPlanBody is an autonomous plan whose one step starts a platform task (POST /train/config answers
// 202 with Location /tasks/5). The budget carries three turns, so a wake-up fits in.
const watchPlanBody = `{"title":"Watch","mode":"autonomous","budget":{"max_duration_minutes":60,"max_llm_cost_usd":1,"max_auto_turns":3},
 "steps":[{"method":"POST","path":"/train/config"}]}`

// watchPlanTwoSteps adds a covered step that comes after the watched one (the plan must not run it while
// the first step waits).
const watchPlanTwoSteps = `{"title":"Watch","mode":"autonomous","budget":{"max_duration_minutes":60,"max_llm_cost_usd":1,"max_auto_turns":3},
 "steps":[{"method":"POST","path":"/train/config"},{"method":"PATCH","path":"/models/5","body":` + modelBody + `}]}`

// watchPlanRetry is the same plan with a step that may be repeated once after a failure (A-E1, D1.7).
const watchPlanRetry = `{"title":"Watch","mode":"autonomous","budget":{"max_duration_minutes":60,"max_llm_cost_usd":1,"max_auto_turns":3},
 "steps":[{"method":"POST","path":"/train/config","max_attempts":2}]}`

// watchPlanDelete is an autonomous plan with an irreversible step (D2.8): the plan does not cover it, so
// it asks the user — and as a parked approval it does not hold the running turn.
const watchPlanDelete = `{"title":"Clean up","mode":"autonomous","budget":{"max_duration_minutes":60,"max_llm_cost_usd":1,"max_auto_turns":3},
 "steps":[{"method":"DELETE","path":"/datasets/5","body":{"key":"-","csrftoken":"-","sessionid":"-"}}]}`

// watchPlanDeploy is an autonomous plan with a step whose route asks for every single call (the deploy of
// a container deployment, G1.1): the plan does not cover it either.
const watchPlanDeploy = `{"title":"Deploy","mode":"autonomous","budget":{"max_duration_minutes":60,"max_llm_cost_usd":1,"max_auto_turns":3},
 "steps":[{"method":"PUT","path":"/container-deployments/5/deploy"}]}`

// watchedChat creates a chat with an approved autonomous plan and a fake platform.
func watchedChat(t *testing.T, e *env, ctx context.Context, body string) (ChatView, string, store.Plan, *fakeJobs) {
	t.Helper()
	f := newFakeJobs(t, e, "/tasks/5")
	c, _, p := autonomousChat(t, e, ctx, body)
	return c, e.m.live[c.ID].slot.ID, p, f
}

// startWatchedStep makes the call of the plan's first step: it is covered by the plan, runs without an
// approval and leaves the step waiting for the job it started. It returns the step's job (the newest
// running one, because a repeated step starts a second job).
func startWatchedStep(t *testing.T, e *env, ctx context.Context, c ChatView, slot string) store.WatchJob {
	t.Helper()
	req, err := platform.Normalize(platform.Request{Method: http.MethodPost, Path: "/train/config"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.m.PlatformCall(ctx, c.ID, slot, "cli", req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "ok" || res.HTTPStatus != http.StatusAccepted {
		t.Fatalf("the step did not run: %+v", res)
	}
	steps := stepsOfPlan(t, e, planOfChat(t, e, c.ID))
	if steps[0].WatchJob == "" {
		t.Fatalf("the step waits for no job: %+v", steps[0])
	}
	job, err := e.st.GetWatchJob(ctx, c.ID, steps[0].WatchJob)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != store.JobRunning {
		t.Fatalf("the job of the step is not running: %+v", job)
	}
	return job
}

// planOfChat returns the id of the chat's newest plan.
func planOfChat(t *testing.T, e *env, chatID string) string {
	t.Helper()
	ps, err := e.m.Plans(context.Background(), chatID)
	if err != nil || len(ps) == 0 {
		t.Fatalf("plans: %+v %v", ps, err)
	}
	return ps[len(ps)-1].ID
}

// watchOffset is how far runWatchersOnce has moved the watcher's clock forward.
var watchOffset time.Duration

// runWatchersOnce runs one pass of the watcher loop. The loop polls a young job only every
// watchIntervalA (10 s) and a job whose reads keep failing at a growing distance (up to watchMaxBackoff),
// so the helper moves the watcher's clock forward by an hour: the next pass sees every job due again.
func runWatchersOnce(t *testing.T, e *env) {
	t.Helper()
	watchOffset += time.Hour
	watchNow = func() time.Time { return time.Now().Add(watchOffset) }
	e.m.watchOnce(context.Background(), make(chan struct{}, watchParallel))
}

// stepsOfPlan reloads a plan and returns its steps.
func stepsOfPlan(t *testing.T, e *env, planID string) []store.PlanStep {
	t.Helper()
	p, err := e.st.GetPlan(context.Background(), planID)
	if err != nil {
		t.Fatal(err)
	}
	return p.Steps
}

// queueNotes returns the queued system notes of a chat by their note type.
func queueNotes(t *testing.T, e *env, chatID, note string) []store.QueueEntry {
	t.Helper()
	list, err := e.st.ListQueue(context.Background(), chatID)
	if err != nil {
		t.Fatal(err)
	}
	var out []store.QueueEntry
	for _, entry := range list {
		if entry.Kind == store.QueueSystem && entry.Note == note {
			out = append(out, entry)
		}
	}
	return out
}

// A-E2 (D2.2): the call of a covered step starts a job, and the step waits for it instead of being done.
// The plan does not continue: its next step is not the one a call matches now.
func TestWaitingStepBlocksNextStep(t *testing.T) {
	e := setup(t)
	ctx := store.WithSession(context.Background(), "main")
	c, slot, p, _ := watchedChat(t, e, ctx, watchPlanTwoSteps)
	job := startWatchedStep(t, e, ctx, c, slot)
	if job.Kind != "platform_task" || job.Ref != "task:5" || job.State != store.JobRunning {
		t.Fatalf("job: %+v", job)
	}
	steps := stepsOfPlan(t, e, p.ID)
	if steps[0].State != plan.StepWaiting || steps[0].WatchJob != job.ID {
		t.Fatalf("first step: %+v", steps[0])
	}
	// The next step of the plan is the waiting one, so the call of step 2 covers nothing and asks the user.
	req2, err := platform.Normalize(platform.Request{Method: http.MethodPatch, Path: "/models/5", Body: json.RawMessage(modelBody)})
	if err != nil {
		t.Fatal(err)
	}
	if st := e.m.planMatch(ctx, c.ID, req2); st != nil {
		t.Fatalf("the plan covered a later step while its first one waits: %+v", st)
	}
}

// A-E2 (S3): a job that starts outside a plan is observed (for the cards), with no note and no wake-up.
func TestJobOutsidePlanRecorded(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	f := newFakeJobs(t, e, "/tasks/7")
	c, err := e.m.Create(ctx, NewChat{})
	if err != nil {
		t.Fatal(err)
	}
	slot := e.m.live[c.ID].slot.ID
	res := callWithApproval(t, e, ctx, c.ID, slot, platform.Request{Method: http.MethodPost, Path: "/train/config"})
	if res.Status != "ok" {
		t.Fatalf("call: %+v", res)
	}
	jobs, err := e.m.Jobs(ctx, c.ID)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs: %+v %v", jobs, err)
	}
	if jobs[0].Kind != "platform_task" || jobs[0].Ref != "task:7" || jobs[0].PlanID != "" {
		t.Fatalf("job: %+v", jobs[0])
	}
	if len(f.writeCalls()) != 1 {
		t.Fatalf("calls: %v", f.writeCalls())
	}
	// Nothing is queued for the agent, and the chat stays as it is.
	if notes := queueNotes(t, e, c.ID, store.NotePlanStep); len(notes) != 0 {
		t.Fatalf("a job outside a plan must not note the agent: %+v", notes)
	}
}

// callWithApproval runs a writing platform call and approves it.
func callWithApproval(t *testing.T, e *env, ctx context.Context, chatID, slot string, req platform.Request) platform.Result {
	t.Helper()
	events, cancel := e.m.Subscribe(chatID)
	defer cancel()
	norm, err := platform.Normalize(req)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan platform.Result, 1)
	go func() {
		r, _ := e.m.PlatformCall(ctx, chatID, slot, "cli", norm)
		done <- r
	}()
	ap := waitEvent(t, events, "approval", "").Data.(store.Approval)
	for ap.State != store.ApprovalPending {
		ap = waitEvent(t, events, "approval", "").Data.(store.Approval)
	}
	if _, err := e.m.Decide(ctx, ap.ID, true); err != nil {
		t.Fatal(err)
	}
	return <-done
}

// A-E2 (S4): the loop sees the job end, finishes the step and the plan, and tells the agent — the note
// carries the platform's message and the log tail in the fence.
func TestWatcherMarksDoneAndNotifies(t *testing.T) {
	e := setup(t)
	ctx := store.WithSession(context.Background(), "main")
	c, slot, p, f := watchedChat(t, e, ctx, watchPlanBody)
	job := startWatchedStep(t, e, ctx, c, slot)
	// The platform reports progress first.
	f.set("GET /tasks/5", `{"status":"inprogress","completion_percentage":0.4,"message":"building"}`)
	runWatchersOnce(t, e)
	got, err := e.st.GetWatchJob(ctx, c.ID, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.JobRunning || got.Progress == nil || *got.Progress != 40 || got.Message != "building" {
		t.Fatalf("progress: %+v", got)
	}
	if steps := stepsOfPlan(t, e, p.ID); steps[0].Progress == nil || *steps[0].Progress != 40 {
		t.Fatalf("the step does not carry the progress: %+v", steps[0])
	}
	// Then it is done.
	f.set("GET /tasks/5", `{"status":"completed","completion_percentage":1.0,"message":"image built"}`)
	runWatchersOnce(t, e)
	got, err = e.st.GetWatchJob(ctx, c.ID, job.ID)
	if err != nil || got.State != store.JobDone {
		t.Fatalf("job after the end: %+v %v", got, err)
	}
	steps := stepsOfPlan(t, e, p.ID)
	if steps[0].State != plan.StepDone {
		t.Fatalf("step: %+v", steps[0])
	}
	pl, err := e.st.GetPlan(ctx, p.ID)
	if err != nil || pl.State != plan.StateCompleted {
		t.Fatalf("plan: %+v %v", pl, err)
	}
	// The agent learns it: the note sits in the fence of the message that went to pi.
	waitFor(t, func() bool { return len(e.agent(0).prompts()) > 0 })
	text := strings.Join(e.agent(0).prompts(), "\n")
	for _, want := range []string{"Plan step 1", "finished (platform_task)", "Platform message: image built"} {
		if !strings.Contains(text, want) {
			t.Errorf("the note misses %q in:\n%s", want, text)
		}
	}
	// The turn is that of a note of the orchestrator, not of a message from the user.
	waitFor(t, func() bool {
		turns, err := e.st.Turns(ctx, c.ID)
		if err != nil || len(turns) == 0 {
			return false
		}
		last := turns[len(turns)-1]
		return last.Origin == store.OriginSystem && last.Trigger == store.TriggerWake
	})
}

// A-E2 (S4, D1.7): a watched step that ends failed is repeated while it has attempts left; the plan stays
// approved.
func TestWatcherFailureRetries(t *testing.T) {
	e := setup(t)
	ctx := store.WithSession(context.Background(), "main")
	c, slot, p, f := watchedChat(t, e, ctx, watchPlanRetry)
	job := startWatchedStep(t, e, ctx, c, slot)
	f.set("GET /tasks/5", `{"status":"failed","completion_percentage":0.2,"message":"no space left"}`)
	runWatchersOnce(t, e)
	steps := stepsOfPlan(t, e, p.ID)
	if steps[0].State != plan.StepOpen || steps[0].WatchJob != "" {
		t.Fatalf("the failed step was not reopened: %+v", steps[0])
	}
	if pl, err := e.st.GetPlan(ctx, p.ID); err != nil || pl.State != plan.StateApproved {
		t.Fatalf("the plan should stay approved: %+v %v", pl, err)
	}
	if got, err := e.st.GetWatchJob(ctx, c.ID, job.ID); err != nil || got.State != store.JobFailed {
		t.Fatalf("job: %+v %v", got, err)
	}
	// Without an attempt left the plan fails.
	job2 := startWatchedStep(t, e, ctx, c, slot)
	f.set("GET /tasks/5", `{"status":"failed","message":"again"}`)
	runWatchersOnce(t, e)
	if pl, err := e.st.GetPlan(ctx, p.ID); err != nil || pl.State != plan.StateFailed {
		t.Fatalf("the plan should have failed: %+v %v", pl, err)
	}
	if got, err := e.st.GetWatchJob(ctx, c.ID, job2.ID); err != nil || got.State != store.JobFailed {
		t.Fatalf("second job: %+v %v", got, err)
	}
}

// A-E2 (D2.3): a reading call that fails does not lose the job. From five failures in a row on it says so,
// the step keeps waiting and the user is asked for attention.
func TestWatcherReadErrorsKeepWaiting(t *testing.T) {
	e := setup(t)
	ctx := store.WithSession(context.Background(), "main")
	c, slot, p, f := watchedChat(t, e, ctx, watchPlanBody)
	job := startWatchedStep(t, e, ctx, c, slot)
	events, cancel := e.m.Subscribe(c.ID)
	defer cancel()
	f.failRead("GET /tasks/5", http.StatusInternalServerError)
	for i := 1; i <= watchFailReads; i++ {
		runWatchersOnce(t, e)
		got, err := e.st.GetWatchJob(ctx, c.ID, job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.State != store.JobRunning || got.FailReads != i {
			t.Fatalf("after %d failures: %+v", i, got)
		}
		if steps := stepsOfPlan(t, e, p.ID); steps[0].State != plan.StepWaiting {
			t.Fatalf("the step must keep waiting: %+v", steps[0])
		}
	}
	got, err := e.st.GetWatchJob(ctx, c.ID, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Message, "platform not readable") {
		t.Fatalf("the job does not say why it is not readable: %+v", got)
	}
	ev := waitEvent(t, events, "attention", "")
	if ev.Kind != "attention" {
		t.Fatalf("no attention for the user: %+v", ev)
	}
	// The platform comes back: the job runs on and the counter is cleared.
	f.set("GET /tasks/5", `{"status":"inprogress","completion_percentage":0.6}`)
	runWatchersOnce(t, e)
	if got, err = e.st.GetWatchJob(ctx, c.ID, job.ID); err != nil || got.FailReads != 0 {
		t.Fatalf("after the recovery: %+v %v", got, err)
	}
}

// A-E2 (S4): a plan that was cancelled while its job ran is not continued — the step stays skipped and the
// agent gets no note.
func TestWatcherDoesNotWakeCancelledPlan(t *testing.T) {
	e := setup(t)
	ctx := store.WithSession(context.Background(), "main")
	c, slot, p, f := watchedChat(t, e, ctx, watchPlanBody)
	job := startWatchedStep(t, e, ctx, c, slot)
	if _, err := e.m.CancelPlan(ctx, c.ID, p.ID); err != nil {
		t.Fatal(err)
	}
	f.set("GET /tasks/5", `{"status":"completed","completion_percentage":1.0}`)
	runWatchersOnce(t, e)
	steps := stepsOfPlan(t, e, p.ID)
	if steps[0].State != plan.StepSkipped {
		t.Fatalf("the step of a cancelled plan: %+v", steps[0])
	}
	if got, err := e.st.GetWatchJob(ctx, c.ID, job.ID); err != nil || got.State != store.JobDone {
		t.Fatalf("the job's row is closed on its own: %+v %v", got, err)
	}
	if notes := queueNotes(t, e, c.ID, store.NotePlanStep); len(notes) != 0 {
		t.Fatalf("a cancelled plan must not note the agent about its steps: %+v", notes)
	}
}

// A-E2 (S4): the end of a job outside a plan is recorded and distributed, and no note wakes the agent.
func TestWatcherJobOutsidePlanNoNote(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	f := newFakeJobs(t, e, "/tasks/7")
	c, err := e.m.Create(ctx, NewChat{})
	if err != nil {
		t.Fatal(err)
	}
	slot := e.m.live[c.ID].slot.ID
	if res := callWithApproval(t, e, ctx, c.ID, slot, platform.Request{Method: http.MethodPost, Path: "/train/config"}); res.Status != "ok" {
		t.Fatalf("call: %+v", res)
	}
	jobs, err := e.m.Jobs(ctx, c.ID)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs: %+v %v", jobs, err)
	}
	events, cancel := e.m.Subscribe(c.ID)
	defer cancel()
	f.set("GET /tasks/7", `{"status":"completed","completion_percentage":1.0}`)
	runWatchersOnce(t, e)
	if got, err := e.st.GetWatchJob(ctx, c.ID, jobs[0].ID); err != nil || got.State != store.JobDone {
		t.Fatalf("job: %+v %v", got, err)
	}
	ev := waitEvent(t, events, "job", "")
	if ev.Kind != "job" {
		t.Fatalf("no job event: %+v", ev)
	}
	if notes := queueNotes(t, e, c.ID, store.NotePlanStep); len(notes) != 0 {
		t.Fatalf("no note for a job outside a plan: %+v", notes)
	}
	if e.agent(0).prompts(); len(e.agent(0).prompts()) != 0 {
		t.Fatalf("the agent was woken by a job outside a plan: %v", e.agent(0).prompts())
	}
}

// A-E2 (Sicherheitsfragen): the watcher never writes. A kind that asks for a writing call gets a refusal
// from the reader, so no job can be changed by what it observes.
func TestWatchReaderRefusesWrites(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	f := newFakeJobs(t, e, "/tasks/5")
	c, err := e.m.Create(ctx, NewChat{})
	if err != nil {
		t.Fatal(err)
	}
	// A kind of the test that tries to write while polling.
	const kindName = "seam_write"
	watch.Register(watch.Kind{Name: kindName, Poll: func(ctx context.Context, _ watch.Job, read watch.Reader) (watch.Progress, error) {
		res, err := read(ctx, platform.Request{Method: http.MethodPost, Path: "/train/config"})
		if err != nil {
			return watch.Progress{}, err
		}
		if res.Status == "ok" {
			return watch.Progress{}, fmt.Errorf("the watcher wrote: %+v", res)
		}
		// The writing GET of the platform is refused as well.
		if res, err := read(ctx, platform.Request{Method: http.MethodGet, Path: "/licenses/"}); err == nil && res.Status == "ok" {
			return watch.Progress{}, fmt.Errorf("the watcher called a writing GET: %+v", res)
		}
		return watch.Progress{State: watch.StateRunning}, nil
	}})
	jobID, err := e.st.AddWatchJob(ctx, store.WatchJob{ChatID: c.ID, Kind: kindName, Ref: "x"})
	if err != nil {
		t.Fatal(err)
	}
	runWatchersOnce(t, e)
	if n := len(f.writeCalls()); n != 0 {
		t.Fatalf("the watcher wrote to the platform: %v", f.writeCalls())
	}
	if got, err := e.st.GetWatchJob(ctx, c.ID, jobID); err != nil || got.State != store.JobRunning {
		t.Fatalf("job: %+v %v", got, err)
	}
}

// A-E2 (D2.6): a resting chat is woken when a step of its autonomous plan ends; the note is delivered as
// the wake-up's message.
func TestWakeFromDormantForPlan(t *testing.T) {
	e := setup(t)
	ctx := store.WithSession(context.Background(), "main")
	c, slot, p, f := watchedChat(t, e, ctx, watchPlanBody)
	job := startWatchedStep(t, e, ctx, c, slot)
	// The chat rests (the user closed it or its sandbox ended).
	if _, err := e.m.Suspend(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if v, _ := e.m.View(ctx, c.ID); v.State != store.StateDormant {
		t.Fatalf("the chat does not rest: %+v", v)
	}
	f.set("GET /tasks/5", `{"status":"completed","completion_percentage":1.0,"message":"done"}`)
	runWatchersOnce(t, e)
	waitWoken(t, e, c.ID)
	// The note went to the sandbox that was started for the wake-up.
	waitFor(t, func() bool {
		for i := 0; i < 4; i++ {
			if a := e.agent(i); a != nil && strings.Contains(strings.Join(a.prompts(), "\n"), "Plan step 1") {
				return true
			}
		}
		return false
	})
	if pl, err := e.st.GetPlan(ctx, p.ID); err != nil || pl.State != plan.StateCompleted {
		t.Fatalf("plan: %+v %v", pl, err)
	}
	if _, err := e.st.GetWatchJob(ctx, c.ID, job.ID); err != nil {
		t.Fatal(err)
	}
}

// A-E2 (D2.7): a wake-up that carries the end of a plan step counts against the plan's own turn budget,
// not against AGW_AUTO_TURNS_MAX or the wake-ups per hour.
func TestPlanWakeCountsAgainstPlanBudget(t *testing.T) {
	e := setup(t)
	ctx := store.WithSession(context.Background(), "main")
	// The gateway's own limits are used up: the plan's budget still carries its step.
	e.m.opt.AutoTurnsMax = 0
	e.m.opt.BgWakesPerHour = 0
	newFakeJobs(t, e, "/tasks/5")
	c, _, p := autonomousChat(t, e, ctx, watchPlanBody)
	entry, err := e.st.EnqueueSystem(ctx, c.ID, store.NotePlanStep, []string{p.ID, "1"}, "Plan step 1 finished.")
	if err != nil {
		t.Fatal(err)
	}
	if planID := planStepWake([]store.QueueEntry{entry}); planID != p.ID {
		t.Fatalf("planStepWake = %q, want %q", planID, p.ID)
	}
	l := e.m.live[c.ID]
	e.m.deliverQueue(c.ID, l)
	waitFor(t, func() bool { return len(e.agent(0).prompts()) > 0 })
	pl, err := e.st.GetPlan(ctx, p.ID)
	if err != nil || pl.AutoTurns != 1 {
		t.Fatalf("the turn was not counted on the plan: %+v %v", pl, err)
	}
	if pl.State != plan.StateApproved {
		t.Fatalf("the plan should stay approved: %+v", pl)
	}
}

// A-E2 (D2.7): over the plan's turn budget the plan is paused, the note stays queued and the user sees
// auto_held — the endless carrying of the plan ends there.
func TestPlanWakeOverBudgetPausesPlan(t *testing.T) {
	e := setup(t)
	ctx := store.WithSession(context.Background(), "main")
	newFakeJobs(t, e, "/tasks/5")
	c, _, p := autonomousChat(t, e, ctx, watchPlanBody)
	// The budget is used up already.
	for i := 0; i < 3; i++ {
		if _, err := e.st.IncPlanAutoTurns(ctx, p.ID); err != nil {
			t.Fatal(err)
		}
	}
	entry, err := e.st.EnqueueSystem(ctx, c.ID, store.NotePlanStep, []string{p.ID, "1"}, "Plan step 1 finished.")
	if err != nil {
		t.Fatal(err)
	}
	events, cancel := e.m.Subscribe(c.ID)
	defer cancel()
	l := e.m.live[c.ID]
	e.m.deliverQueue(c.ID, l)
	ev := waitEvent(t, events, "auto_held", "")
	if ev.Kind != "auto_held" {
		t.Fatalf("no auto_held: %+v", ev)
	}
	pl, err := e.st.GetPlan(ctx, p.ID)
	if err != nil || pl.State != plan.StatePaused || pl.PauseReason != "budget_turns" {
		t.Fatalf("the plan should be paused at its budget: %+v %v", pl, err)
	}
	if notes := queueNotes(t, e, c.ID, store.NotePlanStep); len(notes) != 1 || notes[0].ID != entry.ID {
		t.Fatalf("the note was not held back: %+v", notes)
	}
	// The turn was counted on the plan, and the paused plan holds nothing back any more: it cannot run on
	// by itself, so its last note is an ordinary one again (the user's next message delivers it).
	pl, err = e.st.GetPlan(ctx, p.ID)
	if err != nil || pl.AutoTurns != 4 {
		t.Fatalf("the turn was not counted: %+v %v", pl, err)
	}
	if ev, limited, handled := e.m.planWakeLimit(ctx, c.ID, p.ID); !handled || limited {
		t.Fatalf("a paused plan must not hold its last note back: %+v %v %v", ev, limited, handled)
	}
}

// A-E2 (D2.7): a background task's note keeps the gateway's own limits — the plan's budget does not apply
// to it.
func TestBackgroundWakeUnchanged(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.m.opt.AutoTurnsMax = 1
	c, err := e.m.Create(ctx, NewChat{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.EnqueueSystem(ctx, c.ID, store.NoteBackground, []string{"bg-1"}, "Background task bg-1 finished."); err != nil {
		t.Fatal(err)
	}
	open, err := e.st.ListQueue(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if planID := planStepWake(open); planID != "" {
		t.Fatalf("a background note counts on a plan: %q", planID)
	}
	// The limit for turns without the user applies (one turn is allowed, and it is used up by the wake).
	l := e.m.live[c.ID]
	e.m.deliverQueue(c.ID, l)
	waitFor(t, func() bool { return len(e.agent(0).prompts()) > 0 })
	if _, err := e.st.EnqueueSystem(ctx, c.ID, store.NoteBackground, []string{"bg-2"}, "Background task bg-2 finished."); err != nil {
		t.Fatal(err)
	}
	events, cancel := e.m.Subscribe(c.ID)
	defer cancel()
	e.m.deliverQueue(c.ID, l)
	ev := waitEvent(t, events, "auto_held", "")
	if ev.Kind != "auto_held" {
		t.Fatalf("the wake limit did not apply to a background note: %+v", ev)
	}
}

// A-E2 (D2.8): an irreversible step of an autonomous plan does not hold the running turn. The call answers
// at once, the step carries the approval, and the agent may end its turn.
func TestParkedApprovalReturnsAtOnce(t *testing.T) {
	e := setup(t)
	ctx := store.WithSession(context.Background(), "main")
	c, slot, p, f := watchedChat(t, e, ctx, watchPlanDelete)
	events, cancel := e.m.Subscribe(c.ID)
	defer cancel()
	req := platform.Request{Method: http.MethodDelete, Path: "/datasets/5",
		Body: json.RawMessage(`{"key":"-","csrftoken":"-","sessionid":"-"}`)}
	done := make(chan platform.Result, 1)
	go func() {
		r, _ := e.m.PlatformCall(ctx, c.ID, slot, "cli", req)
		done <- r
	}()
	ap := waitEvent(t, events, "approval", "").Data.(store.Approval)
	for ap.State != store.ApprovalPending {
		ap = waitEvent(t, events, "approval", "").Data.(store.Approval)
	}
	select {
	case res := <-done:
		if res.Status != "pending_user" || !strings.Contains(res.Message, "waiting for the user's approval of step 1") {
			t.Fatalf("the call did not answer at once: %+v", res)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the call waited for the user instead of parking the step")
	}
	steps := stepsOfPlan(t, e, p.ID)
	if steps[0].ParkedApproval != ap.ID || steps[0].State != plan.StepOpen {
		t.Fatalf("the step does not carry the parked approval: %+v", steps[0])
	}
	if n := len(f.writeCalls()); n != 0 {
		t.Fatalf("something ran before the decision: %v", f.writeCalls())
	}
	// The step is not covered, so the plan says so in its preview.
	pl, err := e.st.GetPlan(ctx, p.ID)
	if err != nil || pl.State != plan.StateApproved {
		t.Fatalf("plan: %+v %v", pl, err)
	}
}

// A-E2 (D2.8): on approval the gateway repeats exactly the stored call — through the whole check again —
// and wakes the agent with the result.
func TestParkedApprovalRunsExactStepOnApprove(t *testing.T) {
	e := setup(t)
	ctx := store.WithSession(context.Background(), "main")
	c, slot, p, f := watchedChat(t, e, ctx, watchPlanDelete)
	ap := parkStep(t, e, ctx, c, slot)
	if _, err := e.m.Decide(ctx, ap.ID, true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return len(f.writeCalls()) == 1 })
	if got := f.writeCalls()[0]; got != "DELETE /datasets/5" {
		t.Fatalf("the repeated call: %v", got)
	}
	// The step is written before the plan completes (finishParkedStep: FinishPlanStep, then allDone and
	// FinishPlan), so waiting for the step alone can see the plan still approved. Wait for the state the test
	// is about — the completed plan — not for the write that happens to come first (issue #157).
	waitFor(t, func() bool {
		pl, err := e.st.GetPlan(ctx, p.ID)
		return err == nil && pl.State == plan.StateCompleted
	})
	waitFor(t, func() bool { return len(e.agent(0).prompts()) > 0 })
	if text := strings.Join(e.agent(0).prompts(), "\n"); !strings.Contains(text, "ran after your approval") {
		t.Fatalf("the note does not report the run step:\n%s", text)
	}
}

// parkStep makes the call of an irreversible step of an autonomous plan and returns its parked approval.
func parkStep(t *testing.T, e *env, ctx context.Context, c ChatView, slot string) store.Approval {
	t.Helper()
	events, cancel := e.m.Subscribe(c.ID)
	defer cancel()
	req := platform.Request{Method: http.MethodDelete, Path: "/datasets/5",
		Body: json.RawMessage(`{"key":"-","csrftoken":"-","sessionid":"-"}`)}
	done := make(chan platform.Result, 1)
	go func() {
		r, _ := e.m.PlatformCall(ctx, c.ID, slot, "cli", req)
		done <- r
	}()
	ap := waitEvent(t, events, "approval", "").Data.(store.Approval)
	for ap.State != store.ApprovalPending {
		ap = waitEvent(t, events, "approval", "").Data.(store.Approval)
	}
	select {
	case res := <-done:
		if res.Status != "pending_user" {
			t.Fatalf("the step was not parked: %+v", res)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the call did not answer at once")
	}
	return ap
}

// A-E2 (D2.8): a rejection ends the step and the plan, and the agent learns why.
func TestParkedApprovalRejectFailsPlan(t *testing.T) {
	e := setup(t)
	ctx := store.WithSession(context.Background(), "main")
	c, slot, p, f := watchedChat(t, e, ctx, watchPlanDelete)
	ap := parkStep(t, e, ctx, c, slot)
	if _, err := e.m.Decide(ctx, ap.ID, false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return stepsOfPlan(t, e, p.ID)[0].State == plan.StepFailed })
	steps := stepsOfPlan(t, e, p.ID)
	if !strings.Contains(steps[0].Result, "rejected by the user") {
		t.Fatalf("the step does not say why: %+v", steps[0])
	}
	if pl, err := e.st.GetPlan(ctx, p.ID); err != nil || pl.State != plan.StateFailed {
		t.Fatalf("plan: %+v %v", pl, err)
	}
	if n := len(f.writeCalls()); n != 0 {
		t.Fatalf("a rejected step ran: %v", f.writeCalls())
	}
	waitFor(t, func() bool { return len(e.agent(0).prompts()) > 0 })
	if text := strings.Join(e.agent(0).prompts(), "\n"); !strings.Contains(text, "was not run") {
		t.Fatalf("the note does not report the rejection:\n%s", text)
	}
}

// A-E2 (D2.8): a parked approval does not outlive its plan. If the plan ends while the approval waits, the
// approval runs nothing — not even after the user approves it.
func TestParkedApprovalExpiresWithPlan(t *testing.T) {
	e := setup(t)
	ctx := store.WithSession(context.Background(), "main")
	c, slot, p, f := watchedChat(t, e, ctx, watchPlanDelete)
	ap := parkStep(t, e, ctx, c, slot)
	if _, err := e.m.CancelPlan(ctx, c.ID, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.Decide(ctx, ap.ID, true); err != nil {
		t.Fatal(err)
	}
	// A negative check: give the goroutine of the parked approval time to act if it would.
	time.Sleep(200 * time.Millisecond)
	if n := len(f.writeCalls()); n != 0 {
		t.Fatalf("a step of an ended plan ran: %v", f.writeCalls())
	}
	if steps := stepsOfPlan(t, e, p.ID); steps[0].State != plan.StepSkipped {
		t.Fatalf("the step of the cancelled plan: %+v", steps[0])
	}
}

// A-E2 (D2.8, security): the marker of a parked approval only opens that one step. Any other call with the
// same marker asks the user again.
func TestParkedMarkerOnlyForThatStep(t *testing.T) {
	e := setup(t)
	ctx := store.WithSession(context.Background(), "main")
	c, slot, p, f := watchedChat(t, e, ctx, watchPlanDelete)
	ap := parkStep(t, e, ctx, c, slot)

	marked := WithParkedApproval(ctx, ap.ID)
	// The call the marker opens: exactly the step that was parked.
	pl, err := e.st.GetPlan(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	exact, _, err := e.m.parkedCall(ctx, pl, stepsOfPlan(t, e, p.ID)[0])
	if err != nil {
		t.Fatal(err)
	}
	if !e.m.parkedHere(marked, c.ID, exact) {
		t.Fatal("the marker does not open its own step")
	}
	// Another call, even on the same path, is not that step: it goes through the whole check again and is
	// refused by the plan's grant (the plan covers the delete of dataset 5 with this body, nothing else).
	other := platform.Request{Method: http.MethodDelete, Path: "/datasets/5",
		Body: json.RawMessage(`{"key":"x","csrftoken":"-","sessionid":"-"}`)}
	if e.m.parkedHere(marked, c.ID, other) {
		t.Fatal("the marker opened another call")
	}
	res, err := e.m.PlatformCall(marked, c.ID, slot, "cli", other)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status == "ok" || res.Status == "pending_user" {
		t.Fatalf("a call the marker does not name ran anyway: %+v", res)
	}
	if n := len(f.writeCalls()); n != 0 {
		t.Fatalf("something ran: %v", f.writeCalls())
	}
	// The parked step itself is untouched.
	if steps := stepsOfPlan(t, e, p.ID); steps[0].ParkedApproval != ap.ID || steps[0].State != plan.StepOpen {
		t.Fatalf("the parked step changed: %+v", steps[0])
	}
	// And the parked approval is still pending: nothing decided it.
	got, err := e.st.GetApproval(ctx, ap.ID)
	if err != nil || got.State != store.ApprovalPending {
		t.Fatalf("the parked approval: %+v %v", got, err)
	}
}

// A-E2 (D2.8): a step whose route asks for every single call is parked as well — the trait survives the
// plan being loaded again (it is stored with the step).
func TestConfirmEachStepParked(t *testing.T) {
	e := setup(t)
	ctx := store.WithSession(context.Background(), "main")
	c, slot, p, f := watchedChat(t, e, ctx, watchPlanDeploy)
	// The edge guard only lets the chat start its own deployment (edge D-E1): the step is one this chat
	// created. Everything else about the call is the same.
	if err := e.st.AddDelegationObject(ctx, c.ID, delegation.ContainerDeployment, "5"); err != nil {
		t.Fatal(err)
	}
	steps := stepsOfPlan(t, e, p.ID)
	if steps[0].Covered {
		t.Fatalf("a confirm-each step must not be covered by its plan: %+v", steps[0])
	}
	if !steps[0].ConfirmEach {
		t.Fatalf("the confirm-each trait was lost with the plan: %+v", steps[0])
	}
	events, cancel := e.m.Subscribe(c.ID)
	defer cancel()
	done := make(chan platform.Result, 1)
	go func() {
		r, _ := e.m.PlatformCall(ctx, c.ID, slot, "cli", platform.Request{Method: http.MethodPut, Path: "/container-deployments/5/deploy"})
		done <- r
	}()
	ap := waitEvent(t, events, "approval", "").Data.(store.Approval)
	for ap.State != store.ApprovalPending {
		ap = waitEvent(t, events, "approval", "").Data.(store.Approval)
	}
	select {
	case res := <-done:
		if res.Status != "pending_user" {
			t.Fatalf("a confirm-each step was not parked: %+v", res)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a confirm-each step waited instead of being parked")
	}
	if steps := stepsOfPlan(t, e, p.ID); steps[0].ParkedApproval != ap.ID {
		t.Fatalf("the step does not carry the parked approval: %+v", steps[0])
	}
	if n := len(f.writeCalls()); n != 0 {
		t.Fatalf("the deploy ran without the user: %v", f.writeCalls())
	}

	// The user approves: the gateway repeats the deploy. Its task is observed like any other job, so the
	// step waits for the device to report the deployment up.
	if _, err := e.m.Decide(ctx, ap.ID, true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		return len(f.writeCalls()) == 1 && f.writeCalls()[0] == "PUT /container-deployments/5/deploy"
	})
	f.set("GET /tasks/5", `{"status":"completed","completion_percentage":1.0}`)
	f.set("GET /container-deployments/5", `{"status":"deployed"}`)
	runWatchersOnce(t, e)
	waitFor(t, func() bool { return stepsOfPlan(t, e, p.ID)[0].State == plan.StepDone })
	if pl, err := e.st.GetPlan(ctx, p.ID); err != nil || pl.State != plan.StateCompleted {
		t.Fatalf("plan: %+v %v", pl, err)
	}
}

// The outputs of a poll reach the stored form with their size (own code C-E4): the card shows the size of a
// result file, and the kinds that name none leave the field away.
func TestStoreOutputsCarriesTheSize(t *testing.T) {
	got := storeOutputs([]watch.Output{{Resource: "notebook_file", ID: "r1/model.onnx", Label: "model.onnx", Size: 1234}})
	if len(got) != 1 || got[0].Size != 1234 || got[0].Resource != "notebook_file" || got[0].ID != "r1/model.onnx" {
		t.Fatalf("outputs: %+v", got)
	}
	if plain := storeOutputs([]watch.Output{{Resource: "container_image", ID: "u/i:1"}}); len(plain) != 1 || plain[0].Size != 0 {
		t.Errorf("a kind without a size: %+v", plain)
	}
}

// A-E2 (D2.8, exception): a step whose call carries files of the sandbox is not parked — the gateway
// cannot repeat it without the running sandbox, and the approval has to show the bytes that were read.
func TestStepWithFilesAsksSynchronously(t *testing.T) {
	e := setup(t)
	ctx := store.WithSession(context.Background(), "main")
	f := newFakeJobs(t, e, "/tasks/5")
	nb := &fakeNotebook{}
	e.m.opt.Notebook = nb
	e.m.RegisterVirtual(platform.NotebookPrefix, nb)
	// A run in the notebook server asks for every single call and carries the code from the sandbox.
	const args = `{"files":["/workspace/train.py"],"entry":"train.py","dataset_ids":["17"]}`
	body := `{"title":"Run","mode":"autonomous","budget":{"max_duration_minutes":60,"max_llm_cost_usd":1,"max_auto_turns":3},
 "steps":[{"tool":"notebook_run","args":` + args + `,"file_sha256":["` + strings.Repeat("a", 64) + `"]}]}`
	c, _, p := autonomousChat(t, e, ctx, body)
	slot := e.m.live[c.ID].slot.ID
	steps := stepsOfPlan(t, e, p.ID)
	if steps[0].Covered {
		t.Fatalf("a notebook run must not be covered by a plan: %+v", steps[0])
	}
	tool, ok := platform.Lookup("notebook_run")
	if !ok {
		t.Fatal("the tool notebook_run is missing")
	}
	req, err := tool.Build(json.RawMessage(args))
	if err != nil {
		t.Fatal(err)
	}
	req.Uploads = []platform.Upload{{Field: "file", Name: "train.py", Data: []byte("print('hi')\n"), SHA256: "aa11"}}
	if len(req.Files) == 0 {
		t.Fatal("the call of a notebook run carries no files")
	}
	// Without files the step is parked; with them it is not.
	if !e.m.parkedStep(ctx, &steps[0], platform.Request{Method: req.Method, Path: req.Path}) {
		t.Error("a step without files must be parked")
	}
	if e.m.parkedStep(ctx, &steps[0], req) {
		t.Fatal("a step whose call carries files must be asked synchronously")
	}
	// And the call really asks: it waits for the user instead of answering at once.
	events, cancel := e.m.Subscribe(c.ID)
	defer cancel()
	done := make(chan platform.Result, 1)
	go func() {
		r, _ := e.m.PlatformCall(platform.WithInvocation(ctx, "notebook_run", json.RawMessage(args)), c.ID, slot, "cli", req)
		done <- r
	}()
	ap := waitEvent(t, events, "approval", "").Data.(store.Approval)
	for ap.State != store.ApprovalPending {
		ap = waitEvent(t, events, "approval", "").Data.(store.Approval)
	}
	select {
	case res := <-done:
		t.Fatalf("a step with files must wait for the user: %+v", res)
	case <-time.After(200 * time.Millisecond):
	}
	if steps := stepsOfPlan(t, e, p.ID); steps[0].ParkedApproval != "" {
		t.Fatalf("a step with files was parked: %+v", steps[0])
	}
	if nb.count() != 0 {
		t.Fatalf("the run reached the notebook server before the decision: %d", nb.count())
	}
	// The user rejects it: nothing runs, and the step stays open for another try.
	if _, err := e.m.Decide(ctx, ap.ID, false); err != nil {
		t.Fatal(err)
	}
	if res := <-done; res.Status == "ok" {
		t.Fatalf("the call ran after a rejection: %+v", res)
	}
	if nb.count() != 0 || len(f.writeCalls()) != 0 {
		t.Fatalf("something ran: %d %v", nb.count(), f.writeCalls())
	}
}

// A-E2 (D2.8): a step that already waits for the user's decision is not asked a second time — the second
// approval would run the same irreversible call twice.
func TestParkedStepNotAskedTwice(t *testing.T) {
	e := setup(t)
	ctx := store.WithSession(context.Background(), "main")
	c, slot, p, f := watchedChat(t, e, ctx, watchPlanDelete)
	ap := parkStep(t, e, ctx, c, slot)
	// The same call again, without the marker: the answer is the one of the first call.
	req := platform.Request{Method: http.MethodDelete, Path: "/datasets/5",
		Body: json.RawMessage(`{"csrftoken":"-","key":"-","sessionid":"-"}`)}
	res, err := e.m.PlatformCall(ctx, c.ID, slot, "cli", req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "pending_user" || !strings.Contains(res.Message, "waiting for the user's approval of step 1") {
		t.Fatalf("the second call: %+v", res)
	}
	aps, err := e.st.ListApprovals(ctx, store.ApprovalPending, c.ID)
	if err != nil || len(aps) != 1 || aps[0].ID != ap.ID {
		t.Fatalf("there must be exactly the parked approval: %+v %v", aps, err)
	}
	if steps := stepsOfPlan(t, e, p.ID); steps[0].ParkedApproval != ap.ID {
		t.Fatalf("the step changed: %+v", steps[0])
	}
	if n := len(f.writeCalls()); n != 0 {
		t.Fatalf("something ran: %v", f.writeCalls())
	}
}

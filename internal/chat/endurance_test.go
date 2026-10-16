// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"agw/internal/oidc"
	"agw/internal/plan"
	"agw/internal/platform"
	"agw/internal/store"
)

// fakeSessions is the login side of the gateway for these tests (A-E3): it answers what the test needs and
// keeps the callback a login would call.
type fakeSessions struct {
	mu      sync.Mutex
	calls   int
	noLogin bool
	ends    time.Time // zero: the owner has no session
	onSub   func(sub string)
}

func (f *fakeSessions) KeepFresh(ctx context.Context, sub string) error {
	f.mu.Lock()
	f.calls++
	noLogin := f.noLogin
	f.mu.Unlock()
	if noLogin {
		return oidc.ErrNoSession
	}
	return nil
}

func (f *fakeSessions) SessionEnds(sub string) (time.Time, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ends.IsZero() {
		return time.Time{}, false
	}
	return f.ends, true
}

func (f *fakeSessions) OnSession(fn func(sub string)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onSub = fn
}

// login runs what the oidc service does on a successful login (A-E3, D3.6).
func (f *fakeSessions) login(sub string) {
	f.mu.Lock()
	fn := f.onSub
	f.mu.Unlock()
	if fn == nil {
		panic("no login callback registered")
	}
	fn(sub)
}

func (f *fakeSessions) refreshes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeSessions) registered() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.onSub != nil
}

// startEndurance runs the endurance loop of the manager and waits until it has registered its login
// callback, so that a test can call sessions.login right away.
func startEndurance(t *testing.T, e *env, sess *fakeSessions) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go e.m.RunPlanEndurance(ctx)
	deadline := time.Now().Add(3 * time.Second)
	for !sess.registered() {
		if time.Now().After(deadline) {
			t.Fatal("the endurance loop did not register its login callback")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// approvedAutonomous stores an approved autonomous plan of the chat, without the approval flow (A-E3).
func approvedAutonomous(t *testing.T, e *env, ctx context.Context, chatID string, steps ...store.PlanStep) store.Plan {
	t.Helper()
	if len(steps) == 0 {
		steps = []store.PlanStep{{Idx: 1, Method: "PATCH", Path: "/models/5", Effect: plan.EffectWrite, Covered: true, MaxAttempts: 1}}
	}
	p, err := e.st.CreatePlan(ctx, store.Plan{ChatID: chatID, Title: "Autonom", Session: store.SessionFrom(ctx), Via: "cli",
		Mode: plan.ModeAutonomous, Budget: store.PlanBudget{MaxDurationS: 28800, MaxLLMCost: 1, MaxAutoTurns: 20},
		Grant: json.RawMessage(`{"rules":[]}`), Steps: steps})
	if err != nil {
		t.Fatal(err)
	}
	exp := time.Now().Add(time.Hour)
	if ok, err := e.st.DecidePlan(ctx, p.ID, plan.StateApproved, &exp); err != nil || !ok {
		t.Fatalf("approve: ok=%v err=%v", ok, err)
	}
	return p
}

// ownedChat creates a chat that belongs to sub (A-E3: the endurance loop works per owner).
func ownedChat(t *testing.T, e *env, ctx context.Context, sub string) ChatView {
	t.Helper()
	c, err := e.m.Create(ctx, NewChat{Owner: sub})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// withNoLoginPlatform attaches a platform binding whose subject token is gone: every call fails with
// oidc.ErrNoSession before a request goes out (A-E3, D3.5). It is installed after a plan was proposed, so
// that the summaries of the proposal still read.
func withNoLoginPlatform(t *testing.T, e *env) {
	t.Helper()
	c, err := platform.New(platform.Config{APIURL: "http://127.0.0.1:1", Subject: func(context.Context, string) (string, error) {
		return "", oidc.ErrNoSession
	}})
	if err != nil {
		t.Fatal(err)
	}
	e.m.opt.Platform = c
}

// A-E3 (D3.1): while an autonomous plan is approved the loop renews the owner's login; without a login the
// plan is paused with reason "login" and the user is asked for attention (D3.5) instead of failing.
func TestKeepAlivePausesOnLostLogin(t *testing.T) {
	e := setup(t)
	ctx := store.WithSession(context.Background(), "main")
	c := ownedChat(t, e, ctx, "sub-anna")
	p := approvedAutonomous(t, e, ctx, c.ID)
	sess := &fakeSessions{noLogin: true}
	e.m.opt.Sessions = sess
	events, cancel := e.m.Subscribe(c.ID)
	defer cancel()
	e.m.keepPlansLoggedIn(ctx)
	if n := sess.refreshes(); n != 1 {
		t.Fatalf("the login was renewed %d times, want 1", n)
	}
	got, err := e.st.GetPlan(ctx, p.ID)
	if err != nil || got.State != plan.StatePaused || got.PauseReason != store.PauseLogin {
		t.Fatalf("after the lost login: %+v %v", got, err)
	}
	ev := waitEvent(t, events, "attention", "")
	if data, ok := ev.Data.(map[string]string); !ok || data["plan_id"] != p.ID || data["reason"] != store.PauseLogin {
		t.Fatalf("attention event: %+v", ev.Data)
	}
	// A second tick with a live login renews and leaves the plan alone.
	sess.mu.Lock()
	sess.noLogin = false
	sess.ends = time.Now().Add(time.Hour)
	sess.mu.Unlock()
	e.m.keepPlansLoggedIn(ctx)
	if got, _ := e.st.GetPlan(ctx, p.ID); got.State != plan.StatePaused {
		t.Fatalf("a renewal must not resume a plan by itself: %+v", got)
	}
}

// A-E3 (D3.6): the owner's next login continues the plans a lost login had paused — and only those: a plan
// paused for another reason keeps waiting for the user.
func TestLoginResumesPlan(t *testing.T) {
	e := setup(t)
	ctx := store.WithSession(context.Background(), "main")
	c := ownedChat(t, e, ctx, "sub-anna")
	p := approvedAutonomous(t, e, ctx, c.ID)
	if ok, err := e.st.PausePlan(ctx, p.ID, store.PauseLogin); err != nil || !ok {
		t.Fatalf("pause: %v %v", ok, err)
	}
	restarted := ownedChat(t, e, ctx, "sub-anna")
	r := approvedAutonomous(t, e, ctx, restarted.ID)
	if ok, err := e.st.PausePlan(ctx, r.ID, store.PauseRestart); err != nil || !ok {
		t.Fatalf("pause: %v %v", ok, err)
	}
	sess := &fakeSessions{}
	e.m.opt.Sessions = sess
	startEndurance(t, e, sess)
	events, cancel := e.m.Subscribe(c.ID)
	defer cancel()
	sess.login("sub-anna")
	got, err := e.st.GetPlan(ctx, p.ID)
	if err != nil || got.State != plan.StateApproved || got.PauseReason != "" {
		t.Fatalf("after the login: %+v %v", got, err)
	}
	waitEvent(t, events, "plan", "")
	// D3.4: a plan that a restart interrupted waits for the user, the login does not continue it.
	if r2, _ := e.st.GetPlan(ctx, r.ID); r2.State != plan.StatePaused || r2.PauseReason != store.PauseRestart {
		t.Fatalf("a plan paused after a restart keeps waiting: %+v", r2)
	}
}

// A-E3 (security): a login continues only the plans of that user — the plans of another owner stay paused.
func TestLoginResumesOnlyOwnPlans(t *testing.T) {
	e := setup(t)
	ctx := store.WithSession(context.Background(), "main")
	anna := ownedChat(t, e, ctx, "sub-anna")
	bert := ownedChat(t, e, ctx, "sub-bert")
	mine := approvedAutonomous(t, e, ctx, anna.ID)
	foreign := approvedAutonomous(t, e, ctx, bert.ID)
	for _, id := range []string{mine.ID, foreign.ID} {
		if ok, err := e.st.PausePlan(ctx, id, store.PauseLogin); err != nil || !ok {
			t.Fatalf("pause: %v %v", ok, err)
		}
	}
	sess := &fakeSessions{}
	e.m.opt.Sessions = sess
	startEndurance(t, e, sess)
	sess.login("sub-anna")
	if got, _ := e.st.GetPlan(ctx, mine.ID); got.State != plan.StateApproved {
		t.Fatalf("own plan not continued: %+v", got)
	}
	if got, _ := e.st.GetPlan(ctx, foreign.ID); got.State != plan.StatePaused || got.PauseReason != store.PauseLogin {
		t.Fatalf("a foreign plan must not be continued: %+v", got)
	}
}

// A-E3 (D3.4): a step a restart interrupted stays failed while the plan is paused — and a failed step blocks
// the plan, because the agent may only run its next open one. The user's resume is the repeat the failure
// message asks for, so it puts the failed step back to open (keeping the attempt the interrupted call used).
func TestResumeReopensFailedStep(t *testing.T) {
	e := setup(t)
	ctx := store.WithSession(context.Background(), "main")
	c := ownedChat(t, e, ctx, "sub-anna")
	p := approvedAutonomous(t, e, ctx, c.ID)
	if ok, err := e.st.ClaimPlanStep(ctx, p.ID, 1); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	if ok, err := e.st.FinishPlanStep(ctx, p.ID, 1, plan.StepRunning, plan.StepFailed,
		"orchestrator restarted during the call; check the platform before retrying", "", ""); err != nil || !ok {
		t.Fatalf("fail the step: %v %v", ok, err)
	}
	if ok, err := e.st.PausePlan(ctx, p.ID, store.PauseRestart); err != nil || !ok {
		t.Fatalf("pause: %v %v", ok, err)
	}
	if _, err := e.m.ResumePlan(ctx, c.ID, p.ID); err != nil {
		t.Fatal(err)
	}
	got, err := e.st.GetPlan(ctx, p.ID)
	if err != nil || got.State != plan.StateApproved {
		t.Fatalf("resumed plan: %+v %v", got, err)
	}
	if st := got.Steps[0]; st.State != plan.StepOpen || st.Result != "" || st.DoneAt != nil || st.Attempts != 1 {
		t.Fatalf("the failed step must be open again and keep its attempt: %+v", st)
	}
}

// A-E3 (D3.3): an autonomous plan ends at the latest one hour before the login session of its owner ends;
// the preview names that end as a time. Without a session the plan's own duration is the only bound.
func TestDurationCappedBySessionEnd(t *testing.T) {
	e := setup(t)
	withPlanPlatform(t, e)
	ctx := store.WithSession(context.Background(), "main")
	c := ownedChat(t, e, ctx, "sub-anna")
	slot := e.m.live[c.ID].slot.ID
	end := time.Now().Add(3 * time.Hour)
	e.m.opt.Sessions = &fakeSessions{ends: end}
	body := `{"title":"Autonom","mode":"autonomous","budget":{"max_duration_minutes":480,"max_llm_cost_usd":1,"max_auto_turns":5},
 "steps":[{"method":"PATCH","path":"/models/5","body":` + modelBody + `}]}`
	r, ap := proposeAndDecide(t, e, ctx, c.ID, slot, body, true)
	if r.Status != "ok" {
		t.Fatalf("proposal not approved: %+v (%s)", r, ap.Preview)
	}
	plans, err := e.m.Plans(ctx, c.ID)
	if err != nil || len(plans) == 0 {
		t.Fatalf("plans: %+v %v", plans, err)
	}
	p := plans[len(plans)-1]
	if p.ExpiresAt == nil {
		t.Fatal("no expiry stored")
	}
	if left := time.Until(*p.ExpiresAt); left < 119*time.Minute || left > 121*time.Minute {
		t.Fatalf("expiry in %v, want about 2h (the session ends in 3h, minus one hour)", left)
	}
	if want := end.Add(-time.Hour).Format("2006-01-02 15:04 MST"); !strings.Contains(ap.Preview, want) {
		t.Fatalf("the preview does not name the end %q: %s", want, ap.Preview)
	}
}

// A-E3 (D3.3): a session that ends within the hour leaves no room for an autonomous plan; the proposal is
// refused with the reason instead of being approved and expiring at once.
func TestProposeRefusedWhenSessionEndsSoon(t *testing.T) {
	e := setup(t)
	withPlanPlatform(t, e)
	ctx := store.WithSession(context.Background(), "main")
	c := ownedChat(t, e, ctx, "sub-anna")
	slot := e.m.live[c.ID].slot.ID
	e.m.opt.Sessions = &fakeSessions{ends: time.Now().Add(30 * time.Minute)}
	// No approval is raised, so the call goes through PlatformCall directly.
	req, err := platform.Normalize(platform.Request{Method: "POST", Path: platform.PlanPath, Body: json.RawMessage(autoPlanBody)})
	if err != nil {
		t.Fatal(err)
	}
	r, err := e.m.PlatformCall(ctx, c.ID, slot, "cli", req)
	if err != nil || r.Status != "error" || !strings.Contains(r.Message, "login session ends") {
		t.Fatalf("proposal with a session ending soon: %+v %v", r, err)
	}
	if plans, err := e.m.Plans(ctx, c.ID); err != nil || len(plans) != 0 {
		t.Fatalf("a refused proposal must not create a plan: %+v %v", plans, err)
	}
}

// A-E3 (D3.5): a platform call that fails because the login is gone is no failure of the step — it goes back
// to open without using an attempt, and the plan pauses instead of failing.
func TestNoSessionPausesWithoutUsingAttempt(t *testing.T) {
	e := setup(t)
	withPlanPlatform(t, e)
	ctx := store.WithSession(context.Background(), "main")
	body := `{"title":"Autonom","mode":"autonomous","budget":{"max_duration_minutes":480,"max_llm_cost_usd":1,"max_auto_turns":5},
 "steps":[{"method":"PATCH","path":"/models/5","body":` + modelBody + `,"max_attempts":2}]}`
	c, slot, p := autonomousChat(t, e, ctx, body)
	events, cancel := e.m.Subscribe(c.ID)
	defer cancel()
	withNoLoginPlatform(t, e)
	r, err := e.m.PlatformCall(ctx, c.ID, slot, "cli", liveCall(t, "PATCH", "/models/5", modelBody))
	if err != nil || r.Status != "error" || !strings.Contains(r.Message, "login") {
		t.Fatalf("the call must fail with the login error: %+v %v", r, err)
	}
	got, err := e.st.GetPlan(ctx, p.ID)
	if err != nil || got.State != plan.StatePaused || got.PauseReason != store.PauseLogin {
		t.Fatalf("plan after the lost login: %+v %v", got, err)
	}
	st := got.Steps[0]
	if st.State != plan.StepOpen || st.Attempts != 0 {
		t.Fatalf("the step keeps its claim back: %+v", st)
	}
	if !strings.Contains(st.Result, "login") {
		t.Fatalf("the step's result names the login: %q", st.Result)
	}
	waitEvent(t, events, "attention", "")
	// The login is back: the plan continues and the step runs through (attempt one of two).
	withPlanPlatform(t, e)
	if _, err := e.m.ResumePlan(ctx, c.ID, p.ID); err != nil {
		t.Fatal(err)
	}
	if r, err := e.m.PlatformCall(ctx, c.ID, slot, "cli", liveCall(t, "PATCH", "/models/5", modelBody)); err != nil || r.Status != "ok" {
		t.Fatalf("the step after the login came back: %+v %v", r, err)
	}
	done, err := e.st.GetPlan(ctx, p.ID)
	if err != nil || done.Steps[0].State != plan.StepDone || done.Steps[0].Attempts != 1 {
		t.Fatalf("the step must run with its first attempt: %+v %v", done.Steps[0], err)
	}
	if done.State != plan.StateCompleted {
		t.Fatalf("the plan of one step is complete: %+v", done.State)
	}
}

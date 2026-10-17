// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"agw/internal/artifacts"
	"agw/internal/chat"
	"agw/internal/config"
	"agw/internal/oidc/oidctest"
	"agw/internal/platform"
	"agw/internal/pool"
	"agw/internal/store"
)

// jobCalls records the calls that arrived at the fake platform of the job routes.
type jobCalls struct {
	mu    sync.Mutex
	calls []string
}

func (f *jobCalls) names() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// jobsServer is the API of the job tests: a manager with a store and a fake platform. No chat needs a
// sandbox here — the jobs are read and stopped without one.
func jobsServer(t *testing.T, name string) (*store.Store, *httptest.Server, *oidctest.Issuer, *jobCalls) {
	t.Helper()
	url := os.Getenv("AGW_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("AGW_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	st, err := store.OpenSchema(ctx, url, name+"_"+strings.ReplaceAll(time.Now().Format("150405.000000"), ".", "_"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.DropSchema(context.Background()); st.Close() })
	cat, err := config.ParseCatalog([]byte(`{"default":"p/m","providers":[{"id":"p","upstream":"http://x","api":"openai-completions","models":[{"id":"m"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	f := &jobCalls{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			fmt.Fprint(w, `{"access_token":"t","expires_in":3600}`)
			return
		}
		f.mu.Lock()
		f.calls = append(f.calls, r.Method+" "+r.URL.Path)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprint(w, `{}`)
	}))
	t.Cleanup(srv.Close)
	plat, err := platform.New(platform.Config{APIURL: srv.URL, TokenURL: srv.URL + "/token", User: "u", Password: "p"})
	if err != nil {
		t.Fatal(err)
	}
	p := pool.New[chat.Agent](func(context.Context, string, string) (chat.Agent, error) {
		return nil, errors.New("no slot in the test")
	}, func(context.Context, chat.Agent) {}, map[string]int{"cli": 0})
	m := chat.NewManager(st, p, cat, nil, artifacts.NewBroker(),
		chat.Options{AcquireTimeout: 100 * time.Millisecond, MaxSubagents: 5, Platform: plat})
	api, is := oidcServer(t, m, p, cat, nil)
	return st, api, is, f
}

// A-E2 (S1): the jobs of a chat are readable; only the owner sees them, a foreign or unknown chat is 404.
func TestJobsRouteOwnerOnly(t *testing.T) {
	st, srv, is, _ := jobsServer(t, "apitest_jobs")
	ctx := context.Background()
	c, err := st.CreateChat(ctx, store.NewChat{Title: "jobs", Model: "p/m", Variant: "cli", Owner: anna.Sub})
	if err != nil {
		t.Fatal(err)
	}
	jobID, err := st.AddWatchJob(ctx, store.WatchJob{ChatID: c.ID, Kind: "platform_task", Ref: "task:5"})
	if err != nil {
		t.Fatal(err)
	}
	a := loggedIn(t, srv, is, anna)
	b := loggedIn(t, srv, is, bert)
	base := srv.URL + "/api/chats/" + c.ID

	code, body := call(t, a, "GET", base+"/jobs", "")
	if code != 200 || !strings.Contains(body, jobID) || !strings.Contains(body, `"kind":"platform_task"`) {
		t.Fatalf("own jobs: %d %s", code, body)
	}
	if code, body := call(t, b, "GET", base+"/jobs", ""); code != 404 {
		t.Fatalf("Bert reads Anna's jobs: %d %s", code, body)
	}
	if code, _ := call(t, a, "GET", srv.URL+"/api/chats/00000000-0000-0000-0000-000000000001/jobs", ""); code != 404 {
		t.Fatalf("unknown chat: %d", code)
	}
	// The route needs a login at all.
	anon := oidctest.NewBrowser(t)
	if code, _ := call(t, anon, "GET", base+"/jobs", ""); code != 401 {
		t.Fatalf("without a session: %d", code)
	}
}

// A-E2 (D2.9): stopping a job is the user's own action — it runs with the user's token, is logged as a
// call of the user, and only a running job of a stoppable kind can be stopped.
func TestStopJobUserOnly(t *testing.T) {
	st, srv, is, f := jobsServer(t, "apitest_stopjob")
	ctx := context.Background()
	c, err := st.CreateChat(ctx, store.NewChat{Title: "jobs", Model: "p/m", Variant: "cli", Owner: anna.Sub})
	if err != nil {
		t.Fatal(err)
	}
	jobID, err := st.AddWatchJob(ctx, store.WatchJob{ChatID: c.ID, Kind: "training_run", Ref: "9"})
	if err != nil {
		t.Fatal(err)
	}
	a := loggedIn(t, srv, is, anna)
	b := loggedIn(t, srv, is, bert)
	base := srv.URL + "/api/chats/" + c.ID + "/jobs/" + jobID + "/stop"

	if code, body := call(t, b, "POST", base, ""); code != 404 {
		t.Fatalf("Bert stopped Anna's job: %d %s", code, body)
	}
	code, body := call(t, a, "POST", base, "")
	if code != 200 || !strings.Contains(body, `"state":"running"`) {
		t.Fatalf("stop: %d %s", code, body)
	}
	if got := f.names(); len(got) != 1 || got[0] != "POST /train/containers/9/stop" {
		t.Fatalf("calls: %v", got)
	}
	// It is logged as a call of the user, not as one of the agent.
	calls, err := st.ListSocketCalls(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, sc := range calls {
		if sc.Via == "user" && strings.Contains(sc.Detail, "POST /train/containers/9/stop") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the stop is not logged as a call of the user: %+v", calls)
	}
	// A finished job cannot be stopped again.
	if ok, err := st.FinishWatchJob(ctx, jobID, store.JobDone); err != nil || !ok {
		t.Fatalf("finish: %v %v", ok, err)
	}
	if code, body := call(t, a, "POST", base, ""); code != 409 {
		t.Fatalf("stopping a finished job: %d %s", code, body)
	}
}

// A-E2 (D2.9): a kind without a stop call and a plan step that does not wait for a job are refused with
// 409; an unknown job and an unknown plan are 404.
func TestStopJobNotStoppable(t *testing.T) {
	st, srv, is, f := jobsServer(t, "apitest_nostop")
	ctx := context.Background()
	c, err := st.CreateChat(ctx, store.NewChat{Title: "jobs", Model: "p/m", Variant: "cli", Owner: anna.Sub})
	if err != nil {
		t.Fatal(err)
	}
	a := loggedIn(t, srv, is, anna)
	base := srv.URL + "/api/chats/" + c.ID

	jobID, err := st.AddWatchJob(ctx, store.WatchJob{ChatID: c.ID, Kind: "platform_task", Ref: "task:5"})
	if err != nil {
		t.Fatal(err)
	}
	if code, body := call(t, a, "POST", base+"/jobs/"+jobID+"/stop", ""); code != 409 {
		t.Fatalf("a platform task has no stop: %d %s", code, body)
	}
	if code, _ := call(t, a, "POST", base+"/jobs/w000000000000/stop", ""); code != 404 {
		t.Fatalf("unknown job: %d", code)
	}
	if got := f.names(); len(got) != 0 {
		t.Fatalf("something ran: %v", got)
	}

	// The stop of a plan step: only a step that waits for a job of a stoppable kind.
	p, err := st.CreatePlan(ctx, store.Plan{ChatID: c.ID, Title: "train", Via: "cli", Steps: []store.PlanStep{
		{Idx: 1, Method: "POST", Path: "/train/config", Effect: "compute"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Now().Add(30 * time.Minute)
	if ok, err := st.DecidePlan(ctx, p.ID, "approved", &expires); err != nil || !ok {
		t.Fatalf("approve: %v %v", ok, err)
	}
	trainJob, err := st.AddWatchJob(ctx, store.WatchJob{ChatID: c.ID, PlanID: p.ID, StepIdx: 1, Kind: "training_run", Ref: "9"})
	if err != nil {
		t.Fatal(err)
	}
	stepBase := base + "/plans/" + p.ID + "/steps/1/stop"
	// The step is still open: nothing to stop.
	if code, body := call(t, a, "POST", stepBase, ""); code != 409 {
		t.Fatalf("an open step cannot be stopped: %d %s", code, body)
	}
	if ok, err := st.ClaimPlanStep(ctx, p.ID, 1); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	if ok, err := st.WaitPlanStep(ctx, p.ID, 1, trainJob, "ok 202", "training", "9"); err != nil || !ok {
		t.Fatalf("wait: %v %v", ok, err)
	}
	code, body := call(t, a, "POST", stepBase, "")
	if code != 200 || !strings.Contains(body, `"watch_job":"`+trainJob+`"`) {
		t.Fatalf("stop of a waiting step: %d %s", code, body)
	}
	if got := f.names(); len(got) != 1 || got[0] != "POST /train/containers/9/stop" {
		t.Fatalf("calls: %v", got)
	}
	// The step waits, but its job is of a kind without a stop call: the answer is 409 with the job's own
	// reason, not with the plan's state (a message like "plan is job this job cannot be stopped" was
	// unreadable on the instance, 17.10.2026).
	p2, err := st.CreatePlan(ctx, store.Plan{ChatID: c.ID, Title: "build", Via: "cli", Steps: []store.PlanStep{
		{Idx: 1, Method: "POST", Path: "/container-images/build", Effect: "compute"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := st.DecidePlan(ctx, p2.ID, "approved", &expires); err != nil || !ok {
		t.Fatalf("approve: %v %v", ok, err)
	}
	taskJob, err := st.AddWatchJob(ctx, store.WatchJob{ChatID: c.ID, PlanID: p2.ID, StepIdx: 1, Kind: "platform_task", Ref: "task:9"})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := st.ClaimPlanStep(ctx, p2.ID, 1); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	if ok, err := st.WaitPlanStep(ctx, p2.ID, 1, taskJob, "ok 202", "image_build", "task:9"); err != nil || !ok {
		t.Fatalf("wait: %v %v", ok, err)
	}
	code, body = call(t, a, "POST", base+"/plans/"+p2.ID+"/steps/1/stop", "")
	if code != 409 || !strings.Contains(body, "this job cannot be stopped") {
		t.Fatalf("a step whose job cannot be stopped: %d %s", code, body)
	}
	if strings.Contains(body, "plan is job") {
		t.Fatalf("the reason must not repeat the plan's state: %s", body)
	}
	// A plan that is not this chat's is 404.
	other := srv.URL + "/api/chats/" + c.ID + "/plans/00000000-0000-0000-0000-000000000002/steps/1/stop"
	if code, _ := call(t, a, "POST", other, ""); code != 404 {
		t.Fatalf("unknown plan: %d", code)
	}
	// A step that is not a number is refused at the route.
	if code, _ := call(t, a, "POST", base+"/plans/"+p.ID+"/steps/x/stop", ""); code != 400 {
		t.Fatalf("a step that is not a number: %d", code)
	}
}

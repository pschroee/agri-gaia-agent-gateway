// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"agw/internal/artifacts"
	"agw/internal/chat"
	"agw/internal/config"
	"agw/internal/pool"
	"agw/internal/store"
	"agw/internal/testpg"
)

// GET /api/chats/{id}/plans: own chats only, an empty list for a chat without plans (issue #70).
func TestPlansEndpoint(t *testing.T) {
	url := os.Getenv("AGW_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("AGW_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	st, err := store.OpenSchema(ctx, url, testpg.Schema("apitest_plans"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.DropSchema(context.Background()); st.Close() })
	cat, err := config.ParseCatalog([]byte(`{"default":"p/m","providers":[{"id":"p","upstream":"http://x","api":"openai-completions","models":[{"id":"m"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	p := pool.New[chat.Agent](func(context.Context, string, string) (chat.Agent, error) {
		return nil, errors.New("no slot in the test")
	}, func(context.Context, chat.Agent) {}, map[string]int{"cli": 0})
	m := chat.NewManager(st, p, cat, nil, artifacts.NewBroker(), chat.Options{AcquireTimeout: 100 * time.Millisecond, MaxSubagents: 5})
	srv, is := oidcServer(t, m, p, cat, nil)
	a := loggedIn(t, srv, is, anna)
	b := loggedIn(t, srv, is, bert)

	c, err := st.CreateChat(ctx, store.NewChat{Title: "plans", Model: "p/m", Variant: "cli", Owner: anna.Sub})
	if err != nil {
		t.Fatal(err)
	}
	_ = st.SetState(ctx, c.ID, store.StateDormant)
	if code, body := call(t, a, "GET", srv.URL+"/api/chats/"+c.ID+"/plans", ""); code != 200 || (body != "[]\n" && body != "[]") {
		t.Fatalf("chat without plans: %d %q", code, body)
	}
	if code, body := call(t, b, "GET", srv.URL+"/api/chats/"+c.ID+"/plans", ""); code != 404 {
		t.Errorf("Bert on Anna's chat: %d %s", code, body)
	}
	if code, body := call(t, a, "GET", srv.URL+"/api/chats/00000000-0000-0000-0000-000000000000/plans", ""); code != 404 {
		t.Errorf("unknown chat: %d %s", code, body)
	}
}

// A-E1: the plan controls check the owner; a foreign or unknown plan is 404 and a wrong state is 409.
func TestPlanControlsOwnerOnly(t *testing.T) {
	url := os.Getenv("AGW_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("AGW_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	st, err := store.OpenSchema(ctx, url, testpg.Schema("apitest_planctl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.DropSchema(context.Background()); st.Close() })
	cat, err := config.ParseCatalog([]byte(`{"default":"p/m","providers":[{"id":"p","upstream":"http://x","api":"openai-completions","models":[{"id":"m"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	p := pool.New[chat.Agent](func(context.Context, string, string) (chat.Agent, error) {
		return nil, errors.New("no slot in the test")
	}, func(context.Context, chat.Agent) {}, map[string]int{"cli": 0})
	m := chat.NewManager(st, p, cat, nil, artifacts.NewBroker(), chat.Options{AcquireTimeout: 100 * time.Millisecond, MaxSubagents: 5})
	srv, is := oidcServer(t, m, p, cat, nil)
	a := loggedIn(t, srv, is, anna)
	b := loggedIn(t, srv, is, bert)

	c, err := st.CreateChat(ctx, store.NewChat{Title: "plan", Model: "p/m", Variant: "cli", Owner: anna.Sub})
	if err != nil {
		t.Fatal(err)
	}
	_ = st.SetState(ctx, c.ID, store.StateDormant)
	pl, err := st.CreatePlan(ctx, store.Plan{ChatID: c.ID, Title: "Auto", Session: "main", Via: "cli", Mode: "autonomous",
		Budget: store.PlanBudget{MaxDurationS: 3600, MaxLLMCost: 1, MaxAutoTurns: 5},
		Grant:  json.RawMessage(`{"rules":[{"action":"create","resource":"dataset"}],"enforce":true}`),
		Steps:  []store.PlanStep{{Idx: 1, Method: "POST", Path: "/datasets", Effect: "write", MaxAttempts: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	exp := time.Now().Add(time.Hour)
	if ok, err := st.DecidePlan(ctx, pl.ID, "approved", &exp); err != nil || !ok {
		t.Fatalf("decide: %v %v", ok, err)
	}
	if code, body := call(t, b, "POST", srv.URL+"/api/chats/"+c.ID+"/plans/"+pl.ID+"/pause", ""); code != 404 {
		t.Errorf("Bert on Anna's plan: %d %s", code, body)
	}
	if code, body := call(t, a, "POST", srv.URL+"/api/chats/"+c.ID+"/plans/"+pl.ID+"/pause", ""); code != 200 || !strings.Contains(body, `"state":"paused"`) {
		t.Errorf("Anna pauses: %d %s", code, body)
	}
	// Wrong state: resuming an approved plan is fine, a second pause is not.
	if code, body := call(t, a, "POST", srv.URL+"/api/chats/"+c.ID+"/plans/"+pl.ID+"/pause", ""); code != 409 || !strings.Contains(body, "plan is paused") {
		t.Errorf("second pause: %d %s", code, body)
	}
	if code, body := call(t, a, "POST", srv.URL+"/api/chats/"+c.ID+"/plans/"+pl.ID+"/resume", ""); code != 200 || !strings.Contains(body, `"state":"approved"`) {
		t.Errorf("resume: %d %s", code, body)
	}
	if code, body := call(t, a, "POST", srv.URL+"/api/chats/"+c.ID+"/plans/"+pl.ID+"/cancel", ""); code != 200 || !strings.Contains(body, `"state":"cancelled"`) {
		t.Errorf("cancel: %d %s", code, body)
	}
	if code, body := call(t, a, "POST", srv.URL+"/api/chats/"+c.ID+"/plans/00000000-0000-0000-0000-000000000009/pause", ""); code != 404 {
		t.Errorf("unknown plan: %d %s", code, body)
	}
}

// Issue #172: a step with uploads carries the summary and the manifest hash instead of its files; the list of every
// file is a route of its own, for the owner only.
func TestPlanAPIStepUploads(t *testing.T) {
	url := os.Getenv("AGW_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("AGW_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	st, err := store.OpenSchema(ctx, url, testpg.Schema("apitest_planup"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.DropSchema(context.Background()); st.Close() })
	cat, err := config.ParseCatalog([]byte(`{"default":"p/m","providers":[{"id":"p","upstream":"http://x","api":"openai-completions","models":[{"id":"m"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	p := pool.New[chat.Agent](func(context.Context, string, string) (chat.Agent, error) {
		return nil, errors.New("no slot in the test")
	}, func(context.Context, chat.Agent) {}, map[string]int{"cli": 0})
	m := chat.NewManager(st, p, cat, nil, artifacts.NewBroker(), chat.Options{AcquireTimeout: 100 * time.Millisecond, MaxSubagents: 5})
	srv, is := oidcServer(t, m, p, cat, nil)
	a := loggedIn(t, srv, is, anna)
	b := loggedIn(t, srv, is, bert)

	c, err := st.CreateChat(ctx, store.NewChat{Title: "uploads", Model: "p/m", Variant: "cli", Owner: anna.Sub})
	if err != nil {
		t.Fatal(err)
	}
	_ = st.SetState(ctx, c.ID, store.StateDormant)
	var files []store.PlanFile
	for i := 0; i < 30; i++ {
		n := int64(7)
		files = append(files, store.PlanFile{Field: "files", Path: fmt.Sprintf("/workspace/up/img_%04d.png", i),
			SHA256: strings.Repeat("a", 64), Bytes: &n})
	}
	pl, err := st.CreatePlan(ctx, store.Plan{ChatID: c.ID, Title: "Upload", Session: "main", Via: "cli",
		Steps: []store.PlanStep{{Idx: 1, Method: "POST", Path: "/datasets", Effect: "write", MaxAttempts: 1, Files: files},
			{Idx: 2, Method: "PATCH", Path: "/datasets/1", Effect: "write", MaxAttempts: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	code, body := call(t, a, "GET", srv.URL+"/api/chats/"+c.ID+"/plans", "")
	if code != 200 {
		t.Fatalf("plans: %d %s", code, body)
	}
	var plans []struct {
		Steps []struct {
			Files   []store.PlanFile `json:"files"`
			Uploads *struct {
				Files  int    `json:"files"`
				SHA256 string `json:"manifest_sha256"`
			} `json:"uploads"`
		} `json:"steps"`
	}
	if err := json.Unmarshal([]byte(body), &plans); err != nil || len(plans) != 1 {
		t.Fatalf("plans: %v %s", err, body)
	}
	s1, s2 := plans[0].Steps[0], plans[0].Steps[1]
	if s1.Uploads == nil || s1.Uploads.Files != 30 || s1.Uploads.SHA256 == "" || len(s1.Files) != 0 || strings.Contains(body, "img_0003") {
		t.Fatalf("step with uploads: %s", body)
	}
	if s2.Uploads != nil || len(s2.Files) != 0 {
		t.Fatalf("step without uploads: %+v", s2)
	}
	base := srv.URL + "/api/chats/" + c.ID + "/plans/" + pl.ID + "/steps/"
	code, body = call(t, a, "GET", base+"1/uploads", "")
	if code != 200 || strings.Count(body, `"sha256"`) != 30 || !strings.Contains(body, "img_0003") {
		t.Fatalf("entries: %d %s", code, body)
	}
	if code, _ := call(t, a, "GET", base+"2/uploads", ""); code != 404 {
		t.Errorf("step without uploads: %d", code)
	}
	if code, _ := call(t, b, "GET", base+"1/uploads", ""); code != 404 {
		t.Errorf("Bert on Anna's plan: %d", code)
	}
}

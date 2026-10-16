// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"agw/internal/artifacts"
	"agw/internal/chat"
	"agw/internal/config"
	"agw/internal/oidc/oidctest"
	"agw/internal/pool"
	"agw/internal/store"
)

// assessServer builds the API of a test setup with a database and returns the store and the server.
func assessServer(t *testing.T) (*store.Store, *httptest.Server, *oidctest.Issuer) {
	t.Helper()
	url := os.Getenv("AGW_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("AGW_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	st, err := store.OpenSchema(ctx, url, "apitest_assess_"+strings.ReplaceAll(time.Now().Format("150405.000000"), ".", "_"))
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
	return st, srv, is
}

// The check route (issue #112, H113) belongs to the owner of the chat: a stranger sees nothing.
func TestAssessmentsRouteOwner(t *testing.T) {
	st, srv, is := assessServer(t)
	ctx := context.Background()
	a := loggedIn(t, srv, is, anna)
	bert := loggedIn(t, srv, is, bert)
	c, err := st.CreateChat(ctx, store.NewChat{Title: "checks", Model: "p/m", Variant: "cli", Owner: anna.Sub})
	if err != nil {
		t.Fatal(err)
	}
	_ = st.SetState(ctx, c.ID, store.StateDormant)
	if code, body := call(t, a, "GET", srv.URL+"/api/chats/"+c.ID+"/assessments", ""); code != 200 || !strings.Contains(body, `"checks":[]`) {
		t.Fatalf("chat without checks: %d %q", code, body)
	}
	row, err := st.CreateAssessment(ctx, store.Assessment{ChatID: c.ID, Kind: "training_model", Subject: "model:7",
		EvalDatasetID: 3, Final: true, ToolCallID: "call-9"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishAssessment(ctx, c.ID, row.ID, store.AssessFinished, json.RawMessage(`{"kind":"training_model","verdict":"usable","header":"Result: usable (held-out images, statistics only, no image review)"}`), ""); err != nil {
		t.Fatal(err)
	}
	code, body := call(t, a, "GET", srv.URL+"/api/chats/"+c.ID+"/assessments", "")
	if code != 200 {
		t.Fatalf("owner: %d %s", code, body)
	}
	var answer struct {
		Checks []map[string]any `json:"checks"`
	}
	if err := json.Unmarshal([]byte(body), &answer); err != nil {
		t.Fatalf("answer: %s (%v)", body, err)
	}
	if len(answer.Checks) != 1 {
		t.Fatalf("checks: %s", body)
	}
	line := answer.Checks[0]
	if line["check_id"] != row.ID || line["kind"] != "training_model" || line["subject"] != "model:7" ||
		line["state"] != "finished" || line["final"] != true || line["tool_call_id"] != "call-9" {
		t.Errorf("line: %v", line)
	}
	if line["progress"] != nil {
		t.Errorf("a finished check carries progress: %v", line["progress"])
	}
	rep, _ := line["report"].(map[string]any)
	if rep == nil || rep["header"] != "Result: usable (held-out images, statistics only, no image review)" {
		t.Errorf("report: %v", line["report"])
	}
	// Bert sees nothing of Anna's chat, and an unknown chat is not found either.
	if code, body := call(t, bert, "GET", srv.URL+"/api/chats/"+c.ID+"/assessments", ""); code != 404 {
		t.Errorf("Bert on Anna's chat: %d %s", code, body)
	}
	if code, _ := call(t, a, "GET", srv.URL+"/api/chats/00000000-0000-0000-0000-000000000000/assessments", ""); code != 404 {
		t.Errorf("unknown chat: %d", code)
	}
}

// checksOf reads the check lines out of the route's answer.
func checksOf(t *testing.T, body string) []map[string]any {
	t.Helper()
	var answer struct {
		Checks []map[string]any `json:"checks"`
	}
	if err := json.Unmarshal([]byte(body), &answer); err != nil {
		t.Fatalf("answer: %s (%v)", body, err)
	}
	return answer.Checks
}

// The route hides the checked set: the report names sample sizes and figures, never images, file names
// or SHA-256 of the hold-out set (P11).
func TestAssessmentsRouteHidesHoldout(t *testing.T) {
	st, srv, is := assessServer(t)
	ctx := context.Background()
	a := loggedIn(t, srv, is, anna)
	c, err := st.CreateChat(ctx, store.NewChat{Title: "hidden", Model: "p/m", Variant: "cli", Owner: anna.Sub})
	if err != nil {
		t.Fatal(err)
	}
	_ = st.SetState(ctx, c.ID, store.StateDormant)
	const holdoutSHA = "9f2c1a8b4e6d0f3a5c7b9d1e2f4a6b8c0d2e4f6a8b0c2d4e6f8a0b2c4d6e8f0a"
	report := `{"kind":"training_model","verdict":"usable","final":true,"set":"holdout","measured_images":42,
 "figures":{"images":42,"accuracy":0.9,"macrof1":0.88},"header":"Result: usable (held-out images, statistics only, no image review)"}`
	row, err := st.CreateAssessment(ctx, store.Assessment{ChatID: c.ID, Kind: "training_model", Subject: "model:7", EvalDatasetID: 3, Final: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishAssessment(ctx, c.ID, row.ID, store.AssessFinished, json.RawMessage(report), ""); err != nil {
		t.Fatal(err)
	}
	code, body := call(t, a, "GET", srv.URL+"/api/chats/"+c.ID+"/assessments", "")
	if code != 200 {
		t.Fatalf("route: %d %s", code, body)
	}
	// The report goes through as it was written (the sample size is what the agent reports), and the line
	// carries no field besides the documented ones: nothing of the checked set is added.
	if strings.Contains(body, holdoutSHA) || strings.Contains(body, `"image_ids"`) || strings.Contains(body, `"files"`) {
		t.Errorf("the route names the checked set: %s", body)
	}
	if !strings.Contains(body, `"measured_images":42`) {
		t.Errorf("the route hides the sample size the agent may report: %s", body)
	}
	allowed := map[string]bool{"check_id": true, "kind": true, "subject": true, "final": true, "state": true,
		"report": true, "progress": true, "error": true, "tool_call_id": true, "created_at": true,
		"started_at": true, "ended_at": true}
	for _, raw := range checksOf(t, body) {
		for key := range raw {
			if !allowed[key] {
				t.Errorf("the route adds the field %q: %s", key, body)
			}
		}
	}
	// A running check carries its progress; the route shows it and never guesses it.
	if err := st.SetAssessmentState(ctx, c.ID, row.ID, store.AssessRunning); err != nil {
		t.Fatal(err)
	}
	if err := st.SetAssessmentProgress(ctx, row.ID, json.RawMessage(`{"set":"holdout","images":10,"note":"checking 10 of 42 images"}`)); err != nil {
		t.Fatal(err)
	}
	// A running check has no report yet; the route answers both fields.
	_, body = call(t, a, "GET", srv.URL+"/api/chats/"+c.ID+"/assessments", "")
	if !strings.Contains(body, `"note":"checking 10 of 42 images"`) || !strings.Contains(body, `"state":"running"`) {
		t.Errorf("progress: %s", body)
	}
}

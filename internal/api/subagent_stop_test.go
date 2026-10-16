// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"agw/internal/artifacts"
	"agw/internal/chat"
	"agw/internal/config"
	"agw/internal/pool"
	"agw/internal/store"
)

// The stop route belongs to the owner's chat and answers the refusals of a chat that rests with their codes
// (issue #120, L3, DL3.2).
func TestSubagentStopOwnerOnly(t *testing.T) {
	url := os.Getenv("AGW_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("AGW_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	st, err := store.OpenSchema(ctx, url, "apitest_stop_"+strings.ReplaceAll(time.Now().Format("150405.000000"), ".", "_"))
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

	c, err := st.CreateChat(ctx, store.NewChat{Title: "subagent stop", Model: "p/m", Variant: "cli", Owner: anna.Sub})
	if err != nil {
		t.Fatal(err)
	}
	_ = st.SetState(ctx, c.ID, store.StateDormant)
	if _, err := st.UpsertSubagentRun(ctx, store.SubagentRun{ChatID: c.ID, RunID: "run-a", Agent: "worker", PiRunID: "pi-1", State: "running"}); err != nil {
		t.Fatal(err)
	}
	path := srv.URL + "/api/chats/" + c.ID + "/subagent-runs/run-a/stop"

	if code, body := call(t, b, "POST", path, ""); code != 404 {
		t.Errorf("Bert on Anna's chat: %d %s", code, body)
	}
	if code, body := call(t, a, "POST", srv.URL+"/api/chats/"+c.ID+"/subagent-runs/run-404/stop", ""); code != 404 {
		t.Errorf("unknown run: %d %s", code, body)
	}
	if code, body := call(t, a, "POST", srv.URL+"/api/chats/00000000-0000-0000-0000-000000000000/subagent-runs/run-a/stop", ""); code != 404 {
		t.Errorf("unknown chat: %d %s", code, body)
	}
	// the chat rests (no slot in this test): nothing runs there, so the stop is refused with a code
	if code, body := call(t, a, "POST", path, ""); code != 409 || !strings.Contains(body, "subagents_unavailable") {
		t.Errorf("chat without a sandbox: %d %s", code, body)
	}
	// a run id that must never reach pi is refused like an unknown one
	if code, body := call(t, a, "POST", srv.URL+"/api/chats/"+c.ID+"/subagent-runs/run%201/stop", ""); code != 404 {
		t.Errorf("unsafe run id: %d %s", code, body)
	}
}

// Every refusal of a stop has its status and code; a refusal of pi-subagents travels with its own code (issue #120).
func TestSubagentStopCodes(t *testing.T) {
	cases := []struct {
		err    error
		status int
		code   string
	}{
		{chat.ErrSubagentNotRunning, 409, "not_running"},
		{chat.ErrTrashed, 409, "chat_in_trash"},
		{chat.ErrSubagentsUnavailable, 409, "subagents_unavailable"},
		{store.ErrNotFound, 404, ""},
		{chat.ErrInvalid, 400, ""},
		{fmt.Errorf("wrapped: %w", chat.ErrSubagentNotRunning), 409, "not_running"},
		{&chat.SubagentRelayError{Code: "invalid_state", Message: "run is cancelled"}, 409, "invalid_state"},
		{&chat.SubagentRelayError{Code: "not_found", Message: "gone"}, 404, "not_found"},
		{fmt.Errorf("wrapped: %w", &chat.SubagentRelayError{Code: "execution_failed"}), 409, "execution_failed"},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		subagentStopFail(rec, c.err)
		var body map[string]string
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if rec.Code != c.status || body["code"] != c.code {
			t.Errorf("%v: %d %v, want %d %q", c.err, rec.Code, body, c.status, c.code)
		}
	}
}

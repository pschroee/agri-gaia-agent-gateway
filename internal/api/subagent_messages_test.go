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

// Messages to subagents (issue #81): the route belongs to the owner's chat, refuses a chat in the trash and an
// unknown run, checks the text, and answers 202 with the stored message.
func TestSubagentMessageRoute(t *testing.T) {
	url := os.Getenv("AGW_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("AGW_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	st, err := store.OpenSchema(ctx, url, "apitest_msgs_"+strings.ReplaceAll(time.Now().Format("150405.000000"), ".", "_"))
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

	c, err := st.CreateChat(ctx, store.NewChat{Title: "subagent messages", Model: "p/m", Variant: "cli", Owner: anna.Sub})
	if err != nil {
		t.Fatal(err)
	}
	_ = st.SetState(ctx, c.ID, store.StateDormant)
	if _, err := st.AddSubagentEntries(ctx, []store.SubagentEntry{{ChatID: c.ID, RunID: "run-a", EntryID: "1", Agent: "worker", Kind: "task",
		Payload: json.RawMessage(`{"text":"List the models"}`)}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertSubagentRun(ctx, store.SubagentRun{ChatID: c.ID, RunID: "run-a", Agent: "worker", State: "complete"}); err != nil {
		t.Fatal(err)
	}
	path := srv.URL + "/api/chats/" + c.ID + "/subagent-runs/run-a/messages"

	if code, body := call(t, b, "POST", path, `{"text":"hallo"}`); code != 404 {
		t.Errorf("Bert on Anna's chat: %d %s", code, body)
	}
	if code, body := call(t, a, "POST", srv.URL+"/api/chats/"+c.ID+"/subagent-runs/run-404/messages", `{"text":"hallo"}`); code != 404 {
		t.Errorf("unknown run: %d %s", code, body)
	}
	if code, body := call(t, a, "POST", path, `{"text":"   "}`); code != 400 {
		t.Errorf("empty text: %d %s", code, body)
	}
	if code, body := call(t, a, "POST", path, `{"text":"`+strings.Repeat("x", 8001)+`"}`); code != 400 {
		t.Errorf("text too long: %d %s", code, body)
	}
	code, body := call(t, a, "POST", path, `{"text":"Nimm statt 30 Sekunden nur 10"}`)
	if code != 202 {
		t.Fatalf("message: %d %s", code, body)
	}
	var out struct {
		Message store.SubagentMessage `json:"message"`
		Mode    string                `json:"mode"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if out.Message.Ref == "" || out.Message.RunID != "run-a" || out.Mode != out.Message.Mode || out.Message.Text != "Nimm statt 30 Sekunden nur 10" {
		t.Fatalf("stored message: %+v", out)
	}
	if msgs, err := m.SubagentMessages(ctx, c.ID); err != nil || len(msgs) != 1 {
		t.Fatalf("stored messages: %+v %v", msgs, err)
	}

	if _, err := m.Trash(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if code, body := call(t, a, "POST", path, `{"text":"hallo"}`); code != 409 || !strings.Contains(body, "chat_in_trash") {
		t.Errorf("chat in the trash: %d %s", code, body)
	}
}

// Every refusal of a message to a subagent has its status and code; wrapped errors count the same.
func TestSubagentMessageFailMapping(t *testing.T) {
	cases := []struct {
		err    error
		status int
		code   string
	}{
		{chat.ErrMessageRate, 429, "rate_limited"},
		{chat.ErrSubagentForeground, 409, "foreground_run"},
		{chat.ErrMessagePending, 409, "message_pending"},
		{chat.ErrSubagentLimit, 409, "subagent_limit"},
		{chat.ErrNotResumable, 409, "not_resumable"},
		{chat.ErrSubagentsUnavailable, 409, "subagents_unavailable"},
		{chat.ErrTrashed, 409, "chat_in_trash"},
		{store.ErrNotFound, 404, ""},
		{chat.ErrInvalid, 400, ""},
		{fmt.Errorf("wrapped: %w", chat.ErrMessagePending), 409, "message_pending"},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		subagentMessageFail(rec, c.err)
		var body map[string]string
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if rec.Code != c.status || body["code"] != c.code {
			t.Errorf("%v: %d %v, want %d %q", c.err, rec.Code, body, c.status, c.code)
		}
	}
}

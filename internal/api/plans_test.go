// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package api

import (
	"context"
	"errors"
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

// GET /api/chats/{id}/plans: own chats only, an empty list for a chat without plans (issue #70).
func TestPlansEndpoint(t *testing.T) {
	url := os.Getenv("AGW_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("AGW_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	st, err := store.OpenSchema(ctx, url, "apitest_plans_"+strings.ReplaceAll(time.Now().Format("150405.000000"), ".", "_"))
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

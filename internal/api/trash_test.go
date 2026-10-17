// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package api

import (
	"context"
	"encoding/json"
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
	"agw/internal/testpg"
)

// Issue #76: the trash over the API. Status codes, ownership (foreign and unknown chats are 404, as in #51),
// the 409 for changing a chat in the trash, reads that stay open, and the retention fields of the trash list.
func TestTrashRoutes(t *testing.T) {
	url := os.Getenv("AGW_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("AGW_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	st, err := store.OpenSchema(ctx, url, testpg.Schema("apitest_trash"))
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
	m := chat.NewManager(st, p, cat, nil, artifacts.NewBroker(), chat.Options{AcquireTimeout: 100 * time.Millisecond, MaxSubagents: 5, TrashRetentionDays: 3})
	srv, is := oidcServer(t, m, p, cat, nil)
	a := loggedIn(t, srv, is, anna)
	b := loggedIn(t, srv, is, bert)

	c, err := st.CreateChat(ctx, store.NewChat{Title: "secret", Model: "p/m", Variant: "cli", Owner: anna.Sub})
	if err != nil {
		t.Fatal(err)
	}
	_ = st.SetState(ctx, c.ID, store.StateDormant)
	base := srv.URL + "/api/chats/" + c.ID

	// Bert sees neither the chat nor its trash, and cannot touch it.
	for _, req := range []struct{ method, path string }{{"POST", "/trash"}, {"POST", "/restore"}, {"DELETE", ""}} {
		if code, body := call(t, b, req.method, base+req.path, ""); code != 404 {
			t.Errorf("Bert %s %s on Anna's chat: %d %s", req.method, req.path, code, body)
		}
	}
	if code, body := call(t, b, "GET", srv.URL+"/api/trash", ""); code != 200 || strings.Contains(body, c.ID) {
		t.Errorf("Bert's trash shows Anna's chat: %d %s", code, body)
	}

	// Not in the trash: restore and delete are refused with 409 and a code.
	if code, body := call(t, a, "POST", base+"/restore", ""); code != 409 || !strings.Contains(body, "not_in_trash") {
		t.Errorf("restore outside the trash: %d %s", code, body)
	}
	if code, body := call(t, a, "DELETE", base, ""); code != 409 || !strings.Contains(body, "not_in_trash") {
		t.Errorf("delete outside the trash: %d %s", code, body)
	}

	// Move to the trash: 200 with trashed_at; again 200; the history no longer lists it.
	code, body := call(t, a, "POST", base+"/trash", "")
	if code != 200 || !strings.Contains(body, `"trashed_at"`) {
		t.Fatalf("trash: %d %s", code, body)
	}
	if code, _ := call(t, a, "POST", base+"/trash", ""); code != 200 {
		t.Errorf("second trash: %d", code)
	}
	if code, body := call(t, a, "GET", srv.URL+"/api/chats", ""); code != 200 || strings.Contains(body, c.ID) {
		t.Errorf("history lists the trashed chat: %d", code)
	}

	// In the trash: reads stay open, changes answer 409 chat_in_trash.
	if code, _ := call(t, a, "GET", base, ""); code != 200 {
		t.Errorf("read of a trashed chat: %d", code)
	}
	for _, w := range []struct{ method, path, body string }{
		{"POST", "/messages", `{"text":"hi"}`},
		{"POST", "/queue/send", ""},
		{"POST", "/resume", ""},
		{"POST", "/internet", `{"enabled":true}`},
		{"POST", "/abort", ""},
		{"POST", "/suspend", ""},
		{"DELETE", "/queue/00000000-0000-0000-0000-000000000001", ""},
		{"POST", "/model", `{"model":"p/m"}`},
		{"POST", "/effort", `{"level":"low"}`},
		{"POST", "/autocompact", `{"enabled":true}`},
		{"POST", "/subagents", `{"max":2}`},
		{"POST", "/commands", `{"line":"/rename x"}`},
		{"POST", "/files", ""},
		{"POST", "/tools/call-1/stop", ""},
		{"POST", "/tools/call-1/background", ""},
		{"POST", "/background/bg-1/stop", ""},
	} {
		if code, body := call(t, a, w.method, base+w.path, w.body); code != 409 || !strings.Contains(body, "chat_in_trash") {
			t.Errorf("%s %s in the trash: %d %s", w.method, w.path, code, body)
		}
	}

	// The trash list: own chats, retention fields (purge_at with a retention of 3 days).
	code, body = call(t, a, "GET", srv.URL+"/api/trash", "")
	var list struct {
		RetentionDays int `json:"retention_days"`
		Chats         []struct {
			ID      string     `json:"id"`
			PurgeAt *time.Time `json:"purge_at"`
		} `json:"chats"`
	}
	if code != 200 || json.Unmarshal([]byte(body), &list) != nil || list.RetentionDays != 3 || len(list.Chats) != 1 || list.Chats[0].ID != c.ID {
		t.Fatalf("trash list: %d %s", code, body)
	}
	if list.Chats[0].PurgeAt == nil {
		t.Errorf("no purge_at with a retention")
	}
	if code, body := call(t, b, "POST", srv.URL+"/api/trash/empty", ""); code != 200 || !strings.Contains(body, `"deleted":0`) {
		t.Errorf("Bert empties the trash: %d %s", code, body)
	}

	// Restore, then the history lists it again; trash, then delete for good: 204, then 404.
	if code, _ := call(t, a, "POST", base+"/restore", ""); code != 200 {
		t.Fatalf("restore: %d", code)
	}
	if code, _ := call(t, a, "POST", base+"/trash", ""); code != 200 {
		t.Fatalf("trash again: %d", code)
	}
	if code, body := call(t, a, "DELETE", base, ""); code != 204 {
		t.Fatalf("delete: %d %s", code, body)
	}
	if code, _ := call(t, a, "GET", base, ""); code != 404 {
		t.Errorf("deleted chat: %d", code)
	}
	if code, body := call(t, a, "POST", srv.URL+"/api/trash/empty", ""); code != 200 || !strings.Contains(body, `"deleted":0`) {
		t.Errorf("empty of an empty trash: %d %s", code, body)
	}
}

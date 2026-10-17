// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package api

import (
	"context"
	"encoding/json"
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
	"agw/internal/platform"
	"agw/internal/pool"
	"agw/internal/store"
	"agw/internal/testpg"
)

// A click on "Approve" that finds another platform state answers 409 with code summary_changed and the approval
// itself; the approval stays open and the next click decides (issue L2/#119, DL2.1).
func TestDecideConflictCode(t *testing.T) {
	url := os.Getenv("AGW_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("AGW_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	st, err := store.OpenSchema(ctx, url, testpg.Schema("apitest_decide"))
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

	// The platform answers the read behind the summary with a name the test changes between proposal and click.
	var mu sync.Mutex
	name := "alt"
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			fmt.Fprint(w, `{"access_token":"t","expires_in":3600}`)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && r.URL.Path == "/datasets/16" {
			fmt.Fprintf(w, `{"id":16,"name":%q}`, name)
			return
		}
		fmt.Fprint(w, `{}`)
	}))
	t.Cleanup(apiSrv.Close)
	pc, err := platform.New(platform.Config{APIURL: apiSrv.URL, TokenURL: apiSrv.URL + "/token", User: "u", Password: "p"})
	if err != nil {
		t.Fatal(err)
	}
	m := chat.NewManager(st, p, cat, nil, artifacts.NewBroker(),
		chat.Options{AcquireTimeout: 100 * time.Millisecond, ApprovalTimeout: 30 * time.Second, MaxSubagents: 5, Platform: pc})
	srv, is := oidcServer(t, m, p, cat, nil)
	a := loggedIn(t, srv, is, anna)

	c, err := st.CreateChat(ctx, store.NewChat{Title: "conflict", Model: "p/m", Variant: "cli", Owner: anna.Sub})
	if err != nil {
		t.Fatal(err)
	}
	_ = st.SetState(ctx, c.ID, store.StateDormant)
	req, err := platform.Normalize(platform.Request{Method: "PATCH", Path: "/datasets/16", Body: json.RawMessage(`{"name":"neu"}`)})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		_, _ = m.PlatformCall(ctx, c.ID, "", "cli", req)
		close(done)
	}()
	var ap store.Approval
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		aps, err := st.ListApprovals(ctx, store.ApprovalPending, c.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(aps) > 0 {
			ap = aps[0]
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if ap.ID == "" {
		t.Fatal("no approval was asked")
	}
	mu.Lock()
	name = "other"
	mu.Unlock()

	code, body := call(t, a, "POST", srv.URL+"/api/approvals/"+ap.ID, `{"approve":true}`)
	if code != http.StatusConflict || !strings.Contains(body, `"code":"summary_changed"`) ||
		!strings.Contains(body, `"summary_proposed"`) || !strings.Contains(body, `"summary_changed_at"`) ||
		!strings.Contains(body, `"state":"pending"`) {
		t.Fatalf("conflict: %d %s", code, body)
	}
	if code, body := call(t, a, "POST", srv.URL+"/api/approvals/"+ap.ID, `{"approve":true}`); code != http.StatusOK {
		t.Fatalf("second click: %d %s", code, body)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the approved call did not go through")
	}
}

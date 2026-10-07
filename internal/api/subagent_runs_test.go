package api

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agw/internal/artifacts"
	"agw/internal/chat"
	"agw/internal/config"
	"agw/internal/pool"
	"agw/internal/store"
)

// The short list of a chat's subagent runs (issue #60): own chats only, fields for title and state, and reading it
// neither wakes the chat nor takes a slot.
func TestSubagentRunsList(t *testing.T) {
	url := os.Getenv("AGW_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("AGW_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	st, err := store.OpenSchema(ctx, url, "apitest_runs_"+strings.ReplaceAll(time.Now().Format("150405.000000"), ".", "_"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.DropSchema(context.Background()); st.Close() })
	cat, err := config.ParseCatalog([]byte(`{"default":"p/m","providers":[{"id":"p","upstream":"http://x","api":"openai-completions","models":[{"id":"m"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	var slotsAsked atomic.Int32
	p := pool.New[chat.Agent](func(context.Context, string, string) (chat.Agent, error) {
		slotsAsked.Add(1)
		return nil, errors.New("no slot in the test")
	}, func(context.Context, chat.Agent) {}, map[string]int{"cli": 0})
	m := chat.NewManager(st, p, cat, nil, artifacts.NewBroker(), chat.Options{AcquireTimeout: 100 * time.Millisecond, MaxSubagents: 5})
	srv, is := oidcServer(t, m, p, cat, nil)
	a := loggedIn(t, srv, is, anna)
	b := loggedIn(t, srv, is, bert)

	c, err := st.CreateChat(ctx, store.NewChat{Title: "two subagents", Model: "p/m", Variant: "cli", Owner: anna.Sub})
	if err != nil {
		t.Fatal(err)
	}
	_ = st.SetState(ctx, c.ID, store.StateDormant)
	empty, err := st.CreateChat(ctx, store.NewChat{Title: "none", Model: "p/m", Variant: "cli", Owner: anna.Sub})
	if err != nil {
		t.Fatal(err)
	}
	_ = st.SetState(ctx, empty.ID, store.StateDormant)

	at := func(d time.Duration) *time.Time { v := time.Now().Add(d); return &v }
	entry := func(run, id, agent, kind, payload string) store.SubagentEntry {
		return store.SubagentEntry{ChatID: c.ID, RunID: run, EntryID: id, Agent: agent, Kind: kind, Payload: json.RawMessage(payload)}
	}
	// run-a: status row and entries, finished; run-b: entries only (older chat), long task, last entry a tool call;
	// run-c: status row only, started; run-d: status row without start and without entries (left out).
	if _, err := st.AddSubagentEntries(ctx, []store.SubagentEntry{
		entry("run-a", "1", "worker", "task", `{"text":"## Task\nList the models\nmore"}`),
		entry("run-a", "2", "worker", "text", `{"text":"done"}`),
	}); err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("line\\n", 20) + strings.Repeat("x", 600)
	if _, err := st.AddSubagentEntries(ctx, []store.SubagentEntry{
		entry("run-b", "1", "scout", "task", `{"text":"`+long+`"}`),
		entry("run-b", "2", "scout", "tool_call", `{"name":"bash","id":"c1"}`),
	}); err != nil {
		t.Fatal(err)
	}
	for _, r := range []store.SubagentRun{
		{ChatID: c.ID, RunID: "run-a", Agent: "worker", Label: "models", State: "complete", StartedAt: at(-time.Hour), EndedAt: at(-30 * time.Minute)},
		{ChatID: c.ID, RunID: "run-c", Agent: "worker", Label: "datasets", State: "running", StartedAt: at(time.Hour)},
		{ChatID: c.ID, RunID: "run-d", Agent: "worker", State: "queued"},
	} {
		if _, err := st.UpsertSubagentRun(ctx, r); err != nil {
			t.Fatal(err)
		}
	}

	code, body := call(t, a, "GET", srv.URL+"/api/chats/"+c.ID+"/subagent-runs", "")
	if code != 200 {
		t.Fatalf("list: %d %s", code, body)
	}
	var runs []store.SubagentRunSummary
	if err := json.Unmarshal([]byte(body), &runs); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, r := range runs {
		ids = append(ids, r.RunID)
	}
	if strings.Join(ids, ",") != "run-a,run-b,run-c" {
		t.Fatalf("runs: %v", ids)
	}
	ra, rb, rc := runs[0], runs[1], runs[2]
	if ra.Agent != "worker" || ra.Label != "models" || ra.State != "complete" || ra.TaskHead != "## Task\nList the models\nmore" ||
		ra.StartedAt == nil || ra.EndedAt == nil || ra.LastKind != "text" || ra.Entries != 2 || ra.LastAt == nil {
		t.Fatalf("run-a: %+v", ra)
	}
	if rb.Agent != "scout" || rb.Label != "" || rb.State != "" || rb.LastKind != "tool_call" || rb.StartedAt == nil || rb.EndedAt != nil || rb.Entries != 2 {
		t.Fatalf("run-b: %+v", rb)
	}
	if n := strings.Count(rb.TaskHead, "\n"); n != store.TaskHeadLines-1 || len(rb.TaskHead) > store.TaskHeadBytes {
		t.Fatalf("task head of run-b not shortened: %d lines, %d bytes", n+1, len(rb.TaskHead))
	}
	if rc.Label != "datasets" || rc.State != "running" || rc.TaskHead != "" || rc.Entries != 0 || rc.LastKind != "" {
		t.Fatalf("run-c: %+v", rc)
	}
	// the full task never travels in this list
	if strings.Contains(body, "xxxxxxxxxx") || strings.Contains(body, "payload") {
		t.Fatalf("list carries more than the short form: %s", body)
	}

	if code, body := call(t, a, "GET", srv.URL+"/api/chats/"+empty.ID+"/subagent-runs", ""); code != 200 || body != "[]\n" && body != "[]" {
		t.Fatalf("chat without runs: %d %q", code, body)
	}
	// Bert on Anna's chat and anyone on an unknown chat: 404 like the other routes.
	for _, path := range []string{c.ID, "00000000-0000-0000-0000-000000000000", "not-a-uuid"} {
		if code, body := call(t, b, "GET", srv.URL+"/api/chats/"+path+"/subagent-runs", ""); code != 404 {
			t.Errorf("Bert on %s: %d %s", path, code, body)
		}
	}
	if code, _ := call(t, a, "GET", srv.URL+"/api/chats/00000000-0000-0000-0000-000000000000/subagent-runs", ""); code != 404 {
		t.Errorf("Anna on an unknown chat: %d", code)
	}

	// Nothing woke: no slot was asked for, the chat stays dormant and is not being resumed.
	time.Sleep(200 * time.Millisecond) // negative check: a resume in the background would ask the pool within this time
	if n := slotsAsked.Load(); n != 0 {
		t.Fatalf("listing runs asked the pool for %d slots", n)
	}
	v, err := m.View(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if v.State != store.StateDormant || v.Resuming || v.SlotID != "" {
		t.Fatalf("chat woke: state %s resuming %v slot %q", v.State, v.Resuming, v.SlotID)
	}
}

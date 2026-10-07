package store

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// Tests against a real Postgres. Set AGW_TEST_DATABASE_URL, e.g.
// postgres://agwpoc:<pw>@127.0.0.1:18482/agwpoc?sslmode=disable.
// Every test gets its own schema.
func open(t *testing.T) *Store {
	t.Helper()
	url := os.Getenv("AGW_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("AGW_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	s, err := OpenSchema(ctx, url, "test_"+time.Now().Format("150405_000000"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.DropSchema(context.Background()); s.Close() })
	return s
}

func TestChatLifecycle(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c, err := s.CreateChat(ctx, NewChat{Title: "Test", Model: "deepseek/deepseek-flash", Variant: "cli", Internet: true})
	if err != nil {
		t.Fatal(err)
	}
	if c.ID == "" || c.State != StateActive || !c.Internet {
		t.Fatalf("new chat: %+v", c)
	}
	if err := s.SetState(ctx, c.ID, StateDormant); err != nil {
		t.Fatal(err)
	}
	if err := s.SetInternet(ctx, c.ID, false); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetChat(ctx, c.ID)
	if err != nil || got.State != StateDormant || got.Internet {
		t.Fatalf("GetChat: %+v %v", got, err)
	}
	if _, err := s.GetChat(ctx, "00000000-0000-0000-0000-000000000000"); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	list, err := s.ListChats(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("ListChats: %v %v", list, err)
	}
}

func TestMessagesAndUsageTotals(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c, _ := s.CreateChat(ctx, NewChat{Title: "x", Model: "m/m", Variant: "cli"})
	user := json.RawMessage(`{"role":"user","content":[{"type":"text","text":"hi"}]}`)
	asst := json.RawMessage(`{"role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input":100,"output":20,"cacheRead":50,"totalTokens":170,"cost":{"total":0.0012}}}`)
	asst2 := json.RawMessage(`{"role":"assistant","content":[],"usage":{"input":10,"output":5,"cacheRead":0,"totalTokens":15,"cost":{"total":0.0003}}}`)
	for _, m := range []json.RawMessage{user, asst, asst2} {
		if _, err := s.AppendMessage(ctx, c.ID, m); err != nil {
			t.Fatal(err)
		}
	}
	msgs, err := s.Messages(ctx, c.ID)
	if err != nil || len(msgs) != 3 || msgs[0].Seq != 1 || msgs[0].Role != "user" || msgs[2].Seq != 3 {
		t.Fatalf("Messages: %+v %v", msgs, err)
	}
	got, _ := s.GetChat(ctx, c.ID)
	if got.Tokens.Input != 110 || got.Tokens.Output != 25 || got.Tokens.CacheRead != 50 || got.Tokens.Total != 185 {
		t.Fatalf("Tokens: %+v", got.Tokens)
	}
	if got.Cost < 0.00149 || got.Cost > 0.00151 {
		t.Fatalf("cost: %v", got.Cost)
	}
}

func TestSessionRoundTrip(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c, _ := s.CreateChat(ctx, NewChat{Title: "x", Model: "m/m", Variant: "cli"})
	if b, err := s.LoadSession(ctx, c.ID); err != nil || b != nil {
		t.Fatalf("empty session: %q %v", b, err)
	}
	data := []byte("{\"type\":\"session\"}\n{\"type\":\"message\"}\n")
	if err := s.SaveSession(ctx, c.ID, data); err != nil {
		t.Fatal(err)
	}
	b, err := s.LoadSession(ctx, c.ID)
	if err != nil || string(b) != string(data) {
		t.Fatalf("session: %q %v", b, err)
	}
}

func TestArtifactsUpsertAndCount(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c, _ := s.CreateChat(ctx, NewChat{Title: "x", Model: "m/m", Variant: "cli"})
	a := Artifact{ChatID: c.ID, Kind: KindOutput, Name: "r.csv", Size: 10, SHA256: "aa", ContentType: "text/csv", Via: "cli", ObjectKey: c.ID + "/output/r.csv"}
	if err := s.PutArtifact(ctx, a); err != nil {
		t.Fatal(err)
	}
	a.Size = 12
	if err := s.PutArtifact(ctx, a); err != nil {
		t.Fatal(err)
	}
	in := Artifact{ChatID: c.ID, Kind: KindInput, Name: "r.csv", Size: 3, Via: "ui", ObjectKey: c.ID + "/input/r.csv"}
	if err := s.PutArtifact(ctx, in); err != nil {
		t.Fatal(err)
	}
	list, _ := s.ListArtifacts(ctx, c.ID)
	if len(list) != 2 {
		t.Fatalf("artifacts: %+v", list)
	}
	got, err := s.GetArtifact(ctx, c.ID, KindOutput, "r.csv")
	if err != nil || got.Size != 12 {
		t.Fatalf("GetArtifact: %+v %v", got, err)
	}
	chat, _ := s.GetChat(ctx, c.ID)
	if chat.ArtifactCount != 1 {
		t.Fatalf("artifact_count counts only results: %d", chat.ArtifactCount)
	}
}

func TestApprovalDecideOnlyOnce(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c, _ := s.CreateChat(ctx, NewChat{Title: "x", Model: "m/m", Variant: "cli"})
	ap, err := s.CreateApproval(ctx, Approval{ChatID: c.ID, Kind: "artifact_upload", Via: "mcp", Name: "n.txt", Size: 5, SHA256: "bb", ContentType: "text/plain", PendingKey: "pending/x", Preview: "hello"})
	if err != nil || ap.State != ApprovalPending || ap.ID == "" {
		t.Fatalf("CreateApproval: %+v %v", ap, err)
	}
	chat, _ := s.GetChat(ctx, c.ID)
	if chat.PendingApprovals != 1 {
		t.Fatalf("pending_approvals: %d", chat.PendingApprovals)
	}
	pend, _ := s.ListApprovals(ctx, ApprovalPending, "")
	if len(pend) != 1 {
		t.Fatalf("open: %d", len(pend))
	}
	d, ok, err := s.DecideApproval(ctx, ap.ID, ApprovalApproved)
	if err != nil || !ok || d.State != ApprovalApproved || d.DecidedAt == nil {
		t.Fatalf("decision: %+v %v %v", d, ok, err)
	}
	// A second decision changes nothing.
	d2, ok, err := s.DecideApproval(ctx, ap.ID, ApprovalRejected)
	if err != nil || ok || d2.State != ApprovalApproved {
		t.Fatalf("second decision: %+v %v %v", d2, ok, err)
	}
	n, err := s.ExpirePendingApprovals(ctx)
	if err != nil || n != 0 {
		t.Fatalf("Expire: %d %v", n, err)
	}
}

func TestSocketCalls(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c, _ := s.CreateChat(ctx, NewChat{Title: "x", Model: "m/m", Variant: "mcp"})
	sc, err := s.AddSocketCall(ctx, SocketCall{ChatID: c.ID, SlotID: "p-1", Via: "mcp", Op: "ping", Detail: "{}", Result: "ok"})
	if err != nil || sc.ID == 0 {
		t.Fatalf("AddSocketCall: %+v %v", sc, err)
	}
	if _, err := s.AddSocketCall(ctx, SocketCall{SlotID: "p-2", Via: "cli", Op: "upload", Result: "not assigned"}); err != nil {
		t.Fatal(err)
	}
	list, _ := s.ListSocketCalls(ctx, c.ID)
	if len(list) != 1 || list[0].Op != "ping" {
		t.Fatalf("SocketCalls: %+v", list)
	}
}

// The cost computed by tariff takes precedence over pi's own value.
func TestBilledCostOverridesPiCost(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c, _ := s.CreateChat(ctx, NewChat{Title: "x", Model: "m/m", Variant: "cli"})
	asst := json.RawMessage(`{"role":"assistant","content":[],"usage":{"input":10,"output":5,"totalTokens":15,"cost":{"total":0.002}}}`)
	if _, err := s.AppendBilledMessage(ctx, c.ID, asst, &Billing{Cost: 0.001, Peak: false}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendMessage(ctx, c.ID, asst); err != nil { // without computation: pi's value
		t.Fatal(err)
	}
	got, _ := s.GetChat(ctx, c.ID)
	if got.Cost < 0.00299 || got.Cost > 0.00301 {
		t.Fatalf("sum: %v, expected 0.003", got.Cost)
	}
	msgs, _ := s.Messages(ctx, c.ID)
	if msgs[0].Cost == nil || *msgs[0].Cost != 0.001 || msgs[0].Peak == nil || *msgs[0].Peak || msgs[1].Cost != nil {
		t.Fatalf("messages: %+v %+v", msgs[0], msgs[1])
	}
	_, total, _ := s.Totals(ctx)
	if total < 0.00299 {
		t.Fatalf("Totals: %v", total)
	}
}

func TestCompactionAndContext(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c, _ := s.CreateChat(ctx, NewChat{Title: "k", Model: "m/m", Variant: "cli", AutoCompact: true})
	if !c.AutoCompact || c.Compactions != 0 || c.Context != nil {
		t.Fatalf("new: %+v", c)
	}
	comp := json.RawMessage(`{"role":"compaction","reason":"threshold","tokensBefore":9000,"estimatedTokensAfter":2000,"usage":{"input":9000,"output":300,"totalTokens":9300,"cost":{"total":0.003}}}`)
	if _, err := s.AppendBilledMessage(ctx, c.ID, comp, &Billing{Cost: 0.0015}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetContext(ctx, c.ID, json.RawMessage(`{"tokens":2100,"window":1000000,"percent":0.21}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAutoCompact(ctx, c.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := s.SetCommands(ctx, c.ID, json.RawMessage(`[{"name":"skill:x"}]`)); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetChat(ctx, c.ID)
	if got.AutoCompact || got.Compactions != 1 || !strings.Contains(string(got.Context), `"tokens": 2100`) && !strings.Contains(string(got.Context), `"tokens":2100`) {
		t.Fatalf("after: %+v ctx=%s", got, got.Context)
	}
	if got.Cost < 0.00149 || got.Cost > 0.00151 || got.Tokens.Total != 9300 {
		t.Fatalf("compaction not billed: %v %+v", got.Cost, got.Tokens)
	}
	cmds, _ := s.Commands(ctx, c.ID)
	if !strings.Contains(string(cmds), "skill:x") {
		t.Fatalf("commands: %s", cmds)
	}
}

// Cost comes from the calls captured at the proxy as soon as there are any.
func TestLLMCallsAreAuthoritative(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c, _ := s.CreateChat(ctx, NewChat{Title: "x", Model: "m/m", Variant: "cli", MaxSubagents: 2})
	asst := json.RawMessage(`{"role":"assistant","responseId":"r-main","content":[],"usage":{"input":10,"output":5,"totalTokens":15,"cost":{"total":0.002}}}`)
	_, _ = s.AppendBilledMessage(ctx, c.ID, asst, &Billing{Cost: 0.001})
	got, _ := s.GetChat(ctx, c.ID)
	if got.Cost != 0.001 || got.LLMCalls != 0 || got.MaxSubagents != 2 {
		t.Fatalf("without proxy calls: %+v", got)
	}
	now := time.Now()
	_, err := s.AddLLMCall(ctx, LLMCall{ChatID: c.ID, SlotID: "p", SourceIP: "10.0.0.2", Model: "m/m", ResponseID: "r-main", Status: 200, Input: 10, Output: 5, Cost: 0.001, StartedAt: now, ToolCalls: json.RawMessage(`[{"name":"bash"}]`)})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = s.AddLLMCall(ctx, LLMCall{ChatID: c.ID, SlotID: "p", SourceIP: "10.0.0.2", Model: "m/m", ResponseID: "r-sub", Status: 200, Input: 100, Output: 50, CacheRead: 30, Cost: 0.004, StartedAt: now})
	got, _ = s.GetChat(ctx, c.ID)
	if got.LLMCalls != 2 || got.Cost < 0.00499 || got.Cost > 0.00501 || got.CostOther < 0.00399 || got.CostOther > 0.00401 || got.Tokens.Input != 110 || got.Tokens.CacheRead != 30 {
		t.Fatalf("with proxy calls: %+v", got)
	}
	calls, _ := s.ListLLMCalls(ctx, c.ID)
	if len(calls) != 2 || !calls[0].Main || calls[1].Main {
		t.Fatalf("main session detected wrongly: %+v", calls)
	}
	_, total, _ := s.Totals(ctx)
	if total < 0.00499 {
		t.Fatalf("Totals: %v", total)
	}
	// Subagent entries: only new ones returned, confirmed via the proxy
	es := []SubagentEntry{
		{ChatID: c.ID, RunID: "run1", EntryID: "a", Agent: "scout", Kind: "tool_call", Payload: json.RawMessage(`{"name":"bash"}`), ResponseID: "r-sub"},
		{ChatID: c.ID, RunID: "run1", EntryID: "b", Agent: "scout", Kind: "tool_result", Payload: json.RawMessage(`{"text":"ok"}`)},
	}
	added, err := s.AddSubagentEntries(ctx, es)
	if err != nil || len(added) != 2 || !added[0].Confirmed || added[1].Confirmed {
		t.Fatalf("entries: %+v %v", added, err)
	}
	added, _ = s.AddSubagentEntries(ctx, es)
	if len(added) != 0 {
		t.Fatal("duplicate entries stored")
	}
	got, _ = s.GetChat(ctx, c.ID)
	if got.Subagents != 1 {
		t.Fatalf("started runs: %+v", got)
	}
}

func TestWorkspaceSaveSkipAndChatField(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c, err := s.CreateChat(ctx, NewChat{Title: "ws", Model: "m", Variant: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Workspace != nil {
		t.Fatalf("new chat already has a workspace: %+v", c.Workspace)
	}
	if _, err := s.GetWorkspace(ctx, c.ID); err != ErrNotFound {
		t.Fatalf("GetWorkspace without entry: %v", err)
	}
	// Skipped before anything was ever backed up: field with reason, but without saved_at.
	if err := s.SkipWorkspace(ctx, c.ID, "too large", 300<<20); err != nil {
		t.Fatal(err)
	}
	c, _ = s.GetChat(ctx, c.ID)
	if c.Workspace == nil || c.Workspace.SavedAt != nil || c.Workspace.SkippedReason == nil || *c.Workspace.SkippedReason != "too large" || *c.Workspace.SkippedSize != 300<<20 {
		t.Fatalf("after skipping: %+v", c.Workspace)
	}
	w := Workspace{ChatID: c.ID, ObjectKey: c.ID + "/workspace.tar.gz", Fingerprint: "fp1",
		WorkspaceInfo: WorkspaceInfo{Size: 1234, ArchiveSize: 500, Files: 3, SHA256: "abc"}}
	if err := s.PutWorkspace(ctx, w); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetWorkspace(ctx, c.ID)
	if err != nil || got.ObjectKey != w.ObjectKey || got.Fingerprint != "fp1" || got.Files != 3 || got.SavedAt == nil || got.SkippedReason != nil {
		t.Fatalf("after backup: %+v %v", got, err)
	}
	// Skipped again: the last valid backup stays.
	_ = s.SkipWorkspace(ctx, c.ID, "too large", 1<<30)
	got, _ = s.GetWorkspace(ctx, c.ID)
	if got.ObjectKey != w.ObjectKey || got.Size != 1234 || got.SkippedReason == nil {
		t.Fatalf("backup lost after skipping: %+v", got)
	}
	c, _ = s.GetChat(ctx, c.ID)
	b, _ := json.Marshal(c)
	if !strings.Contains(string(b), `"workspace":{"size":1234,"archive_size":500,"files":3`) || !strings.Contains(string(b), `"skipped_reason":"too large"`) {
		t.Fatalf("API field: %s", b)
	}
	if strings.Contains(string(b), "fp1") || strings.Contains(string(b), "workspace.tar.gz") {
		t.Fatalf("internal details in the API field: %s", b)
	}
	_ = s.ClearWorkspaceSkip(ctx, c.ID)
	got, _ = s.GetWorkspace(ctx, c.ID)
	if got.SkippedReason != nil || got.SkippedAt != nil {
		t.Fatalf("note not cleared: %+v", got)
	}
}

func TestToolExecutions(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c, _ := s.CreateChat(ctx, NewChat{Title: "x", Model: "m/m", Variant: "cli"})
	other, _ := s.CreateChat(ctx, NewChat{Title: "y", Model: "m/m", Variant: "cli"})
	code := 3
	now := time.Now().UTC().Truncate(time.Millisecond)
	e, err := s.AddToolExecution(ctx, ToolExecution{ChatID: c.ID, SlotID: "p-1", ToolCallID: "call_00_a", Tool: "bash", Op: "bash",
		Args: json.RawMessage(`{"command":"exit 3"}`), ExitCode: &code, OutputExcerpt: "x", OutputSHA256: "ab", OutputBytes: 1, StartedAt: now, DurationMs: 12})
	if err != nil || e.ID == 0 || e.Session != "main" {
		t.Fatalf("Add: %+v %v", e, err)
	}
	_, _ = s.AddToolExecution(ctx, ToolExecution{ChatID: c.ID, SlotID: "p-1", Session: "run-1", ToolCallID: "call_01_b", Tool: "read", Op: "read", Error: "ENOENT", StartedAt: now})
	_, _ = s.AddToolExecution(ctx, ToolExecution{ChatID: other.ID, SlotID: "p-2", ToolCallID: "call_x", Tool: "ls", Op: "stat", StartedAt: now})
	list, err := s.ListToolExecutions(ctx, c.ID)
	if err != nil || len(list) != 2 {
		t.Fatalf("List: %+v %v", list, err)
	}
	if list[0].ExitCode == nil || *list[0].ExitCode != 3 || list[0].DurationMs != 12 || !list[0].StartedAt.Equal(now) || string(list[0].Args) != `{"command": "exit 3"}` {
		t.Fatalf("first entry: %+v args=%s", list[0], list[0].Args)
	}
	if list[1].ExitCode != nil || list[1].Session != "run-1" || list[1].Error != "ENOENT" || string(list[1].Args) != "{}" {
		t.Fatalf("second entry: %+v", list[1])
	}
}

// K1: Postgres rejects NUL in text (\x00) and jsonb (\u0000). An entry with binary output
// must not get lost nevertheless.
func TestToolExecutionWithNUL(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c, _ := s.CreateChat(ctx, NewChat{Title: "x", Model: "m/m", Variant: "cli"})
	now := time.Now().UTC()
	e, err := s.AddToolExecution(ctx, ToolExecution{ChatID: c.ID, SlotID: "p", ToolCallID: "call_nul", Tool: "read", Op: "read",
		Args: json.RawMessage(`{"path":"/workspace/a\u0000b.png"}`), Error: "broken\x00", OutputExcerpt: "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR",
		OutputSHA256: "ab", OutputBytes: 16, StartedAt: now})
	if err != nil || e.ID == 0 {
		t.Fatalf("entry with NUL: %v", err)
	}
	list, _ := s.ListToolExecutions(ctx, c.ID)
	if len(list) != 1 || strings.ContainsRune(list[0].OutputExcerpt, 0) || !strings.Contains(list[0].OutputExcerpt, "IHDR") ||
		strings.ContainsRune(list[0].Error, 0) || strings.Contains(string(list[0].Args), `\u0000`) || !strings.Contains(string(list[0].Args), "a") {
		t.Fatalf("stored: %+v args=%s", list, list[0].Args)
	}
	// If the insert fails for another reason (here: no valid JSON), a fallback row without
	// excerpt remains, so that the reconciliation does not report the execution as missing.
	e, err = s.AddToolExecution(ctx, ToolExecution{ChatID: c.ID, SlotID: "p", ToolCallID: "call_broken", Tool: "bash", Op: "bash",
		Args: json.RawMessage(`{"command":`), OutputExcerpt: "x", OutputSHA256: "cd", OutputBytes: 1, StartedAt: now})
	if err != nil || e.ID == 0 {
		t.Fatalf("fallback row: %v", err)
	}
	list, _ = s.ListToolExecutions(ctx, c.ID)
	if len(list) != 2 || list[1].ToolCallID != "call_broken" || list[1].OutputExcerpt != "" || list[1].OutputSHA256 != "cd" ||
		!strings.Contains(list[1].Error, "not stored completely") {
		t.Fatalf("fallback row: %+v", list)
	}
}

// M1: the proxy records whether a reply was complete; the session provides hints that pi
// refused a call (not tamper-proof, for display only).
func TestLLMCallCompletenessAndRejections(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c, _ := s.CreateChat(ctx, NewChat{Title: "x", Model: "m/m", Variant: "cli"})
	now := time.Now().UTC()
	if _, err := s.AddLLMCall(ctx, LLMCall{ChatID: c.ID, SlotID: "p", SourceIP: "1", Model: "m/m", Status: 200, StartedAt: now,
		FinishReason: "tool_calls", Complete: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddLLMCall(ctx, LLMCall{ChatID: c.ID, SlotID: "p", SourceIP: "1", Model: "m/m", Status: 200, StartedAt: now}); err != nil {
		t.Fatal(err)
	}
	calls, _ := s.ListLLMCalls(ctx, c.ID)
	if len(calls) != 2 || calls[0].FinishReason != "tool_calls" || !calls[0].Complete || calls[1].Complete || calls[1].FinishReason != "" {
		t.Fatalf("calls: %+v", calls)
	}
	msg := func(v string) json.RawMessage { return json.RawMessage(v) }
	_, _ = s.AppendMessage(ctx, c.ID, msg(`{"role":"toolResult","toolCallId":"call_a","toolName":"grep","content":[{"type":"text","text":"Tool grep not found"}],"isError":true}`))
	_, _ = s.AppendMessage(ctx, c.ID, msg(`{"role":"toolResult","toolCallId":"call_b","toolName":"bash","content":[{"type":"text","text":"ok"}],"isError":false}`))
	_, _ = s.AddSubagentEntries(ctx, []SubagentEntry{{ChatID: c.ID, RunID: "r1", EntryID: "e1", Agent: "worker", Kind: "tool_result",
		Payload: msg(`{"name":"read","text":"Validation failed for tool \"read\"","is_error":true,"tool_call_id":"call_c"}`)}})
	rej, err := s.ToolRejections(ctx, c.ID)
	if err != nil || len(rej) != 2 || rej["call_a"] != "Tool grep not found" || !strings.Contains(rej["call_c"], "Validation failed") {
		t.Fatalf("rejections: %v %v", rej, err)
	}
}

func TestChatOwner(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	a, err := s.CreateChat(ctx, NewChat{Title: "A", Model: "m/x", Variant: "cli", Owner: "sub-anna"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateChat(ctx, NewChat{Title: "B", Model: "m/x", Variant: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	if a.Owner != "sub-anna" || b.Owner != "" {
		t.Fatalf("owner: %q %q", a.Owner, b.Owner)
	}
	if o, err := s.ChatOwner(ctx, a.ID); err != nil || o != "sub-anna" {
		t.Fatalf("ChatOwner: %q %v", o, err)
	}
	if o, err := s.ChatOwner(ctx, b.ID); err != nil || o != "" {
		t.Fatalf("without owner: %q %v", o, err)
	}
	if _, err := s.ChatOwner(ctx, "00000000-0000-0000-0000-000000000000"); err != ErrNotFound {
		t.Fatalf("unknown: %v", err)
	}
	if _, err := s.ChatOwner(ctx, "not-a-uuid"); err != ErrNotFound {
		t.Fatalf("not a UUID: %v", err)
	}
}

// A rename by the user changes title and title_source but not updated_at, so the chat keeps its
// place in the list (ordered by updated_at), and automatic naming no longer replaces the title.
func TestSetTitleKeepsOrder(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	older, _ := s.CreateChat(ctx, NewChat{Title: "New chat 07.10. 09:00", TitleSource: TitleDefault, Model: "m", Variant: "cli"})
	newer, _ := s.CreateChat(ctx, NewChat{Title: "Newer", Model: "m", Variant: "cli"})
	if _, err := s.pool.Exec(ctx, `UPDATE chats SET updated_at = now() - interval '1 hour' WHERE id = $1`, older.ID); err != nil {
		t.Fatal(err)
	}
	before, _ := s.GetChat(ctx, older.ID)
	order := func() []string {
		l, err := s.ListChats(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, c := range l {
			ids = append(ids, c.ID)
		}
		return ids
	}
	if got := order(); len(got) != 2 || got[0] != newer.ID || got[1] != older.ID {
		t.Fatalf("order before: %v", got)
	}
	if err := s.SetTitle(ctx, older.ID, "Renamed"); err != nil {
		t.Fatal(err)
	}
	if got := order(); got[0] != newer.ID || got[1] != older.ID {
		t.Fatalf("rename moved the chat: %v", got)
	}
	after, _ := s.GetChat(ctx, older.ID)
	if after.Title != "Renamed" || !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("title %q, updated_at %v → %v", after.Title, before.UpdatedAt, after.UpdatedAt)
	}
	var src string
	if err := s.pool.QueryRow(ctx, `SELECT title_source FROM chats WHERE id = $1`, older.ID).Scan(&src); err != nil || src != TitleUser {
		t.Fatalf("title_source %q (%v)", src, err)
	}
	if ok, _ := s.AutoTitle(ctx, older.ID, "From the question"); ok {
		t.Fatal("automatic title replaced the user's")
	}
	if ok, _ := s.ModelTitle(ctx, older.ID, "From the model"); ok {
		t.Fatal("model title replaced the user's")
	}
}

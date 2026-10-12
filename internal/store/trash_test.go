// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// trashFixture creates a chat of owner alice with one call of each logged kind (issue #76).
func trashFixture(t *testing.T, s *Store, owner string) (chatID string, apprID string) {
	t.Helper()
	ctx := context.Background()
	c, err := s.CreateChat(ctx, NewChat{Title: "Secret project", Model: "deepseek/deepseek-flash", Variant: "cli", Owner: owner})
	if err != nil {
		t.Fatal(err)
	}
	dur := 12.5
	if _, err := s.AddSocketCall(ctx, SocketCall{ChatID: c.ID, SlotID: "s1", Via: "cli", Op: "platform",
		Detail: "GET /datasets?name=secret (multipart)", Result: "ok 200 · violation, logged only: body with secret",
		DurationMs: &dur, Effect: "read", ToolCallID: "t0"}); err != nil {
		t.Fatal(err)
	}
	// the approval exists before the call is logged, as in the real flow
	ap, err := s.CreateApproval(ctx, Approval{ChatID: c.ID, Kind: "internet_access", Via: "cli", Name: "reason", ContentType: "text/plain",
		PendingKey: "-", ToolCallID: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.DecideApproval(ctx, ap.ID, ApprovalExpired); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddSocketCall(ctx, SocketCall{ChatID: c.ID, SlotID: "s1", Via: "cli", Op: "internet",
		Detail: "a secret reason", Result: "rejected: no decision", ToolCallID: "t1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddLLMCall(ctx, LLMCall{ChatID: c.ID, SlotID: "s1", SourceIP: "10.0.0.2", Model: "m", Status: 200,
		ToolCalls: json.RawMessage(`[{"id":"c1","name":"bash","arguments":{"command":"cat secret"}}]`), StartedAt: time.Now(), Complete: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddToolExecution(ctx, ToolExecution{ChatID: c.ID, SlotID: "s1", Session: "main", ToolCallID: "t2", Tool: "bash",
		Op: "exec", Args: json.RawMessage(`{"command":"cat secret"}`), OutputExcerpt: "secret output", OutputSHA256: "abc",
		OutputBytes: 13, StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddWebRequest(ctx, WebRequest{ChatID: c.ID, SlotID: "s1", Method: "GET", Host: "example.org", Port: 443,
		Path: "/private?q=secret", Status: 200, StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	return c.ID, ap.ID
}

func TestTrashHidesChatsAndRestores(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	a, _ := s.CreateChat(ctx, NewChat{Title: "A", Model: "m", Variant: "cli", Owner: "alice"})
	b, _ := s.CreateChat(ctx, NewChat{Title: "B", Model: "m", Variant: "cli", Owner: "bob"})
	moved, err := s.MoveToTrash(ctx, a.ID, time.Now())
	if err != nil || !moved {
		t.Fatalf("MoveToTrash: %v %v", moved, err)
	}
	again, _ := s.MoveToTrash(ctx, a.ID, time.Now())
	if again {
		t.Fatal("a chat in the trash moved twice")
	}
	list, err := s.ListChats(ctx)
	if err != nil || len(list) != 1 || list[0].ID != b.ID {
		t.Fatalf("ListChats without the trashed chat: %+v %v", list, err)
	}
	got, err := s.GetChat(ctx, a.ID)
	if err != nil || got.TrashedAt == nil || got.State != StateDormant {
		t.Fatalf("GetChat of a trashed chat: %+v %v", got, err)
	}
	if trashed, err := s.ChatTrashed(ctx, a.ID); err != nil || !trashed {
		t.Fatalf("ChatTrashed: %v %v", trashed, err)
	}
	alice := "alice"
	items, err := s.ListTrash(ctx, &alice)
	if err != nil || len(items) != 1 || items[0].ID != a.ID {
		t.Fatalf("ListTrash of alice: %+v %v", items, err)
	}
	bob := "bob"
	if items, _ := s.ListTrash(ctx, &bob); len(items) != 0 {
		t.Fatalf("ListTrash shows another owner's chat: %+v", items)
	}
	if ok, err := s.Restore(ctx, a.ID); err != nil || !ok {
		t.Fatalf("Restore: %v %v", ok, err)
	}
	if ok, _ := s.Restore(ctx, a.ID); ok {
		t.Fatal("Restore of a chat that is not in the trash reported success")
	}
	if list, _ := s.ListChats(ctx); len(list) != 2 {
		t.Fatalf("restored chat missing from the list: %d", len(list))
	}
}

func TestDeleteChatAnonymisesLogs(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	chatID, _ := trashFixture(t, s, "alice")
	if _, err := s.MoveToTrash(ctx, chatID, time.Now()); err != nil {
		t.Fatal(err)
	}
	tomb, err := s.DeleteChatAnonymised(ctx, chatID)
	if err != nil || tomb == "" {
		t.Fatalf("DeleteChatAnonymised: %q %v", tomb, err)
	}
	if _, err := s.GetChat(ctx, chatID); err != ErrNotFound {
		t.Fatalf("chat still there: %v", err)
	}
	var detail, result string
	var chatNull bool
	if err := s.pool.QueryRow(ctx, `SELECT chat_id IS NULL, detail, result FROM socket_calls WHERE deleted_chat = $1 AND op = 'platform'`, tomb).
		Scan(&chatNull, &detail, &result); err != nil {
		t.Fatal(err)
	}
	if !chatNull || detail != "GET /datasets" || result != "ok 200 · violation, logged only" {
		t.Fatalf("platform call not anonymised: null=%v detail=%q result=%q", chatNull, detail, result)
	}
	var internet string
	if err := s.pool.QueryRow(ctx, `SELECT result FROM socket_calls WHERE deleted_chat = $1 AND op = 'internet'`, tomb).Scan(&internet); err != nil {
		t.Fatal(err)
	}
	if internet != "expired" {
		t.Fatalf("expired internet request keeps %q", internet)
	}
	var calls, execs, webs int
	var toolCalls, args, excerpt, path string
	if err := s.pool.QueryRow(ctx, `SELECT tool_calls::text FROM llm_calls WHERE deleted_chat = $1`, tomb).Scan(&toolCalls); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*), COALESCE(max(args::text), ''), COALESCE(max(output_excerpt), '') FROM tool_executions WHERE deleted_chat = $1`, tomb).
		Scan(&execs, &args, &excerpt); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*), COALESCE(max(path), 'x') FROM web_requests WHERE deleted_chat = $1`, tomb).Scan(&webs, &path); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM socket_calls WHERE chat_id IS NOT NULL OR deleted_chat IS NULL`).Scan(&calls); err != nil {
		t.Fatal(err)
	}
	if toolCalls != `[{"id": "c1", "name": "bash"}]` {
		t.Fatalf("tool calls keep arguments: %s", toolCalls)
	}
	if execs != 1 || args != "{}" || excerpt != "" {
		t.Fatalf("tool execution keeps content: n=%d args=%s excerpt=%q", execs, args, excerpt)
	}
	if webs != 1 || path != "" {
		t.Fatalf("web request keeps its path: n=%d path=%q", webs, path)
	}
	if calls != 0 {
		t.Fatalf("%d socket calls still tied to a live chat or without tombstone", calls)
	}
}

func TestDeletedChatStaysInActivity(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	chatID, _ := trashFixture(t, s, "alice")
	if _, err := s.MoveToTrash(ctx, chatID, time.Now()); err != nil {
		t.Fatal(err)
	}
	before, err := s.ListActivity(ctx, ActivityFilter{Kind: KindAll})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteChatAnonymised(ctx, chatID); err != nil {
		t.Fatal(err)
	}
	alice := "alice"
	after, err := s.ListActivity(ctx, ActivityFilter{Kind: KindAll, Owner: &alice})
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Calls) != len(before.Calls) || len(after.Calls) != 2 {
		t.Fatalf("calls before %d, after %d", len(before.Calls), len(after.Calls))
	}
	if before.Summary.Outcomes["logged"] != after.Summary.Outcomes["logged"] || after.Summary.Outcomes["logged"] != 1 {
		t.Fatalf("outcomes changed: before %+v after %+v", before.Summary.Outcomes, after.Summary.Outcomes)
	}
	for i := range before.Calls {
		b, a := before.Calls[i], after.Calls[i]
		if b.Outcome != a.Outcome || (b.Internet != nil) != (a.Internet != nil) {
			t.Fatalf("call %d: outcome %q -> %q", i, b.Outcome, a.Outcome)
		}
		if a.Internet != nil && a.Internet.Result != b.Internet.Result {
			t.Fatalf("internet result %q -> %q", b.Internet.Result, a.Internet.Result)
		}
	}
	tomb := after.Calls[0].ChatID
	if !after.Chats[tomb].Deleted || after.Chats[tomb].Title != "" {
		t.Fatalf("deleted chat in activity: %+v", after.Chats[tomb])
	}
	bob := "bob"
	other, err := s.ListActivity(ctx, ActivityFilter{Kind: KindAll, Owner: &bob})
	if err != nil || len(other.Calls) != 0 {
		t.Fatalf("another user sees the deleted chat's calls: %d %v", len(other.Calls), err)
	}
}

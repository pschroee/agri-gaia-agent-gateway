// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestSocketCallDuration(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c, _ := s.CreateChat(ctx, NewChat{Title: "x", Model: "m/m", Variant: "mcp"})
	ms := 12.5
	if _, err := s.AddSocketCall(ctx, SocketCall{ChatID: c.ID, SlotID: "p", Via: "mcp", Op: "platform", Detail: "GET /datasets", Result: "ok 200", DurationMs: &ms}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddSocketCall(ctx, SocketCall{ChatID: c.ID, SlotID: "p", Via: "mcp", Op: "upload", Result: "ok"}); err != nil {
		t.Fatal(err)
	}
	list, _ := s.ListSocketCalls(ctx, c.ID)
	if len(list) != 2 || list[0].DurationMs == nil || *list[0].DurationMs != 12.5 || list[1].DurationMs != nil {
		t.Fatalf("durations: %+v", list)
	}
	b, _ := json.Marshal(list[1])
	if strings.Contains(string(b), "duration_ms") {
		t.Fatalf("unmeasured call must not carry duration_ms: %s", b)
	}
}

func TestDurationMsFrom(t *testing.T) {
	if DurationMsFrom(context.Background()) != nil {
		t.Fatal("without duration: want nil")
	}
	got := DurationMsFrom(WithDuration(context.Background(), 1234567*time.Nanosecond))
	if got == nil || *got != 1.2 {
		t.Fatalf("1.234567 ms rounds to 1.2: %v", got)
	}
}

// Activity across chats: only the owner's chats, newest first, filters, cursor, approval and summary.
func TestListActivity(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	mk := func(title, owner, delegation string) Chat {
		nc := NewChat{Title: title, Model: "p/m", Variant: "cli", Owner: owner}
		if delegation != "" {
			nc.Delegation = json.RawMessage(delegation)
		}
		c, err := s.CreateChat(ctx, nc)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	a1 := mk("Anna one", "sub-anna", `{"rules":[]}`)
	a2 := mk("Anna two", "sub-anna", "")
	b1 := mk("Bert", "sub-bert", "")
	call := func(chat, detail, result, toolCall string, ms float64) SocketCall {
		sc := SocketCall{ChatID: chat, SlotID: "p", Via: "mcp", Op: "platform", Detail: detail, Result: result, ToolCallID: toolCall}
		if ms > 0 {
			sc.DurationMs = &ms
		}
		out, err := s.AddSocketCall(ctx, sc)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	// An approval for the writing call, created before it and decided.
	ap, err := s.CreateApproval(ctx, Approval{ChatID: a1.ID, Kind: "platform_write", Via: "mcp", Name: "POST /datasets", PendingKey: "-", ToolCallID: "tc-1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.DecideApproval(ctx, ap.ID, ApprovalApproved); err != nil {
		t.Fatal(err)
	}
	old := call(a1.ID, "GET /models", "ok 200", "", 30)
	call(a1.ID, "POST /datasets", "ok 201", "tc-1", 100)
	call(a1.ID, "DELETE /models/3", "violation blocked: no right", "tc-2", 0)
	call(a2.ID, "GET /datasets", "ok 200 · violation, logged only: outside", "", 10)
	call(a2.ID, "POST /train", "rejected", "tc-3", 0)
	call(a2.ID, "GET /x", "error 404", "", 20)
	call(a2.ID, "GET /urls", "refused: path is blocked", "", 0)
	call(b1.ID, "GET /bert-only", "ok 200", "", 5)
	if _, err := s.AddSocketCall(ctx, SocketCall{ChatID: a1.ID, SlotID: "p", Via: "cli", Op: "upload", Detail: "x", Result: "ok"}); err != nil {
		t.Fatal(err)
	}
	// The oldest call lies yesterday.
	if _, err := s.pool.Exec(ctx, `UPDATE socket_calls SET created_at = now() - interval '1 day' WHERE id = $1`, old.ID); err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{a1.ID, a2.ID, b1.ID} {
		if _, err := s.CreateTurn(ctx, c, "user", "user", nil, nil); err != nil {
			t.Fatal(err)
		}
	}

	anna := "sub-anna"
	p, err := s.ListActivity(ctx, ActivityFilter{Owner: &anna})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Calls) != 7 || p.NextBefore != 0 {
		t.Fatalf("Anna's calls: %d, next %d", len(p.Calls), p.NextBefore)
	}
	for i, c := range p.Calls {
		if c.ChatID == b1.ID || c.Detail == "GET /bert-only" {
			t.Fatalf("Bert's call in Anna's activity: %+v", c)
		}
		if i > 0 && c.ID > p.Calls[i-1].ID {
			t.Fatalf("not newest first: %+v", p.Calls)
		}
	}
	want := map[string]string{"GET /models": "ok", "POST /datasets": "ok", "DELETE /models/3": "blocked", "GET /datasets": "logged",
		"POST /train": "rejected", "GET /x": "error", "GET /urls": "refused"}
	for _, c := range p.Calls {
		if want[c.Detail] != c.Outcome {
			t.Errorf("%s: outcome %q, want %q", c.Detail, c.Outcome, want[c.Detail])
		}
		if (c.Detail == "POST /datasets") != (c.Approval != nil) {
			t.Errorf("%s: approval %+v", c.Detail, c.Approval)
		}
		if c.Approval != nil && (c.Approval.ID != ap.ID || c.Approval.State != ApprovalApproved || c.Approval.DecidedAt == nil) {
			t.Errorf("approval: %+v", c.Approval)
		}
	}
	if len(p.Chats) != 2 || p.Chats[a1.ID].Title != "Anna one" || string(p.Chats[a1.ID].Delegation) == "" || p.Chats[a2.ID].Delegation != nil {
		t.Fatalf("chats: %+v", p.Chats)
	}
	sum := p.Summary
	if sum.Total != 7 || sum.Chats != 2 || sum.Runs != 2 || sum.Outcomes["ok"] != 2 || sum.Outcomes["blocked"] != 1 ||
		sum.Outcomes["logged"] != 1 || sum.Outcomes["rejected"] != 1 || sum.Outcomes["error"] != 1 || sum.Outcomes["refused"] != 1 {
		t.Fatalf("summary: %+v", sum)
	}
	if d := sum.Duration; d.Count != 4 || d.AvgMs == nil || *d.AvgMs != 40 || d.MaxMs == nil || *d.MaxMs != 100 || d.P95Ms == nil || *d.P95Ms < 80 {
		t.Fatalf("duration: count %d avg %v p95 %v max %v", d.Count, deref(d.AvgMs), deref(d.P95Ms), deref(d.MaxMs))
	}

	// Period: since the start of today leaves out yesterday's call; until leaves out today's.
	today := time.Now().Add(-time.Hour)
	p, _ = s.ListActivity(ctx, ActivityFilter{Owner: &anna, Since: today})
	if len(p.Calls) != 6 || p.Summary.Total != 6 || p.Summary.Duration.Count != 3 {
		t.Fatalf("since: %d calls, summary %+v", len(p.Calls), p.Summary)
	}
	p, _ = s.ListActivity(ctx, ActivityFilter{Owner: &anna, Until: today})
	if len(p.Calls) != 1 || p.Calls[0].Detail != "GET /models" || p.Summary.Runs != 0 {
		t.Fatalf("until: %+v %+v", p.Calls, p.Summary)
	}

	// Outcome filters the calls, not the summary.
	p, _ = s.ListActivity(ctx, ActivityFilter{Owner: &anna, Outcome: OutcomeOK})
	if len(p.Calls) != 2 || p.Summary.Total != 7 {
		t.Fatalf("outcome ok: %d calls, total %d", len(p.Calls), p.Summary.Total)
	}

	// Chat filter, including another user's chat (empty).
	p, _ = s.ListActivity(ctx, ActivityFilter{Owner: &anna, ChatID: a2.ID})
	if len(p.Calls) != 4 || p.Summary.Chats != 1 || p.Summary.Runs != 1 {
		t.Fatalf("chat a2: %d %+v", len(p.Calls), p.Summary)
	}
	p, _ = s.ListActivity(ctx, ActivityFilter{Owner: &anna, ChatID: b1.ID})
	if len(p.Calls) != 0 || p.Summary.Total != 0 || p.Summary.Runs != 0 || p.Summary.Duration.AvgMs != nil {
		t.Fatalf("Bert's chat for Anna: %+v %+v", p.Calls, p.Summary)
	}
	p, err = s.ListActivity(ctx, ActivityFilter{Owner: &anna, ChatID: "not-a-uuid"})
	if err != nil || len(p.Calls) != 0 {
		t.Fatalf("invalid chat id: %v %+v", err, p.Calls)
	}

	// Pages of three: 3 + 3 + 1, without overlap.
	seen := map[int64]bool{}
	var before int64
	for i, n := range []int{3, 3, 1} {
		p, err = s.ListActivity(ctx, ActivityFilter{Owner: &anna, Limit: 3, Before: before})
		if err != nil || len(p.Calls) != n {
			t.Fatalf("page %d: %d calls %v", i, len(p.Calls), err)
		}
		for _, c := range p.Calls {
			if seen[c.ID] {
				t.Fatalf("call %d on two pages", c.ID)
			}
			seen[c.ID] = true
		}
		before = p.NextBefore
		if (i < 2) != (before != 0) {
			t.Fatalf("page %d: next_before %d", i, before)
		}
	}

	// Without owner (token mode): all chats.
	p, _ = s.ListActivity(ctx, ActivityFilter{})
	if len(p.Calls) != 8 || p.Summary.Runs != 3 {
		t.Fatalf("token mode: %d calls, %d runs", len(p.Calls), p.Summary.Runs)
	}
}

func deref(v *float64) any {
	if v == nil {
		return nil
	}
	return *v
}

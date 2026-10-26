// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// The first reason of an aborted turn wins: a turn ended by the time limit and only then moved to the trash keeps
// the time limit (issue #120, L3).
func TestTurnAbortReasonFirstWins(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c, err := s.CreateChat(ctx, NewChat{Title: "Chat", Model: "deepseek/deepseek-flash", Variant: "cli", Owner: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.CreateTurn(ctx, c.ID, TriggerUser, OriginUser, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetTurnAbort(ctx, id, "time_limit", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTurnAbort(ctx, id, "trash", ""); err != nil {
		t.Fatal(err)
	}
	var got *string
	if err := s.pool.QueryRow(ctx, `SELECT abort_reason FROM chat_turns WHERE id=$1`, id).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got == nil || *got != "time_limit" {
		t.Fatalf("abort_reason = %v, want time_limit", got)
	}
	// a turn that was never aborted stays NULL, and an unknown turn changes nothing
	other, err := s.CreateTurn(ctx, c.ID, TriggerUser, OriginUser, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetTurnAbort(ctx, other, "user", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTurnAbort(ctx, 999999, "user", ""); err != nil {
		t.Fatalf("unknown turn: %v", err)
	}
	if err := s.SetTurnAbort(ctx, 0, "user", ""); err != nil {
		t.Fatalf("turn 0: %v", err)
	}
	if err := s.SetTurnAbort(ctx, other, "", ""); err != nil {
		t.Fatalf("empty reason: %v", err)
	}
}

// Every stored message of an aborted turn carries its reason, so a reloaded chat still shows why a step was stopped
// (issue #120, L3; DL3.6). Messages of other turns and turns without a reason carry nothing.
func TestMessagesCarryAbortReason(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c, err := s.CreateChat(ctx, NewChat{Title: "Chat", Model: "deepseek/deepseek-flash", Variant: "cli", Owner: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	aborted, err := s.CreateTurn(ctx, c.ID, TriggerUser, OriginUser, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := s.CreateTurn(ctx, c.ID, TriggerUser, OriginUser, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetTurnAbort(ctx, aborted, "subagent_limit", ""); err != nil {
		t.Fatal(err)
	}
	user := json.RawMessage(`{"role":"user","content":[{"type":"text","text":"hallo"}]}`)
	answer := json.RawMessage(`{"role":"assistant","content":[{"type":"text","text":"ok"}],"stopReason":"aborted"}`)
	if _, err := s.AppendTurnMessage(ctx, c.ID, user, nil, &MessageMeta{TurnID: aborted, Trigger: TriggerUser, Origin: OriginUser}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendTurnMessage(ctx, c.ID, answer, nil, &MessageMeta{TurnID: aborted, Trigger: TriggerUser}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendTurnMessage(ctx, c.ID, user, nil, &MessageMeta{TurnID: plain, Trigger: TriggerUser, Origin: OriginUser}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendTurnMessage(ctx, c.ID, user, nil, nil); err != nil {
		t.Fatal(err)
	}
	msgs, err := s.Messages(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 4 {
		t.Fatalf("messages: %d", len(msgs))
	}
	for i := 0; i < 2; i++ {
		if msgs[i].AbortReason != "subagent_limit" {
			t.Fatalf("message %d: abort_reason %q", i, msgs[i].AbortReason)
		}
	}
	for i := 2; i < 4; i++ {
		if msgs[i].AbortReason != "" {
			t.Fatalf("message %d without an aborted turn: abort_reason %q", i, msgs[i].AbortReason)
		}
	}
	// the other fields keep coming along
	if msgs[0].Trigger != TriggerUser || msgs[0].Origin != OriginUser || msgs[0].TurnID == nil {
		t.Fatalf("turn details: %+v", msgs[0])
	}
}

// Who stopped a subagent run: the first writer wins, and the value reaches both forms the UI reads (issue #120, L3).
func TestSubagentStoppedByOnce(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c, err := s.CreateChat(ctx, NewChat{Title: "Subagent chat", Model: "deepseek/deepseek-flash", Variant: "cli", Owner: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertSubagentRun(ctx, SubagentRun{ChatID: c.ID, RunID: "run-1", Agent: "worker", State: "running"}); err != nil {
		t.Fatal(err)
	}
	// a run that was never stopped carries nothing
	before, err := s.SubagentRunOf(ctx, c.ID, "run-1")
	if err != nil || before.StoppedBy != "" {
		t.Fatalf("fresh run: %+v %v", before, err)
	}
	if err := s.SetSubagentStoppedBy(ctx, c.ID, "run-1", "user"); err != nil {
		t.Fatal(err)
	}
	// the subagent limit that follows does not overwrite the user's stop
	if err := s.SetSubagentStoppedBy(ctx, c.ID, "run-1", "limit"); err != nil {
		t.Fatal(err)
	}
	runs, err := s.ListSubagentRuns(ctx, c.ID)
	if err != nil || len(runs) != 1 || runs[0].StoppedBy != "user" {
		t.Fatalf("ListSubagentRuns: %+v %v", runs, err)
	}
	after, err := s.SubagentRunOf(ctx, c.ID, "run-1")
	if err != nil || after.StoppedBy != "user" {
		t.Fatalf("SubagentRunOf: %+v %v", after, err)
	}
	// the short list of the history carries it too, so "stopped" is not read as "ended"
	if _, err := s.AddSubagentEntries(ctx, []SubagentEntry{{ChatID: c.ID, RunID: "run-1", EntryID: "e1", Agent: "worker",
		Kind: "task", Payload: json.RawMessage(`{"text":"zähle"}`)}}); err != nil {
		t.Fatal(err)
	}
	sums, err := s.ListSubagentRunSummaries(ctx, c.ID)
	if err != nil || len(sums) != 1 || sums[0].StoppedBy != "user" {
		t.Fatalf("ListSubagentRunSummaries: %+v %v", sums, err)
	}
	// a state update from the sandbox does not touch it, and the answer of the upsert carries it: the chat layer
	// publishes this very value as the SSE event subagent_run (issue #120)
	upd, err := s.UpsertSubagentRun(ctx, SubagentRun{ChatID: c.ID, RunID: "run-1", State: "cancelled"})
	if err != nil {
		t.Fatal(err)
	}
	if upd.StoppedBy != "user" || upd.State != "cancelled" {
		t.Fatalf("UpsertSubagentRun: %+v", upd)
	}
	final, _ := s.SubagentRunOf(ctx, c.ID, "run-1")
	if final.StoppedBy != "user" || final.State != "cancelled" {
		t.Fatalf("after the state update: %+v", final)
	}
	// an empty reason and an unknown chat are refused
	if err := s.SetSubagentStoppedBy(ctx, c.ID, "run-1", ""); err == nil {
		t.Fatal("an empty stopped_by was accepted")
	}
	if err := s.SetSubagentStoppedBy(ctx, "not-a-uuid", "run-1", "user"); err != ErrNotFound {
		t.Fatalf("foreign chat: %v", err)
	}
}

// The detail goes in with the reason, and the first reason wins together with its detail (#217).
func TestAbortDetailFirstWins(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c, err := s.CreateChat(ctx, NewChat{Title: "Chat", Model: "deepseek/deepseek-flash", Variant: "cli", Owner: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.CreateTurn(ctx, c.ID, TriggerUser, OriginUser, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetTurnAbort(ctx, id, "failed_calls", "5 failed calls of get_dataset in a row (limit 5). Last error: x"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTurnAbort(ctx, id, "user", "later"); err != nil {
		t.Fatal(err)
	}
	var reason, detail string
	if err := s.pool.QueryRow(ctx, `SELECT COALESCE(abort_reason,''), COALESCE(abort_detail,'') FROM chat_turns WHERE id=$1`, id).Scan(&reason, &detail); err != nil {
		t.Fatal(err)
	}
	if reason != "failed_calls" || !strings.HasPrefix(detail, "5 failed calls of get_dataset") {
		t.Fatalf("stored %q / %q", reason, detail)
	}
	// An empty detail stays NULL.
	other, _ := s.CreateTurn(ctx, c.ID, TriggerUser, OriginUser, nil, nil)
	if err := s.SetTurnAbort(ctx, other, "user", ""); err != nil {
		t.Fatal(err)
	}
	var isNull bool
	if err := s.pool.QueryRow(ctx, `SELECT abort_detail IS NULL FROM chat_turns WHERE id=$1`, other).Scan(&isNull); err != nil || !isNull {
		t.Fatalf("empty detail should stay NULL: %v %v", isNull, err)
	}
}

// Every stored message of the turn carries the detail (#217).
func TestAbortDetailOnMessages(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c, err := s.CreateChat(ctx, NewChat{Title: "Chat", Model: "deepseek/deepseek-flash", Variant: "cli", Owner: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	turn, _ := s.CreateTurn(ctx, c.ID, TriggerUser, OriginUser, nil, nil)
	plain, _ := s.CreateTurn(ctx, c.ID, TriggerUser, OriginUser, nil, nil)
	if err := s.SetTurnAbort(ctx, turn, "failed_calls", "detail text"); err != nil {
		t.Fatal(err)
	}
	user := json.RawMessage(`{"role":"user","content":[{"type":"text","text":"hallo"}]}`)
	if _, err := s.AppendTurnMessage(ctx, c.ID, user, nil, &MessageMeta{TurnID: turn, Trigger: TriggerUser, Origin: OriginUser}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendTurnMessage(ctx, c.ID, user, nil, &MessageMeta{TurnID: plain, Trigger: TriggerUser, Origin: OriginUser}); err != nil {
		t.Fatal(err)
	}
	msgs, err := s.Messages(ctx, c.ID)
	if err != nil || len(msgs) != 2 {
		t.Fatalf("messages: %d %v", len(msgs), err)
	}
	if msgs[0].AbortReason != "failed_calls" || msgs[0].AbortDetail != "detail text" || msgs[1].AbortDetail != "" {
		t.Fatalf("detail: %+v / %+v", msgs[0], msgs[1])
	}
}

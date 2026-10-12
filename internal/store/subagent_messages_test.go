// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestSubagentMessagesCRUD(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c, err := s.CreateChat(ctx, NewChat{Title: "Subagent chat", Model: "deepseek/deepseek-flash", Variant: "cli", Owner: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	m, err := s.CreateSubagentMessage(ctx, SubagentMessage{ChatID: c.ID, RunID: "run-1", Ref: "agw-0a1b2c3d", Mode: "steer",
		Text: "Nimm statt 30 Sekunden nur 10", Status: SubagentMsgSending})
	if err != nil || m.ID == 0 || m.Status != SubagentMsgSending || m.CreatedAt.IsZero() {
		t.Fatalf("CreateSubagentMessage: %+v %v", m, err)
	}
	if pending, err := s.PendingSubagentMessage(ctx, c.ID, "run-1"); err != nil || !pending {
		t.Fatalf("a sending message is not pending: %v %v", pending, err)
	}
	if _, err := s.SetSubagentMessageStatus(ctx, c.ID, m.Ref, SubagentMsgSent, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetSubagentMessageStatus(ctx, c.ID, m.Ref, SubagentMsgDelivered, "", "", ""); err != nil {
		t.Fatal(err)
	}
	got, err := s.SubagentMessageByRef(ctx, c.ID, m.Ref)
	if err != nil || got.Status != SubagentMsgDelivered || got.DeliveredAt == nil || got.Text != m.Text {
		t.Fatalf("SubagentMessageByRef after delivery: %+v %v", got, err)
	}
	if pending, _ := s.PendingSubagentMessage(ctx, c.ID, "run-1"); pending {
		t.Fatal("a delivered message is still pending")
	}
	if _, err := s.SetSubagentMessageStatus(ctx, c.ID, m.Ref, SubagentMsgFailed, "relay timed out", "restart", "run-2"); err != nil {
		t.Fatal(err)
	}
	failed, _ := s.SubagentMessageByRef(ctx, c.ID, m.Ref)
	if failed.Status != SubagentMsgFailed || failed.Error != "relay timed out" || failed.TargetRunID != "run-2" || failed.Mode != "restart" {
		t.Fatalf("failed message: %+v", failed)
	}
	if _, err := s.SetSubagentMessageStatus(ctx, c.ID, "agw-ffffffff", SubagentMsgSent, "", "", ""); err != ErrNotFound {
		t.Fatalf("unknown ref: %v", err)
	}
	list, err := s.ListSubagentMessages(ctx, c.ID)
	if err != nil || len(list) != 1 || list[0].ID != m.ID {
		t.Fatalf("ListSubagentMessages: %+v %v", list, err)
	}
	n, err := s.CountSubagentMessages(ctx, c.ID, time.Now().Add(-time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("CountSubagentMessages: %d %v", n, err)
	}
}

func TestSubagentMessagesCascade(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c, err := s.CreateChat(ctx, NewChat{Title: "Gone soon", Model: "deepseek/deepseek-flash", Variant: "cli", Owner: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateSubagentMessage(ctx, SubagentMessage{ChatID: c.ID, RunID: "run-1", Ref: "agw-11111111", Mode: "resume",
		Text: "follow-up", Status: SubagentMsgSending}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM chats WHERE id = $1`, c.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM subagent_messages WHERE ref = 'agw-11111111'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("messages survive the chat: n=%d err=%v", n, err)
	}
}

func TestRunContinuesNotOverwrittenByUpsert(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c, err := s.CreateChat(ctx, NewChat{Title: "Continued", Model: "deepseek/deepseek-flash", Variant: "cli", Owner: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if _, err := s.UpsertSubagentRun(ctx, SubagentRun{ChatID: c.ID, RunID: "run-2", Agent: "researcher", State: "running", StartedAt: &started}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRunContinues(ctx, c.ID, "run-2", "run-1"); err != nil {
		t.Fatal(err)
	}
	// the sandbox reports the run again, with an empty or a different continuation: the link stays
	if _, err := s.UpsertSubagentRun(ctx, SubagentRun{ChatID: c.ID, RunID: "run-2", State: "done", ContinuesRunID: "forged"}); err != nil {
		t.Fatal(err)
	}
	runs, err := s.ListSubagentRuns(ctx, c.ID)
	if err != nil || len(runs) != 1 || runs[0].ContinuesRunID != "run-1" || runs[0].State != "done" {
		t.Fatalf("ListSubagentRuns after upsert: %+v %v", runs, err)
	}
	sum, err := s.ListSubagentRunSummaries(ctx, c.ID)
	if err != nil || len(sum) != 1 || sum[0].ContinuesRunID != "run-1" {
		t.Fatalf("summary keeps no continuation: %+v %v", sum, err)
	}
	r, err := s.SubagentRunOf(ctx, c.ID, "run-2")
	if err != nil || r.ContinuesRunID != "run-1" {
		t.Fatalf("SubagentRunOf: %+v %v", r, err)
	}
	if _, err := s.SubagentRunOf(ctx, c.ID, "run-404"); err != ErrNotFound {
		t.Fatalf("unknown run: %v", err)
	}
}

// A passive note about a message to a subagent (issue #82) carries its type, run, agent and the beginning of the
// text in the queue; other system notes carry no subagent part.
func TestQueueEntryNoteType(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c, err := s.CreateChat(ctx, NewChat{Title: "Subagent note", Model: "deepseek/deepseek-flash", Variant: "cli", Owner: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	text := "The user wrote directly to subagent 0000-run (scout); it receives the message after its current step.\n" +
		"Scout, please check the second source first. " + strings.Repeat("ä", 250)
	if _, err := s.EnqueueSystem(ctx, c.ID, NoteSubagentUser, []string{"0000-run"}, text); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnqueueSystem(ctx, c.ID, NoteBackground, []string{"bg-1"}, "Background task bg-1 ended.\nout"); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListQueue(ctx, c.ID)
	if err != nil || len(list) != 2 {
		t.Fatalf("ListQueue: %+v %v", list, err)
	}
	var sub, bg QueueEntry
	for _, e := range list {
		if e.Note == NoteSubagentUser {
			sub = e
		} else {
			bg = e
		}
	}
	if sub.Subagent == nil || sub.Subagent.RunID != "0000-run" || sub.Subagent.Agent != "scout" {
		t.Fatalf("subagent part: %+v", sub.Subagent)
	}
	if !strings.HasPrefix(sub.Subagent.TextHead, "Scout, please check") || !strings.HasSuffix(sub.Subagent.TextHead, " …") ||
		utf8.RuneCountInString(sub.Subagent.TextHead) != subagentTextHeadRunes+2 {
		t.Fatalf("text head: %q", sub.Subagent.TextHead)
	}
	if !strings.Contains(sub.Text, "Scout, please check the second source first.") {
		t.Fatalf("the whole note stays in text: %q", sub.Text)
	}
	if bg.Subagent != nil {
		t.Fatalf("other note with subagent part: %+v", bg.Subagent)
	}
}

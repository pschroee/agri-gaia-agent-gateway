// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"testing"
	"time"
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
	if _, err := s.UpsertSubagentRun(ctx, SubagentRun{ChatID: c.ID, RunID: "run-2", Agent: "researcher", State: "running"}); err != nil {
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

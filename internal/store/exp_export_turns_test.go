// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"
)

// BM3: the turn of each call, the user's request per turn and the hash of the task text.

// Two turns: each call belongs to the last turn that began at or before it; a call before the first turn has none.
func TestExportTurnIDOnCalls(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	chat := codingRun(t, s, "t1", 1)
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	at := func(m int) time.Time { return base.Add(time.Duration(m) * time.Minute) }
	t1, err := s.CreateTurn(ctx, chat, TriggerUser, OriginUser, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t2, err := s.CreateTurn(ctx, chat, TriggerUser, OriginUser, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for id, m := range map[int64]int{t1: 10, t2: 20} {
		if _, err := s.pool.Exec(ctx, `UPDATE chat_turns SET created_at=$2 WHERE id=$1`, id, at(m)); err != nil {
			t.Fatal(err)
		}
	}
	callAt := func(detail string, m int) {
		rec, err := s.AddSocketCallRecord(ctx, CallRecord{ChatID: chat, SlotID: "s", Via: "api", Op: "platform", Detail: detail, Result: "ok"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.pool.Exec(ctx, `UPDATE socket_calls SET created_at=$2 WHERE id=$1`, rec.ID, at(m)); err != nil {
			t.Fatal(err)
		}
	}
	callAt("GET /before", 5)
	callAt("GET /first", 12)
	callAt("GET /second", 25)
	cols, rows := exportRows(t, s, "platform_calls")
	if !hasColumn(cols, "turn_id") || cols[len(cols)-1] != "turn_id" {
		t.Fatalf("turn_id must be the last column: %v", cols)
	}
	got := map[string]any{}
	for _, r := range rows {
		got[r["detail"].(string)] = r["turn_id"]
	}
	if got["GET /before"] != nil || got["GET /first"] != t1 || got["GET /second"] != t2 {
		t.Fatalf("turn ids: %+v (turns %d, %d)", got, t1, t2)
	}

	// The same for the shell executions.
	if _, err := s.AddToolExecution(ctx, ToolExecution{ChatID: chat, SlotID: "s", Session: "main", ToolCallID: "c1", Tool: "bash", Op: "exec",
		StartedAt: at(15)}); err != nil {
		t.Fatal(err)
	}
	cols, rows = exportRows(t, s, "tool_executions")
	if !hasColumn(cols, "turn_id") || len(rows) != 1 || rows[0]["turn_id"] != t1 {
		t.Fatalf("tool_executions turn_id: %v %+v", cols, rows)
	}
}

// The user's request per turn comes from the user message of the turn; a wake-up turn has none.
func TestExportTurnsUserText(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	chat := codingRun(t, s, "t1", 1)
	t1, err := s.CreateTurn(ctx, chat, TriggerUser, OriginUser, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	seq, err := s.AppendTurnMessage(ctx, chat, json.RawMessage(`{"role":"user","content":[{"type":"text","text":"Please count the cars."}]}`), nil,
		&MessageMeta{TurnID: t1, Trigger: TriggerUser, Origin: OriginUser})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendTurnMessage(ctx, chat, json.RawMessage(`{"role":"assistant","content":[{"type":"text","text":"Done."}]}`), nil,
		&MessageMeta{TurnID: t1, Trigger: TriggerUser}); err != nil {
		t.Fatal(err)
	}
	t2, err := s.CreateTurn(ctx, chat, TriggerWake, OriginSystem, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	cols, rows := exportRows(t, s, "turns")
	if !hasColumn(cols, "user_seq") || !hasColumn(cols, "user_text") {
		t.Fatalf("columns: %v", cols)
	}
	if len(rows) != 2 {
		t.Fatalf("rows: %+v", rows)
	}
	if rows[0]["id"] != t1 || rows[0]["user_text"] != "Please count the cars." || rows[0]["user_seq"] != int32(seq) {
		t.Errorf("user turn: %+v", rows[0])
	}
	if rows[1]["id"] != t2 || rows[1]["user_text"] != "" || rows[1]["user_seq"] != nil {
		t.Errorf("wake turn: %+v", rows[1])
	}
}

func TestExportRunsFirstUserTextSHA(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	chat := codingRun(t, s, "t1", 1)
	empty := codingRun(t, s, "t2", 1)
	task := "Aufgabe: Zähle die Autos."
	if _, err := s.AppendMessage(ctx, chat, json.RawMessage(`{"role":"user","content":[{"type":"text","text":"Aufgabe: Zähle die Autos."}]}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendMessage(ctx, chat, json.RawMessage(`{"role":"user","content":[{"type":"text","text":"a later message"}]}`)); err != nil {
		t.Fatal(err)
	}
	_ = empty
	_, rows := exportRows(t, s, "runs")
	sum := sha256.Sum256([]byte(task))
	byTask := map[string]any{}
	for _, r := range rows {
		byTask[r["task"].(string)] = r["first_user_text_sha256"]
	}
	if byTask["t1"] != hex.EncodeToString(sum[:]) {
		t.Errorf("t1: %v, expected %s", byTask["t1"], hex.EncodeToString(sum[:]))
	}
	if byTask["t2"] != "" {
		t.Errorf("t2 without a message: %v", byTask["t2"])
	}
}

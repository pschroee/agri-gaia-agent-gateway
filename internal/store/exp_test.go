// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestRunLifecycle(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	run := ExpRun{ID: "pilot/mcp/list/1", Key: "pilot/mcp/list/1", Plan: "pilot", PlanSHA256: "abc",
		Cell: "mcp", Task: "list", Rep: 1, Seed: 7, Position: 3, Model: "deepseek/flash"}
	if err := s.StartExpRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if err := s.StartExpRun(ctx, run); !errors.Is(err, ErrExpRunExists) {
		t.Fatalf("second start: %v", err)
	}
	got, err := s.GetExpRun(ctx, run.ID)
	if err != nil || got.Status != ExpStarted || got.EndedAt != nil || got.ChatID != "" {
		t.Fatalf("new run: %+v %v", got, err)
	}
	pass := true
	evidence := json.RawMessage(`{"pass":true}`)
	if err := s.EndExpRun(ctx, run.ID, ExpRunEnd{Status: ExpDone, Pass: &pass, CheckEvidence: evidence}); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetExpRun(ctx, run.ID)
	if err != nil || got.Status != ExpDone || got.Pass == nil || !*got.Pass || got.EndedAt == nil {
		t.Fatalf("ended run: %+v %v", got, err)
	}
	var ev map[string]any
	if err := json.Unmarshal(got.CheckEvidence, &ev); err != nil || ev["pass"] != true {
		t.Fatalf("evidence: %s %v", got.CheckEvidence, err)
	}
	list, err := s.ListExpRuns(ctx, "pilot", "mcp")
	if err != nil || len(list) != 1 || list[0].ID != run.ID {
		t.Fatalf("list: %+v %v", list, err)
	}
	if list, err := s.ListExpRuns(ctx, "pilot", "api"); err != nil || len(list) != 0 {
		t.Fatalf("other cell: %+v %v", list, err)
	}
	if err := s.EndExpRun(ctx, "does-not-exist", ExpRunEnd{Status: ExpDone}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown run: %v", err)
	}
}

func TestSupersedeKeepsOldRun(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	old := ExpRun{ID: "pilot/mcp/list/1", Key: "pilot/mcp/list/1", Plan: "pilot", Cell: "mcp",
		Task: "list", Rep: 1, Position: 1}
	if err := s.StartExpRun(ctx, old); err != nil {
		t.Fatal(err)
	}
	if err := s.SupersedeExpRun(ctx, old.ID, "pilot/mcp/list/1#2"); err != nil {
		t.Fatal(err)
	}
	next := ExpRun{ID: "pilot/mcp/list/1#2", Key: old.Key, Plan: "pilot", Cell: "mcp",
		Task: "list", Rep: 1, Position: 1}
	if err := s.StartExpRun(ctx, next); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListExpRuns(ctx, "pilot", "")
	if err != nil || len(list) != 2 {
		t.Fatalf("both attempts stay visible: %+v %v", list, err)
	}
	row, err := s.GetExpRun(ctx, old.ID)
	if err != nil || row.Status != ExpAborted || row.SupersededBy != next.ID || row.EndedAt == nil {
		t.Fatalf("old attempt: %+v %v", row, err)
	}
	row, err = s.GetExpRun(ctx, next.ID)
	if err != nil || row.Status != ExpStarted || row.SupersededBy != "" {
		t.Fatalf("new attempt: %+v %v", row, err)
	}
	if err := s.SupersedeExpRun(ctx, "does-not-exist", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown attempt: %v", err)
	}
}

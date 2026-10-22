// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// X5.7: only the chats of the plan are exported; the mandatory run fields and the decision columns are present.
func TestExportOnlyPlanChats(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	mine, err := s.CreateChat(ctx, NewChat{Title: "mine", Model: "model-a", Variant: "api",
		Experiment: &Experiment{Protection: "service", Cell: "api-service-none-model-a", Run: "pilot/api-service-none-model-a/t1/1"}})
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.CreateChat(ctx, NewChat{Title: "other", Model: "model-a", Variant: "mcp",
		Experiment: &Experiment{Protection: "service", Cell: "mcp-service-none-model-a", Run: "main/mcp-service-none-model-a/t1/1"}})
	if err != nil {
		t.Fatal(err)
	}
	// X1 records the run; only the pilot chat is part of the exported plan.
	if err := s.StartExpRun(ctx, ExpRun{ID: "pilot/api-service-none-model-a/t1/1", Key: "pilot/api-service-none-model-a/t1/1",
		Plan: "pilot", Cell: "api-service-none-model-a", Task: "t1", Rep: 1, ChatID: mine.ID, Model: "model-a"}); err != nil {
		t.Fatal(err)
	}
	passed := true
	if err := s.EndExpRun(ctx, "pilot/api-service-none-model-a/t1/1", ExpRunEnd{Status: ExpDone, EndReason: "done", Pass: &passed}); err != nil {
		t.Fatal(err)
	}
	if err := s.StartExpRun(ctx, ExpRun{ID: "main/mcp-service-none-model-a/t1/1", Key: "main/mcp-service-none-model-a/t1/1",
		Plan: "main", Cell: "mcp-service-none-model-a", Task: "t1", Rep: 1, ChatID: other.ID, Model: "model-a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddFixture(ctx, NewFixture{Plan: "pilot", Cell: "api-service-none-model-a", ManifestID: "fix-1",
		Kind: "dataset", Owner: FixturePrimary, ObjectID: "12"}); err != nil {
		t.Fatal(err)
	}
	within := false
	for _, chat := range []string{mine.ID, other.ID} {
		if _, err := s.AddSocketCallRecord(ctx, CallRecord{ChatID: chat, SlotID: "slot", Via: "api", Op: "platform",
			Detail: "POST /datasets", Result: "ok", Decision: "allowed_violation", WithinDelegation: &within}); err != nil {
			t.Fatal(err)
		}
	}
	runs, err := s.ExportTable(ctx, "pilot", "runs")
	if err != nil || len(runs.Rows) != 1 {
		t.Fatalf("runs: %+v %v", runs, err)
	}
	calls, err := s.ExportTable(ctx, "pilot", "platform_calls")
	if err != nil || len(calls.Rows) != 1 {
		t.Fatalf("calls: %+v %v", calls, err)
	}
	idx := func(cols []string, name string) int {
		for i, c := range cols {
			if c == name {
				return i
			}
		}
		t.Fatalf("column %q missing", name)
		return -1
	}
	runID := runs.Rows[0][idx(runs.Columns, "run_id")].(string)
	if runID != "pilot/api-service-none-model-a/t1/1" {
		t.Fatalf("run_id: %q", runID)
	}
	for _, c := range []string{"cell", "task", "rep", "pass", "model_role", "end_reason", "binding", "protection", "attack"} {
		idx(runs.Columns, c)
	}
	if runs.Rows[0][idx(runs.Columns, "binding")] != "api" || runs.Rows[0][idx(runs.Columns, "protection")] != "service" ||
		runs.Rows[0][idx(runs.Columns, "attack")] != "none" {
		t.Fatalf("cell factors: %+v", runs.Rows[0])
	}
	if runs.Rows[0][idx(runs.Columns, "pass")] != true || runs.Rows[0][idx(runs.Columns, "end_reason")] != "done" {
		t.Fatalf("result: %+v", runs.Rows[0])
	}
	fixtures, err := s.ExportTable(ctx, "pilot", "fixtures")
	if err != nil || len(fixtures.Rows) != 1 || fixtures.Rows[0][idx(fixtures.Columns, "manifest_id")] != "fix-1" {
		t.Fatalf("fixtures: %+v %v", fixtures, err)
	}
	if calls.Rows[0][idx(calls.Columns, "decision")] != "allowed_violation" {
		t.Fatalf("decision: %+v", calls.Rows[0])
	}
	if calls.Rows[0][idx(calls.Columns, "within_delegation")] != false {
		t.Fatalf("within_delegation: %+v", calls.Rows[0])
	}
}

// X5.7: the llm_calls export carries the token names the evaluation reads.
func TestExportLLMCallTokens(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c, err := s.CreateChat(ctx, NewChat{Title: "x", Model: "m", Variant: "api",
		Experiment: &Experiment{Cell: "api-service-none-m", Run: "pilot/api-service-none-m/t1/1"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.StartExpRun(ctx, ExpRun{ID: "pilot/api-service-none-m/t1/1", Key: "pilot/api-service-none-m/t1/1",
		Plan: "pilot", Cell: "api-service-none-m", Task: "t1", Rep: 1, ChatID: c.ID, Model: "m"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddLLMCall(ctx, LLMCall{ChatID: c.ID, SlotID: "s", Model: "m", Status: 200, Input: 10, Output: 20,
		StartedAt: time.Now(), ToolCalls: json.RawMessage(`[]`)}); err != nil {
		t.Fatal(err)
	}
	tbl, err := s.ExportTable(ctx, "pilot", "llm_calls")
	if err != nil || len(tbl.Rows) != 1 {
		t.Fatalf("llm_calls: %+v %v", tbl, err)
	}
	cols := map[string]any{}
	for i, c := range tbl.Columns {
		cols[c] = tbl.Rows[0][i]
	}
	if cols["tokens_in"] != int64(10) || cols["tokens_out"] != int64(20) || cols["run_id"] == "" {
		t.Fatalf("tokens: %+v", cols)
	}
}

// #207: a fixture seeded for a run belongs to that run only; an older row without run_key still
// belongs to every run of its cell.
func TestExportFixturesByRunKey(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	cell := "api-service-none-model-a"
	keys := []string{"pilot/" + cell + "/t1/1", "pilot/" + cell + "/t1/2"}
	for i, key := range keys {
		c, err := s.CreateChat(ctx, NewChat{Title: key, Model: "model-a", Variant: "api",
			Experiment: &Experiment{Protection: "service", Cell: cell, Run: key}})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.StartExpRun(ctx, ExpRun{ID: key, Key: key, Plan: "pilot", Cell: cell, Task: "t1", Rep: i + 1,
			ChatID: c.ID, Model: "model-a"}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.AddFixture(ctx, NewFixture{Plan: "pilot", Cell: cell, ManifestID: fmt.Sprintf("own-%d", i+1),
			Kind: "dataset", Owner: FixturePrimary, ObjectID: "1", RunKey: key}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.AddFixture(ctx, NewFixture{Plan: "pilot", Cell: cell, ManifestID: "by-hand", Kind: "dataset",
		Owner: FixturePrimary, ObjectID: "9"}); err != nil {
		t.Fatal(err)
	}
	tbl, err := s.ExportTable(ctx, "pilot", "fixtures")
	if err != nil {
		t.Fatal(err)
	}
	col := map[string]int{}
	for i, c := range tbl.Columns {
		col[c] = i
	}
	pairs := map[string]bool{}
	for _, row := range tbl.Rows {
		pairs[fmt.Sprintf("%s|%s", row[col["manifest_id"]], row[col["run_id"]])] = true
	}
	want := []string{"own-1|" + keys[0], "own-2|" + keys[1], "by-hand|" + keys[0], "by-hand|" + keys[1]}
	if len(pairs) != len(want) || len(tbl.Rows) != len(want) {
		t.Fatalf("rows: %v", pairs)
	}
	for _, w := range want {
		if !pairs[w] {
			t.Fatalf("missing %s in %v", w, pairs)
		}
	}
	if _, ok := col["run_key"]; !ok {
		t.Fatal("column run_key missing")
	}
}

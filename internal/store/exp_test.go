// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
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
	// X8.2: the image IDs and checksums of the slot are recorded with the run when the chat is linked.
	const chatID = "00000000-0000-0000-0000-000000000001"
	if err := s.SetExpRunChat(ctx, run.ID, chatID, ExpRunEnvironment{
		ImagePi: "sha256:pi", ImageExec: "sha256:exec", CatalogSHA256: "cat", SkillsSHA256: "skills",
	}); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetExpRun(ctx, run.ID)
	if err != nil || got.ChatID != chatID || got.ImagePi != "sha256:pi" || got.ImageExec != "sha256:exec" ||
		got.CatalogSHA256 != "cat" || got.SkillsSHA256 != "skills" {
		t.Fatalf("environment: %+v %v", got, err)
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

// #161: the plan usage sums input + output of the plan's chats and the time of its attempts, and
// leaves the runs of other plans out.
func TestExpUsage(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	newChat := func(title string) string {
		t.Helper()
		c, err := s.CreateChat(ctx, NewChat{Title: title, Model: "m", Variant: "api"})
		if err != nil {
			t.Fatal(err)
		}
		return c.ID
	}
	chatA, chatB, chatOther := newChat("a"), newChat("b"), newChat("other")
	for _, run := range []ExpRun{
		{ID: "pilot/a/t1/1", Key: "pilot/a/t1/1", Plan: "pilot", Cell: "a", Task: "t1", Rep: 1, ChatID: chatA},
		{ID: "pilot/b/t1/1", Key: "pilot/b/t1/1", Plan: "pilot", Cell: "b", Task: "t1", Rep: 1, ChatID: chatB},
		{ID: "main/a/t1/1", Key: "main/a/t1/1", Plan: "main", Cell: "a", Task: "t1", Rep: 1, ChatID: chatOther},
	} {
		if err := s.StartExpRun(ctx, run); err != nil {
			t.Fatal(err)
		}
		if err := s.EndExpRun(ctx, run.ID, ExpRunEnd{Status: ExpDone}); err != nil {
			t.Fatal(err)
		}
	}
	for _, call := range []LLMCall{
		{ChatID: chatA, SlotID: "s", Model: "m", Status: 200, Input: 100, Output: 20, StartedAt: time.Now(), ToolCalls: json.RawMessage(`[]`)},
		{ChatID: chatB, SlotID: "s", Model: "m", Status: 200, Input: 7, Output: 3, StartedAt: time.Now(), ToolCalls: json.RawMessage(`[]`)},
		{ChatID: chatOther, SlotID: "s", Model: "m", Status: 200, Input: 999, Output: 999, StartedAt: time.Now(), ToolCalls: json.RawMessage(`[]`)},
	} {
		if _, err := s.AddLLMCall(ctx, call); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.pool.Exec(ctx, `UPDATE exp_runs SET started_at=now()-interval '2 minutes', ended_at=now()
		WHERE id='pilot/a/t1/1'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE exp_runs SET started_at=now()-interval '30 seconds', ended_at=now()
		WHERE id='pilot/b/t1/1'`); err != nil {
		t.Fatal(err)
	}
	usage, err := s.ExpUsage(ctx, "pilot")
	if err != nil {
		t.Fatal(err)
	}
	if usage.TokensTotal != 130 || usage.Runs != 2 {
		t.Fatalf("usage: %+v", usage)
	}
	got, err := time.ParseDuration(usage.DurationTotal)
	if err != nil || got < 150*time.Second || got > 151*time.Second {
		t.Fatalf("duration %q: %v", usage.DurationTotal, err)
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

// #213: only runs that are going, not superseded and without a limit reason are checked by the gateway.
func TestExpOpenRunChats(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	mk := func(key string) string {
		c, err := s.CreateChat(ctx, NewChat{Title: key, Model: "m", Variant: "api",
			Experiment: &Experiment{Protection: "none", Cell: "c", Run: key}})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.StartExpRun(ctx, ExpRun{ID: key, Key: key, Plan: "p", Cell: "c", Task: "t", Rep: 1, ChatID: c.ID, Model: "m"}); err != nil {
			t.Fatal(err)
		}
		return c.ID
	}
	open1 := mk("p/c/t/1")
	ended := mk("p/c/t/2")
	if err := s.EndExpRun(ctx, "p/c/t/2", ExpRunEnd{Status: ExpDone}); err != nil {
		t.Fatal(err)
	}
	old := mk("p/c/t/3")
	if err := s.SupersedeExpRun(ctx, "p/c/t/3", "p/c/t/3b"); err != nil {
		t.Fatal(err)
	}
	limited := mk("p/c/t/4")
	if ok, err := s.SetExperimentLimit(ctx, limited, "tokens"); err != nil || !ok {
		t.Fatalf("set limit: %v %v", ok, err)
	}
	ids, err := s.ExpOpenRunChats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != open1 {
		t.Fatalf("open run chats: %v (ended %s, superseded %s, limited %s)", ids, ended, old, limited)
	}
}

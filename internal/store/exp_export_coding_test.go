// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"
)

// BM3: export tables for the coding of level 2 and the protocol fields of FF4.

// codingRun creates a chat recorded as one run of the plan "bm3" and returns the chat id.
func codingRun(t *testing.T, s *Store, task string, rep int) string {
	t.Helper()
	ctx := context.Background()
	cell := "api-service-none-model-a"
	key := "bm3/" + cell + "/" + task + "/" + string(rune('0'+rep))
	c, err := s.CreateChat(ctx, NewChat{Title: key, Model: "model-a", Variant: "api",
		Experiment: &Experiment{Protection: "service", Cell: cell, Run: key}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.StartExpRun(ctx, ExpRun{ID: key, Key: key, Plan: "bm3", Cell: cell, Task: task, Rep: rep, ChatID: c.ID, Model: "model-a"}); err != nil {
		t.Fatal(err)
	}
	return c.ID
}

// exportRows returns the rows of one table as column-name maps.
func exportRows(t *testing.T, s *Store, table string) ([]string, []map[string]any) {
	t.Helper()
	tbl, err := s.ExportTable(context.Background(), "bm3", table)
	if err != nil {
		t.Fatalf("%s: %v", table, err)
	}
	rows := make([]map[string]any, 0, len(tbl.Rows))
	for _, r := range tbl.Rows {
		m := map[string]any{}
		for i, c := range tbl.Columns {
			m[c] = r[i]
		}
		rows = append(rows, m)
	}
	return tbl.Columns, rows
}

func hasColumn(cols []string, name string) bool {
	for _, c := range cols {
		if c == name {
			return true
		}
	}
	return false
}

func TestExportNamesContainCodingTables(t *testing.T) {
	names := ExportNames()
	for _, want := range []string{"messages", "approvals", "internet_grants", "web_requests", "notebook_runs", "artifacts", "plan_steps"} {
		found := false
		for _, n := range names {
			found = found || n == want
		}
		if !found {
			t.Errorf("export table %q missing", want)
		}
	}
}

// The text column holds only the text items; the full JSON keeps tool calls and thinking; rows follow seq.
func TestExportMessages(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	chat := codingRun(t, s, "t1", 1)
	other, err := s.CreateChat(ctx, NewChat{Title: "not in the plan", Model: "m", Variant: "api"})
	if err != nil {
		t.Fatal(err)
	}
	msgs := []string{
		`{"role":"user","content":[{"type":"text","text":"Train a model."}]}`,
		`{"role":"assistant","content":[{"type":"thinking","thinking":"hmm"},{"type":"text","text":"First."},{"type":"toolCall","id":"c1","name":"bash","arguments":{"command":"ls"}},{"type":"text","text":"Second."}]}`,
		`{"role":"toolResult","content":[{"type":"text","text":"file.txt"}]}`,
		`{"role":"user","content":"plain string"}`,
	}
	for _, m := range msgs {
		if _, err := s.AppendMessage(ctx, chat, json.RawMessage(m)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.AppendMessage(ctx, other.ID, json.RawMessage(msgs[0])); err != nil {
		t.Fatal(err)
	}
	cols, rows := exportRows(t, s, "messages")
	for _, c := range []string{"run_id", "cell", "task", "rep", "chat_id", "seq", "role", "text", "message", "created_at"} {
		if !hasColumn(cols, c) {
			t.Errorf("column %q missing", c)
		}
	}
	if len(rows) != 4 {
		t.Fatalf("rows: %d, expected 4 (the foreign chat stays out)", len(rows))
	}
	wantText := []string{"Train a model.", "First.\nSecond.", "file.txt", "plain string"}
	for i, r := range rows {
		if r["seq"] != int32(i+1) {
			t.Errorf("row %d: seq %v, expected %d", i, r["seq"], i+1)
		}
		if r["text"] != wantText[i] {
			t.Errorf("row %d: text %q, expected %q", i, r["text"], wantText[i])
		}
		if r["task"] != "t1" || r["run_id"] == "" {
			t.Errorf("row %d: run fields %+v", i, r)
		}
	}
	full, _ := json.Marshal(rows[1]["message"])
	if !strings.Contains(string(full), `"toolCall"`) || !strings.Contains(string(full), `"thinking"`) {
		t.Errorf("message JSON incomplete: %s", full)
	}
	if strings.Contains(rows[1]["text"].(string), "hmm") || strings.Contains(rows[1]["text"].(string), "bash") {
		t.Errorf("text carries thinking or a tool call: %q", rows[1]["text"])
	}
}

func TestExportApprovalsDecidedBy(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	chat := codingRun(t, s, "t1", 1)
	a, err := s.CreateApproval(ctx, Approval{ChatID: chat, Kind: "platform_write", Via: "api", Name: "POST /datasets", Effect: "write",
		Summary: "creates a dataset", PendingKey: "k", SHA256: "x", ContentType: "text/plain"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE approvals SET state='approved', decided_by=$2, decided_at=now() WHERE id=$1`, a.ID, DecidedByExperiment); err != nil {
		t.Fatal(err)
	}
	cols, rows := exportRows(t, s, "approvals")
	for _, c := range []string{"id", "chat_id", "kind", "via", "name", "effect", "state", "decided_by", "created_at", "decided_at", "summary", "internet"} {
		if !hasColumn(cols, c) {
			t.Errorf("column %q missing", c)
		}
	}
	if len(rows) != 1 || rows[0]["kind"] != "platform_write" || rows[0]["decided_by"] != DecidedByExperiment ||
		rows[0]["state"] != "approved" || rows[0]["effect"] != "write" || rows[0]["summary"] != "creates a dataset" {
		t.Fatalf("approvals: %+v", rows)
	}
}

func TestExportWebRequestsNoSourceIP(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	chat := codingRun(t, s, "t1", 1)
	if _, err := s.AddWebRequest(ctx, WebRequest{ChatID: chat, SlotID: "s", SourceIP: "10.1.2.3", Method: "GET", Host: "example.org",
		Port: 443, Path: "/a", Status: 200, BytesDown: 1234, StartedAt: time.Now(), Origin: "pi", Count: 1}); err != nil {
		t.Fatal(err)
	}
	cols, rows := exportRows(t, s, "web_requests")
	if hasColumn(cols, "source_ip") {
		t.Fatalf("source_ip must not be exported: %v", cols)
	}
	if len(rows) != 1 || rows[0]["host"] != "example.org" || rows[0]["bytes_down"] != int64(1234) || rows[0]["method"] != "GET" {
		t.Fatalf("web_requests: %+v", rows)
	}
	for _, v := range rows[0] {
		if v == "10.1.2.3" {
			t.Fatalf("source address leaked: %+v", rows[0])
		}
	}
}

func TestExportInternetGrantsNoSecrets(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	chat := codingRun(t, s, "t1", 1)
	now := time.Now()
	exp := now.Add(time.Hour)
	if _, err := s.CreateGrant(ctx, Grant{ChatID: chat, Kind: "domains", Origin: "agent", Via: "api", Reason: "look up docs",
		Domains: []string{"docs.example.org"}, State: "active", MaxMinutes: 10, MaxBytes: 1 << 20, StartedAt: &now, ExpiresAt: &exp}); err != nil {
		t.Fatal(err)
	}
	cols, rows := exportRows(t, s, "internet_grants")
	secret := regexp.MustCompile(`(?i)token|key|secret|password`)
	for _, c := range cols {
		if secret.MatchString(c) {
			t.Errorf("column %q looks like a secret", c)
		}
	}
	if len(rows) != 1 || rows[0]["kind"] != "domains" || rows[0]["origin"] != "agent" || rows[0]["state"] != "active" {
		t.Fatalf("internet_grants: %+v", rows)
	}
	doms, _ := json.Marshal(rows[0]["domains"])
	if string(doms) != `["docs.example.org"]` {
		t.Errorf("domains: %s", doms)
	}
}

func TestExportNotebookRunsDatasetIDs(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	chat := codingRun(t, s, "t1", 1)
	exit := 0
	fin := time.Now()
	if err := s.CreateNotebookRun(ctx, NotebookRun{ID: "nb-1", ChatID: chat, HubUser: "u", Server: "srv", Entry: "train.py",
		Files: []NotebookFile{}, DatasetIDs: []int64{812, 813}, TimeoutMin: 30, StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE notebook_runs SET finished_at=$1, exit_code=$2 WHERE id='nb-1'`, fin, exit); err != nil {
		t.Fatal(err)
	}
	_, rows := exportRows(t, s, "notebook_runs")
	if len(rows) != 1 || rows[0]["entry"] != "train.py" || rows[0]["timeout_min"] != int32(30) {
		t.Fatalf("notebook_runs: %+v", rows)
	}
	ids, _ := json.Marshal(rows[0]["dataset_ids"])
	if string(ids) != "[812,813]" {
		t.Errorf("dataset_ids: %s", ids)
	}
	if rows[0]["exit_code"] != int32(0) {
		t.Errorf("exit_code: %v", rows[0]["exit_code"])
	}
}

func TestExportArtifacts(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	chat := codingRun(t, s, "t1", 1)
	if err := s.PutArtifact(ctx, Artifact{ChatID: chat, Kind: "output", Name: "report.md", Size: 42, SHA256: "ab", ContentType: "text/markdown",
		Via: "cli", ObjectKey: "internal/path/report.md"}); err != nil {
		t.Fatal(err)
	}
	cols, rows := exportRows(t, s, "artifacts")
	if hasColumn(cols, "object_key") {
		t.Fatalf("object_key must not be exported: %v", cols)
	}
	if len(rows) != 1 || rows[0]["name"] != "report.md" || rows[0]["kind"] != "output" || rows[0]["size"] != int64(42) ||
		rows[0]["content_type"] != "text/markdown" || rows[0]["via"] != "cli" {
		t.Fatalf("artifacts: %+v", rows)
	}
}

func TestExportPlanSteps(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	chat := codingRun(t, s, "t1", 1)
	other, err := s.CreateChat(ctx, NewChat{Title: "foreign", Model: "m", Variant: "api"})
	if err != nil {
		t.Fatal(err)
	}
	mk := func(chatID string) {
		_, err := s.CreatePlan(ctx, Plan{ChatID: chatID, Title: "plan", Via: "api", Steps: []PlanStep{
			{Idx: 1, Description: "create dataset", Method: "POST", Path: "/datasets", Body: json.RawMessage(`{"name":"x"}`), Effect: "write"},
			{Idx: 2, Description: "train", Tool: "start_training", Method: "POST", Path: "/trainings", Effect: "compute"},
		}})
		if err != nil {
			t.Fatal(err)
		}
	}
	mk(chat)
	mk(other.ID)
	cols, rows := exportRows(t, s, "plan_steps")
	for _, c := range []string{"run_id", "plan_id", "idx", "description", "tool", "method", "path", "body", "effect", "state", "created_resource", "created_id", "done_at"} {
		if !hasColumn(cols, c) {
			t.Errorf("column %q missing", c)
		}
	}
	if len(rows) != 2 {
		t.Fatalf("rows: %d, expected 2 (the foreign chat's plan stays out)", len(rows))
	}
	if rows[0]["idx"] != int32(1) || rows[0]["path"] != "/datasets" || rows[0]["effect"] != "write" || rows[0]["body"] != `{"name":"x"}` ||
		rows[1]["tool"] != "start_training" || rows[1]["effect"] != "compute" || rows[0]["task"] != "t1" {
		t.Fatalf("plan_steps: %+v", rows)
	}
}

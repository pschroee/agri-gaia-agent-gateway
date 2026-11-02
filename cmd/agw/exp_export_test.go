// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agw/internal/agwclient"
	"agw/internal/exp"
	"agw/internal/store"
)

// X5.7: JSONL and CSV are written with fixed columns, row counts and checksums.
func TestExportWritesFiles(t *testing.T) {
	dir := t.TempDir()
	rows := []map[string]any{
		{"run_id": "p/c/t/1", "cell": "api-service-none-m", "task": "t", "rep": 1, "pass": true, "decision": "allowed_violation"},
		{"run_id": "p/c/t/2", "cell": "api-service-none-m", "task": "t", "rep": 2, "pass": false, "decision": "allowed"},
	}
	cols := []string{"run_id", "cell", "task", "rep", "pass", "decision"}
	entries, err := writeExportFiles(dir, "runs", cols, rows, map[string]bool{"jsonl": true, "csv": true})
	if err != nil || len(entries) != 2 {
		t.Fatalf("entries: %+v %v", entries, err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "runs.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	if !strings.Contains(string(raw), `"decision":"allowed_violation"`) || !strings.Contains(string(raw), "\n") {
		t.Fatalf("jsonl: %s", raw)
	}
	cs, err := os.ReadFile(filepath.Join(dir, "runs.csv"))
	if err != nil || !strings.HasPrefix(string(cs), "run_id,cell,task,rep,pass,decision\n") {
		t.Fatalf("csv: %s %v", cs, err)
	}
	for _, e := range entries {
		if e["rows"] != 2 || e["sha256"] == "" {
			t.Fatalf("entry: %+v", e)
		}
	}
	if entries[0]["sha256"] != hex.EncodeToString(sum[:]) {
		t.Fatalf("checksum: %v", entries[0]["sha256"])
	}
	back, err := readExportRows(strings.NewReader(string(raw)))
	if err != nil || len(back) != 2 || back[0]["run_id"] != "p/c/t/1" {
		t.Fatalf("read back: %+v %v", back, err)
	}
}

// X5.7/#137: model_role and the cell factors come from plan.yaml when the server could not know them.
func TestEnrichRunsFromPlan(t *testing.T) {
	rows := []map[string]any{{"cell": "api-service-none-klein", "model": "klein", "model_role": "", "binding": "", "protection": "", "attack": ""}}
	roles := map[string]string{"klein": exp.RoleSecond}
	cells := map[string]exp.Cell{"api-service-none-klein": {ID: "api-service-none-klein", Binding: "api", Protection: "service", Attack: true, Model: "second"}}
	enrichRuns(rows, roles, cells)
	if rows[0]["model_role"] != exp.RoleSecond || rows[0]["binding"] != "api" || rows[0]["protection"] != "service" || rows[0]["attack"] != "attack" {
		t.Fatalf("enriched: %+v", rows[0])
	}
}

// #161: the manifest names the plan-wide limits, the consumption and what is left before each limit.
func TestExpLimitReport(t *testing.T) {
	report, err := expLimitReport(exp.Limits{TokensTotal: 1000, DurationTotal: "2h"},
		agwclient.ExpUsage{TokensTotal: 400, DurationTotal: "30m0s"})
	if err != nil {
		t.Fatal(err)
	}
	if report["tokens_total"] != int64(1000) || report["tokens_used"] != int64(400) ||
		report["tokens_remaining"] != int64(600) {
		t.Fatalf("tokens: %+v", report)
	}
	if report["duration_total"] != "2h0m0s" || report["duration_used"] != "30m0s" ||
		report["duration_remaining"] != "1h30m0s" {
		t.Fatalf("duration: %+v", report)
	}
	// A plan over its limit reports zero left instead of a negative rest.
	report, err = expLimitReport(exp.Limits{TokensTotal: 100, DurationTotal: "1m"},
		agwclient.ExpUsage{TokensTotal: 400, DurationTotal: "2m0s"})
	if err != nil || report["tokens_remaining"] != int64(0) || report["duration_remaining"] != "0s" {
		t.Fatalf("over the limit: %+v %v", report, err)
	}
}

// Nested fields go into one CSV cell as JSON text.
func TestCsvValueNested(t *testing.T) {
	v := csvValue(map[string]any{"rule": "r"})
	if !json.Valid([]byte(v)) || !strings.Contains(v, `"rule":"r"`) {
		t.Fatalf("nested: %s", v)
	}
	if csvValue(true) != "true" || csvValue(nil) != "" {
		t.Fatalf("scalars: %q %q", csvValue(true), csvValue(nil))
	}
}

// BM1: the plan's role of the cell wins over what the server derived from the cell identifier.
func TestEnrichRunsRoleFromCell(t *testing.T) {
	rows := []map[string]any{{"cell": "cli-service-no-deepseek-flash", "model": "deepseek/deepseek-flash", "model_role": "wrong"}}
	cells := map[string]exp.Cell{"cli-service-no-deepseek-flash": {ID: "cli-service-no-deepseek-flash", Binding: "cli", Protection: "service", Model: "deepseek-flash"}}
	enrichRuns(rows, map[string]string{"deepseek/deepseek-flash": "deepseek-flash"}, cells)
	if rows[0]["model_role"] != "deepseek-flash" {
		t.Fatalf("enriched: %+v", rows[0])
	}
}

// BM3: --workspaces writes one archive per run attempt, records its checksum in the manifest entry and notes a run
// without an archive instead of failing; a checksum that differs from the recorded one stops the export.
func TestCLIExportWorkspacesManifest(t *testing.T) {
	archive := "tar-gz-bytes"
	sum := sha256.Sum256([]byte(archive))
	good := hex.EncodeToString(sum[:])
	recorded := good
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("chat") {
		case "chat-1", "chat-3":
			w.Header().Set("X-Agw-Workspace-SHA256", recorded)
			_, _ = w.Write([]byte(archive))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not found: no workspace archive (workspace too large)"}`))
		}
	}))
	defer srv.Close()
	dir := t.TempDir()
	var stdout strings.Builder
	a := &app{ctx: context.Background(), c: agwclient.New(srv.URL), stdout: &stdout}
	runs := []map[string]any{
		{"run_id": "p/c/t/1", "id": "p/c/t/1", "chat_id": "chat-1"},
		{"run_id": "p/c/t/2", "id": "p/c/t/2", "chat_id": "chat-2"},
		{"run_id": "p/c/t/3", "id": "p/c/t/3#2", "chat_id": "chat-3"},
		{"run_id": "p/c/t/4", "id": "p/c/t/4", "chat_id": ""},
	}
	entries, err := a.exportWorkspaces(dir, runs)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 4 {
		t.Fatalf("entries: %+v", entries)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "workspaces", "p__c__t__1.tar.gz"))
	if err != nil || string(raw) != archive {
		t.Fatalf("archive: %q %v", raw, err)
	}
	if entries[0]["sha256"] != good || entries[0]["recorded_sha256"] != good || entries[0]["run_id"] != "p/c/t/1" ||
		entries[0]["name"] != "workspaces/p__c__t__1.tar.gz" || entries[0]["bytes"] != int64(len(archive)) {
		t.Fatalf("entry 1: %+v", entries[0])
	}
	if s, _ := entries[1]["workspace"].(string); !strings.HasPrefix(s, "skipped: ") || !strings.Contains(s, "workspace too large") || entries[1]["run_id"] != "p/c/t/2" {
		t.Fatalf("entry 2 (no archive): %+v", entries[1])
	}
	if _, err := os.Stat(filepath.Join(dir, "workspaces", "p__c__t__2.tar.gz")); err == nil {
		t.Fatal("a skipped run must not leave a file")
	}
	// A retry attempt gets its own file; the run key stays the logical one.
	if entries[2]["name"] != "workspaces/p__c__t__3-attempt2.tar.gz" || entries[2]["run_id"] != "p/c/t/3" {
		t.Fatalf("entry 3: %+v", entries[2])
	}
	if s, _ := entries[3]["workspace"].(string); s != "skipped: run has no chat" {
		t.Fatalf("entry 4 (no chat): %+v", entries[3])
	}
	// The manifest line must be valid JSON as written.
	if _, err := json.Marshal(entries); err != nil {
		t.Fatal(err)
	}

	// A different checksum than the recorded one: the export stops and leaves no archive behind.
	recorded = strings.Repeat("0", 64)
	dir2 := t.TempDir()
	if _, err := a.exportWorkspaces(dir2, runs[:1]); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("checksum mismatch must fail: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir2, "workspaces", "p__c__t__1.tar.gz")); err == nil {
		t.Fatal("a mismatching archive must be removed")
	}
}

func TestWorkspaceFileName(t *testing.T) {
	if got := workspaceFileName("bm/api-none-none-a/t1/2#3"); got != "bm__api-none-none-a__t1__2-attempt3.tar.gz" {
		t.Fatalf("name: %s", got)
	}
}

// The CLI writes exactly the tables the gateway offers, in the same order.
func TestExportNamesMatchGateway(t *testing.T) {
	if strings.Join(agwExportNames, ",") != strings.Join(store.ExportNames(), ",") {
		t.Fatalf("CLI %v, gateway %v", agwExportNames, store.ExportNames())
	}
}

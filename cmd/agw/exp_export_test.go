// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agw/internal/exp"
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

// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package agwclient

import (
	"encoding/json"
	"testing"
)

// The runner sends its start record before the gateway has set the times. Empty strings must not go
// out there: the server decodes them into time.Time and answers 400 invalid JSON.
func TestExpRunStartOmitsServerTimes(t *testing.T) {
	run := ExpRun{ID: "pilot/mcp/list/1", Key: "pilot/mcp/list/1", Plan: "pilot", Cell: "mcp",
		Task: "list", Rep: 1, Seed: 7, Position: 1, Status: "started", Model: "deepseek/flash"}
	b, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"started_at", "ended_at"} {
		if _, ok := m[field]; ok {
			t.Errorf("%s in the start payload: %s", field, b)
		}
	}
	if m["id"] != run.ID || m["status"] != "started" {
		t.Fatalf("payload: %s", b)
	}
}

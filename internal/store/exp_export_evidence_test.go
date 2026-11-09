// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"encoding/json"
	"testing"
)

// BM13: the runs table carries check_evidence, serialized with sorted keys, so that the same run always exports
// to the same bytes. A run without evidence has an empty cell.
func TestExportRunsCheckEvidence(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	codingRun(t, s, "t1", 1)
	codingRun(t, s, "t2", 1)
	key := "bm3/api-service-none-model-a/t1/1"
	pass := true
	// keys deliberately not in sorted order, with a large number that a float64 would round
	evidence := json.RawMessage(`{"pass":true,"no_attempt":[{"path":"/datasets/*","attempts":[]}],"conditions":[],"big":12345678901234567890}`)
	if err := s.EndExpRun(ctx, key, ExpRunEnd{Status: ExpDone, EndReason: "done", Pass: &pass, CheckEvidence: evidence}); err != nil {
		t.Fatal(err)
	}
	cols, rows := exportRows(t, s, "runs")
	if !hasColumn(cols, "check_evidence") {
		t.Fatalf("column check_evidence missing: %v", cols)
	}
	byTask := map[string]any{}
	for _, r := range rows {
		byTask[r["task"].(string)] = r["check_evidence"]
	}
	got, ok := byTask["t1"].(json.RawMessage)
	if !ok {
		t.Fatalf("t1: want JSON, got %T %v", byTask["t1"], byTask["t1"])
	}
	want := `{"big":12345678901234567890,"conditions":[],"no_attempt":[{"attempts":[],"path":"/datasets/*"}],"pass":true}`
	if string(got) != want {
		t.Fatalf("t1 evidence:\n got %s\nwant %s", got, want)
	}
	if byTask["t2"] != nil {
		t.Fatalf("t2 has no evidence, want an empty cell: %v", byTask["t2"])
	}
	// a second export gives the same bytes
	_, again := exportRows(t, s, "runs")
	for _, r := range again {
		if r["task"] == "t1" && string(r["check_evidence"].(json.RawMessage)) != want {
			t.Fatalf("second export differs: %s", r["check_evidence"])
		}
	}
}

func TestCanonicalJSON(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{``, ``},
		{`null`, ``},
		{`{"b":1,"a":{"z":1,"y":[2,1]}}`, `{"a":{"y":[2,1],"z":1},"b":1}`},
		{"{ \"a\" : 1.50 }", `{"a":1.50}`},
	} {
		got, err := canonicalJSON([]byte(c.in))
		if err != nil || string(got) != c.want {
			t.Errorf("%q: got %q err %v, want %q", c.in, got, err, c.want)
		}
	}
	if _, err := canonicalJSON([]byte(`{`)); err == nil {
		t.Error("invalid JSON must fail")
	}
}

// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package delegation

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agw/internal/platform"
)

func sampleResults(path string) []Result {
	cases := []Case{
		{Name: "list datasets", Req: platform.Request{Method: "GET", Path: "/datasets"}, Allowed: true, Group: "control"},
		{Name: "delete another user's dataset", Req: platform.Request{Method: "DELETE", Path: "/datasets/5"}, Group: "action"},
	}
	return []Result{
		ResultFor(cases[0], path, true, "allowed: read dataset"),
		ResultFor(cases[1], path, false, "not in the delegated rights: delete dataset 5"),
	}
}

func TestReportMarkdownListsAllCases(t *testing.T) {
	results := sampleResults(CheckPure)
	md := ReportMarkdown(results, Meta{Version: "abc123", Time: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)})
	for _, want := range []string{
		"Gateway version: abc123",
		"2026-10-17T12:00:00Z",
		"pure (without the manager)",
		"Cases: 2 (expected refusals: 1, allowed controls: 1)",
		"Refused: 1 · deviations: 1",
		"list datasets",
		"delete another user's dataset",
		"Deviations:",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("report does not name %q:\n%s", want, md)
		}
	}
}

func TestReportJSONRoundTrip(t *testing.T) {
	results := sampleResults(CheckManager)
	data, err := ReportJSON(results, Meta{Version: "abc123", Time: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	var r Report
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatal(err)
	}
	if r.Check != CheckManager || r.Cases != 2 || r.Forbidden != 1 || r.Controls != 1 || r.Refused != 1 || len(r.Deviations) != 1 {
		t.Fatalf("round trip lost the summary: %+v", r)
	}
	if r.Results[0].Case != "list datasets" || !r.Results[0].Want || !r.Results[0].Got {
		t.Errorf("round trip lost a row: %+v", r.Results[0])
	}
	if r.Meta.Version != "abc123" || !r.Meta.Time.Equal(time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("round trip lost the meta data: %+v", r.Meta)
	}
}

func TestWriteConformanceReportMergesChecks(t *testing.T) {
	dir := t.TempDir()
	mdPath := filepath.Join(dir, "conformance.md")
	meta := Meta{Version: "abc123", Time: time.Now()}
	if err := WriteConformanceReport(mdPath, meta, sampleResults(CheckPure)); err != nil {
		t.Fatal(err)
	}
	if err := WriteConformanceReport(mdPath, meta, sampleResults(CheckManager)); err != nil {
		t.Fatal(err)
	}
	md, err := os.ReadFile(mdPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"pure (without the manager)", "through the manager and the platform", "Gateway version: abc123"} {
		if !strings.Contains(string(md), want) {
			t.Errorf("composed report does not name %q:\n%s", want, md)
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, "conformance.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc reportDocument
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Version != "abc123" || len(doc.Checks) != 2 {
		t.Fatalf("combined JSON: %+v", doc)
	}
	if doc.Checks[0].Check != CheckPure || doc.Checks[1].Check != CheckManager {
		t.Errorf("checks out of order: %+v", doc.Checks)
	}
	// Without the variable the report is not written at all.
	if err := WriteConformanceReport("", meta, sampleResults(CheckPure)); err != nil {
		t.Errorf("empty path: %v", err)
	}
}

// Stage "no protection": a violation runs and is only logged; the fixed locks still hold (XE1).
func TestEvaluateAppliesProtectionLevels(t *testing.T) {
	noEnforce := `{"rules":[{"action":"read","resource":"dataset","ids":["*"]}],"enforce":false,"confirm":"none"}`
	enforce := `{"rules":[{"action":"read","resource":"dataset","ids":["*"]}],"confirm":"none"}`
	now := time.Now()
	rows := []struct {
		name string
		c    Case
		want bool
		why  string
	}{
		{"none: violation runs", Case{Req: platform.Request{Method: "PATCH", Path: "/datasets/5", Body: json.RawMessage(`{"name":"x"}`)}, Rules: noEnforce}, true, "allowed violation"},
		{"none: blocked path", Case{Req: platform.Request{Method: "GET", Path: "/urls/basic-auth"}, Rules: noEnforce}, false, "blocked"},
		{"none: field rule", Case{Req: platform.Request{Method: "PATCH", Path: "/datasets/5", Body: json.RawMessage(`{"bucket_name":"x"}`)}, Rules: noEnforce}, false, "field rule"},
		{"none: control", Case{Req: platform.Request{Method: "GET", Path: "/datasets"}, Rules: noEnforce}, true, "allowed"},
		{"service: violation", Case{Req: platform.Request{Method: "PATCH", Path: "/datasets/5", Body: json.RawMessage(`{"name":"x"}`)}, Rules: enforce}, false, "not in the delegated rights"},
		{"service: control", Case{Req: platform.Request{Method: "GET", Path: "/datasets"}, Rules: enforce}, true, "allowed"},
	}
	for _, r := range rows {
		got, why := Evaluate(r.c, now)
		if got != r.want || !strings.Contains(why, r.why) {
			t.Errorf("%s: allowed=%v, why=%q (want %v, %q)", r.name, got, why, r.want, r.why)
		}
	}
}

func TestBuildVersion(t *testing.T) {
	if v := BuildVersion(); v == "" {
		t.Fatal("empty version")
	}
}

// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agw/internal/agwclient"
	"agw/internal/exp"
)

// Issue #281: the plan's compaction field reaches every chat of the runner, and the runner logs each compaction.
func TestRunnerSendsPlanCompaction(t *testing.T) {
	for _, tc := range []struct {
		name  string
		field string
		want  *agwclient.ExpCompaction
	}{
		{"absent", "", nil},
		{"off", "compaction:\n  mode: off\n", nil},
		{"auto", "compaction:\n  mode: auto\n  reserve_tokens: 26214\n", &agwclient.ExpCompaction{Mode: "auto", ReserveTokens: 26214}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeExpMaterial(t, "pilot", 1)
			raw, err := os.ReadFile(filepath.Join(dir, "plan.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			plan := strings.Replace(string(raw), "limits:\n", tc.field+"limits:\n", 1)
			if err := os.WriteFile(filepath.Join(dir, "plan.yaml"), []byte(plan), 0o644); err != nil {
				t.Fatal(err)
			}
			api := newFakeExpAPI()
			var out, errw strings.Builder
			r, err := newExpRunner(api, expRunOptions{PlanPath: dir, Poll: time.Millisecond, StartGrace: 20 * time.Millisecond, Heartbeat: time.Hour}, &out, &errw)
			if err != nil {
				t.Fatal(err)
			}
			if err := r.run(context.Background()); err != nil {
				t.Fatalf("run: %v (%s)", err, errw.String())
			}
			if len(api.creates) == 0 {
				t.Fatal("no chat created")
			}
			for _, req := range api.creates {
				got := req.Experiment.Compaction
				if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
					t.Fatalf("compaction in the profile: %+v, want %+v", got, tc.want)
				}
			}
		})
	}
}

func TestCatalogueWindows(t *testing.T) {
	got := catalogueWindows([]agwclient.Model{{ID: "a/b", ContextWindow: 262144}, {ID: "c/d"}})
	if got["a/b"] != 262144 || got["c/d"] != 0 || len(got) != 2 {
		t.Fatalf("windows: %v", got)
	}
	if p := compactionProfile(&exp.Plan{Compaction: exp.Compaction{Mode: "auto", ReserveTokens: 5}}); p == nil || p.ReserveTokens != 5 {
		t.Fatalf("profile: %+v", p)
	}
	if compactionProfile(&exp.Plan{}) != nil || compactionProfile(nil) != nil {
		t.Fatal("no compaction without the field")
	}
}

// Each compaction message is reported once, with reason, tokens before and after and the summary call.
func TestCompactionLogReportsEachOnce(t *testing.T) {
	var sb strings.Builder
	l := newCompactionLog(&sb, "c1")
	det := agwclient.ChatDetail{Messages: []agwclient.StoredMessage{
		{Seq: 1, Role: "user"},
		{Seq: 2, Role: "compaction", Message: json.RawMessage(`{"reason":"threshold","tokensBefore":236000,"estimatedTokensAfter":31000,"usage":{"input":150000,"output":4000}}`)},
	}}
	l.observe(det)
	l.observe(det)
	out := sb.String()
	if strings.Count(out, "context compacted") != 1 ||
		!strings.Contains(out, "(threshold): 236000 tokens before, about 31000 after; summary call 150000 in, 4000 out tokens; 1 so far") {
		t.Fatalf("log: %q", out)
	}
	det.Messages = append(det.Messages, agwclient.StoredMessage{Seq: 9, Role: "compaction", Message: json.RawMessage(`{"reason":"threshold"}`)})
	l.observe(det)
	if strings.Count(sb.String(), "context compacted") != 2 || !strings.Contains(sb.String(), "2 so far") {
		t.Fatalf("second: %q", sb.String())
	}
}

// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

import (
	"strings"
	"testing"
)

// A registered blocker refuses calls of the agent; a call of the gateway itself (normalize with
// agent false) passes it (G1.6).
func TestRegisteredBlockerAgentOnly(t *testing.T) {
	refuser := func(r Request) string {
		if strings.HasPrefix(r.Path, "/seam/") {
			return "the path " + r.Path + " is blocked for the agent"
		}
		return ""
	}
	registerBlocker(refuser)
	// The cleanup pops whatever is last, so it also covers the case that the refusing blocker was
	// already dropped below; a guard keeps it from popping past the end.
	t.Cleanup(func() {
		if len(blockers) > 0 {
			blockers = blockers[:len(blockers)-1]
		}
	})

	req := Request{Method: "POST", Path: "/seam/build", Body: []byte(`{"a":1}`)}
	if _, err := Normalize(req); err == nil || !strings.Contains(err.Error(), "blocked for the agent") {
		t.Fatalf("agent call not refused: %v", err)
	}
	if _, err := normalize(req, false); err != nil {
		t.Fatalf("the gateway's own call must pass: %v", err)
	}
	// Another path and the fixed list are unaffected.
	if _, err := Normalize(Request{Method: "POST", Path: "/seam_other/build"}); err != nil {
		t.Fatalf("other path refused: %v", err)
	}
	if _, err := normalize(Request{Method: "GET", Path: "/urls/basic-auth"}, false); err == nil {
		t.Error("the fixed list must apply to the gateway's own calls too")
	}
	// A blocker sees the canonical request. The refusing blocker is dropped first, so that it does not
	// stop the call before the observing one runs.
	blockers = blockers[:len(blockers)-1]
	var seen Request
	registerBlocker(func(r Request) string { seen = r; return "" })
	t.Cleanup(func() { blockers = blockers[:len(blockers)-1] })
	if _, err := Normalize(Request{Method: "post", Path: "/seam/1", Body: []byte("{ \"a\" : 1 }")}); err != nil {
		t.Fatal(err)
	}
	if seen.Method != "POST" || string(seen.Body) != `{"a":1}` {
		t.Errorf("blocker saw %+v", seen)
	}
}

// A registered describer describes its call; calls it does not know keep their built-in template
// (G1.6).
func TestRegisteredDescriberRuns(t *testing.T) {
	registerDescriber(func(c SummaryCall, s *summarizer) (string, []string, bool) {
		if !strings.HasPrefix(c.Path, "/seam/") {
			return "", nil, false
		}
		return "Seam call " + c.Path, []string{"Detail: a seam"}, true
	})
	t.Cleanup(func() { describers = describers[:len(describers)-1] })

	got := Summarize(t.Context(), nil, SummaryCall{Method: "POST", Path: "/seam/7"})
	if got != "Seam call /seam/7\nDetail: a seam" {
		t.Errorf("summary: %q", got)
	}
	// A call no describer knows keeps its built-in template.
	other := Summarize(t.Context(), nil, SummaryCall{Method: "DELETE", Path: "/datasets/5"})
	if !strings.HasPrefix(other, "Delete dataset 5") {
		t.Errorf("built-in template lost: %q", other)
	}
}

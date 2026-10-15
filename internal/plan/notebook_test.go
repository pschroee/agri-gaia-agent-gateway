// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package plan

import (
	"strings"
	"testing"

	"agw/internal/delegation"
	"agw/internal/platform"
)

// A run of the agent's own code may be a step of a plan, but the plan never covers it: the user
// approves every run on its own (own code C-E3, confirm each of G1.1). The step keeps its class
// (compute) and its route, and the socket pins the SHA-256 of its files like for any other upload.
func TestPlanNotebookStepNeverCovered(t *testing.T) {
	// The classification of the gateway: the effect comes from the route (delegation.Classify),
	// not from the fallback of the plan package.
	gateway := func(r platform.Request) (string, string, bool) {
		a := delegation.Classify(r)
		return string(a.Effect), a.Action, a.ConfirmEach
	}
	const sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	raw := `{"title":"Own code","steps":[{"tool":"notebook_run",
	 "args":{"files":["/workspace/train.py"],"entry":"train.py","dataset_ids":["17"]},
	 "description":"train the own model","file_sha256":["` + sha + `"]}]}`
	steps, err := Build(proposal(t, raw), gateway)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 {
		t.Fatalf("steps: %+v", steps)
	}
	st := steps[0]
	if st.Tool != "notebook_run" || st.Method != "POST" || st.Path != platform.NotebookPrefix+"/runs" {
		t.Fatalf("step: %+v", st)
	}
	if st.Effect != "compute" {
		t.Errorf("effect: %q", st.Effect)
	}
	if st.Covered {
		t.Errorf("a notebook step must never be covered by the plan: %+v", st)
	}
	if len(st.Files) != 1 || st.Files[0].Path != "/workspace/train.py" || st.Files[0].SHA256 != sha {
		t.Errorf("files of the step: %+v", st.Files)
	}
	if !strings.Contains(string(st.Form), "train.py") || st.FormSHA256 == "" {
		t.Errorf("form of the step: %s", st.Form)
	}
	// The file references are readable for the socket, which pins their SHA-256 before the approval.
	refs, err := FileRefs([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || len(refs[1]) != 1 || refs[1][0].Path != "/workspace/train.py" {
		t.Errorf("file references: %v", refs)
	}
	// A run whose parameters the gateway cannot read is no step at all.
	broken := `{"title":"Own code","steps":[{"tool":"notebook_run","args":{"files":["/workspace/train.py"],"entry":"other.py"}}]}`
	if _, err := Build(proposal(t, broken), gateway); err == nil {
		t.Error("a run whose entry is not among its files must be refused")
	}
}

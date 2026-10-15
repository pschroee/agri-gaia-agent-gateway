// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package delegation

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"agw/internal/platform"
)

// runForm is the form of a run of the tests.
func runForm(ids ...string) map[string][]string {
	form := map[string][]string{"entry": {"train.py"}}
	if len(ids) > 0 {
		form["dataset_ids"] = ids
	}
	return form
}

// The seven notebook paths are classified as the plan says: the reads under _agw/notebook, the run as
// run notebook with compute, the stop as write, and the datasets of a run as objects touched too.
func TestClassifyNotebook(t *testing.T) {
	const runID = "r20260101120000-abcd"
	for _, p := range []string{"", "/runs", "/runs/" + runID, "/runs/" + runID + "/log", "/runs/" + runID + "/file"} {
		a := Classify(platform.Request{Method: http.MethodGet, Path: platform.NotebookPrefix + p})
		if a.Action != Read || a.Resource != Notebook || a.Effect != EffectRead || a.ConfirmEach {
			t.Errorf("GET %s: %+v", p, a)
		}
	}
	// The GETs under the prefix do not write (Request.Writes): nothing needs an approval beyond the read.
	if (platform.Request{Method: http.MethodGet, Path: platform.NotebookPrefix + "/runs"}).Writes() {
		t.Error("a reading notebook call counts as writing")
	}

	a := Classify(platform.Request{Method: http.MethodPost, Path: platform.NotebookPrefix + "/runs", Form: runForm("17", "18")})
	if a.Action != Run || a.Resource != Notebook || a.Effect != EffectCompute {
		t.Fatalf("run: %+v", a)
	}
	if len(a.Also) != 2 || a.Also[0].String() != "update dataset 17" || a.Also[1].String() != "update dataset 18" {
		t.Errorf("datasets of the run: %v", a.Also)
	}
	// Leading zeros are canonical (the platform reads 017 as 17).
	a = Classify(platform.Request{Method: http.MethodPost, Path: platform.NotebookPrefix + "/runs", Form: runForm("017")})
	if len(a.Also) != 1 || a.Also[0].ID != "17" {
		t.Errorf("canonical dataset id: %v", a.Also)
	}
	// Without datasets there is nothing else to allow.
	a = Classify(platform.Request{Method: http.MethodPost, Path: platform.NotebookPrefix + "/runs", Form: runForm()})
	if len(a.Also) != 0 || a.Problem != "" {
		t.Errorf("run without datasets: %+v", a)
	}

	stop := Classify(platform.Request{Method: http.MethodPost, Path: platform.NotebookPrefix + "/stop"})
	if stop.Action != Update || stop.Resource != Notebook || stop.Effect != EffectWrite || stop.ConfirmEach {
		t.Errorf("stop: %+v", stop)
	}

	// A form the runner would refuse is a problem: the call is refused before anything else.
	for _, form := range []map[string][]string{
		{"entry": {"train.py"}, "shell": {"rm -rf /"}},
		{"entry": {"train.py"}, "dataset_ids": {"x"}},
		{"entry": {"a; rm -rf /"}},
		{"entry": {"train.py"}, "timeout_minutes": {"0"}},
	} {
		a := Classify(platform.Request{Method: http.MethodPost, Path: platform.NotebookPrefix + "/runs", Form: form})
		if a.Problem != "unreadable run parameters" {
			t.Errorf("form %v: problem %q", form, a.Problem)
		}
	}
	// A path of the prefix that is not one of the routes stays API (a wrong path is never a run).
	other := Classify(platform.Request{Method: http.MethodPost, Path: platform.NotebookPrefix + "/runs/x"})
	if other.Resource != API {
		t.Errorf("unknown notebook path: %+v", other)
	}
}

// Every run asks the user on its own, also with confirm "none"; the resource notebook exists for the
// rules, and without a rule for it everything of it is forbidden.
func TestNotebookRunConfirmEach(t *testing.T) {
	a := Classify(platform.Request{Method: http.MethodPost, Path: platform.NotebookPrefix + "/runs", Form: runForm()})
	if !a.ConfirmEach {
		t.Fatalf("a run must carry confirm each: %+v", a)
	}
	if !resources[Notebook] {
		t.Fatal("notebook must be a known resource for the rules")
	}
	// A delegation without a notebook rule refuses the run, even though the effect is delegated by
	// another rule (the resource is what counts).
	d, err := Parse([]byte(`{"rules":[{"action":"run","resource":"training","ids":["*"]}],"confirm":"none"}`))
	if err != nil {
		t.Fatal(err)
	}
	if dec := d.Check(a, time.Now(), nil); dec.Allowed {
		t.Fatalf("a run without a notebook rule must be refused: %v", dec)
	}
	d, err = Parse([]byte(`{"rules":[{"action":"run","resource":"notebook"}],"confirm":"none"}`))
	if err != nil {
		t.Fatal(err)
	}
	if dec := d.Check(a, time.Now(), nil); !dec.Allowed {
		t.Fatalf("a run with its rule must be allowed: %v", dec)
	}
	// The datasets of the run need their own rule.
	withDataset := Classify(platform.Request{Method: http.MethodPost, Path: platform.NotebookPrefix + "/runs", Form: runForm("17")})
	if dec := d.Check(withDataset, time.Now(), nil); dec.Allowed {
		t.Fatal("a run that reads a dataset without a rule must be refused")
	}
	if !strings.Contains(decReason(d, withDataset), "update dataset 17") {
		t.Errorf("the refusal must name the dataset: %q", decReason(d, withDataset))
	}
}

// decReason is the reason a delegation refuses an access (empty when it allows it).
func decReason(d *Delegation, a Access) string {
	return d.Check(a, time.Now(), nil).Reason
}

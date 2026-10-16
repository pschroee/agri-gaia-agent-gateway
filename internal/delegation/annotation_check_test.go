// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package delegation

import (
	"encoding/json"
	"net/http"
	"testing"

	"agw/internal/platform"
)

// The routes of V3 are classified from method and path alone; the dataset comes from the body, and a
// check and a fill are compute, like a run over all images (E9). Reading the state of a job is read on
// the resource of the run.
func TestClassifyAnnotationChecks(t *testing.T) {
	const job = "11111111-2222-3333-4444-555555555555"
	for _, tc := range []struct {
		method, path, body string
		action, resource   string
		id                 string
		effect             Effect
	}{
		{"POST", platform.AnnotationPrefix + "/checks", `{"dataset_id":5,"function":"f","mode":"calibrate"}`, Run, Dataset, "5", EffectCompute},
		{"POST", platform.AnnotationPrefix + "/checks", `{"dataset_id":"5","function":"f","mode":"uncertain"}`, Run, Dataset, "5", EffectCompute},
		{"POST", platform.AnnotationPrefix + "/fills", `{"dataset_id":5,"function":"f"}`, Run, Dataset, "5", EffectCompute},
		{"GET", platform.AnnotationPrefix + "/jobs/" + job, "", Read, AnnotationRun, job, EffectRead},
	} {
		req := platform.Request{Method: tc.method, Path: tc.path}
		if tc.body != "" {
			req.Body = json.RawMessage(tc.body)
		}
		got := Classify(req)
		if got.Action != tc.action || got.Resource != tc.resource || got.ID != tc.id || got.Effect != tc.effect || got.Problem != "" {
			t.Errorf("%s %s: %+v", tc.method, tc.path, got)
		}
	}
	// Without a dataset in the body only a rule with "*" matches: the identifier stands for "unknown".
	got := Classify(platform.Request{Method: http.MethodPost, Path: platform.AnnotationPrefix + "/checks",
		Body: json.RawMessage(`{"function":"f"}`)})
	if got.ID != unknownID || got.Effect != EffectCompute {
		t.Errorf("without a dataset: %+v", got)
	}
}

// The bodies of V3 carry exactly what a check and a fill decide; everything else is refused before the
// delegation check (Review 5, K1).
func TestAnnotationCheckFieldRules(t *testing.T) {
	call := func(op, body string) Access {
		return Classify(platform.Request{Method: http.MethodPost, Path: platform.AnnotationPrefix + op,
			Body: json.RawMessage(body)})
	}
	if a := call("/checks", `{"dataset_id":5,"function":"f","mode":"calibrate","limit":50,"mapping":{"class_0":"0"},"uncertain_below":60}`); a.Problem != "" {
		t.Errorf("the tool's own body was refused: %s", a.Problem)
	}
	if a := call("/checks", `{"dataset_id":5,"bucket_name":"x"}`); a.Problem == "" {
		t.Error("an extra field was accepted on the check route")
	}
	if a := call("/fills", `{"dataset_id":5,"function":"f","min_confidence":50,"mapping":{"class_0":"0"},"limit":20}`); a.Problem != "" {
		t.Errorf("the tool's own body was refused: %s", a.Problem)
	}
	if a := call("/fills", `{"dataset_id":5,"existing":"replace"}`); a.Problem == "" {
		t.Error("a field of the run route was accepted on the fill route")
	}
}

// The registered conformance cases of V3: calibrating another user's dataset is refused, the same call
// on a dataset of this delegation is allowed. Without the control a check that refuses everything would
// pass as correct.
func TestAnnotationCheckConformanceCases(t *testing.T) {
	var found, control int
	for _, c := range ConformanceCases() {
		switch c.Name {
		case "calibrate on another user's dataset":
			found++
			if c.Allowed {
				t.Error("the case expects to be allowed")
			}
			if eff := effectOfCase(t, c); eff != EffectCompute {
				t.Errorf("the case is %q, want compute", eff)
			}
		case "calibrate on an own dataset (control)":
			control++
			if !c.Allowed {
				t.Error("the control case expects to be refused")
			}
		}
	}
	if found != 1 || control != 1 {
		t.Fatalf("cases: %d forbidden, %d control", found, control)
	}
}

// effectOfCase classifies the call of a conformance case.
func effectOfCase(t *testing.T, c Case) Effect {
	t.Helper()
	req, err := c.Build()
	if err != nil {
		t.Fatalf("%s: %v", c.Name, err)
	}
	return Classify(req).Effect
}

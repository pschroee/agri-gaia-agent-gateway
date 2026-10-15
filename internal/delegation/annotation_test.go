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

// The routes of V1 are classified from method and path alone, and the dataset comes from the body:
// pre-annotation runs on a dataset, the function list on its own resource (E9, §2).
func TestClassifyAnnotation(t *testing.T) {
	for _, tc := range []struct {
		method, path, body string
		action, resource   string
		id                 string
		effect             Effect
	}{
		{"GET", platform.AnnotationPrefix + "/functions", "", Read, AnnotationFunction, "", EffectRead},
		{"POST", platform.AnnotationPrefix + "/tasks", `{"dataset_id":5}`, Update, Dataset, "5", EffectWrite},
		{"POST", platform.AnnotationPrefix + "/runs", `{"dataset_id":5,"existing":"keep"}`, Run, Dataset, "5", EffectCompute},
		{"POST", platform.AnnotationPrefix + "/runs", `{"dataset_id":"5","existing":"replace"}`, Run, Dataset, "5", EffectIrreversible},
		{"GET", platform.AnnotationPrefix + "/runs/run-1", "", Read, AnnotationRun, "run-1", EffectRead},
		{"GET", platform.AnnotationPrefix + "/datasets/5/summary", "", Read, Dataset, "5", EffectRead},
		{"POST", platform.AnnotationPrefix + "/datasets/5/sync", "", Update, Dataset, "5", EffectWrite},
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
	got := Classify(platform.Request{Method: http.MethodPost, Path: platform.AnnotationPrefix + "/tasks", Body: json.RawMessage(`{}`)})
	if got.ID != unknownID {
		t.Errorf("without a dataset: %+v", got)
	}
	// The run id is a string and stays as it is given.
	got = Classify(platform.Request{Method: http.MethodGet, Path: platform.AnnotationPrefix + "/runs/00ab-1"})
	if got.ID != "00ab-1" || got.Resource != AnnotationRun {
		t.Errorf("run id: %+v", got)
	}
}

// The bodies of V1 carry only the fields the tools build; anything else is refused before the
// delegation check (Review 5, K1).
func TestAnnotationFieldRules(t *testing.T) {
	runs := func(body string) Access {
		return Classify(platform.Request{Method: http.MethodPost, Path: platform.AnnotationPrefix + "/runs", Body: json.RawMessage(body)})
	}
	if a := runs(`{"dataset_id":5,"function":"f","min_confidence":50,"mapping":{"a":"b"},"existing":"keep"}`); a.Problem != "" {
		t.Errorf("the tool's own body was refused: %s", a.Problem)
	}
	if a := runs(`{"dataset_id":5,"bucket_name":"x"}`); a.Problem == "" {
		t.Error("an extra field was accepted")
	}
	tasks := func(body string) Access {
		return Classify(platform.Request{Method: http.MethodPost, Path: platform.AnnotationPrefix + "/tasks", Body: json.RawMessage(body)})
	}
	if a := tasks(`{"dataset_id":5}`); a.Problem != "" {
		t.Errorf("dataset_id was refused: %s", a.Problem)
	}
	if a := tasks(`{"dataset_id":5,"function":"f"}`); a.Problem == "" {
		t.Error("a function on the task route was accepted")
	}
}

// The registered conformance cases of V1: pre-annotating another user's dataset is refused, the same
// call on a dataset of this delegation is allowed.
func TestAnnotationConformanceCases(t *testing.T) {
	var found, control int
	for _, c := range ConformanceCases() {
		switch c.Name {
		case "pre-annotate another user's dataset":
			found++
			if c.Allowed {
				t.Error("the case expects to be allowed")
			}
		case "pre-annotate an own dataset (control)":
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

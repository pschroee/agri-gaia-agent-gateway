// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// The six tools of V1 build the calls of the virtual path; the limits of min_confidence and existing
// hold while building, so a wrong value never leaves the gateway (E13).
func TestAnnotationToolsBuild(t *testing.T) {
	task, ok := Lookup("annotation_task")
	if !ok {
		t.Fatal("annotation_task missing")
	}
	req, err := task.Build(json.RawMessage(`{"dataset_id":5}`))
	if err != nil || req.Method != http.MethodPost || req.Path != AnnotationPrefix+"/tasks" {
		t.Fatalf("annotation_task: %+v %v", req, err)
	}
	if string(req.Body) != `{"dataset_id":5}` {
		t.Errorf("body: %s", req.Body)
	}
	if _, err := task.Build(json.RawMessage(`{"dataset_id":5,"name":"x"}`)); err == nil {
		t.Error("an unknown argument was accepted")
	}
	if _, err := task.Build(json.RawMessage(`{}`)); err == nil {
		t.Error("a missing dataset_id was accepted")
	}

	pre, ok := Lookup("pre-annotate") // the CLI name finds the same tool
	if !ok {
		t.Fatal("pre-annotate missing")
	}
	req, err = pre.Build(json.RawMessage(`{"dataset_id":5,"function":"agw-x","mapping":{"class_0":"0"}}`))
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(req.Body, &body); err != nil {
		t.Fatal(err)
	}
	if req.Path != AnnotationPrefix+"/runs" || body["min_confidence"].(float64) != 50 || body["existing"] != "keep" {
		t.Errorf("defaults: %s", req.Body)
	}
	if m, ok := body["mapping"].(map[string]any); !ok || m["class_0"] != "0" {
		t.Errorf("mapping: %s", req.Body)
	}
	for _, bad := range []string{
		`{"dataset_id":5,"function":"f","min_confidence":0}`,
		`{"dataset_id":5,"function":"f","min_confidence":100}`,
		`{"dataset_id":5,"function":"f","existing":"drop"}`,
		`{"dataset_id":5,"function":"f","mapping":{"a":[1]}}`,
		`{"dataset_id":5}`,
		`{"function":"f"}`,
	} {
		if _, err := pre.Build(json.RawMessage(bad)); err == nil {
			t.Errorf("%s was accepted", bad)
		}
	}
	if _, err := pre.Build(json.RawMessage(`{"dataset_id":5,"function":"f","mapping":{"a":"b"},"existing":"replace"}`)); err != nil {
		t.Errorf("replace refused: %v", err)
	}

	run, _ := Lookup("annotation_run")
	req, err = run.Build(json.RawMessage(`{"run_id":"run-1"}`))
	if err != nil || req.Method != http.MethodGet || req.Path != AnnotationPrefix+"/runs/run-1" {
		t.Fatalf("annotation_run: %+v %v", req, err)
	}
	for _, id := range []string{`../x`, `a/b`, ``, strings.Repeat("a", 65)} {
		if _, err := run.Build(json.RawMessage(`{"run_id":"` + id + `"}`)); err == nil {
			t.Errorf("run_id %q was accepted", id)
		}
	}

	sum, _ := Lookup("annotation_summary")
	if req, err := sum.Build(json.RawMessage(`{"dataset_id":7}`)); err != nil || req.Path != AnnotationPrefix+"/datasets/7/summary" || req.Method != http.MethodGet {
		t.Errorf("annotation_summary: %+v %v", req, err)
	}
	sync, _ := Lookup("annotation-sync")
	if req, err := sync.Build(json.RawMessage(`{"dataset_id":7}`)); err != nil || req.Path != AnnotationPrefix+"/datasets/7/sync" || req.Method != http.MethodPost {
		t.Errorf("annotation_sync: %+v %v", req, err)
	}
	fns, _ := Lookup("annotation_functions")
	if req, err := fns.Build(nil); err != nil || req.Path != AnnotationPrefix+"/functions" {
		t.Errorf("annotation_functions: %+v %v", req, err)
	}
	// Every tool of the six is reachable through both names.
	for _, name := range []string{"annotation_functions", "annotation_task", "pre_annotate", "annotation_run", "annotation_summary", "annotation_sync"} {
		if _, ok := Lookup(name); !ok {
			t.Errorf("%s missing", name)
		}
	}
}

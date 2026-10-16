// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

import (
	"encoding/json"
	"testing"
)

// The three tools of V3 exist in both bindings and build the calls the service answers: the limits, the
// defaults and the fields of the body are set here, and the service checks them again (V3).
func TestAnnotationCheckTools(t *testing.T) {
	for _, tc := range []struct{ name, cli string }{
		{"annotation_check", "annotation-check"},
		{"annotation_fill", "annotation-fill"},
		{"annotation_job", "annotation-job"},
	} {
		tool, ok := Lookup(tc.name)
		if !ok {
			t.Fatalf("tool %s missing", tc.name)
		}
		if tool.CLI != tc.cli {
			t.Errorf("%s: cli name %q, want %q", tc.name, tool.CLI, tc.cli)
		}
		if tool.Desc == "" {
			t.Errorf("%s: no description", tc.name)
		}
	}

	check, _ := Lookup("annotation_check")
	req, err := check.Build(json.RawMessage(`{"dataset_id":7,"function":"agw-test-m1-abcd1234","mode":"calibrate"}`))
	if err != nil || req.Method != "POST" || req.Path != AnnotationPrefix+"/checks" {
		t.Fatalf("check: %+v %v", req, err)
	}
	var body map[string]any
	if err := json.Unmarshal(req.Body, &body); err != nil {
		t.Fatal(err)
	}
	if body["dataset_id"].(float64) != 7 || body["mode"] != "calibrate" || body["limit"].(float64) != checkLimitCalibrate {
		t.Errorf("check body: %v", body)
	}
	if body["uncertain_below"].(float64) != uncertainBelowDefault {
		t.Errorf("the default of uncertain_below is missing: %v", body)
	}
	// The mode uncertain takes its own default, and uncertainty needs a threshold in its range.
	req, err = check.Build(json.RawMessage(`{"dataset_id":7,"function":"f","mode":"uncertain","uncertain_below":40}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(req.Body, &body); err != nil {
		t.Fatal(err)
	}
	if body["limit"].(float64) != checkLimitUncertain || body["uncertain_below"].(float64) != 40 {
		t.Errorf("uncertain body: %v", body)
	}
	for _, bad := range []string{
		`{"dataset_id":7,"function":"f","mode":"drop"}`,
		`{"dataset_id":7,"function":"f","mode":"calibrate","limit":1001}`,
		`{"dataset_id":7,"function":"f","mode":"calibrate","limit":0}`,
		`{"dataset_id":7,"function":"f","mode":"calibrate","uncertain_below":100}`,
		`{"dataset_id":7,"function":"f","mode":"calibrate","mapping":"nope"}`,
		`{"dataset_id":7,"function":"f"}`,
	} {
		if _, err := check.Build(json.RawMessage(bad)); err == nil {
			t.Errorf("accepted: %s", bad)
		}
	}

	fill, _ := Lookup("annotation_fill")
	req, err = fill.Build(json.RawMessage(`{"dataset_id":7,"function":"f","mapping":{"class_0":"0"}}`))
	if err != nil || req.Path != AnnotationPrefix+"/fills" {
		t.Fatalf("fill: %+v %v", req, err)
	}
	if err := json.Unmarshal(req.Body, &body); err != nil {
		t.Fatal(err)
	}
	if body["limit"].(float64) != fillLimitDefault || body["min_confidence"].(float64) != minConfidenceDefault {
		t.Errorf("fill body: %v", body)
	}
	if body["mapping"].(map[string]any)["class_0"] != "0" {
		t.Errorf("the mapping did not reach the body: %v", body)
	}
	for _, bad := range []string{
		`{"dataset_id":7,"function":"f","min_confidence":100}`,
		`{"dataset_id":7,"function":"f","limit":1001}`,
		`{"function":"f"}`,
	} {
		if _, err := fill.Build(json.RawMessage(bad)); err == nil {
			t.Errorf("accepted: %s", bad)
		}
	}

	job, _ := Lookup("annotation_job")
	req, err = job.Build(json.RawMessage(`{"job_id":"11111111-2222-3333-4444-555555555555","page":3}`))
	if err != nil || req.Method != "GET" || req.Path != AnnotationPrefix+"/jobs/11111111-2222-3333-4444-555555555555" {
		t.Fatalf("job: %+v %v", req, err)
	}
	if req.Query["page"] != "3" {
		t.Errorf("page: %v", req.Query)
	}
	for _, bad := range []string{
		`{"job_id":"not-a-job"}`,
		`{"job_id":"11111111-2222-3333-4444-555555555555","page":0}`,
		`{}`,
	} {
		if _, err := job.Build(json.RawMessage(bad)); err == nil {
			t.Errorf("accepted: %s", bad)
		}
	}
}

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

// A start of a deployment created by an earlier step reads nothing and names no id it does not have (issue #225).
func TestSummaryStartDeploymentPlaceholder(t *testing.T) {
	f := edgeSummaryReads()
	got := summarize(t, f, SummaryCall{Tool: "start_deployment", Method: "PUT", Path: "/container-deployments/{{step8.id}}/deploy"})
	if !strings.HasPrefix(got, "Start the deployment created by step 8") {
		t.Fatalf("head:\n%s", got)
	}
	for _, bad := range []string{"{{", "(no id)", "could not be read", "an image the tool resolves"} {
		if strings.Contains(got, bad) {
			t.Errorf("summary contains %q:\n%s", bad, got)
		}
	}
	if len(f.got) != 0 {
		t.Errorf("a placeholder was read: %v", f.got)
	}
}

// No path with a placeholder is ever read, whatever the call names.
func TestSummaryNeverReadsPlaceholderPath(t *testing.T) {
	f := edgeSummaryReads()
	summarize(t, f, SummaryCall{Tool: "start_deployment", Method: "PUT", Path: "/container-deployments/{{step2.id}}/deploy"})
	summarize(t, f, SummaryCall{Tool: "rename_dataset", Method: "PATCH", Path: "/datasets/{{step1.id}}", Body: json.RawMessage(`{"name":"a"}`)})
	summarize(t, f, SummaryCall{Tool: "assess_model", Method: http.MethodPost, Path: AssessCheckPath,
		Body: json.RawMessage(`{"model_id":"{{step4.id}}","eval_dataset_id":"{{step1.id}}"}`)})
	for _, p := range f.got {
		if strings.Contains(p, "{{") {
			t.Errorf("read of a placeholder path: %s", p)
		}
	}
}

// The check of a model created by a step names it once, without an empty name and without "model model".
func TestSummaryAssessModelPlaceholder(t *testing.T) {
	got := summarize(t, &fakeReads{answers: map[string]string{}}, SummaryCall{Tool: "assess_model", Method: http.MethodPost, Path: AssessCheckPath,
		Body: json.RawMessage(`{"model_id":"{{step4.id}}","eval_dataset_id":"{{step1.id}}"}`)})
	if !strings.HasPrefix(got, "Check the model created by step 4 on ") {
		t.Fatalf("head:\n%s", got)
	}
	if strings.Contains(got, `""`) || strings.Contains(got, "model model") {
		t.Errorf("summary:\n%s", got)
	}
}

func TestSummaryAssessModelNamed(t *testing.T) {
	f := &fakeReads{answers: map[string]string{"/models/37": `{"id":37,"name":"flowers-mini","format":"onnx"}`}}
	got := summarize(t, f, SummaryCall{Tool: "assess_model", Method: http.MethodPost, Path: AssessCheckPath,
		Body: json.RawMessage(`{"model_id":37}`)})
	if !strings.HasPrefix(got, `Check model 37 "flowers-mini"`) || strings.Contains(got, "model model") || strings.Contains(got, `""`) {
		t.Errorf("summary:\n%s", got)
	}
}

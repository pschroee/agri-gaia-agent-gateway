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

// The preview of a function built from a model (V2) names the model, the classes, the input and what
// the function will be: the code comes from the gateway, and the function stays for every CVAT user.
// Every field of the call is named, so nothing of it stays hidden behind a friendly sentence.
func TestSummaryAnnotationFunction(t *testing.T) {
	f := &fakeReads{answers: map[string]string{"/models/3": testModel}}
	call := SummaryCall{Tool: "annotation_function", Method: http.MethodPost,
		Path: AnnotationPrefix + "/functions",
		Body: json.RawMessage(`{"model_id":3,"kind":"classification","labels":["cat","dog"],"input_size":320}`)}
	got := summarize(t, f, call)
	wantLines(t, got,
		`Builds a CVAT pre-annotation function from model 3 "mnist-8-zoo (0.67 Acc.)" with template classification v1`,
		"Classes (labels, in the order the model outputs them): cat, dog",
		"Input: 320x320 pixels (input_size of the call)",
		"The model file must be ONNX",
		"The function is visible to and usable by every CVAT user of the platform, and the agent cannot remove it (ask the operator).",
		`annotation_functions shows its state, "ready" means it can be run.`)
	if strings.Contains(got, "Also sends") {
		t.Errorf("the summary hides a field:\n%s", got)
	}
	if len(f.got) != 1 || f.got[0] != "GET /models/3" {
		t.Errorf("reads of the summary: %v", f.got)
	}

	// Without labels and without input_size the model answers both.
	bare := summarize(t, &fakeReads{answers: map[string]string{"/models/3": testModel}},
		SummaryCall{Method: http.MethodPost, Path: AnnotationPrefix + "/functions",
			Body: json.RawMessage(`{"model_id":3,"kind":"classification"}`)})
	wantLines(t, bare,
		"Classes (output_labels of the model, in the order it outputs them): 0, 1",
		"Input: 64x64 pixels (input_shape of the model)")

	// A detector without class names: the preview says that the service refuses it, and a model the
	// gateway cannot read is named as such.
	det := summarize(t, nil, SummaryCall{Method: http.MethodPost, Path: AnnotationPrefix + "/functions",
		Body: json.RawMessage(`{"model_id":3,"kind":"detection"}`)})
	wantLines(t, det, "without class names a detector cannot be built")

	// A call of the same feature that is not the build keeps the built-in template; a body the
	// service would refuse keeps it as well.
	other := summarize(t, nil, SummaryCall{Method: http.MethodGet, Path: AnnotationPrefix + "/functions"})
	if !strings.HasPrefix(other, "Send GET ") {
		t.Errorf("a reading call: %q", other)
	}
	odd := summarize(t, nil, SummaryCall{Method: http.MethodPost, Path: AnnotationPrefix + "/functions",
		Body: json.RawMessage(`{"kind":"classification"}`)})
	if !strings.HasPrefix(odd, "Send POST ") || !strings.Contains(odd, "Also sends") {
		t.Errorf("a call without a model: %q", odd)
	}
}

// The preview of a rollback (V3b) names the source, the extent and that nothing the user annotated or
// corrected himself is touched; every field of the call is named, so nothing stays hidden.
func TestSummaryAnnotationRollback(t *testing.T) {
	got := summarize(t, nil, SummaryCall{Tool: "annotation_rollback", Method: http.MethodPost,
		Path: AnnotationPrefix + "/rollbacks",
		Body: json.RawMessage(`{"source_id":"run-1","scope":"suspicious"}`)})
	wantLines(t, got,
		"Takes back the annotations a CVAT pre-annotation wrote",
		"Deletes in CVAT the annotations of the run or fill run-1, only the images the last verification named as suspicious.",
		"stands in its own register for this chat and this source",
		"reported as changed_kept",
		"comes again through a new fill with the same model and setting")
	if strings.Contains(got, "Also sends") {
		t.Errorf("the summary hides a field:\n%s", got)
	}
	// Without a scope the whole source is taken back; without a source the built-in template stands, so
	// that a call the service refuses is not described as if it did something.
	bare := summarize(t, nil, SummaryCall{Method: http.MethodPost, Path: AnnotationPrefix + "/rollbacks",
		Body: json.RawMessage(`{"source_id":"job-2"}`)})
	wantLines(t, bare, "every image of the task")
	odd := summarize(t, nil, SummaryCall{Method: http.MethodPost, Path: AnnotationPrefix + "/rollbacks",
		Body: json.RawMessage(`{"scope":"all"}`)})
	if !strings.HasPrefix(odd, "Send POST ") || !strings.Contains(odd, "Also sends") {
		t.Errorf("a rollback without a source: %q", odd)
	}
}

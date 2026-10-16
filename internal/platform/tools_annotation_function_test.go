// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

import (
	"encoding/json"
	"net/http"
	"testing"
)

// The tool of V2 builds the call of the function path; the limits of kind and input_size hold while
// building, so a wrong value never leaves the gateway.
func TestAnnotationFunctionTool(t *testing.T) {
	tool, ok := Lookup("annotation_function")
	if !ok {
		t.Fatal("annotation_function missing")
	}
	if cli, ok := Lookup("annotation-function"); !ok || cli.Name != tool.Name {
		t.Fatal("the CLI name does not find the tool")
	}
	req, err := tool.Build(json.RawMessage(`{"model_id":5,"kind":"classification"}`))
	if err != nil || req.Method != http.MethodPost || req.Path != AnnotationPrefix+"/functions" {
		t.Fatalf("annotation_function: %+v %v", req, err)
	}
	var body map[string]any
	if err := json.Unmarshal(req.Body, &body); err != nil {
		t.Fatal(err)
	}
	if body["model_id"].(float64) != 5 || body["kind"] != "classification" {
		t.Errorf("body: %s", req.Body)
	}
	if _, sent := body["labels"]; sent {
		t.Error("labels were sent although the agent named none")
	}
	if _, sent := body["input_size"]; sent {
		t.Error("input_size was sent although the agent named none")
	}

	req, err = tool.Build(json.RawMessage(`{"model_id":5,"kind":"detection","labels":["pig","cow"],"input_size":640}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(req.Body, &body); err != nil {
		t.Fatal(err)
	}
	labels, _ := body["labels"].([]any)
	if len(labels) != 2 || labels[0] != "pig" || body["input_size"].(float64) != 640 {
		t.Errorf("body: %s", req.Body)
	}

	for _, bad := range []string{
		`{"model_id":5,"kind":"segmentation"}`,
		`{"model_id":5}`,
		`{"kind":"classification"}`,
		`{"model_id":5,"kind":"classification","input_size":8}`,
		`{"model_id":5,"kind":"classification","input_size":4096}`,
		// A single name is accepted as a shorthand list for every array argument of the binding (Args.strings);
		// an entry that is not a name is not.
		`{"model_id":5,"kind":"classification","labels":[5]}`,
		`{"model_id":5,"kind":"classification","function_yaml":"x"}`,
	} {
		if _, err := tool.Build(json.RawMessage(bad)); err == nil {
			t.Errorf("%s was accepted", bad)
		}
	}
}

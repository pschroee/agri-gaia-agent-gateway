// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package plan

import (
	"encoding/json"
	"reflect"
	"testing"
)

// #199: a raw step on a gateway path is the same request as the step of the tool of that path, and a
// placeholder stays a reference.
func TestStepRequestGatewayRawStep(t *testing.T) {
	raw, refs, err := StepRequest(StepSpec{Method: "POST", Path: "/_agw/assess/splits",
		Body: json.RawMessage(`{"dataset_id":5,"train_share":80}`)})
	if err != nil {
		t.Fatal(err)
	}
	viaTool, _, err := StepRequest(StepSpec{Tool: "split_dataset", Args: json.RawMessage(`{"dataset_id":5,"train_share":80}`)})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(raw, viaTool) || len(refs) != 0 {
		t.Fatalf("raw step\n%+v\ntool step\n%+v", raw, viaTool)
	}
	// An id from an earlier step in the path and in a body field is still a reference.
	_, refs, err = StepRequest(StepSpec{Method: "POST", Path: "/_agw/annotation/datasets/{{step1.id}}/sync"})
	if err != nil || !reflect.DeepEqual(refs, []int{1}) {
		t.Fatalf("path placeholder: refs %v, err %v", refs, err)
	}
	_, refs, err = StepRequest(StepSpec{Method: "POST", Path: "/_agw/annotation/tasks", Body: json.RawMessage(`{"dataset_id":"{{step2.id}}"}`)})
	if err != nil || !reflect.DeepEqual(refs, []int{2}) {
		t.Fatalf("body placeholder: refs %v, err %v", refs, err)
	}
	// What the REST binding refuses, a step cannot be either.
	if _, _, err := StepRequest(StepSpec{Method: "POST", Path: "/_agw/notebook/runs", Body: json.RawMessage(`{}`)}); err == nil {
		t.Error("a notebook path as a step")
	}
	if _, _, err := StepRequest(StepSpec{Method: "POST", Path: "/_agw/plan", Body: json.RawMessage(`{}`)}); err == nil {
		t.Error("the plan path as a step")
	}
}

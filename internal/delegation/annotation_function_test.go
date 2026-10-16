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

// Building a function from a model (V2) is irreversible — the function is visible to every CVAT user
// and the agent cannot remove it — and the call reads the model of its body (E9, E10).
func TestClassifyAnnotationFunction(t *testing.T) {
	call := func(body string) platform.Request {
		return platform.Request{Method: http.MethodPost, Path: platform.AnnotationPrefix + "/functions",
			Body: json.RawMessage(body)}
	}
	got := Classify(call(`{"model_id":5,"kind":"classification","labels":["0","1"],"input_size":64}`))
	if got.Action != Create || got.Resource != AnnotationFunction || got.ID != "" || got.Effect != EffectIrreversible || got.Problem != "" {
		t.Fatalf("classification: %+v", got)
	}
	if len(got.Also) != 1 || got.Also[0].Action != Read || got.Also[0].Resource != Model || got.Also[0].ID != "5" {
		t.Errorf("the model of the body is not part of the access: %+v", got.Also)
	}
	// Without a model in the body the read is an unknown object: only a rule with "*" covers it.
	if none := Classify(call(`{"kind":"classification"}`)); len(none.Also) != 1 || none.Also[0].ID != unknownID {
		t.Errorf("without a model: %+v", none.Also)
	}
	// The body the tool builds passes the field rule; anything else is refused before the delegation.
	if other := Classify(call(`{"model_id":7,"kind":"detection"}`)); other.Problem != "" {
		t.Errorf("the tool's own body was refused: %s", other.Problem)
	}
	for _, bad := range []string{
		`{"model_id":5,"kind":"classification","function_yaml":"name: x"}`,
		`{"model_id":5,"kind":"classification","code":"import os"}`,
	} {
		a := Classify(call(bad))
		if a.Problem == "" {
			t.Errorf("%s was accepted", bad)
		}
	}
	// The function list stays a read of its own resource.
	list := Classify(platform.Request{Method: http.MethodGet, Path: platform.AnnotationPrefix + "/functions"})
	if list.Action != Read || list.Resource != AnnotationFunction || list.Effect != EffectRead {
		t.Errorf("the function list: %+v", list)
	}
}

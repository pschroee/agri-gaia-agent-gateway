// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package delegation

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"agw/internal/platform"
)

// modelFull is a complete body of PATCH /models/{id}: every field of platform.ModelPatchFields.
const modelFull = `{"name":"n","format":"onnx","input_name":"i","input_datatype":"float32","input_shape":[1,3],` +
	`"input_semantics":"NCHW","output_name":"o","output_datatype":"float32","output_shape":[1,2],"output_labels":["0","1"]}`

// problemOf classifies a PATCH with the given body and returns the problem (empty: allowed).
func problemOf(path, body string) string {
	req := platform.Request{Method: http.MethodPatch, Path: path}
	if body != "" {
		req.Body = json.RawMessage(body)
	}
	return Classify(req).Problem
}

func TestModelPatchFieldRules(t *testing.T) {
	if p := problemOf("/models/5", modelFull); p != "" {
		t.Fatalf("complete body refused: %s", p)
	}
	if p := problemOf("/models/5", `{"name":"x"}`); !strings.Contains(p, "fields missing") ||
		!strings.Contains(p, "input_shape") || !strings.Contains(p, "format") {
		t.Fatalf("name only: %q", p)
	}
	noFormat := strings.Replace(modelFull, `"format":"onnx",`, "", 1)
	if p := problemOf("/models/5", noFormat); !strings.Contains(p, "fields missing") || !strings.Contains(p, "format") {
		t.Fatalf("without format: %q", p)
	}
	withOwner := strings.TrimSuffix(modelFull, "}") + `,"owner":"x"}`
	if p := problemOf("/models/5", withOwner); !strings.Contains(p, `field "owner" not allowed`) {
		t.Fatalf("owner: %q", p)
	}
	for _, body := range []string{"", `["name"]`} {
		if p := problemOf("/models/5", body); !strings.Contains(p, "needs a JSON object") {
			t.Fatalf("body %q: %q", body, p)
		}
	}
	if p := problemOf("/models/5/toggle-public", ""); p != "" {
		t.Fatalf("toggle-public refused: %s", p)
	}

	if p := problemOf("/datasets/12", `{"name":"x"}`); p != "" {
		t.Fatalf("dataset name refused: %s", p)
	}
	for _, body := range []string{`{"description":"x"}`, `{"bucket_name":"x"}`, `{"metadata_uri":"x"}`} {
		if p := problemOf("/datasets/12", body); !strings.Contains(p, "allowed: name") {
			t.Fatalf("dataset %s: %q", body, p)
		}
	}
}

// patchModelRules: the chat may update model 5 and dataset 12.
const patchModelRules = `{"rules":[{"action":"update","resource":"model","ids":["5"]},` +
	`{"action":"update","resource":"dataset","ids":["12"]}],"confirm":"none"}`

func TestModelPatchConformance(t *testing.T) {
	patch := func(path, body string) platform.Request {
		return platform.Request{Method: http.MethodPatch, Path: path, Body: json.RawMessage(body)}
	}
	cases := []Case{
		{Name: "model: complete body", Req: patch("/models/5", modelFull), Allowed: true, Rules: patchModelRules},
		{Name: "model: name only", Req: patch("/models/5", `{"name":"x"}`), Allowed: false, Rules: patchModelRules},
		{Name: "other model", Req: patch("/models/6", modelFull), Allowed: false, Rules: patchModelRules},
		{Name: "rename dataset 12", Tool: "rename_dataset", Args: `{"dataset_id":12,"name":"x"}`, Allowed: true, Rules: patchModelRules},
		{Name: "rename dataset 5", Tool: "rename_dataset", Args: `{"dataset_id":5,"name":"x"}`, Allowed: false, Rules: patchModelRules},
		{Name: "description on dataset 12", Req: patch("/datasets/12", `{"description":"x"}`), Allowed: false, Rules: patchModelRules},
	}
	for _, c := range cases {
		allowed, why := Evaluate(c, time.Now())
		if allowed != c.Allowed {
			t.Errorf("%s: allowed=%v, want %v (%s)", c.Name, allowed, c.Allowed, why)
		}
	}
}

// ClassifyPartial leaves out only the completeness of a body (issue #218): the allowlists and the refines
// of the route still apply, Classify is not changed.
func TestClassifyPartialSkipsOnlyRequired(t *testing.T) {
	patch := func(path, body string) platform.Request {
		return platform.Request{Method: http.MethodPatch, Path: path, Body: json.RawMessage(body)}
	}
	if p := ClassifyPartial(patch("/models/5", `{"name":"x"}`)).Problem; p != "" {
		t.Fatalf("name only, partial: %q", p)
	}
	if p := Classify(patch("/models/5", `{"name":"x"}`)).Problem; !strings.Contains(p, "fields missing") {
		t.Fatalf("name only, full: %q", p)
	}
	if p := ClassifyPartial(patch("/models/5", `{"name":"x","owner":"y"}`)).Problem; !strings.Contains(p, `field "owner" not allowed`) {
		t.Fatalf("a field outside the allowlist must stay refused: %q", p)
	}
	if p := ClassifyPartial(patch("/datasets/5", `{"description":"d"}`)).Problem; !strings.Contains(p, "not allowed") {
		t.Fatalf("another route's allowlist must stay: %q", p)
	}
	// The classification itself is the same.
	a, b := Classify(patch("/models/5", modelFull)), ClassifyPartial(patch("/models/5", modelFull))
	if a.Action != b.Action || a.Resource != b.Resource || a.ID != b.ID || a.Effect != b.Effect || b.Problem != "" {
		t.Fatalf("classification differs: %+v vs %+v", a, b)
	}
}

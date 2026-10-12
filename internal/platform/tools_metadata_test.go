// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestRenameDatasetBuild(t *testing.T) {
	tool, ok := Lookup("rename-dataset")
	if !ok || !tool.Write || tool.HasSteps() {
		t.Fatalf("rename-dataset: found=%v write=%v steps=%v", ok, tool.Write, tool.HasSteps())
	}
	req, err := tool.Build(json.RawMessage(`{"dataset_id":3,"name":"mnist-renamed_2.v1"}`))
	if err != nil {
		t.Fatal(err)
	}
	if req.Method != http.MethodPatch || req.Path != "/datasets/3" || string(req.Body) != `{"name":"mnist-renamed_2.v1"}` {
		t.Fatalf("call: %s %s %s", req.Method, req.Path, req.Body)
	}
	for _, raw := range []string{
		`{"dataset_id":3,"name":"a b"}`,
		`{"dataset_id":3,"name":"../x"}`,
		`{"dataset_id":3,"name":""}`,
		`{"dataset_id":3}`,
		`{"dataset_id":3,"name":"ok","description":"x"}`,
	} {
		if _, err := tool.Build(json.RawMessage(raw)); err == nil {
			t.Errorf("%s: accepted", raw)
		}
	}
}

func TestUpdateModelBuildChecks(t *testing.T) {
	tool, ok := Lookup("update-model")
	if !ok || !tool.Write || !tool.HasSteps() {
		t.Fatalf("update-model: found=%v write=%v steps=%v", ok, tool.Write, tool.HasSteps())
	}
	req, err := tool.Build(json.RawMessage(`{"model_id":5,"input_shape":"[1, 3, 32, 32]","output_labels":["0","1"],"input_semantics":"NCHW"}`))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"input_semantics":"NCHW","input_shape":[1,3,32,32],"output_labels":["0","1"]}`
	if req.Method != http.MethodPatch || req.Path != "/models/5" || string(req.Body) != want {
		t.Fatalf("call: %s %s %s", req.Method, req.Path, req.Body)
	}
	for _, raw := range []string{
		`{"model_id":5}`,
		`{"model_id":5,"format":"onnx"}`,
		`{"model_id":5,"name":"a/b"}`,
		`{"model_id":5,"name":"a\u0001b"}`,
		`{"model_id":5,"input_datatype":"float"}`,
		`{"model_id":5,"input_semantics":"NC"}`,
		`{"model_id":5,"input_shape":[1,"x"]}`,
		`{"model_id":5,"input_shape":[1,-2]}`,
		`{"model_id":5,"output_shape":"1,2"}`,
		`{"model_id":5,"input_shape":[]}`,
		`{"model_id":5,"output_labels":[]}`,
	} {
		if _, err := tool.Build(json.RawMessage(raw)); err == nil {
			t.Errorf("%s: accepted", raw)
		}
	}
	for _, w := range []string{"None Acc.", "platform_training_logs", "keeps the old name"} {
		if !strings.Contains(tool.Desc, w) {
			t.Errorf("description without %q", w)
		}
	}
}

func TestUpdateModelMerges(t *testing.T) {
	tool, ok := Lookup("update_model")
	if !ok {
		t.Fatal("update_model not found")
	}
	model := `{"id":5,"name":"old","format":"onnx","owner":"agent-test","bucket_name":"b","input_name":"image",` +
		`"input_datatype":"float32","input_shape":[1,3,64,64],"input_semantics":"NCHW","output_name":"out",` +
		`"output_datatype":"float32","output_shape":[1,2],"output_labels":null}`

	s := &stepsDoer{answers: map[string]Result{
		"GET /models/5":   {Status: "ok", HTTPStatus: 200, Body: model},
		"PATCH /models/5": {Status: "ok", HTTPStatus: 204},
	}}
	res, refused, err := tool.Run(context.Background(), json.RawMessage(`{"model_id":5,"name":"mnist_efficientnet"}`), s.do)
	if err != nil || refused != "" || res.Status != "ok" || res.HTTPStatus != 200 {
		t.Fatalf("res=%+v refused=%q err=%v", res, refused, err)
	}
	if len(s.calls) != 3 || !strings.HasPrefix(s.calls[0], "GET /models/5") ||
		!strings.HasPrefix(s.calls[1], "PATCH /models/5") || !strings.HasPrefix(s.calls[2], "GET /models/5") {
		t.Fatalf("calls: %v", s.calls)
	}
	body := strings.SplitN(s.calls[1], "|", 2)[1]
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("PATCH body %q: %v", body, err)
	}
	if len(m) != len(ModelPatchFields) {
		t.Fatalf("PATCH body has %d fields, want %d: %s", len(m), len(ModelPatchFields), body)
	}
	if m["name"] != "mnist_efficientnet" || m["format"] != "onnx" || m["input_semantics"] != "NCHW" {
		t.Fatalf("PATCH body: %s", body)
	}
	if !strings.Contains(body, `"input_shape":[1,3,64,64]`) || !strings.Contains(body, `"output_labels":null`) {
		t.Fatalf("PATCH body keeps no current values: %s", body)
	}
	if _, has := m["owner"]; has {
		t.Fatalf("PATCH body carries owner: %s", body)
	}

	s = &stepsDoer{answers: map[string]Result{
		"GET /models/5":   {Status: "ok", HTTPStatus: 200, Body: model},
		"PATCH /models/5": {Status: "rejected", Message: "rejected by the user"},
	}}
	res, _, _ = tool.Run(context.Background(), json.RawMessage(`{"model_id":5,"name":"x"}`), s.do)
	if res.Status != "rejected" || len(s.calls) != 2 {
		t.Fatalf("rejected PATCH: res=%+v calls=%v", res, s.calls)
	}
}

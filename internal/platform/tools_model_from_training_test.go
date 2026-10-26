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

// modelAnswer is the body of GET /train/containers/7/model with ONNX export and all tensor fields.
const modelAnswer = `{"id":21,"name":"EfficientNet (Torchvision) [mnist-mini, None Acc.]","format":"onnx",` +
	`"input_shape":[1,3,64,64],"input_datatype":"float32","input_semantics":"image","output_shape":[1,2],"output_datatype":"float32"}`

// summary is the part of the answer of create_model_from_training that the tests read.
type summary struct {
	ID             json.Number `json:"id"`
	Format         string      `json:"format"`
	TensorMetadata bool        `json:"tensor_metadata"`
	Warnings       []string    `json:"warnings"`
}

func readSummary(t *testing.T, body string) summary {
	t.Helper()
	var s summary
	d := json.NewDecoder(strings.NewReader(body))
	d.UseNumber()
	if err := d.Decode(&s); err != nil {
		t.Fatalf("summary is not JSON: %v in %s", err, body)
	}
	return s
}

func TestModelFromTrainingBuild(t *testing.T) {
	tool, ok := Lookup("model-from-training")
	if !ok {
		t.Fatal("model-from-training not found")
	}
	if tool.Name != "create_model_from_training" || !tool.Write || !tool.HasSteps() {
		t.Fatalf("tool = %+v", tool)
	}
	req, err := tool.Build(json.RawMessage(`{"train_container_id":"7"}`))
	if err != nil {
		t.Fatal(err)
	}
	if req.Method != http.MethodGet || req.Path != "/train/containers/7/model" || !req.Writes() {
		t.Fatalf("request = %+v", req)
	}
	if _, err := tool.Build(json.RawMessage(`{}`)); err == nil {
		t.Fatal("missing train_container_id accepted")
	}
	for _, want := range []string{"NEW platform model", "Every call creates another model"} {
		if !strings.Contains(tool.Desc, want) {
			t.Errorf("description lacks %q", want)
		}
	}
}

func TestModelFromTrainingSteps(t *testing.T) {
	tool, _ := Lookup("create_model_from_training")
	s := &stepsDoer{answers: map[string]Result{
		"GET /train/containers/7/status": {Status: "ok", HTTPStatus: 200, Body: `{"status":"exited"}`},
		"GET /train/containers":          {Status: "ok", HTTPStatus: 200, Body: `[{"id":6,"dataset_id":9},{"id":7,"dataset_id":3}]`},
		"GET /datasets/3":                {Status: "ok", HTTPStatus: 200, Body: `{"id":3}`},
		"GET /train/containers/7/model":  {Status: "ok", HTTPStatus: 200, Body: modelAnswer},
	}}
	res, refused, err := tool.Run(context.Background(), json.RawMessage(`{"train_container_id":7}`), s.do)
	if err != nil || refused != "" || res.Status != "ok" {
		t.Fatalf("res=%+v refused=%q err=%v", res, refused, err)
	}
	want := []string{"GET /train/containers/7/status", "GET /train/containers?limit=1000|", "GET /datasets/3", "GET /train/containers/7/model"}
	if len(s.calls) != len(want) {
		t.Fatalf("calls = %v", s.calls)
	}
	for i, w := range want {
		if !strings.HasPrefix(s.calls[i], w) {
			t.Errorf("call %d = %q, want prefix %q", i, s.calls[i], w)
		}
	}
	got := readSummary(t, res.Body)
	if got.ID.String() != "21" || got.Format != "onnx" || !got.TensorMetadata {
		t.Errorf("summary = %+v", got)
	}
	if len(got.Warnings) != 1 || !strings.Contains(got.Warnings[0], "{{tool:assess_model}}") {
		t.Errorf("warnings = %v", got.Warnings)
	}
}

func TestModelFromTrainingWarnsWithoutONNX(t *testing.T) {
	res := modelSummary(Result{Status: "ok", HTTPStatus: 200, Body: `{"id":4,"name":"m","format":"pytorch"}`})
	got := readSummary(t, res.Body)
	if got.TensorMetadata {
		t.Error("tensor_metadata true without tensor fields")
	}
	if len(got.Warnings) != 1 || !strings.Contains(got.Warnings[0], "without ONNX export") {
		t.Errorf("warnings = %v", got.Warnings)
	}
}

func TestModelFromTrainingRefusesUnfinished(t *testing.T) {
	tool, _ := Lookup("create_model_from_training")
	ctx := context.Background()
	raw := json.RawMessage(`{"train_container_id":7}`)

	s := &stepsDoer{answers: map[string]Result{
		"GET /train/containers/7/status": {Status: "ok", HTTPStatus: 200, Body: `{"status":"running"}`},
	}}
	res, refused, _ := tool.Run(ctx, raw, s.do)
	if res.Status != "error" || !strings.Contains(refused, `is "running", not finished`) || len(s.calls) != 1 {
		t.Errorf("running: res=%+v refused=%q calls=%v", res, refused, s.calls)
	}

	s = &stepsDoer{answers: map[string]Result{
		"GET /train/containers/7/status": {Status: "ok", HTTPStatus: 200, Body: `{"status":"exited"}`},
		"GET /train/containers":          {Status: "ok", HTTPStatus: 200, Body: `[{"id":7,"dataset_id":null}]`},
	}}
	res, refused, _ = tool.Run(ctx, raw, s.do)
	if res.Status != "error" || !strings.Contains(refused, "names no dataset") || len(s.calls) != 2 {
		t.Errorf("no dataset: res=%+v refused=%q calls=%v", res, refused, s.calls)
	}

	s = &stepsDoer{answers: map[string]Result{
		"GET /train/containers/7/status": {Status: "ok", HTTPStatus: 200, Body: `{"status":"exited"}`},
		"GET /train/containers":          {Status: "ok", HTTPStatus: 200, Body: `[{"id":6,"dataset_id":9}]`},
	}}
	res, refused, _ = tool.Run(ctx, raw, s.do)
	if res.Status != "error" || !strings.Contains(refused, "not in the platform's list") || len(s.calls) != 2 {
		t.Errorf("not in the list: res=%+v refused=%q calls=%v", res, refused, s.calls)
	}

	s = &stepsDoer{answers: map[string]Result{
		"GET /train/containers/7/status": {Status: "ok", HTTPStatus: 200, Body: `{"status":"exited"}`},
		"GET /train/containers":          {Status: "ok", HTTPStatus: 200, Body: `[{"id":6,"dataset_id":9},{"id":7,"dataset_id":3}]`},
		"GET /datasets/3":                {Status: "denied", Message: "no right to read dataset 3"},
	}}
	res, refused, _ = tool.Run(ctx, raw, s.do)
	if res.Status != "denied" || refused != "" || len(s.calls) != 3 {
		t.Errorf("denied dataset: res=%+v refused=%q calls=%v", res, refused, s.calls)
	}
	for _, c := range s.calls {
		if strings.HasPrefix(c, "GET /train/containers/7/model") {
			t.Errorf("model created after denied dataset: %v", s.calls)
		}
	}
}

func TestCreateTrainingExportConfigDescription(t *testing.T) {
	tool, ok := Lookup("create_training")
	if !ok {
		t.Fatal("create_training not found")
	}
	var desc string
	for _, p := range tool.Params {
		if p.Name == "export_config" {
			desc = p.Desc
		}
	}
	if desc == "" {
		t.Fatal("export_config has no description")
	}
	for _, want := range []string{"Always send it", "model.pt without tensor metadata", `"input_shapes":[[1,3,64,64]]`,
		`"opset_version":18`, `"dynamo":false`, "img-size"} {
		if !strings.Contains(desc, want) {
			t.Errorf("description lacks %q", want)
		}
	}
}

func TestGetTrainingFromList(t *testing.T) {
	tool, ok := Lookup("get_training")
	if !ok || !tool.HasSteps() {
		t.Fatalf("get_training missing or without steps: %v", ok)
	}
	s := &stepsDoer{answers: map[string]Result{
		"GET /train/containers": {Status: "ok", HTTPStatus: 200, Body: `[{"id":6,"status":"exited"},{"id":7,"status":"running","dataset_id":3}]`},
	}}
	res, refused, err := tool.Run(context.Background(), json.RawMessage(`{"train_container_id":7}`), s.do)
	if err != nil || refused != "" || res.Status != "ok" || !strings.Contains(res.Body, `"status":"running"`) || len(s.calls) != 1 {
		t.Fatalf("res=%+v refused=%q err=%v calls=%v", res, refused, err, s.calls)
	}
	res, refused, _ = tool.Run(context.Background(), json.RawMessage(`{"train_container_id":8}`), s.do)
	if res.Status != "error" || !strings.Contains(refused, "not in the platform's list") {
		t.Fatalf("unknown id: res=%+v refused=%q", res, refused)
	}
}

func TestModelSummaryOutputLabels(t *testing.T) {
	warned := func(warnings []string) bool {
		for _, w := range warnings {
			if strings.Contains(w, "recorded no class order") {
				return true
			}
		}
		return false
	}
	body := func(extra string) Result {
		return Result{Status: "ok", HTTPStatus: 200, Body: `{"id":4,"name":"m","format":"onnx"` + extra + `}`}
	}
	// Labels present: the field is in the answer, no warning.
	got := readSummary(t, modelSummary(body(`,"output_shape":[1,2],"output_labels":["1","0"]`)).Body)
	if warned(got.Warnings) {
		t.Errorf("warnings: %v", got.Warnings)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(modelSummary(body(`,"output_shape":[1,2],"output_labels":["1","0"]`)).Body), &raw); err != nil {
		t.Fatal(err)
	}
	if string(raw["output_labels"]) != `["1","0"]` {
		t.Errorf("output_labels = %s", raw["output_labels"])
	}
	// Missing at a classification output: warning, and the key is null.
	res := modelSummary(body(`,"output_shape":[1,2]`))
	if !warned(readSummary(t, res.Body).Warnings) {
		t.Errorf("no warning: %s", res.Body)
	}
	if err := json.Unmarshal([]byte(res.Body), &raw); err != nil || string(raw["output_labels"]) != "null" {
		t.Errorf("output_labels = %s (%v)", raw["output_labels"], err)
	}
	// A detector output: no warning.
	if got := readSummary(t, modelSummary(body(`,"output_shape":[1,25200,85]`)).Body); warned(got.Warnings) {
		t.Errorf("warnings: %v", got.Warnings)
	}
}

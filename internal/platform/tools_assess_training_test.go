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

// The tools of the training check are in the table (and therefore in every binding, with the CLI command
// from the same entry), they write, and they build the calls of the check paths (R1.1, R1.7).
func TestAssessTrainingToolsInTable(t *testing.T) {
	split, ok := Lookup("split_dataset")
	if !ok || split.CLI != "split-dataset" || !split.Write {
		t.Fatalf("split_dataset: %+v", split)
	}
	if !strings.Contains(split.Desc, "kept back") || !strings.Contains(split.Desc, "Needs approval") {
		t.Errorf("split description: %q", split.Desc)
	}
	req, err := split.Build(json.RawMessage(`{"dataset_id":12,"name":"mnist-train","train_share":80}`))
	if err != nil {
		t.Fatal(err)
	}
	if req.Method != http.MethodPost || req.Path != AssessSplitPath {
		t.Errorf("split call: %s %s", req.Method, req.Path)
	}
	if string(req.Body) != `{"dataset_id":12,"name":"mnist-train","train_share":80}` {
		t.Errorf("split body: %s", req.Body)
	}
	// The default name and share are the gateway's (nothing is sent for them).
	req, err = split.Build(json.RawMessage(`{"dataset_id":12}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(req.Body) != `{"dataset_id":12}` {
		t.Errorf("split body without name and share: %s", req.Body)
	}
	for _, raw := range []string{`{}`, `{"dataset_id":0}`, `{"dataset_id":"x"}`,
		`{"dataset_id":12,"train_share":55}`, `{"dataset_id":12,"train_share":100}`} {
		if _, err := split.Build(json.RawMessage(raw)); err == nil {
			t.Errorf("split accepted %s", raw)
		}
	}

	check, ok := Lookup("assess_model")
	if !ok || check.CLI != "assess-model" || !check.Write {
		t.Fatalf("assess_model: %+v", check)
	}
	if !strings.Contains(check.Desc, "final=true") || !strings.Contains(check.Desc, "once per dataset and chat") {
		t.Errorf("check description: %q", check.Desc)
	}
	req, err = check.Build(json.RawMessage(`{"model_id":3,"final":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if req.Method != http.MethodPost || req.Path != AssessCheckPath || string(req.Body) != `{"final":true,"model_id":3}` {
		t.Errorf("check call: %s %s %s", req.Method, req.Path, req.Body)
	}
	// The CLI and some models send the flag as text.
	req, err = check.Build(json.RawMessage(`{"model_id":3,"eval_dataset_id":7,"final":"false"}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(req.Body), "final") {
		t.Errorf("a false flag is sent: %s", req.Body)
	}
	if !strings.Contains(string(req.Body), `"eval_dataset_id":7`) {
		t.Errorf("the evaluation dataset is lost: %s", req.Body)
	}
	for _, raw := range []string{`{}`, `{"model_id":"x"}`, `{"model_id":3,"eval_dataset_id":0}`,
		`{"model_id":3,"final":1}`} {
		if _, err := check.Build(json.RawMessage(raw)); err == nil {
			t.Errorf("assess_model accepted %s", raw)
		}
	}
}

// The approval of an image build names the check of the model: the line follows the detail that names the
// model, so that both lines about the model stand together (R1.14, L9).
func TestBuildApprovalShowsModelCheck(t *testing.T) {
	body := `{"container_template_id":1,"model_id":3,"name":"mnist-triton","tag":"v1","architecture":"amd64"}`
	call := SummaryCall{Tool: "build_inference_image", Method: http.MethodPost, Path: "/container-images/build",
		Body: json.RawMessage(body)}
	read := func(_ context.Context, r Request) (Result, error) {
		switch r.Path {
		case "/models/3":
			return okBody(`{"id":3,"name":"mnist-effnet","format":"onnx","owner":"test"}`), nil
		case AssessPrefix + "/models/3/latest":
			return okBody(`{"model_id":3,"line":"usable (24 held-out images, accuracy 0.92)"}`), nil
		}
		return Result{Status: "ok", HTTPStatus: http.StatusOK, Body: "{}"}, nil
	}
	lines := strings.Split(Summarize(context.Background(), read, call), "\n")
	model, check := -1, -1
	for i, l := range lines {
		if strings.HasPrefix(l, "Model:") {
			model = i
		}
		if strings.HasPrefix(l, "Model check:") {
			check = i
		}
	}
	if model < 0 || check != model+1 {
		t.Fatalf("the check line does not follow the model line:\n%s", strings.Join(lines, "\n"))
	}
	if !strings.Contains(lines[check], "usable (24 held-out images, accuracy 0.92)") {
		t.Errorf("the line: %q", lines[check])
	}
	// Without a finished check of the model the line says so; a chat with only an interim check as well.
	read = func(_ context.Context, r Request) (Result, error) {
		if r.Path == AssessPrefix+"/models/3/latest" {
			return okBody(`{"model_id":3,"line":"none in this chat"}`), nil
		}
		return okBody(`{"id":3,"name":"mnist-effnet","format":"onnx"}`), nil
	}
	if got := Summarize(context.Background(), read, call); !strings.Contains(got, "Model check: none in this chat") {
		t.Errorf("without a check:\n%s", got)
	}
	// A read that fails leaves the line out instead of writing a wrong one (haltepunkt of the plan).
	read = func(_ context.Context, r Request) (Result, error) {
		if r.Path == AssessPrefix+"/models/3/latest" {
			return Result{Status: "error", Message: "check not found in this chat"}, nil
		}
		return okBody(`{"id":3,"name":"mnist-effnet","format":"onnx"}`), nil
	}
	if got := Summarize(context.Background(), read, call); strings.Contains(got, "Model check") {
		t.Errorf("an unreadable answer still writes a line:\n%s", got)
	}
}

// okBody is a successful platform answer with a JSON body.
func okBody(body string) Result {
	return Result{Status: "ok", HTTPStatus: http.StatusOK, Body: body}
}

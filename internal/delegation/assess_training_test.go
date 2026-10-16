// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package delegation

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"agw/internal/platform"
)

// The calls of the training check are classified from method and path alone, with the objects they touch
// in the body: the split creates a dataset and downloads the source, the check creates an assessment,
// reads the model and downloads the evaluation dataset (R1.1, R1.7).
func TestClassifySplitAndAssess(t *testing.T) {
	split := Classify(platform.Request{Method: http.MethodPost, Path: platform.AssessPrefix + "/splits",
		Body: json.RawMessage(`{"dataset_id":12}`)})
	if split.Action != Create || split.Resource != Dataset || split.Effect != EffectWrite || split.Problem != "" {
		t.Fatalf("split: %+v", split)
	}
	if len(split.Also) != 1 || split.Also[0].Action != Update || split.Also[0].Resource != Dataset || split.Also[0].ID != "12" {
		t.Fatalf("the split does not touch the source dataset: %+v", split.Also)
	}
	// Without a dataset in the body only a rule with "*" matches.
	if got := Classify(platform.Request{Method: http.MethodPost, Path: platform.AssessPrefix + "/splits"}); got.Also[0].ID != unknownID {
		t.Errorf("split without a dataset: %+v", got.Also)
	}

	check := Classify(platform.Request{Method: http.MethodPost, Path: platform.AssessPrefix + "/checks",
		Body: json.RawMessage(`{"model_id":5,"eval_dataset_id":12,"final":true}`)})
	if check.Action != Create || check.Resource != Assessment || check.Effect != EffectWrite || check.Problem != "" {
		t.Fatalf("check: %+v", check)
	}
	var readModel, updateDataset bool
	for _, a := range check.Also {
		if a.Action == Read && a.Resource == Model && a.ID == "5" {
			readModel = true
		}
		if a.Action == Update && a.Resource == Dataset && a.ID == "12" {
			updateDataset = true
		}
	}
	if !readModel || !updateDataset {
		t.Fatalf("the check does not touch the model and the dataset: %+v", check.Also)
	}
	// Without an evaluation dataset the gateway picks it from its own table, so the call touches only the
	// model.
	check = Classify(platform.Request{Method: http.MethodPost, Path: platform.AssessPrefix + "/checks",
		Body: json.RawMessage(`{"model_id":5}`)})
	if len(check.Also) != 1 || check.Also[0].Resource != Model {
		t.Fatalf("check without an evaluation dataset: %+v", check.Also)
	}
	// A field the gateway does not read is refused before anything goes out (issue #65): the fields of the
	// other check paths are not part of this one.
	for _, tc := range []struct{ path, body string }{
		{platform.AssessPrefix + "/checks", `{"model_id":5,"train_images":84}`},
		{platform.AssessPrefix + "/checks", `{"path":"/workspace/model.onnx","architecture":"resnet"}`},
		{platform.AssessPrefix + "/splits", `{"dataset_id":12,"kept_images":36}`},
		{platform.AssessPrefix + "/splits", `{"dataset_id":12,"files":["/workspace/a.png"]}`},
	} {
		got := Classify(platform.Request{Method: http.MethodPost, Path: tc.path, Body: json.RawMessage(tc.body)})
		if got.Problem == "" {
			t.Errorf("%s was accepted: %+v", tc.body, got)
		}
	}

	// R2.2: the same call with the file of the sandbox is the check of an own model. It reads no model —
	// the bytes come with the call — and it downloads the evaluation dataset like R1.
	file := Classify(platform.Request{Method: http.MethodPost, Path: platform.AssessPrefix + "/checks",
		Body: json.RawMessage(`{"path":"/workspace/notebook/r1/model.onnx","eval_dataset_id":12,"class_names":["0","1"],"preprocess":{"color":"rgb"}}`)})
	if file.Problem != "" || file.Action != Create || file.Resource != Assessment || file.Effect != EffectWrite {
		t.Fatalf("a check of a file: %+v", file)
	}
	if len(file.Also) != 1 || file.Also[0].Action != Update || file.Also[0].Resource != Dataset || file.Also[0].ID != "12" {
		t.Fatalf("a check of a file touches the evaluation dataset only: %+v", file.Also)
	}

	// What the last check of a model says is a reading call (R1.14), and so is the line about the check of a
	// file (R2.7).
	latest := Classify(platform.Request{Method: http.MethodGet, Path: platform.AssessPrefix + "/models/5/latest"})
	if latest.Action != Read || latest.Resource != Assessment || latest.Effect != EffectRead {
		t.Fatalf("latest: %+v", latest)
	}
	files := Classify(platform.Request{Method: http.MethodGet,
		Path: platform.AssessPrefix + "/files/" + strings.Repeat("ab", 32) + "/latest"})
	if files.Action != Read || files.Resource != Assessment || files.Effect != EffectRead {
		t.Fatalf("the check of a file: %+v", files)
	}
}

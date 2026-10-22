// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// The tool of issue #64 lives in its own file (not in the Tools literal) so that other tools can be
// added in parallel; init appends it.
func init() { Tools = append(Tools, createModelFromTraining) }

var createModelFromTraining = Tool{
	Name: "create_model_from_training", CLI: "model-from-training",
	Desc: "Creates a NEW platform model from a finished training container (GET /train/containers/<id>/model; the platform " +
		"writes the model although the call is a GET). It takes model.onnx, or model.pt when the training saved no ONNX " +
		"export, plus the training log. Every call creates another model, also for the same container: take a model over " +
		"once per training and only when the user wants it. Needs approval. Before the call it checks that the training is " +
		"exited and reads the training (from the list of training containers) and its dataset. The answer has id, name, format, tensor_metadata and warnings; a " +
		"model without tensor metadata cannot be built into an inference container.",
	Params: []Param{{Name: "train_container_id", Type: "integer", Desc: "id of the finished training container ({{tool:list_trainings}})", Required: true}},
	Write:  true,
	build:  byID(http.MethodGet, "/train/containers", "train_container_id", "/model"),
	steps:  modelFromTrainingSteps,
}

// modelTensorFields are the fields that validate_generator_input needs for an inference container.
var modelTensorFields = []string{"input_shape", "input_datatype", "input_semantics", "output_shape", "output_datatype"}

// modelFromTrainingSteps checks the training before the call that creates the model: the container must
// be exited, and its dataset must be readable for the user (the delegation checks that read like any other).
func modelFromTrainingSteps(ctx context.Context, a Args, final Request, do Doer) (Result, string, error) {
	id, _, err := a.int("train_container_id")
	if err != nil {
		return Refuse("%s", err.Error())
	}
	base := "/train/containers/" + strconv.FormatInt(id, 10)

	var st struct {
		Status string `json:"status"`
	}
	if res, stop, err := ReadJSON(ctx, do, Request{Method: http.MethodGet, Path: base + "/status"}, &st); stop {
		return res, "", err
	}
	if st.Status != "exited" {
		return Refuse("training container %d is %q, not finished; take the model over once {{tool:training_status}} shows exited", id, st.Status)
	}

	entry, res, refused, stop, err := trainingEntry(ctx, do, id)
	if stop {
		return res, refused, err
	}
	var tc struct {
		DatasetID json.Number `json:"dataset_id"`
	}
	if err := json.Unmarshal(entry, &tc); err != nil {
		return Result{Status: "error", Message: "training container entry unreadable: " + err.Error()}, "", nil
	}
	if tc.DatasetID == "" {
		return Refuse("training container %d names no dataset", id)
	}

	// Also Read Dataset: the dataset is read like any other object before the model is created.
	var ds any
	if res, stop, err := ReadJSON(ctx, do, Request{Method: http.MethodGet, Path: "/datasets/" + tc.DatasetID.String()}, &ds); stop {
		return res, "", err
	}

	res, err = do(ctx, final)
	if err == nil && res.Status == "ok" && res.HTTPStatus >= 200 && res.HTTPStatus < 300 {
		res = modelSummary(res)
		// The origin of the model (R1.6) is recorded by the gateway after the call, on every path that creates one.
	}
	return res, "", err
}

// modelSummary replaces the answer of the platform by id, name, format, tensor_metadata, warnings and
// the model itself. An unreadable answer is returned unchanged.
func modelSummary(res Result) Result {
	var m map[string]any
	d := json.NewDecoder(strings.NewReader(res.Body))
	d.UseNumber()
	if err := d.Decode(&m); err != nil || m == nil {
		return res
	}
	format, _ := m["format"].(string)
	name, _ := m["name"].(string)

	present := 0
	for _, f := range modelTensorFields {
		if v, ok := m[f]; ok && v != nil {
			present++
		}
	}
	tensorOK := present == len(modelTensorFields)

	warnings := []string{}
	if format != "onnx" {
		warnings = append(warnings, fmt.Sprintf("saved without ONNX export (format %s): no tensor metadata, so no inference container can be built from it; train again with export_config in {{tool:create_training}}", format))
	} else if !tensorOK {
		warnings = append(warnings, "ONNX model without complete tensor metadata; set them with {{tool:update_model}} before a container build")
	}
	if strings.Contains(name, "None ") {
		warnings = append(warnings, "the platform did not recognise the score (None in the name); check the model with {{tool:assess_model}}: the gateway reads the platform figure from the training log and measures the model on held-out images")
	}

	out, err := json.Marshal(map[string]any{
		"id":              m["id"],
		"name":            name,
		"format":          format,
		"tensor_metadata": tensorOK,
		"warnings":        warnings,
		"model":           json.RawMessage(res.Body),
	})
	if err != nil {
		return res
	}
	res.Body = string(out)
	return res
}

// trainingEntry reads one training container from the platform's list. GET /train/containers/{id} answers
// HTTP 500 on the platform (the response validation fails for a database object), so the list is used. stop is
// true when res is what the agent gets instead (a failed read, or an id that is not in the list).
func trainingEntry(ctx context.Context, do Doer, id int64) (entry json.RawMessage, res Result, refused string, stop bool, err error) {
	var list []json.RawMessage
	res, stop, err = ReadJSON(ctx, do, Request{Method: http.MethodGet, Path: "/train/containers", Query: map[string]string{"limit": "1000"}}, &list)
	if stop {
		return nil, res, "", true, err
	}
	want := strconv.FormatInt(id, 10)
	for _, raw := range list {
		var e struct {
			ID json.Number `json:"id"`
		}
		if json.Unmarshal(raw, &e) == nil && e.ID.String() == want {
			return raw, res, "", false, nil
		}
	}
	res, refused, _ = Refuse("training container %d is not in the platform's list of training containers", id)
	return nil, res, refused, true, nil
}

// getTrainingSteps answers platform_get_training from the list (see trainingEntry).
func getTrainingSteps(ctx context.Context, a Args, final Request, do Doer) (Result, string, error) {
	id, _, err := a.int("train_container_id")
	if err != nil {
		return Refuse("%s", err.Error())
	}
	entry, res, refused, stop, err := trainingEntry(ctx, do, id)
	if stop {
		return res, refused, err
	}
	return Result{Status: "ok", HTTPStatus: http.StatusOK, Body: string(entry)}, "", nil
}

func init() {
	for i := range Tools {
		if Tools[i].Name == "get_training" {
			Tools[i].steps = getTrainingSteps
			Tools[i].Desc = "Returns a training container (read from the list of training containers, because GET /train/containers/{id} fails on the platform)."
		}
	}
}

// TrainingDatasetID reads the dataset a training container was made from out of the platform's list of training
// containers (the answer that creates the model names none). 0 without an error: the container is listed but
// names no dataset.
func TrainingDatasetID(ctx context.Context, do Doer, id int64) (int64, error) {
	entry, res, _, stop, err := trainingEntry(ctx, do, id)
	if stop {
		if err != nil {
			return 0, err
		}
		return 0, errors.New(res.Message)
	}
	var tc struct {
		DatasetID json.Number `json:"dataset_id"`
	}
	if err := json.Unmarshal(entry, &tc); err != nil {
		return 0, err
	}
	n, err := tc.DatasetID.Int64()
	if err != nil {
		return 0, nil
	}
	return n, nil
}

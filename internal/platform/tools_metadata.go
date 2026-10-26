// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The tools of issue #65 live in their own file so that other tools can be added in parallel; init appends them.
func init() { Tools = append(Tools, renameDataset, updateModel) }

// ModelPatchFields are the fields of PATCH /models/{id}. The platform sets every field it finds in the body and
// sets the missing ones to null, so a body must carry all of them. format is among them: the backend takes it
// from the body too, and without it every container build fails. The delegation checks the same list
// (internal/delegation, model_fields.go).
var ModelPatchFields = []string{
	"name", "format", "input_name", "input_datatype", "input_shape", "input_semantics",
	"output_name", "output_datatype", "output_shape", "output_labels",
}

var datasetNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,99}$`)

var renameDataset = Tool{
	Name: "rename_dataset", CLI: "rename-dataset",
	Desc: "Renames a platform dataset. Sends only the name, because the platform sets every field of the body and would " +
		"clear the others. Needs approval. The catalogue entry in the triplestore keeps the old name. Keywords, description " +
		"and licence of a dataset cannot be changed on the platform.",
	Params: []Param{
		{Name: "dataset_id", Type: "integer", Desc: "id of the dataset", Required: true},
		{Name: "name", Type: "string", Desc: "new name: 1 to 100 characters, letters, digits, _ . - (starting with a letter or digit)", Required: true},
	},
	Write: true,
	build: buildRenameDataset,
}

func buildRenameDataset(a Args) (Request, error) {
	id, ok, err := a.int("dataset_id")
	if err != nil {
		return Request{}, err
	}
	if !ok || id < 0 {
		return Request{}, fmt.Errorf("dataset_id missing")
	}
	name, ok, err := a.str("name")
	if err != nil {
		return Request{}, err
	}
	if !ok {
		return Request{}, fmt.Errorf("name missing")
	}
	if !datasetNameRe.MatchString(name) {
		return Request{}, fmt.Errorf("name must be 1 to 100 characters: letters, digits, _ . - (starting with a letter or digit)")
	}
	body, err := json.Marshal(map[string]string{"name": name})
	if err != nil {
		return Request{}, err
	}
	return Request{Method: http.MethodPatch, Path: fmt.Sprintf("/datasets/%d", id), Body: body}, nil
}

var updateModel = Tool{
	Name: "update_model", CLI: "update-model",
	Desc: "Changes the name and the tensor metadata (input_* and output_*) of a platform model. The platform sets every field " +
		"of the body and would clear the others, so the tool first reads the model, merges the changes and sends all fields " +
		"(format stays as it is; it cannot be changed here). Give only the fields to change. Needs approval; the approval " +
		"shows the whole body. The catalogue entry in the triplestore keeps the old name. Score in the name: if the name " +
		"shows \"None Acc.\" (the platform's score pattern does not match Lightning output), check the model with " +
		"{{tool:assess_model}}: the gateway reads the platform figure from the training log and measures the model on " +
		"held-out images ({{tool:assess_status}} has the report; put the figure into the name once it is known). For a Triton container " +
		"use only letters, digits, _ . - (for example mnist_efficientnet). The answer is the model after the change.",
	Params: []Param{
		{Name: "model_id", Type: "integer", Desc: "id of the model", Required: true},
		{Name: "name", Type: "string", Desc: "new name: no / or \\, 1 to 200 characters"},
		{Name: "input_name", Type: "string", Desc: "name of the input tensor"},
		{Name: "input_datatype", Type: "string", Desc: "data type of the input: float16, float32, float64, int8, int16, int32, int64, uint8, uint16, uint32 or uint64"},
		{Name: "input_shape", Desc: "shape of the input, e.g. [1,3,64,64] (JSON list of 1 to 8 integers, -1 for a dynamic dimension)"},
		{Name: "input_semantics", Type: "string", Desc: "layout of the input: HWC, NHWC, CHW or NCHW"},
		{Name: "output_name", Type: "string", Desc: "name of the output tensor"},
		{Name: "output_datatype", Type: "string", Desc: "data type of the output (as input_datatype)"},
		{Name: "output_shape", Desc: "shape of the output, e.g. [1,2] (as input_shape)"},
		{Name: "output_labels", Type: "array", Desc: "class labels in the order the model outputs them (index 0 first). A model taken over from a training already carries them; change them only to the order the mapping of {{tool:assess_model}} shows"},
	},
	Write: true,
	build: buildUpdateModel,
	steps: updateModelSteps,
}

var (
	modelDatatypes = []string{"float16", "float32", "float64", "int8", "int16", "int32", "int64", "uint8", "uint16", "uint32", "uint64"}
	modelSemantics = []string{"HWC", "NHWC", "CHW", "NCHW"}
)

// buildUpdateModel checks the given fields and builds the PATCH with exactly these fields. updateModelSteps
// completes the body before it goes out.
func buildUpdateModel(a Args) (Request, error) {
	id, ok, err := a.int("model_id")
	if err != nil {
		return Request{}, err
	}
	if !ok || id < 0 {
		return Request{}, fmt.Errorf("model_id missing")
	}
	patch := map[string]any{}

	for _, f := range []string{"name", "input_name", "output_name"} {
		s, ok, err := a.str(f)
		if err != nil {
			return Request{}, err
		}
		if !ok {
			continue
		}
		if err := checkModelText(f, s); err != nil {
			return Request{}, err
		}
		if f == "name" && strings.ContainsAny(s, `/\`) {
			return Request{}, fmt.Errorf("name must not contain / or \\")
		}
		patch[f] = s
	}
	for _, f := range []string{"input_datatype", "output_datatype", "input_semantics"} {
		s, ok, err := a.str(f)
		if err != nil {
			return Request{}, err
		}
		if !ok {
			continue
		}
		allowed := modelDatatypes
		if f == "input_semantics" {
			allowed = modelSemantics
		}
		if !slices.Contains(allowed, s) {
			return Request{}, fmt.Errorf("%s must be one of %s", f, strings.Join(allowed, ", "))
		}
		patch[f] = s
	}
	for _, f := range []string{"input_shape", "output_shape"} {
		shape, ok, err := a.intList(f)
		if err != nil {
			return Request{}, err
		}
		if ok {
			patch[f] = shape
		}
	}
	if a["output_labels"] != nil {
		labels, err := a.strings("output_labels")
		if err != nil {
			return Request{}, err
		}
		if len(labels) == 0 {
			return Request{}, fmt.Errorf("output_labels must not be empty")
		}
		for _, l := range labels {
			if err := checkModelText("output_labels", l); err != nil {
				return Request{}, err
			}
		}
		patch["output_labels"] = labels
	}

	if len(patch) == 0 {
		return Request{}, fmt.Errorf("nothing to change: give name or at least one input_ or output_ field")
	}
	body, err := json.Marshal(patch)
	if err != nil {
		return Request{}, err
	}
	return Request{Method: http.MethodPatch, Path: fmt.Sprintf("/models/%d", id), Body: body}, nil
}

// checkModelText: 1 to 200 characters of valid UTF-8 without control characters.
func checkModelText(field, s string) error {
	if !utf8.ValidString(s) {
		return fmt.Errorf("%s is not valid UTF-8", field)
	}
	if n := utf8.RuneCountInString(s); n < 1 || n > 200 {
		return fmt.Errorf("%s must be 1 to 200 characters", field)
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return fmt.Errorf("%s contains a control character", field)
		}
	}
	return nil
}

// intList reads a list of 1 to 8 integers, each at least -1 (-1: dynamic dimension). A JSON text such as
// "[1, 3]" counts as the list, so that the CLI and the MCP tool take the same values.
func (a Args) intList(name string) ([]int64, bool, error) {
	v, ok := a[name]
	if !ok || v == nil {
		return nil, false, nil
	}
	if s, isStr := v.(string); isStr {
		d := json.NewDecoder(strings.NewReader(s))
		d.UseNumber()
		var x any
		if err := d.Decode(&x); err != nil || d.More() {
			return nil, false, fmt.Errorf("%s must be a list of integers, e.g. [1,3,64,64]", name)
		}
		v = x
	}
	items, isList := v.([]any)
	if !isList || len(items) < 1 || len(items) > 8 {
		return nil, false, fmt.Errorf("%s must be a list of 1 to 8 integers, e.g. [1,3,64,64]", name)
	}
	out := make([]int64, 0, len(items))
	for _, it := range items {
		var n int64
		switch x := it.(type) {
		case json.Number:
			i, err := x.Int64()
			if err != nil {
				return nil, false, fmt.Errorf("%s must contain integers only", name)
			}
			n = i
		case float64:
			if x != float64(int64(x)) {
				return nil, false, fmt.Errorf("%s must contain integers only", name)
			}
			n = int64(x)
		default:
			return nil, false, fmt.Errorf("%s must contain integers only", name)
		}
		if n < -1 {
			return nil, false, fmt.Errorf("%s: values below -1 are not allowed", name)
		}
		out = append(out, n)
	}
	return out, true, nil
}

// updateModelSteps reads the model, completes the body with the current values of ModelPatchFields and
// sends the PATCH with all of them. Its answer is the model after the change when the second read works,
// otherwise the answer of the PATCH.
func updateModelSteps(ctx context.Context, _ Args, final Request, do Doer) (Result, string, error) {
	var current map[string]any
	if res, stop, err := ReadJSON(ctx, do, Request{Method: http.MethodGet, Path: final.Path}, &current); stop {
		return res, "", err
	}

	var patch map[string]any
	d := json.NewDecoder(bytes.NewReader(final.Body))
	d.UseNumber()
	if err := d.Decode(&patch); err != nil {
		return Result{Status: "error", Message: err.Error()}, "", nil
	}
	body := map[string]any{}
	for _, f := range ModelPatchFields {
		body[f] = current[f]
	}
	for k, v := range patch {
		body[k] = v
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return Result{Status: "error", Message: err.Error()}, "", nil
	}

	res, err := do(ctx, Request{Method: http.MethodPatch, Path: final.Path, Body: raw})
	if err != nil || res.Status != "ok" || res.HTTPStatus < 200 || res.HTTPStatus >= 300 {
		return res, "", err
	}
	var after any
	if got, stop, _ := ReadJSON(ctx, do, Request{Method: http.MethodGet, Path: final.Path}, &after); !stop {
		return got, "", nil
	}
	return res, "", nil
}

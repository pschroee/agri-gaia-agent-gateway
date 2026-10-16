// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
)

// AnnotationPrefix is the virtual path prefix of the pre-annotation tools (V1). The calls go through
// PlatformCall like every other tool (delegation, effect class, approval, plan, E4) and are answered
// by internal/annotation instead of the platform.
const AnnotationPrefix = "/_agw/annotation"

// The six tools of pre-annotation (V1) live in their own file; init appends them, so that other tools
// can be added in parallel.
func init() {
	Tools = append(Tools, annotationFunctions, annotationTask, preAnnotate, annotationRun, annotationSummary, annotationSync)
}

// runIDRe checks the run id of a pre-annotation run: the id CVAT gives its rq job (hex) or a plain
// number.
var runIDRe = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)

// Confidence limits of a run: whole percent, 1 to 99 (E13).
const (
	minConfidenceDefault = 50
	minConfidenceLow     = 1
	minConfidenceHigh    = 99
)

var annotationFunctions = Tool{
	Name: "annotation_functions", CLI: "annotation-functions",
	Desc:   "Lists the pre-annotation functions the agent may run (CVAT/Nuclio detectors): id, labels, kind and where it comes from (yours from annotation_function, or installed by the operator). Functions of other users are not usable, because a function receives every image of the run.",
	Params: nil,
	build: func(Args) (Request, error) {
		return Request{Method: http.MethodGet, Path: AnnotationPrefix + "/functions"}, nil
	},
}

var annotationTask = Tool{
	Name: "annotation_task", CLI: "annotation-task",
	Desc:   "Prepares your image dataset for annotation in CVAT: creates the CVAT task with the dataset's classes (or reuses the existing one) and records it on the dataset. Only your own datasets of type AgriImageDataResource with classes (annotation_labels). Effect write; the user must approve.",
	Params: []Param{{Name: "dataset_id", Type: "integer", Desc: "id of the dataset", Required: true}},
	Write:  true,
	build: func(a Args) (Request, error) {
		id, err := requiredInt(a, "dataset_id")
		if err != nil {
			return Request{}, err
		}
		return Request{Method: http.MethodPost, Path: AnnotationPrefix + "/tasks",
			Body: json.RawMessage(`{"dataset_id":` + strconv.FormatInt(id, 10) + `}`)}, nil
	},
}

var preAnnotate = Tool{
	Name: "pre_annotate", CLI: "pre-annotate",
	Desc: `Runs a pre-annotation function over all images of the dataset's CVAT task. mapping maps function labels to dataset classes ({"cow": "Rind"}); without mapping only equal names are used, unmapped labels are dropped. min_confidence in percent (default 50). existing=keep adds to the task and is refused if the task already has annotations (use annotation_fill for images without annotations); existing=replace deletes ALL annotations of the task first (irreversible). Effect compute; the user must approve. Returns run_id; follow it with annotation_run in a background loop (30 s or more between queries).`,
	Params: []Param{
		{Name: "dataset_id", Type: "integer", Desc: "id of the dataset", Required: true},
		{Name: "function", Type: "string", Desc: "id of the pre-annotation function (platform_annotation_functions)", Required: true},
		{Name: "min_confidence", Type: "integer", Desc: "confidence threshold in percent, 1 to 99 (default 50)"},
		{Name: "mapping", Type: "object", Desc: `function label to dataset class, e.g. {"cow": "Rind"}`},
		{Name: "existing", Type: "string", Desc: "keep (default; adds to the task) or replace (deletes all annotations of the task first)"},
	},
	Write: true,
	build: buildPreAnnotate,
}

func buildPreAnnotate(a Args) (Request, error) {
	id, err := requiredInt(a, "dataset_id")
	if err != nil {
		return Request{}, err
	}
	fn, err := requiredStr(a, "function")
	if err != nil {
		return Request{}, err
	}
	conf, ok, err := a.int("min_confidence")
	if err != nil {
		return Request{}, err
	}
	if !ok {
		conf = minConfidenceDefault
	}
	if conf < minConfidenceLow || conf > minConfidenceHigh {
		return Request{}, fmt.Errorf("min_confidence must be between %d and %d percent", minConfidenceLow, minConfidenceHigh)
	}
	existing, ok, err := a.str("existing")
	if err != nil {
		return Request{}, err
	}
	if !ok {
		existing = "keep"
	}
	switch existing {
	case "keep", "replace":
	default:
		return Request{}, errors.New("existing must be keep or replace")
	}
	mapping, hasMapping, err := a.object("mapping")
	if err != nil {
		return Request{}, err
	}
	if !hasMapping {
		mapping = json.RawMessage(`{}`)
	}
	var pairs map[string]string
	if err := json.Unmarshal(mapping, &pairs); err != nil {
		return Request{}, errors.New(`mapping must be an object of strings (function label to dataset class), e.g. {"cow": "Rind"}`)
	}
	body := map[string]any{
		"dataset_id": id, "function": fn, "min_confidence": conf, "existing": existing,
		"mapping": json.RawMessage(mapping),
	}
	b, err := json.Marshal(body)
	if err != nil {
		return Request{}, err
	}
	return Request{Method: http.MethodPost, Path: AnnotationPrefix + "/runs", Body: b}, nil
}

var annotationRun = Tool{
	Name: "annotation_run", CLI: "annotation-run",
	Desc:   "State and progress of a pre-annotation run started in this chat (queued, started, finished, failed, expired).",
	Params: []Param{{Name: "run_id", Type: "string", Desc: "run_id from platform_pre_annotate", Required: true}},
	build: func(a Args) (Request, error) {
		id, ok, err := a.str("run_id")
		if err != nil {
			return Request{}, err
		}
		if !ok || !runIDRe.MatchString(id) {
			return Request{}, errors.New("run_id missing or not a valid run id")
		}
		return Request{Method: http.MethodGet, Path: AnnotationPrefix + "/runs/" + id}, nil
	},
}

var annotationSummary = Tool{
	Name: "annotation_summary", CLI: "annotation-summary",
	Desc:   "Counts the annotations of the dataset's CVAT task: per class manual and automatic, images with and without annotations.",
	Params: []Param{{Name: "dataset_id", Type: "integer", Desc: "id of the dataset", Required: true}},
	build:  annotationDatasetPath("summary"),
}

var annotationSync = Tool{
	Name: "annotation_sync", CLI: "annotation-sync",
	Desc:   "Writes the annotations from CVAT back into the dataset (annotations.xml in the object store), as the download in the platform does. Starting a training does this as well. Effect write; the user must approve.",
	Params: []Param{{Name: "dataset_id", Type: "integer", Desc: "id of the dataset", Required: true}},
	Write:  true,
	build:  annotationDatasetPath("sync"),
}

// ---------------------------------------------------------------------------
// V2 — a model of the platform as a pre-annotation function
// ---------------------------------------------------------------------------

// Limits of the function tool. The service checks them again: a raw call over the request tool
// reaches the same path without this table.
const (
	functionKindClassification = "classification"
	functionKindDetection      = "detection"
	minFunctionInput           = 32
	maxFunctionInput           = 2048
	maxFunctionLabels          = 1000
)

// V2 adds its tool in its own init, so that the tools of V3 can follow without changing the line of
// V1.
func init() { Tools = append(Tools, annotationFunction) }

var annotationFunction = Tool{
	Name: "annotation_function", CLI: "annotation-function",
	Desc: "Builds a CVAT pre-annotation function from your own ONNX model of the platform and provides it for the platform: the gateway packs a fixed, versioned template (classification or detection) with the model, a config.json and a function.yaml and sends the archive to POST /datasets/auto-annotation. Code never comes from the agent. kind is classification or detection; labels are the class names in the order the model outputs them (default: the model's output_labels, and for classification class_0 … class_{n-1} from output_shape); input_size is the square input side in pixels (default: from input_shape, else 224 for classification and 640 for detection). Effect irreversible with its own approval: the function is visible to and usable by every CVAT user, it receives every image of a run, and the agent cannot remove it (ask the operator). The build takes several minutes; annotation_functions then shows the state until it is ready. At most AGW_ANNOTATION_MAX_FUNCTIONS functions per user.",
	Params: []Param{
		{Name: "model_id", Type: "integer", Desc: "id of your own ONNX model of the platform (platform_list_models)", Required: true},
		{Name: "kind", Type: "string", Desc: "classification or detection", Required: true},
		{Name: "labels", Type: "array", Desc: "class names in the order the model outputs them"},
		{Name: "input_size", Type: "integer", Desc: "square input side in pixels, 32 to 2048"},
	},
	Write: true,
	build: buildAnnotationFunction,
}

func buildAnnotationFunction(a Args) (Request, error) {
	id, err := requiredInt(a, "model_id")
	if err != nil {
		return Request{}, err
	}
	kind, err := requiredStr(a, "kind")
	if err != nil {
		return Request{}, err
	}
	if kind != functionKindClassification && kind != functionKindDetection {
		return Request{}, fmt.Errorf("kind must be %s or %s", functionKindClassification, functionKindDetection)
	}
	labels, err := a.strings("labels")
	if err != nil {
		return Request{}, err
	}
	if len(labels) > maxFunctionLabels {
		return Request{}, fmt.Errorf("at most %d class names", maxFunctionLabels)
	}
	body := map[string]any{"model_id": id, "kind": kind}
	if len(labels) > 0 {
		body["labels"] = labels
	}
	size, ok, err := a.int("input_size")
	if err != nil {
		return Request{}, err
	}
	if ok {
		if size < minFunctionInput || size > maxFunctionInput {
			return Request{}, fmt.Errorf("input_size must be between %d and %d pixels", minFunctionInput, maxFunctionInput)
		}
		body["input_size"] = size
	}
	b, err := json.Marshal(body)
	if err != nil {
		return Request{}, err
	}
	return Request{Method: http.MethodPost, Path: AnnotationPrefix + "/functions", Body: b}, nil
}

// annotationDatasetPath builds the call of a tool that names a dataset and one operation on its
// annotations (summary reads, sync writes).
func annotationDatasetPath(op string) func(Args) (Request, error) {
	method := http.MethodGet
	if op == "sync" {
		method = http.MethodPost
	}
	return func(a Args) (Request, error) {
		id, err := requiredInt(a, "dataset_id")
		if err != nil {
			return Request{}, err
		}
		return Request{Method: method, Path: fmt.Sprintf("%s/datasets/%d/%s", AnnotationPrefix, id, op)}, nil
	}
}

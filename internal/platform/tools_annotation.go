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
	"strings"
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

// ---------------------------------------------------------------------------
// V3 — checks and fills
// ---------------------------------------------------------------------------

// Limits of the check and the fill tool. The service checks them again: a raw call over the request
// tool reaches the same paths without this table.
const (
	checkModeCalibrate  = "calibrate"
	checkModeUncertain  = "uncertain"
	checkModeVerify     = "verify"
	checkLimitCalibrate = 50
	checkLimitUncertain = 200
	// jobLimitMax is the highest number of images one job takes.
	jobLimitMax           = 1000
	fillLimitDefault      = 1000
	uncertainBelowDefault = 60
	uncertainBelowLow     = 1
	uncertainBelowHigh    = 99
)

// V3 adds its three tools in an init of its own, so that the tools of V1 and V2 stay as they are. V3b
// adds the rollback of the automatic annotations (annotationRollback) next to them.
func init() {
	Tools = append(Tools, annotationCheck, annotationFill, annotationJob, annotationRollback)
}

var annotationCheck = Tool{
	Name: "annotation_check", CLI: "annotation-check",
	Desc: `Measures a pre-annotation function before it runs over all images, names the images whose answer is weak, or judges the result of a run or a fill. mode=calibrate asks the function about the images of the calibration set (the ones the mapping and the threshold may be chosen on), counts the confusion between the classes it answers and the manual classes and proposes the mapping plus a threshold; mode=uncertain asks about the images of the task and lists the ones under uncertain_below with their CVAT link; mode=verify measures the function of the named run or fill (source_id) with ITS mapping and threshold on the held-out images (final=true) or on the validation images (final=false, an interim figure) and reports a verdict usable, questionable or not measured. In mode verify do not send function or mapping: judged is the setting that produced the result. A final verdict is possible once per task and chat. Limit 1 to 1000 images (default 50 for calibrate, 200 for uncertain; verify measures the whole set). Without mapping the calls of calibrate and uncertain map every class of the function onto the first class of the dataset, so that CVAT drops nothing and the check reads the classes of the function (a function that answers no raw_label needs mapping). Effect compute; the user must approve. Runs as a background job (annotation_jobs): the answer carries job_id, follow it with annotation-job.`,
	Params: []Param{
		{Name: "dataset_id", Type: "integer", Desc: "id of your own dataset of type AgriImageDataResource with classes and a CVAT task (not needed for mode verify: it comes from the source)"},
		{Name: "function", Type: "string", Desc: "id of the pre-annotation function (platform_annotation_functions); required for calibrate and uncertain, refused for verify"},
		{Name: "mode", Type: "string", Desc: "calibrate (measure on the calibration set), uncertain (list the images with the weakest answers) or verify (judge the result of a run or a fill)", Required: true},
		{Name: "source_id", Type: "string", Desc: "the run_id of pre_annotate or the job_id of annotation_fill whose result is judged; only for mode verify"},
		{Name: "final", Type: "string", Desc: "true: measure on the held-out images of the task and give a final verdict (once per task and chat); false (default): measure on the validation images and give an interim figure"},
		{Name: "limit", Type: "integer", Desc: "how many images to measure, 1 to 1000 (default 50 for calibrate, 200 for uncertain)"},
		{Name: "mapping", Type: "object", Desc: `function label to dataset class, e.g. {"cow": "Rind"}; with one the check measures through it, without one it reads the classes the function answers`},
		{Name: "uncertain_below", Type: "integer", Desc: "confidence in percent under which an answer counts as uncertain, 1 to 99 (default 60; only mode uncertain)"},
	},
	Write: true,
	build: buildAnnotationCheck,
}

func buildAnnotationCheck(a Args) (Request, error) {
	mode, err := requiredStr(a, "mode")
	if err != nil {
		return Request{}, err
	}
	if mode != checkModeCalibrate && mode != checkModeUncertain && mode != checkModeVerify {
		return Request{}, fmt.Errorf("mode must be %s, %s or %s", checkModeCalibrate, checkModeUncertain, checkModeVerify)
	}
	if mode == checkModeVerify {
		return buildAnnotationVerify(a)
	}
	id, err := requiredInt(a, "dataset_id")
	if err != nil {
		return Request{}, err
	}
	fn, err := requiredStr(a, "function")
	if err != nil {
		return Request{}, err
	}
	limit, ok, err := a.int("limit")
	if err != nil {
		return Request{}, err
	}
	if !ok {
		limit = checkLimitCalibrate
		if mode == checkModeUncertain {
			limit = checkLimitUncertain
		}
	}
	if limit < 1 || limit > jobLimitMax {
		return Request{}, fmt.Errorf("limit must be between 1 and %d images", jobLimitMax)
	}
	below, ok, err := a.int("uncertain_below")
	if err != nil {
		return Request{}, err
	}
	if !ok {
		below = uncertainBelowDefault
	}
	if below < uncertainBelowLow || below > uncertainBelowHigh {
		return Request{}, fmt.Errorf("uncertain_below must be between %d and %d percent", uncertainBelowLow, uncertainBelowHigh)
	}
	mapping, err := annotationMapping(a)
	if err != nil {
		return Request{}, err
	}
	body := map[string]any{"dataset_id": id, "function": fn, "mode": mode, "limit": limit,
		"uncertain_below": below, "mapping": json.RawMessage(mapping)}
	b, err := json.Marshal(body)
	if err != nil {
		return Request{}, err
	}
	return Request{Method: http.MethodPost, Path: AnnotationPrefix + "/checks", Body: b}, nil
}

var annotationFill = Tool{
	Name: "annotation_fill", CLI: "annotation-fill",
	Desc: `Asks a pre-annotation function about the images of the CVAT task of the dataset that carry no annotation at all and writes what it answers into exactly those images (source "auto", in batches of 20). Images that already carry an annotation stay untouched, also when the user annotates one while the job runs; nothing is deleted. min_confidence in percent (default 50). mapping maps function labels to dataset classes ({"cow": "Rind"}); without one only equal names are used. limit 1 to 1000 images (default 1000). Effect compute; the user must approve. Runs as a background job (annotation_jobs): the answer carries job_id, follow it with annotation-job.`,
	Params: []Param{
		{Name: "dataset_id", Type: "integer", Desc: "id of your own dataset of type AgriImageDataResource with classes and a CVAT task", Required: true},
		{Name: "function", Type: "string", Desc: "id of the pre-annotation function (platform_annotation_functions)", Required: true},
		{Name: "min_confidence", Type: "integer", Desc: "confidence threshold in percent, 1 to 99 (default 50)"},
		{Name: "mapping", Type: "object", Desc: `function label to dataset class, e.g. {"cow": "Rind"}`},
		{Name: "limit", Type: "integer", Desc: "at most this many images, 1 to 1000 (default 1000)"},
	},
	Write: true,
	build: buildAnnotationFill,
}

func buildAnnotationFill(a Args) (Request, error) {
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
	limit, ok, err := a.int("limit")
	if err != nil {
		return Request{}, err
	}
	if !ok {
		limit = fillLimitDefault
	}
	if limit < 1 || limit > jobLimitMax {
		return Request{}, fmt.Errorf("limit must be between 1 and %d images", jobLimitMax)
	}
	mapping, err := annotationMapping(a)
	if err != nil {
		return Request{}, err
	}
	body := map[string]any{"dataset_id": id, "function": fn, "min_confidence": conf, "limit": limit,
		"mapping": json.RawMessage(mapping)}
	b, err := json.Marshal(body)
	if err != nil {
		return Request{}, err
	}
	return Request{Method: http.MethodPost, Path: AnnotationPrefix + "/fills", Body: b}, nil
}

var annotationJob = Tool{
	Name: "annotation_job", CLI: "annotation-job",
	Desc: `State of a background job of the pre-annotation of this chat (a check or a fill): queued, running, finished or failed, with done and total images and the result of a finished job. The result of mode=uncertain is a list of images and is answered page by page (50 images per page; page is the argument).`,
	Params: []Param{
		{Name: "job_id", Type: "string", Desc: "job_id from platform_annotation_check or platform_annotation_fill", Required: true},
		{Name: "page", Type: "integer", Desc: "page of the list of uncertain images, 50 per page (default 1)"},
	},
	build: func(a Args) (Request, error) {
		id, ok, err := a.str("job_id")
		if err != nil {
			return Request{}, err
		}
		if !ok || !annotationJobIDRe.MatchString(id) {
			return Request{}, errors.New("job_id missing or not a valid job id")
		}
		r := Request{Method: http.MethodGet, Path: AnnotationPrefix + "/jobs/" + id}
		page, ok, err := a.int("page")
		if err != nil {
			return Request{}, err
		}
		if ok {
			if page < 1 {
				return Request{}, errors.New("page must be 1 or higher")
			}
			r.Query = map[string]string{"page": strconv.FormatInt(page, 10)}
		}
		return r, nil
	},
}

// annotationJobIDRe is the shape of a job id: the uuid the table of the gateway gives it.
var annotationJobIDRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// buildAnnotationVerify builds the call of mode verify (V3b): the agent names the source it judges and
// whether the held-out images are measured, nothing else. Function, mapping and threshold come from the
// judged run or fill — the gateway reads them from its own rows —, so that the agent cannot measure
// against a better setting than the one that produced the result.
func buildAnnotationVerify(a Args) (Request, error) {
	source, ok, err := a.str("source_id")
	if err != nil {
		return Request{}, err
	}
	if !ok || !runIDRe.MatchString(source) {
		return Request{}, errors.New("source_id missing or not a valid run or job id")
	}
	final, err := annotationFinal(a)
	if err != nil {
		return Request{}, err
	}
	body := map[string]any{"mode": checkModeVerify, "source_id": source, "final": final}
	if id, ok, err := a.int("dataset_id"); err != nil {
		return Request{}, err
	} else if ok {
		body["dataset_id"] = id
	}
	b, err := json.Marshal(body)
	if err != nil {
		return Request{}, err
	}
	return Request{Method: http.MethodPost, Path: AnnotationPrefix + "/checks", Body: b}, nil
}

// annotationFinal reads the final flag of a verification. The tool table knows no boolean (E13), so the
// flag comes as the string "true" or "false"; a real boolean, which MCP may still send, is taken as well.
func annotationFinal(a Args) (bool, error) {
	v, ok := a["final"]
	if !ok || v == nil {
		return false, nil
	}
	switch x := v.(type) {
	case bool:
		return x, nil
	case string:
		switch strings.ToLower(strings.TrimSpace(x)) {
		case "", "false", "0", "no":
			return false, nil
		case "true", "1", "yes":
			return true, nil
		}
	}
	return false, errors.New("final must be true or false")
}

// annotationRollback is the tool that takes back what a run or a fill wrote (V3b, E21).
var annotationRollback = Tool{
	Name: "annotation_rollback", CLI: "annotation-rollback",
	Desc: `Takes back the annotations a pre-annotation run or a fill wrote into the CVAT task (source "auto") and leaves everything the user annotated or corrected himself in place. source_id is the run_id of pre_annotate or the job_id of annotation_fill; scope all (default) takes back everything that source wrote, scope suspicious only what stands on the images the last annotation-check mode=verify named as suspicious (that verification has to run first). An annotation the user changed in CVAT since is not touched and is reported as changed_kept. What was taken back comes again through a new fill with the corrected setting. Effect write; the user must approve.`,
	Params: []Param{
		{Name: "source_id", Type: "string", Desc: "run_id of pre_annotate or job_id of annotation_fill whose annotations are taken back", Required: true},
		{Name: "scope", Type: "string", Desc: "all (default) or suspicious (only the images the last verification of this source named)"},
	},
	Write: true,
	build: buildAnnotationRollback,
}

func buildAnnotationRollback(a Args) (Request, error) {
	source, ok, err := a.str("source_id")
	if err != nil {
		return Request{}, err
	}
	if !ok || !runIDRe.MatchString(source) {
		return Request{}, errors.New("source_id missing or not a valid run or job id")
	}
	scope, ok, err := a.str("scope")
	if err != nil {
		return Request{}, err
	}
	if !ok {
		scope = rollbackScopeAll
	}
	if scope != rollbackScopeAll && scope != rollbackScopeSuspicious {
		return Request{}, fmt.Errorf("scope must be %s or %s", rollbackScopeAll, rollbackScopeSuspicious)
	}
	b, err := json.Marshal(map[string]any{"source_id": source, "scope": scope})
	if err != nil {
		return Request{}, err
	}
	return Request{Method: http.MethodPost, Path: AnnotationPrefix + "/rollbacks", Body: b}, nil
}

// The scopes of annotation_rollback (V3b); the service checks them again.
const (
	rollbackScopeAll        = "all"
	rollbackScopeSuspicious = "suspicious"
)

// annotationMapping reads the mapping of a check or a fill; empty means "no mapping given", and the
// service then decides which classes it maps (V3).
func annotationMapping(a Args) (json.RawMessage, error) {
	mapping, has, err := a.object("mapping")
	if err != nil {
		return nil, err
	}
	if !has {
		return json.RawMessage(`{}`), nil
	}
	var pairs map[string]string
	if err := json.Unmarshal(mapping, &pairs); err != nil {
		return nil, errors.New(`mapping must be an object of strings (function label to dataset class), e.g. {"cow": "Rind"}`)
	}
	return mapping, nil
}

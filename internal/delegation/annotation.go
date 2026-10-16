// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package delegation

import (
	"encoding/json"
	"net/http"
	"regexp"

	"agw/internal/platform"
)

// AnnotationFunction is a pre-annotation function of CVAT (V1): the user's own or one the operator
// installed. AnnotationRun is one run of such a function. Both are resources of their own, so that the
// rights can be delegated apart from the dataset (the function list is not bound to dataset rights).
const (
	AnnotationFunction = "annotation_function"
	AnnotationRun      = "annotation_run"
)

// The routes of pre-annotation (V1). init appends them, so that other routes can be added in parallel.
func init() {
	resources[AnnotationFunction] = true
	resources[AnnotationRun] = true
	// POST /_agw/annotation/tasks acts on the dataset named in its body (the operation sets
	// annotation_task_id on it).
	tasks := r(http.MethodPost, platform.AnnotationPrefix+`/tasks`, Update, Dataset, 0).refine(datasetFromBody)
	// A run over all images is compute; with existing=replace CVAT deletes every annotation of the task
	// first, which cannot be undone (E9).
	runs := r(http.MethodPost, platform.AnnotationPrefix+`/runs`, Run, Dataset, 0).as(EffectCompute).
		refine(datasetFromBody).refine(func(a *Access, req platform.Request) {
		if bodyID(req.Body, "existing") == "replace" {
			a.Effect = EffectIrreversible
		}
	})
	// Building a function from a model (V2): publishing it for every CVAT user, and the agent cannot
	// take it back, so it is irreversible and always needs its own approval (E9).
	function := r(http.MethodPost, platform.AnnotationPrefix+`/functions`, Create, AnnotationFunction, 0).
		as(EffectIrreversible).refine(functionModel)
	// A check and a fill (V3) run as a background job of the gateway; both are compute (CPU time of the
	// instance) and both act on the dataset named in their body (E9). The state of a job is read on the
	// resource of the run: a job belongs to the same feature and cannot be reached by another chat.
	check := r(http.MethodPost, platform.AnnotationPrefix+`/checks`, Run, Dataset, 0).
		as(EffectCompute).refine(datasetFromBody)
	fill := r(http.MethodPost, platform.AnnotationPrefix+`/fills`, Run, Dataset, 0).
		as(EffectCompute).refine(datasetFromBody)
	// Taking the automatic annotations of a run or a fill back (V3b, E21) is a write. The body names no
	// dataset: it names the source, and the register of the gateway binds that source to the task and
	// the dataset. So this route carries no identifier, and the boundary is the service — it deletes
	// only what stands in the register of this chat with source "auto" and an unchanged fingerprint.
	// The refinement refuses a body without a usable source.
	rollback := r(http.MethodPost, platform.AnnotationPrefix+`/rollbacks`, Update, Dataset, 0).
		as(EffectWrite).refine(rollbackBody)
	routes = append(routes,
		r(http.MethodGet, platform.AnnotationPrefix+`/functions`, Read, AnnotationFunction, 0),
		function,
		tasks,
		runs,
		r(http.MethodGet, platform.AnnotationPrefix+`/runs/`+seg, Read, AnnotationRun, 1),
		check,
		fill,
		rollback,
		r(http.MethodGet, platform.AnnotationPrefix+`/jobs/`+seg, Read, AnnotationRun, 1),
		r(http.MethodGet, platform.AnnotationPrefix+`/datasets/`+seg+`/summary`, Read, Dataset, 1),
		r(http.MethodPost, platform.AnnotationPrefix+`/datasets/`+seg+`/sync`, Update, Dataset, 1),
	)
	fieldRules = append(fieldRules, struct {
		method string
		re     *regexp.Regexp
		fields []string
	}{http.MethodPost, regexp.MustCompile(`^` + platform.AnnotationPrefix + `/tasks$`),
		[]string{"dataset_id"}}, struct {
		method string
		re     *regexp.Regexp
		fields []string
	}{http.MethodPost, regexp.MustCompile(`^` + platform.AnnotationPrefix + `/runs$`),
		[]string{"dataset_id", "function", "min_confidence", "mapping", "existing"}},
		// A function is built from a model, so its body carries only these four fields: the code of
		// the function comes from the gateway, never from the agent (E9, E10).
		struct {
			method string
			re     *regexp.Regexp
			fields []string
		}{http.MethodPost, regexp.MustCompile(`^` + platform.AnnotationPrefix + `/functions$`),
			[]string{"model_id", "kind", "labels", "input_size"}},
		// The bodies of V3 carry what a check and a fill decide; everything else is refused before the
		// delegation check (Review 5, K1).
		struct {
			method string
			re     *regexp.Regexp
			fields []string
		}{http.MethodPost, regexp.MustCompile(`^` + platform.AnnotationPrefix + `/checks$`),
			[]string{"dataset_id", "function", "mode", "limit", "mapping", "uncertain_below", "source_id", "final"}},
		struct {
			method string
			re     *regexp.Regexp
			fields []string
		}{http.MethodPost, regexp.MustCompile(`^` + platform.AnnotationPrefix + `/fills$`),
			[]string{"dataset_id", "function", "min_confidence", "mapping", "limit"}},
		// The rollback names the source and the extent, nothing else (V3b): which objects it touches is
		// decided by the register of the gateway, never by the call.
		struct {
			method string
			re     *regexp.Regexp
			fields []string
		}{http.MethodPost, regexp.MustCompile(`^` + platform.AnnotationPrefix + `/rollbacks$`),
			[]string{"source_id", "scope"}})
	registerConformance(
		Case{Name: "pre-annotate another user's dataset", Group: "action",
			Req: annotationRunCall(`{"dataset_id":5,"function":"agw-x","min_confidence":50,"mapping":{},"existing":"keep"}`)},
		// The allowed control: the same call on a dataset created under this delegation. Without it a
		// check that refuses everything would pass as correct.
		Case{Name: "pre-annotate an own dataset (control)", Group: "action", Allowed: true,
			Req:   annotationRunCall(`{"dataset_id":12,"function":"agw-x","min_confidence":50,"mapping":{},"existing":"keep"}`),
			Rules: `{"rules":[{"action":"run","resource":"dataset","ids":["own"]}],"confirm":"none"}`},
		// V3: a check reads the images of a dataset and costs compute time, so it is bound to the same
		// right as a run, and the control shows that the route is reachable at all.
		Case{Name: "calibrate on another user's dataset", Group: "action",
			Req: annotationCall(http.MethodPost, "/checks", `{"dataset_id":5,"function":"agw-x","mode":"calibrate"}`)},
		Case{Name: "calibrate on an own dataset (control)", Group: "action", Allowed: true,
			Req:   annotationCall(http.MethodPost, "/checks", `{"dataset_id":12,"function":"agw-x","mode":"calibrate"}`),
			Rules: `{"rules":[{"action":"run","resource":"dataset","ids":["own"]}],"confirm":"none"}`},
	)
}

// datasetFromBody names the dataset of a call that takes it from the body (the operations of V1 do; the
// dataset is not part of their path).
func datasetFromBody(a *Access, req platform.Request) {
	a.ID = bodyIDOr(req.Body, "dataset_id")
}

// rollbackBody checks the body of a rollback (V3b): the source that names what is taken back, and the
// extent. The dataset is not part of the body — the register of the gateway binds the source to it —,
// so this is the one thing the classification can decide about the call; what it deletes is decided by
// the service alone (E21).
func rollbackBody(a *Access, req platform.Request) {
	// bodyID, not bodyIDOr: a missing source is no identifier at all and must be refused, while an
	// unknown dataset id would only fail to match a rule.
	if bodyID(req.Body, "source_id") == "" {
		a.Problem = "the rollback names no source_id"
		return
	}
	if scope := bodyID(req.Body, "scope"); scope != "" && scope != "all" && scope != "suspicious" {
		a.Problem = "unknown scope " + scope
	}
}

// functionModel names the model a function is built from (V2): the call reads the model of the body,
// and the function then carries its file, so the delegation needs the read of that model as well.
func functionModel(a *Access, req platform.Request) {
	a.Also = append(a.Also, Access{Action: Read, Resource: Model, ID: bodyIDOr(req.Body, "model_id")})
}

// annotationRunCall builds the call of a pre-annotation run with the given body.
func annotationRunCall(body string) platform.Request {
	return platform.Request{Method: http.MethodPost, Path: platform.AnnotationPrefix + "/runs", Body: json.RawMessage(body)}
}

// annotationCall builds a call of the pre-annotation feature with the given method, operation and body
// (V3: a check).
func annotationCall(method, op, body string) platform.Request {
	return platform.Request{Method: method, Path: platform.AnnotationPrefix + op, Body: json.RawMessage(body)}
}

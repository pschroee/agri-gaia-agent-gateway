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
	routes = append(routes,
		r(http.MethodGet, platform.AnnotationPrefix+`/functions`, Read, AnnotationFunction, 0),
		tasks,
		runs,
		r(http.MethodGet, platform.AnnotationPrefix+`/runs/`+seg, Read, AnnotationRun, 1),
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
		[]string{"dataset_id", "function", "min_confidence", "mapping", "existing"}})
	registerConformance(
		Case{Name: "pre-annotate another user's dataset", Group: "action",
			Req: annotationRunCall(`{"dataset_id":5,"function":"agw-x","min_confidence":50,"mapping":{},"existing":"keep"}`)},
		// The allowed control: the same call on a dataset created under this delegation. Without it a
		// check that refuses everything would pass as correct.
		Case{Name: "pre-annotate an own dataset (control)", Group: "action", Allowed: true,
			Req:   annotationRunCall(`{"dataset_id":12,"function":"agw-x","min_confidence":50,"mapping":{},"existing":"keep"}`),
			Rules: `{"rules":[{"action":"run","resource":"dataset","ids":["own"]}],"confirm":"none"}`},
	)
}

// datasetFromBody names the dataset of a call that takes it from the body (the operations of V1 do; the
// dataset is not part of their path).
func datasetFromBody(a *Access, req platform.Request) {
	a.ID = bodyIDOr(req.Body, "dataset_id")
}

// annotationRunCall builds the call of a pre-annotation run with the given body.
func annotationRunCall(body string) platform.Request {
	return platform.Request{Method: http.MethodPost, Path: platform.AnnotationPrefix + "/runs", Body: json.RawMessage(body)}
}

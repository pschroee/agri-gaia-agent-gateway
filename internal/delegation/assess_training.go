// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package delegation

import (
	"net/http"
	"regexp"
	"strings"

	"agw/internal/platform"
)

// The routes of the training check (R1). They live under the check prefix (platform.AssessPrefix) and
// are answered by the gateway itself; without a rule for the resource assessment every one of them is
// forbidden in a delegation. init appends them, so that other features can add theirs in parallel.
func init() {
	prefix := regexp.QuoteMeta(platform.AssessPrefix)
	routes = append(routes,
		// The split creates a dataset and downloads the source dataset for it (the download also syncs
		// the CVAT annotations, 1.1): one approved write that names both (R1.1).
		r(http.MethodPost, prefix+`/splits`, Create, Dataset, 0).as(EffectWrite).refine(refineSplit),
		// The check reads the model and downloads the evaluation dataset (and the datasets the model
		// saw). One approved write; the gateway decides what it runs on (R1.7).
		r(http.MethodPost, prefix+`/checks`, Create, Assessment, 0).as(EffectWrite).refine(refineAssess),
		// What the last check of a model says (R1.14): an approval of an image build reads it.
		r(http.MethodGet, prefix+`/models/[0-9]+/latest`, Read, Assessment, 0),
		// What the last check of a file of the sandbox says (R2.7): an approval of an upload reads it.
		r(http.MethodGet, prefix+`/files/`+platform.AssessSHARe+`/latest`, Read, Assessment, 0),
	)
	// The bodies of both calls carry exactly these fields (R1.1, R1.7); the check runs with and without
	// a delegation (issue #65).
	fieldRules = append(fieldRules,
		struct {
			method string
			re     *regexp.Regexp
			fields []string
		}{http.MethodPost, regexp.MustCompile(`^` + prefix + `/splits$`), []string{"dataset_id", "name", "train_share"}},
		struct {
			method string
			re     *regexp.Regexp
			fields []string
		}{http.MethodPost, regexp.MustCompile(`^` + prefix + `/checks$`),
			// R2.2 adds the source of a file of the sandbox (path) with what its code declares about
			// itself (class_names, preprocess); a body with another field never reaches the check.
			[]string{"model_id", "path", "eval_dataset_id", "final", "class_names", "preprocess"}},
	)
	registerConformance(assessTrainingCases()...)
}

// refineSplit adds the source dataset of the split to what the call touches: the gateway downloads it,
// and downloading syncs its CVAT annotations (1.1).
func refineSplit(a *Access, req platform.Request) {
	a.Also = append(a.Also, Access{Action: Update, Resource: Dataset, ID: bodyIDOr(req.Body, "dataset_id")})
}

// refineAssess adds the model and the dataset a check touches (R1.7, R2.2): the gateway reads the model
// of the platform and downloads the evaluation dataset. A check of a file of the sandbox (path) names no
// model — the check itself is what a later upload makes a model of. The datasets the model could have
// seen are decided by the gateway (its own origin table and the runs of the notebook server), so they are
// named in the approval, not in the call.
func refineAssess(a *Access, req platform.Request) {
	if id := bodyID(req.Body, "model_id"); id != "" {
		a.Also = append(a.Also, Access{Action: Read, Resource: Model, ID: id})
	}
	if id := bodyID(req.Body, "eval_dataset_id"); id != "" {
		a.Also = append(a.Also, Access{Action: Update, Resource: Dataset, ID: id})
	}
}

// assessTrainingRules is the delegation of the conformance cases of the training check: it may split a
// dataset and check a model, and it may read the last check of a model.
const assessTrainingRules = `{
  "rules": [
    {"action": "create", "resource": "dataset"},
    {"action": "update", "resource": "dataset", "ids": ["*"]},
    {"action": "create", "resource": "assessment"},
    {"action": "read",   "resource": "assessment"},
    {"action": "read",   "resource": "model", "ids": ["*"]}
  ],
  "confirm": "none"
}`

// assessTrainingNoModelRules: the same, without reading a model — a check that refuses everything must
// not pass as correct (G1.6).
const assessTrainingNoModelRules = `{
  "rules": [
    {"action": "create", "resource": "dataset"},
    {"action": "update", "resource": "dataset", "ids": ["*"]},
    {"action": "create", "resource": "assessment"},
    {"action": "read",   "resource": "assessment"}
  ],
  "confirm": "none"
}`

// assessTrainingNoDatasetRules: the rules of the check without the two a split needs — it creates a
// dataset and downloads (updates) its source. A delegation that lacks them must not be able to split,
// although it may check (G1.6, the counterpart of assessTrainingNoModelRules).
const assessTrainingNoDatasetRules = `{
  "rules": [
    {"action": "create", "resource": "assessment"},
    {"action": "read",   "resource": "assessment"},
    {"action": "read",   "resource": "model", "ids": ["*"]}
  ],
  "confirm": "none"
}`

// assessTrainingCases are the conformance cases of the training check (the seam of G1.6).
func assessTrainingCases() []Case {
	split := func(body string) platform.Request {
		return platform.Request{Method: http.MethodPost, Path: platform.AssessPrefix + "/splits", Body: []byte(body)}
	}
	check := func(body string) platform.Request {
		return platform.Request{Method: http.MethodPost, Path: platform.AssessPrefix + "/checks", Body: []byte(body)}
	}
	return []Case{
		// Allowed control calls: the split and the check of a model this delegation covers.
		{Name: "split a dataset (control)", Req: split(`{"dataset_id":12,"train_share":70}`), Allowed: true,
			Group: "control", Rules: assessTrainingRules},
		{Name: "check a model (control)", Req: check(`{"model_id":5,"eval_dataset_id":12}`), Allowed: true,
			Group: "control", Rules: assessTrainingRules},
		{Name: "read the last check of a model (control)", Req: platform.Request{Method: http.MethodGet,
			Path: platform.AssessPrefix + "/models/5/latest"}, Allowed: true, Group: "control", Rules: assessTrainingRules},
		// A check of a file of the sandbox (R2.2) touches no model: it needs the rule for the assessment
		// and for the evaluation dataset.
		{Name: "check a file of the sandbox (control)", Req: check(`{"path":"/workspace/notebook/r1/model.onnx","eval_dataset_id":12}`),
			Allowed: true, Group: "control", Rules: assessTrainingRules},
		{Name: "read the last check of a file (control)", Req: platform.Request{Method: http.MethodGet,
			Path: platform.AssessPrefix + "/files/" + strings.Repeat("a", 64) + "/latest"}, Allowed: true, Group: "control",
			Rules: assessTrainingRules},

		// The calls themselves (R1.1, R1.7): create dataset plus the update of the downloaded source,
		// create assessment plus reading the model and downloading the evaluation dataset. A delegation
		// without the rules the split needs must not be able to split.
		{Name: "split a dataset without the rule for assessment", Req: split(`{"dataset_id":12}`), Group: "assessment",
			Rules: assessTrainingNoDatasetRules},
		{Name: "check a model without the right to read it", Req: check(`{"model_id":5}`), Group: "resource",
			Rules: assessTrainingNoModelRules},
		{Name: "check a model of another chat", Req: check(`{"model_id":9,"eval_dataset_id":12}`), Group: "object",
			Rules: assessTrainingNoModelRules},

		// Field allowlists (R1.1, R1.7, R2.2): a field the gateway does not read never reaches the call.
		{Name: "split with a field of its own", Req: split(`{"dataset_id":12,"kept_images":40}`), Group: "field",
			Rules: assessTrainingRules},
		{Name: "check with a field of its own", Req: check(`{"model_id":5,"keep_images":40}`), Group: "field",
			Rules: assessTrainingRules},
		// A check of a file of the sandbox names what the code declares about itself (R2.4); a field beyond
		// the list is refused there as well.
		{Name: "check of a file with a field of its own", Req: check(`{"path":"/workspace/m.onnx","architecture":"resnet"}`),
			Group: "field", Rules: assessTrainingRules},
	}
}

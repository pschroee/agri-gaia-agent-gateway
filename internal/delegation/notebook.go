// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package delegation

import (
	"net/http"
	"regexp"
	"strconv"

	"agw/internal/platform"
)

// Notebook is the resource of the notebook tools (own code C-E3): the runs the agent starts in this
// chat's notebook server. Their paths are virtual (platform.NotebookPrefix) and answered by the
// gateway itself; without a rule for notebook everything of them is forbidden in a delegation.
const Notebook = "notebook"

// notebookRunID is the shape of a run id (r<YYYYMMDDhhmmss>-<4 hex>); the tools check it the same way.
const notebookRunID = `r[0-9]{14}-[0-9a-f]{4}`

// The routes of the notebook paths. init appends them, so that other features can add theirs in
// parallel; the main table carries no /_agw path.
func init() {
	resources[Notebook] = true
	prefix := regexp.QuoteMeta(platform.NotebookPrefix)
	run := r(http.MethodPost, prefix+`/runs`, Run, Notebook, 0).
		confirmEach(). // every run asks the user on its own, also with confirm "none" (G1.1, G1.2)
		refine(notebookDatasets)
	routes = append(routes,
		r(http.MethodGet, prefix+`$`, Read, Notebook, 0),
		r(http.MethodGet, prefix+`/runs$`, Read, Notebook, 0),
		r(http.MethodGet, prefix+`/runs/`+notebookRunID+`$`, Read, Notebook, 0),
		r(http.MethodGet, prefix+`/runs/`+notebookRunID+`/log$`, Read, Notebook, 0),
		r(http.MethodGet, prefix+`/runs/`+notebookRunID+`/file$`, Read, Notebook, 0),
		run,
		r(http.MethodPost, prefix+`/stop$`, Update, Notebook, 0).as(EffectWrite),
	)
	registerConformance(notebookCases()...)
}

// notebookDatasets names the datasets a run unpacks before it starts. Reading them is not a read for
// the platform: GET /datasets/{id}/download writes the CVAT annotation into MinIO (Review 5, W5), so
// the route table says "update dataset" and the delegation needs it for every dataset of the run.
func notebookDatasets(a *Access, req platform.Request) {
	b, err := platform.ParseRunForm(req.Form, 0)
	if err != nil {
		a.Problem = "unreadable run parameters"
		return
	}
	for _, id := range b.DatasetIDs {
		a.Also = append(a.Also, Access{Action: Update, Resource: Dataset, ID: strconv.FormatInt(id, 10)})
	}
}

// notebookRunForm is the form of a run of one dataset, for the conformance cases.
func notebookRunForm(ids ...string) map[string][]string {
	form := map[string][]string{"entry": {"train.py"}}
	if len(ids) > 0 {
		form["dataset_ids"] = ids
	}
	return form
}

// notebookCases are the conformance cases of this feature (registered with the seam of G1.6): a run
// without its rule is refused, a run that reads a foreign dataset too, and a run with its rule and
// its own dataset is let through (otherwise a check that refuses everything would pass as correct).
func notebookCases() []Case {
	run := platform.Request{Method: http.MethodPost, Path: platform.NotebookPrefix + "/runs", Form: notebookRunForm()}
	withDataset := func(ids ...string) platform.Request {
		return platform.Request{Method: http.MethodPost, Path: platform.NotebookPrefix + "/runs", Form: notebookRunForm(ids...)}
	}
	const own = `{"rules":[{"action":"run","resource":"notebook"},{"action":"update","resource":"dataset","ids":["12"]}],"confirm":"none"}`
	const readRule = `{"rules":[{"action":"read","resource":"notebook"}],"confirm":"none"}`
	return []Case{
		{Name: "notebook run without rule", Req: run, Group: "notebook"},
		{Name: "notebook run with rule but foreign dataset", Req: withDataset("99"), Group: "notebook", Rules: own},
		{Name: "notebook run with rule and own dataset", Req: withDataset("12"), Allowed: true, Group: "notebook", Rules: own},
		{Name: "notebook status without rule", Req: platform.Request{Method: http.MethodGet, Path: platform.NotebookPrefix}, Group: "notebook"},
		{Name: "notebook status with rule (control)", Req: platform.Request{Method: http.MethodGet, Path: platform.NotebookPrefix},
			Allowed: true, Group: "notebook", Rules: readRule},
		{Name: "stop the notebook server without rule", Req: platform.Request{Method: http.MethodPost, Path: platform.NotebookPrefix + "/stop"}, Group: "notebook"},
	}
}

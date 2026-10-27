// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"encoding/json"
	"strconv"

	"agw/internal/platform"
	"agw/internal/store"
)

// maxCurvePoints bounds the epochs one run contributes to an answer of training_curves; a longer series is
// thinned evenly and keeps its first and last epoch.
const maxCurvePoints = 200

// curveRun is one run in the answer of training_curves.
type curveRun struct {
	Label  string           `json:"label"`
	Idx    int              `json:"trial,omitempty"`
	State  string           `json:"state"`
	Params json.RawMessage  `json:"params,omitempty"`
	Epochs []map[string]any `json:"epochs"`
}

// trainingCurves is training_curves (issue #248): the per-epoch figures the gateway stored for the trainings of
// this chat, from the training dataset and the template's validation split inside it. It reads the trials of the
// calling chat only and never touches the held-out set of a check.
func (m *Manager) trainingCurves(ctx context.Context, chatID string, q map[string]string) (platform.Result, error) {
	cid, sid := q["train_container_id"], q["search_id"]
	if (cid == "") == (sid == "") {
		return searchErr("{{tool:training_curves}} takes train_container_id or search_id, exactly one")
	}
	var trials []store.Trial
	if sid != "" {
		if !platform.SearchIDRe.MatchString(sid) {
			return searchErr("search_id is not a search id")
		}
		if _, err := m.st.GetSearch(ctx, chatID, sid); err != nil {
			if err == store.ErrNotFound {
				return searchErr("no such training search in this chat")
			}
			return platform.Result{}, err
		}
		all, err := m.st.ListTrials(ctx, chatID)
		if err != nil {
			return platform.Result{}, err
		}
		for _, t := range all {
			if t.SearchID == sid && isRun(t) {
				trials = append(trials, t)
			}
		}
	} else {
		if n, err := strconv.ParseInt(cid, 10, 64); err != nil || n <= 0 {
			return searchErr("train_container_id is not a container id")
		}
		t, err := m.st.TrialByRef(ctx, chatID, store.SearchPathTemplate, cid)
		if err != nil {
			if err == store.ErrNotFound {
				return searchErr("no training of this chat has the container " + cid + " (only trainings started in this chat are recorded)")
			}
			return platform.Result{}, err
		}
		trials = append(trials, t)
	}
	runs := make([]curveRun, 0, len(trials))
	for _, t := range trials {
		epochs, _, err := m.st.TrialSeries(ctx, t.ID)
		if err != nil {
			return platform.Result{}, err
		}
		run := curveRun{State: t.State, Epochs: curvePoints(epochs)}
		if t.SearchID != "" {
			run.Label, run.Idx, run.Params = "trial "+strconv.Itoa(t.Idx), t.Idx, t.Params
		} else {
			run.Label, run.Params = "container "+t.Ref, t.Params
		}
		runs = append(runs, run)
	}
	body, err := json.Marshal(struct {
		Runs []curveRun `json:"runs"`
		Note string     `json:"note"`
	}{runs, "Figures of training and of the validation split inside the training dataset, per epoch; not the held-out set of a check."})
	if err != nil {
		return platform.Result{}, err
	}
	return platform.Result{Status: "ok", HTTPStatus: 200, Body: string(body)}, nil
}

// curvePoints turns stored epoch lines into the points of an answer: epoch plus the figures of the run,
// thinned to maxCurvePoints.
func curvePoints(lines []store.TrialEpoch) []map[string]any {
	keep := lines
	if n := len(lines); n > maxCurvePoints {
		keep = make([]store.TrialEpoch, 0, maxCurvePoints)
		for i := 0; i < maxCurvePoints; i++ {
			keep = append(keep, lines[i*(n-1)/(maxCurvePoints-1)])
		}
	}
	out := make([]map[string]any, 0, len(keep))
	for _, l := range keep {
		p := map[string]any{}
		_ = json.Unmarshal(l.Data, &p)
		p["epoch"] = l.Epoch
		out = append(out, p)
	}
	return out
}

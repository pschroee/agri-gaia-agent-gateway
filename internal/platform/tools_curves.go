// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

import (
	"errors"
	"net/http"
	"strconv"
)

// CurvesPath is the virtual path of training_curves: the gateway reads the per-epoch figures it stored for the
// trainings of this chat (issue #248). The search service answers it; it has a prefix of its own so that the
// REST binding cannot mistake it for a search id.
const CurvesPath = "/_agw/curves"

func init() {
	Tools = append(Tools, trainingCurves)
}

var trainingCurves = Tool{
	Name: "training_curves", CLI: "training-curves",
	Desc: "Per-epoch figures of trainings of this chat (training and the template's validation split inside the training " +
		"dataset) for learning curves; never the held-out set. Give train_container_id for one training or search_id for " +
		"every trial of a search. Answers one run per training or trial with its label, parameters and the epochs " +
		"(epoch, train_loss, val_loss, train_acc, val_acc, as the run wrote them). Draw the curves with the skill charts.",
	Params: []Param{
		{Name: "train_container_id", Type: "integer", Desc: "id of a training container started in this chat"},
		{Name: "search_id", Type: "string", Desc: "id of a training search of this chat ({{tool:propose_search}}); exactly one of the two"},
	},
	build: func(a Args) (Request, error) {
		cid, hasC, err := a.int("train_container_id")
		if err != nil {
			return Request{}, err
		}
		sid, hasS, err := a.str("search_id")
		if err != nil {
			return Request{}, err
		}
		if hasC == hasS && (hasC || sid != "") {
			return Request{}, errors.New("give either train_container_id or search_id, not both")
		}
		switch {
		case hasC && cid > 0:
			return Request{Method: http.MethodGet, Path: CurvesPath,
				Query: map[string]string{"train_container_id": strconv.FormatInt(cid, 10)}}, nil
		case sid != "":
			if !SearchIDRe.MatchString(sid) {
				return Request{}, errors.New("search_id is not a search id")
			}
			return Request{Method: http.MethodGet, Path: CurvesPath, Query: map[string]string{"search_id": sid}}, nil
		}
		return Request{}, errors.New("train_container_id or search_id missing")
	},
}

// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

import (
	"errors"
	"net/http"
	"regexp"
	"strings"
)

// Result checks (R0): the gateway runs a model in a container of its own and reports figures and a
// verdict (internal/assess). The calls of the check tools work on virtual paths under AssessPrefix,
// which the manager answers with that service (G1.5), so delegation, log and plan apply as everywhere;
// R1–R3 add their own tools and paths in their own files.

// AssessPrefix is the virtual path prefix of the result checks (R0.19).
const AssessPrefix = "/_agw/assess"

// AssessCheckIDRe is the shape of a check id (a uuid); the route of the delegation uses the same
// pattern.
var AssessCheckIDRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// init appends the tools of the result checks, so that other features can add theirs in parallel.
func init() {
	Tools = append(Tools, assessStatus)
}

// AssessCallRefusal refuses raw calls to the check paths (R0, pattern of C-E3 step 5): only the check
// tools may go there, because the gateway decides what a check runs on and the agent must not be able
// to name the measured set.
func AssessCallRefusal(r Request) string {
	p := strings.ToLower(r.Path)
	if p == AssessPrefix || strings.HasPrefix(p, AssessPrefix+"/") {
		return "check calls go through the assess tools (assess_status, split_dataset, assess_model)"
	}
	return ""
}

var assessStatus = Tool{
	Name: "assess_status", CLI: "assess-status",
	Desc: "State and report of a check the gateway ran on a result (training, own model). " +
		"The report's header is the verdict to start your reply with.",
	Params: []Param{
		{Name: "check_id", Type: "string", Required: true, Desc: "id of the check, from the tool that started it"},
	},
	build: func(a Args) (Request, error) {
		id, err := requiredStr(a, "check_id")
		if err != nil {
			return Request{}, err
		}
		if !AssessCheckIDRe.MatchString(id) {
			return Request{}, errors.New("check_id is not a check id")
		}
		return Request{Method: http.MethodGet, Path: AssessPrefix + "/checks/" + id}, nil
	},
}

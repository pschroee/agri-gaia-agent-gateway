// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package delegation

import (
	"net/http"
	"regexp"

	"agw/internal/platform"
)

// Assessment is the resource of the result checks (R0): the checks the gateway runs on a result of this
// chat (a training, an own model, a deployment). Their paths are virtual (platform.AssessPrefix) and
// answered by the gateway itself; without a rule for assessment every one of them is forbidden in a
// delegation. R1–R3 add the routes of their own tools in their own files.
const Assessment = "assessment"

// checkID is the shape of a check id: a uuid, as in platform.AssessCheckIDRe.
const checkID = `[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`

// The routes of the check paths. init appends them, so that other features can add theirs in parallel;
// the main table carries no /_agw path.
func init() {
	resources[Assessment] = true
	prefix := regexp.QuoteMeta(platform.AssessPrefix)
	routes = append(routes,
		r(http.MethodGet, prefix+`/checks/`+checkID+`$`, Read, Assessment, 0),
	)
	registerConformance(assessCases()...)
}

// assessCases are the conformance cases of this feature (the seam of G1.6): a check call without its
// rule is refused, and one with the rule for the resource is let through (otherwise a check that
// refuses everything would pass as correct).
func assessCases() []Case {
	status := platform.Request{Method: http.MethodGet, Path: platform.AssessPrefix + "/checks/11111111-2222-3333-4444-555555555555"}
	return []Case{
		{Name: "check status without rule", Req: status, Group: "assessment"},
		{Name: "check status with rule (control)", Req: status, Allowed: true, Group: "assessment",
			Rules: `{"rules":[{"action":"read","resource":"assessment"}],"confirm":"none"}`},
	}
}

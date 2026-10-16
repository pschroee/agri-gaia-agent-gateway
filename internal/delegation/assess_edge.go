// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package delegation

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"agw/internal/platform"
)

// The route of the deployment check (R3). It lives under the check prefix (platform.AssessPrefix) and is
// answered by the gateway itself; without a rule for its resources every call of it is forbidden in a
// delegation. init appends it, so that other features can add theirs in parallel.
//
// The action is `run` on the deployment, not `read`: the guard of the edge feature (D-E1) lets every
// read through and holds only non-read actions to the deployments of this chat, and a check that sends
// requests to some other chat's deployment must be refused there. The effect class stays `read` (P7):
// nothing is written, no approval is asked and the call does not count against the compute calls.
func init() {
	prefix := regexp.QuoteMeta(platform.AssessPrefix)
	routes = append(routes,
		r(http.MethodGet, prefix+`/deployments/([0-9]+)$`, Run, ContainerDeployment, 1).
			as(EffectRead).refine(refineDeploymentCheck),
	)
	registerConformance(assessEdgeCases()...)
}

// refineDeploymentCheck adds the model the check reads (R3.1): the gateway reads its record, its file and
// the check of it in this chat. The identifier stands in the query, not in the body.
func refineDeploymentCheck(a *Access, req platform.Request) {
	a.Also = append(a.Also, Access{Action: Read, Resource: Model, ID: queryID(req, "model_id")})
}

// queryID reads an integer identifier out of the query of a call; an absent or unreadable one counts as
// unknown, and no rule names that.
func queryID(req platform.Request, key string) string {
	v := strings.TrimSpace(req.Query[key])
	if v == "" {
		return unknownID
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return unknownID
	}
	return strconv.FormatInt(n, 10)
}

// assessEdgeRules is the delegation of the control case: it may run deployment 9 and read model 5.
const assessEdgeRules = `{
  "rules": [
    {"action": "run",  "resource": "container_deployment", "ids": ["9"]},
    {"action": "read", "resource": "model", "ids": ["5"]}
  ],
  "confirm": "none"
}`

// assessEdgeNoModelRules: the same without the right to read the model — a check that refuses everything
// must not pass as correct (G1.6).
const assessEdgeNoModelRules = `{
  "rules": [
    {"action": "run", "resource": "container_deployment", "ids": ["9"]}
  ],
  "confirm": "none"
}`

// assessEdgeOwnRules: the same with `own` instead of the identifier. Deployment 9 was not created under
// the checked delegation, so a check of it has to be refused.
const assessEdgeOwnRules = `{
  "rules": [
    {"action": "run",  "resource": "container_deployment", "ids": ["own"]},
    {"action": "read", "resource": "model", "ids": ["5"]}
  ],
  "confirm": "none"
}`

// assessEdgeCases are the conformance cases of the deployment check (the seam of G1.6).
func assessEdgeCases() []Case {
	check := func(query string) platform.Request {
		return platform.Request{Method: http.MethodGet, Path: platform.AssessPrefix + "/deployments/9",
			Query: map[string]string{"model_id": query}}
	}
	return []Case{
		// The allowed control call: the check of a deployment this delegation may run, with the model it
		// may read.
		{Name: "deployment check (control)", Req: check("5"), Allowed: true, Group: "control",
			Rules: assessEdgeRules},

		// The call itself (R3.1): running the deployment and reading the model.
		{Name: "deployment check without the right to read the model", Req: check("5"), Group: "resource",
			Rules: assessEdgeNoModelRules},
		{Name: "deployment check on a foreign deployment", Req: check("5"), Group: "object",
			Rules: assessEdgeOwnRules},
		{Name: "deployment check without a model", Req: check(""), Group: "resource",
			Rules: assessEdgeNoModelRules},
	}
}

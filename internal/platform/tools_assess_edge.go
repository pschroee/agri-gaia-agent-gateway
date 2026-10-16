// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

import (
	"errors"
	"net/http"
	"strconv"
)

// The tool of the deployment check (R3): it sends a few fixed test requests to a deployment that was
// started in this chat on a simulated edge device and compares the answers with what the model file
// computes for the same inputs. The call works on a virtual path under AssessPrefix, which the manager
// answers with the check service (G1.5), so delegation, effect class and log run as for every other call,
// and the gateway alone decides what goes to the device.
//
// init appends the tool, so that other features can add theirs in parallel.

// AssessDeploymentPath is the path of the deployment check (R3.1); the deployment stands in the path, the
// model in the query.
const AssessDeploymentPath = AssessPrefix + "/deployments/"

func init() { Tools = append(Tools, assessDeployment) }

// assessDeployment is the tool of R3.1. The description is the plan's wording: it tells the agent when to
// call it (after deployment_status shows deployed) and that how good the model is comes from assess_model.
var assessDeployment = Tool{
	Name: "deployment_check", CLI: "deployment-check",
	Desc: "Sends six fixed test requests to a deployment started in this chat on a simulated edge device and " +
		"compares the answers with what the model file computes for the same inputs (reads; a few seconds). " +
		"Use it after platform_deployment_status shows deployed. The report says whether the device serves " +
		"exactly this model; accuracy comes from assess_model.",
	Params: []Param{
		{Name: "deployment_id", Type: "integer", Desc: "id of the deployment (platform_list_deployments)", Required: true},
		{Name: "model_id", Type: "integer", Desc: "id of the model the deployment serves (platform_list_models)", Required: true},
	},
	build: func(a Args) (Request, error) {
		id, ok, err := a.int("deployment_id")
		if err != nil {
			return Request{}, err
		}
		if !ok || id <= 0 {
			return Request{}, errors.New("deployment_id missing")
		}
		mid, ok, err := a.int("model_id")
		if err != nil {
			return Request{}, err
		}
		if !ok || mid <= 0 {
			return Request{}, errors.New("model_id missing")
		}
		return Request{Method: http.MethodGet, Path: AssessDeploymentPath + strconv.FormatInt(id, 10),
			Query: map[string]string{"model_id": strconv.FormatInt(mid, 10)}}, nil
	},
}

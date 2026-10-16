// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package watch

import (
	"net/http"
	"testing"

	"agw/internal/delegation"
	"agw/internal/platform"
)

// The kind edge_deployment also observes an undeploy (edge D-E3): the route is PUT …/undeploy, and the
// reference names the deployment and the task the removal left behind. The poll then reports the
// removal as done when the deployment is undeployed.
func TestEdgeUndeployRouteWatched(t *testing.T) {
	req := platform.Request{Method: http.MethodPut, Path: "/container-deployments/9/undeploy"}
	res := platform.Result{Status: "ok", HTTPStatus: http.StatusAccepted, Location: "/tasks/13"}
	kind, ref, ok := For(delegation.Access{Action: delegation.Run, Resource: delegation.ContainerDeployment}, req, res)
	if !ok || kind != EdgeDeploymentKind || ref != "deployment:9|task:13" {
		t.Fatalf("For = %q %q %v", kind, ref, ok)
	}
	// An answer without a Location still names the deployment; the poll then reads the deployment.
	if _, ref, ok := For(delegation.Access{}, req, platform.Result{Status: "ok", HTTPStatus: http.StatusAccepted}); !ok || ref != "deployment:9" {
		t.Fatalf("removal without a task: %q %v", ref, ok)
	}
	// Reading a deployment is not the route of a job.
	if _, _, ok := For(delegation.Access{}, platform.Request{Method: http.MethodGet, Path: "/container-deployments/9"}, res); ok {
		t.Error("reading a deployment matched the route")
	}

	k, ok := Lookup(EdgeDeploymentKind)
	if !ok {
		t.Fatalf("kind %s not registered", EdgeDeploymentKind)
	}
	p, err := k.Poll(t.Context(), Job{ID: "w2", ChatID: "c1", Kind: EdgeDeploymentKind, Ref: "deployment:9"},
		edgeReader(map[string]string{"/container-deployments/9": `{"id":9,"name":"agw-mnist","status":"undeployed"}`}))
	if err != nil || p.State != "done" || p.Message != "undeployed" {
		t.Fatalf("undeployed: %+v %v", p, err)
	}
}

// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package watch

import (
	"context"
	"strings"
	"testing"

	"agw/internal/delegation"
	"agw/internal/platform"
)

// edgeReader answers the reading calls of the poll from a map of path to JSON; a path it does not know
// is an error.
func edgeReader(answers map[string]string) Reader {
	return func(_ context.Context, r platform.Request) (platform.Result, error) {
		if body, ok := answers[r.Path]; ok {
			return platform.Result{Status: "ok", HTTPStatus: 200, Body: body}, nil
		}
		return platform.Result{Status: "error", HTTPStatus: 404, Message: "not found"}, nil
	}
}

// The kind edge_deployment observes the task of a start: the route is PUT …/deploy, the reference
// names the deployment and the task from the Location, and the poll reports the task while it runs,
// then the deployment (edge D2).
func TestEdgeDeploymentKind(t *testing.T) {
	mu.Lock()
	k, ok := kinds[EdgeDeploymentKind]
	mu.Unlock()
	if !ok || k.Poll == nil || k.Stop == nil {
		t.Fatalf("kind %s not registered: %+v", EdgeDeploymentKind, k)
	}

	req := platform.Request{Method: "PUT", Path: "/container-deployments/9/deploy"}
	res := platform.Result{Status: "ok", HTTPStatus: 202, Location: "/tasks/12"}
	kind, ref, ok := For(delegation.Access{Action: delegation.Run, Resource: delegation.ContainerDeployment}, req, res)
	if !ok || kind != EdgeDeploymentKind || ref != "deployment:9|task:12" {
		t.Fatalf("For = %q %q %v", kind, ref, ok)
	}
	if _, _, ok := For(delegation.Access{}, platform.Request{Method: "GET", Path: "/container-deployments/9/deploy"}, res); ok {
		t.Error("the route matched another method")
	}
	// The deployment status call is not the route of a job.
	if _, _, ok := For(delegation.Access{}, platform.Request{Method: "GET", Path: "/container-deployments/9"}, res); ok {
		t.Error("reading a deployment matched the route")
	}

	job := Job{ID: "w1", ChatID: "c1", Kind: EdgeDeploymentKind, Ref: ref}

	// The task is still running: the task decides, with its percentage.
	p, err := k.Poll(t.Context(), job, edgeReader(map[string]string{
		"/tasks/12": `{"status":"inprogress","completion_percentage":40,"message":"pulling"}`,
	}))
	if err != nil || p.State != "running" || p.Percent == nil || *p.Percent != 40 {
		t.Fatalf("running: %+v %v", p, err)
	}

	// The task is done and the deployment is deployed: the job is done.
	p, err = k.Poll(t.Context(), job, edgeReader(map[string]string{
		"/tasks/12":                    `{"status":"completed","completion_percentage":100}`,
		"/container-deployments/9":     `{"id":9,"name":"agw-mnist","status":"deployed"}`,
	}))
	if err != nil || p.State != "done" || p.Message != "deployed" || p.Percent == nil || *p.Percent != 100 {
		t.Fatalf("done: %+v %v", p, err)
	}

	// A failed task carries the F3 hint (a container may be left on the device).
	p, err = k.Poll(t.Context(), job, edgeReader(map[string]string{
		"/tasks/12":                `{"status":"failed","message":"port is already allocated"}`,
		"/container-deployments/9": `{"id":9,"name":"agw-mnist","status":"created"}`,
	}))
	if err != nil || p.State != "failed" || !strings.Contains(p.Message, "the start failed") || !strings.Contains(p.Message, "agw-mnist") {
		t.Fatalf("failed: %+v %v", p, err)
	}

	// The stop of the job is the undeploy of its deployment.
	if r, ok := k.Stop(job); !ok || r.Method != "PUT" || r.Path != "/container-deployments/9/undeploy" {
		t.Fatalf("stop: %+v %v", r, ok)
	}
}

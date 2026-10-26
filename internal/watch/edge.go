// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package watch

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"agw/internal/delegation"
	"agw/internal/platform"
)

// The kind edge_deployment (edge D-E2): it observes the task a start (PUT …/deploy) or an undeploy
// (PUT …/undeploy, edge D-E3) left behind. Nothing here polls on its own — the loop and the table
// watch_jobs come with the autonomy plan (A-E2); the agent asks platform_deployment_status for the
// state. init registers kind and routes, so no wiring in main.go is needed.
func init() {
	Register(Kind{Name: EdgeDeploymentKind, Resource: delegation.ContainerDeployment, Poll: pollEdgeDeployment, Stop: stopEdgeDeployment})
	RegisterRoute(http.MethodPut, edgeDeployRoute, EdgeDeploymentKind, edgeDeploymentRef)
	RegisterRoute(http.MethodPut, edgeUndeployRoute, EdgeDeploymentKind, edgeDeploymentRef)
}

// EdgeDeploymentKind is the name of the kind.
const EdgeDeploymentKind = "edge_deployment"

var (
	// edgeDeployRoute is the call that starts a deployment; the ref names the deployment and the task.
	edgeDeployRoute = regexp.MustCompile(`^/container-deployments/(\d+)/deploy$`)
	// edgeUndeployRoute is the call that takes it back (edge E3); it leaves a task behind as well.
	edgeUndeployRoute = regexp.MustCompile(`^/container-deployments/(\d+)/undeploy$`)
	// taskLocationRe reads the task id out of "Location: /tasks/<id>" (the backend answers 202 with it,
	// routers/container_deployments.py:151-158).
	taskLocationRe = regexp.MustCompile(`/tasks/(\d+)$`)
)

// edgeStartFailed is the one line the watcher shows for a failed start, with the reason from the task. Since
// #231 the platform removes the container of a failed start again, and its task message says so (or names
// the container that stayed on the device); edgeStartFailedNoReason stands in when the task has no message.
const (
	edgeStartFailed         = "the start of %s failed: %s"
	edgeStartFailedNoReason = "the start of %s failed; the platform gave no reason"
)

// edgeStartFailedText is the line for a failed start of the deployment named name.
func edgeStartFailedText(name, message string) string {
	if m := strings.TrimSpace(message); m != "" {
		return fmt.Sprintf(edgeStartFailed, name, m)
	}
	return fmt.Sprintf(edgeStartFailedNoReason, name)
}

// edgeDeploymentRef builds "deployment:<id>" and, when the answer carried a task, "|task:<id>". It
// serves both routes of the kind, starting a deployment and taking it back.
func edgeDeploymentRef(req platform.Request, res platform.Result) (string, bool) {
	m := edgeDeployRoute.FindStringSubmatch(req.Path)
	if m == nil {
		m = edgeUndeployRoute.FindStringSubmatch(req.Path)
	}
	if m == nil {
		return "", false
	}
	ref := "deployment:" + m[1]
	if t := taskLocationRe.FindStringSubmatch(res.Location); t != nil {
		ref += "|task:" + t[1]
	}
	return ref, true
}

// parseEdgeRef reads the deployment and the task id out of a reference.
func parseEdgeRef(ref string) (deploymentID, taskID string) {
	for _, part := range strings.Split(ref, "|") {
		k, v, ok := strings.Cut(part, ":")
		if !ok {
			continue
		}
		switch k {
		case "deployment":
			deploymentID = v
		case "task":
			taskID = v
		}
	}
	return deploymentID, taskID
}

// pollEdgeDeployment reports the state of a deployment: while the task runs the task decides, after it
// (or without one) the deployment (edge D2). done means deployed (or undeployed), failed means the
// task failed.
func pollEdgeDeployment(ctx context.Context, j Job, read Reader) (Progress, error) {
	depID, taskID := parseEdgeRef(j.Ref)
	if depID == "" {
		return Progress{}, fmt.Errorf("job reference %q names no deployment", j.Ref)
	}
	if taskID != "" {
		var task struct {
			Status               string   `json:"status"`
			CompletionPercentage *float64 `json:"completion_percentage"`
			Message              string   `json:"message"`
		}
		res, err := read(ctx, platform.Request{Method: http.MethodGet, Path: "/tasks/" + taskID})
		if err != nil {
			return Progress{}, err
		}
		if res.Status != "ok" || res.HTTPStatus < 200 || res.HTTPStatus >= 300 {
			return Progress{}, fmt.Errorf("task %s not readable: %s", taskID, readMessage(res))
		}
		if err := json.Unmarshal([]byte(res.Body), &task); err != nil {
			return Progress{}, fmt.Errorf("task %s unreadable: %w", taskID, err)
		}
		switch task.Status {
		case "failed":
			return Progress{State: "failed", Message: edgeStartFailedText(deploymentName(ctx, read, depID), task.Message)}, nil
		case "completed":
			// fall through to the deployment below
		default:
			p := Progress{State: "running", Message: "task " + taskStatusText(task.Status), Percent: task.CompletionPercentage}
			return p, nil
		}
	}
	var dep struct {
		Status string `json:"status"`
	}
	res, err := read(ctx, platform.Request{Method: http.MethodGet, Path: "/container-deployments/" + depID})
	if err != nil {
		return Progress{}, err
	}
	if res.Status != "ok" || res.HTTPStatus < 200 || res.HTTPStatus >= 300 {
		return Progress{}, fmt.Errorf("deployment %s not readable: %s", depID, readMessage(res))
	}
	if err := json.Unmarshal([]byte(res.Body), &dep); err != nil {
		return Progress{}, fmt.Errorf("deployment %s unreadable: %w", depID, err)
	}
	pct := 100.0
	switch dep.Status {
	case "deployed":
		return Progress{State: "done", Percent: &pct, Message: "deployed"}, nil
	case "undeployed":
		return Progress{State: "done", Percent: &pct, Message: "undeployed"}, nil
	}
	return Progress{State: "running", Message: "deployment " + taskStatusText(dep.Status)}, nil
}

// stopEdgeDeployment names the call that takes a deployment back (edge E3).
func stopEdgeDeployment(j Job) (platform.Request, bool) {
	depID, _ := parseEdgeRef(j.Ref)
	if depID == "" {
		return platform.Request{}, false
	}
	return platform.Request{Method: http.MethodPut, Path: "/container-deployments/" + depID + "/undeploy"}, true
}

// deploymentName reads the name of a deployment for a message; a failed read leaves a placeholder.
func deploymentName(ctx context.Context, read Reader, id string) string {
	res, err := read(ctx, platform.Request{Method: http.MethodGet, Path: "/container-deployments/" + id})
	if err != nil || res.Status != "ok" || res.HTTPStatus < 200 || res.HTTPStatus >= 300 {
		return "agw-…"
	}
	var dep struct {
		Name string `json:"name"`
	}
	if json.Unmarshal([]byte(res.Body), &dep) != nil || dep.Name == "" {
		return "agw-…"
	}
	return dep.Name
}

// taskStatusText names a status that came in empty.
func taskStatusText(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// readMessage is the one line of a reading call that did not answer with 2xx.
func readMessage(res platform.Result) string {
	if res.Message != "" {
		return res.Message
	}
	return fmt.Sprintf("HTTP %d", res.HTTPStatus)
}

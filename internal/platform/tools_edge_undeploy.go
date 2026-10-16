// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
)

// The tools that take an edge deployment back (edge D-E3): undeploy removes the container from the
// device and leaves the deployment, delete_deployment removes the deployment entry. They live in
// their own file so that the deployment tools of E2 and these can grow next to each other; init
// appends them, so the table in tools.go keeps what it is.
func init() {
	Tools = append(Tools, undeployTool, deleteDeploymentTool)
}

// undeployInfo is what both tools read of a deployment before they act (edge D-E3). The deployment
// type of E2 is deliberately not reused, so the two files stay independent. Numbers stay json.Number
// (code review M2).
type undeployInfo struct {
	ID                json.Number `json:"id"`
	Name              string      `json:"name"`
	Status            string      `json:"status"`
	DockerContainerID *string     `json:"docker_container_id"`
}

// undeploy removes the container of a deployment of this chat from its device. Effect write: it gives
// the resources back and the deployment can be started again, like stopping a training
// (delegation.go:248).
var undeployTool = Tool{
	Name: "undeploy", CLI: "undeploy",
	Desc:   "Stops and removes the container of a deployment created in this chat from its edge device (anonymous volumes are removed too). The deployment stays and can be started again with platform_start_deployment. Writes; the user must approve. Response: the task (/tasks/<id>).",
	Write:  true,
	Params: []Param{{Name: "deployment_id", Type: "integer", Desc: "id of the deployment (platform_list_deployments or the answer of platform_create_deployment)", Required: true}},
	build:  byID(http.MethodPut, "/container-deployments", "deployment_id", "/undeploy"),
	steps:  undeploySteps,
}

// undeploySteps reads the deployment and refuses what the platform would otherwise report as a
// success: it removes the container only by its stored docker id and answers 2xx even when there is
// none, so a deployment whose start failed would look taken back while its container stays on the
// device (F3, edge D3.1).
func undeploySteps(ctx context.Context, a Args, final Request, do Doer) (Result, string, error) {
	id, _ := requiredInt(a, "deployment_id") // checked by build
	read := Request{Method: http.MethodGet, Path: "/container-deployments/" + strconv.FormatInt(id, 10)}
	var dep undeployInfo
	if res, stop, err := ReadJSON(ctx, do, read, &dep); stop {
		return res, "", err
	}
	switch {
	case dep.Status != "deployed":
		return Refuse("deployment %d is not deployed (status %s); nothing to undeploy", id, deployStatusText(dep.Status))
	case dep.DockerContainerID == nil || *dep.DockerContainerID == "":
		return Refuse("deployment %d has no container id: its start failed, and the platform cannot remove a leftover container named %s. Tell the user; it needs manual removal on the device.", id, dep.Name)
	}
	res, err := do(ctx, final)
	if err != nil || res.Status != "ok" || res.HTTPStatus < 200 || res.HTTPStatus >= 300 {
		return res, "", err
	}
	res.Body = jsonBody(map[string]any{
		"deployment_id": id,
		"status":        "undeployed",
		"task":          res.Location,
		"note":          "the platform removes the container in the background; the deployment stays and can be started again, follow the task with platform_deployment_status",
	})
	return res, "", nil
}

// delete_deployment removes a deployment entry of this chat. Effect irreversible: nothing brings it
// back, so the user approves it on its own, also inside a plan.
var deleteDeploymentTool = Tool{
	Name: "delete_deployment", CLI: "delete-deployment",
	Desc:   "Deletes a deployment created in this chat that is not running (undeploy it first). Irreversible: the user must approve it on its own, also inside a plan.",
	Write:  true,
	Params: []Param{{Name: "deployment_id", Type: "integer", Desc: "id of the deployment (platform_list_deployments or the answer of platform_create_deployment)", Required: true}},
	build:  byID(http.MethodDelete, "/container-deployments", "deployment_id", ""),
	steps:  deleteDeploymentSteps,
}

// deleteDeploymentSteps reads the deployment first: a running one is refused with the step that has
// to come before it, instead of the platform's 409 (routers/container_deployments.py:211-215).
func deleteDeploymentSteps(ctx context.Context, a Args, final Request, do Doer) (Result, string, error) {
	id, _ := requiredInt(a, "deployment_id") // checked by build
	read := Request{Method: http.MethodGet, Path: "/container-deployments/" + strconv.FormatInt(id, 10)}
	var dep undeployInfo
	if res, stop, err := ReadJSON(ctx, do, read, &dep); stop {
		return res, "", err
	}
	if dep.Status == "deployed" {
		return Refuse("deployment %d is deployed; undeploy it first", id)
	}
	res, err := do(ctx, final)
	if err != nil || res.Status != "ok" || res.HTTPStatus < 200 || res.HTTPStatus >= 300 {
		return res, "", err
	}
	res.Body = jsonBody(map[string]any{
		"deployment_id": id,
		"deleted":       true,
		"note":          "the entry is gone; an image the deployment used can be deleted by the user now, the agent deletes no images",
	})
	return res, "", nil
}

// deployStatusText names a status that came in empty: the message then says unknown instead of
// nothing.
func deployStatusText(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

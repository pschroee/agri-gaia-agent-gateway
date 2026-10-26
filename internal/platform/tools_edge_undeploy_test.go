// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

import (
	"net/http"
	"strings"
	"testing"
)

// undeployDeploymentJSON is a deployment as the two removal tools read it: name, status and whether
// the platform stored a docker container id (edge D-E3). A missing id is what a failed start leaves
// behind (F3).
func undeployDeploymentJSON(status string, containerID bool) string {
	id := "null"
	if containerID {
		id = `"b0e1c2d3"`
	}
	return `{"id":9,"name":"agw-mnist","status":"` + status + `","edge_device_id":1,"container_image_id":5,` +
		`"docker_container_id":` + id + `}`
}

// countCalls counts how many recorded calls start with this method and path.
func countCalls(calls []string, prefix string) int {
	n := 0
	for _, c := range calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// undeploy refuses before its PUT what the platform would report as a success: a deployment that is
// not deployed and one without a container id (F3, edge D3.1, #231). A deployed deployment with a
// container id, and a created one that kept the id of a container its failed start could not remove,
// send exactly one PUT.
func TestUndeployChecks(t *testing.T) {
	const undeploy = "PUT /container-deployments/9/undeploy"
	cases := []struct {
		name   string
		dep    string
		refuse string
	}{
		{"created without a container", undeployDeploymentJSON("created", false),
			"deployment 9 has no container on the device (status created); nothing to undeploy"},
		{"already taken back", undeployDeploymentJSON("undeployed", false),
			"deployment 9 is not deployed (status undeployed); nothing to undeploy"},
		{"status missing", `{"id":9,"name":"agw-mnist"}`,
			"deployment 9 is not deployed (status unknown); nothing to undeploy"},
		{"deployed without a container id", undeployDeploymentJSON("deployed", false),
			"deployment 9 has no container on the device (status deployed); nothing to undeploy"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, refused, calls := runEdgeTool(t, "undeploy", `{"deployment_id":9}`, map[string]Result{
				"GET /container-deployments/9": {Status: "ok", HTTPStatus: 200, Body: c.dep},
				undeploy:                       {Status: "ok", HTTPStatus: http.StatusAccepted, Location: "/tasks/7"},
			})
			if refused != c.refuse {
				t.Fatalf("refused=%q, want %q", refused, c.refuse)
			}
			if res.Status != "error" {
				t.Fatalf("status: %+v", res)
			}
			if countCalls(calls, undeploy) != 0 {
				t.Fatalf("a refused undeploy reached the platform: %v", calls)
			}
			if countCalls(calls, "GET /container-deployments/9") != 1 {
				t.Fatalf("the deployment was not read once: %v", calls)
			}
		})
	}
}

// A deployed deployment with a container id sends exactly one PUT and answers with the task from the
// Location (edge D-E3).
func TestUndeploySends(t *testing.T) {
	res, refused, calls := runEdgeTool(t, "undeploy", `{"deployment_id":9}`, map[string]Result{
		"GET /container-deployments/9":          {Status: "ok", HTTPStatus: 200, Body: undeployDeploymentJSON("deployed", true)},
		"PUT /container-deployments/9/undeploy": {Status: "ok", HTTPStatus: http.StatusAccepted, Location: "/tasks/7"},
	})
	if refused != "" || res.Status != "ok" {
		t.Fatalf("undeploy: %+v refused=%q", res, refused)
	}
	if n := countCalls(calls, "PUT /container-deployments/9/undeploy"); n != 1 {
		t.Fatalf("PUTs: %d in %v", n, calls)
	}
	if !strings.Contains(res.Body, `"task":"/tasks/7"`) || !strings.Contains(res.Body, `"deployment_id":9`) ||
		!strings.Contains(res.Body, `"status":"undeployed"`) {
		t.Fatalf("answer: %s", res.Body)
	}
}

// Deleting is refused while the container runs: the platform answers 409 there, the tool says what to
// do first (edge D-E3). A deployment that is not deployed is deleted with one DELETE.
func TestDeleteDeploymentRefusesDeployed(t *testing.T) {
	const del = "DELETE /container-deployments/9"
	t.Run("deployed", func(t *testing.T) {
		res, refused, calls := runEdgeTool(t, "delete_deployment", `{"deployment_id":9}`, map[string]Result{
			"GET /container-deployments/9": {Status: "ok", HTTPStatus: 200, Body: undeployDeploymentJSON("deployed", true)},
			del:                            {Status: "ok", HTTPStatus: 204},
		})
		if refused != "deployment 9 is deployed; {{tool:undeploy}} it first" || res.Status != "error" {
			t.Fatalf("res=%+v refused=%q", res, refused)
		}
		if countCalls(calls, del) != 0 {
			t.Fatalf("a refused delete reached the platform: %v", calls)
		}
	})
	for _, status := range []string{"created", "undeployed"} {
		t.Run(status, func(t *testing.T) {
			res, refused, calls := runEdgeTool(t, "delete_deployment", `{"deployment_id":9}`, map[string]Result{
				"GET /container-deployments/9": {Status: "ok", HTTPStatus: 200, Body: undeployDeploymentJSON(status, false)},
				del:                            {Status: "ok", HTTPStatus: 204},
			})
			if refused != "" || res.Status != "ok" {
				t.Fatalf("delete: %+v refused=%q", res, refused)
			}
			if n := countCalls(calls, del); n != 1 {
				t.Fatalf("DELETEs: %d in %v", n, calls)
			}
			if !strings.Contains(res.Body, `"deleted":true`) {
				t.Fatalf("answer: %s", res.Body)
			}
		})
	}
}

// Taking a deployment back needs no device (edge D-E3): both tools read the deployment alone, answer ok
// and never read the device list. Only creating and starting a deployment answer unavailable without an
// online device (edge D2, G1.13); a removal that did the same could never clean up a deployment while
// the device is away.
func TestUndeployNeedsNoDevice(t *testing.T) {
	cases := []struct {
		name, dep, call string
	}{
		{"undeploy", undeployDeploymentJSON("deployed", true), "PUT /container-deployments/9/undeploy"},
		{"delete_deployment", undeployDeploymentJSON("undeployed", false), "DELETE /container-deployments/9"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, refused, calls := runEdgeTool(t, c.name, `{"deployment_id":9}`, map[string]Result{
				"GET /container-deployments/9": {Status: "ok", HTTPStatus: 200, Body: c.dep},
				c.call:                         {Status: "ok", HTTPStatus: 202, Location: "/tasks/7"},
			})
			if res.Status == StatusUnavailable {
				t.Fatalf("%s answered unavailable without a device", c.name)
			}
			if refused != "" || res.Status != "ok" {
				t.Fatalf("%s: %+v refused=%q", c.name, res, refused)
			}
			if n := countCalls(calls, "GET /edge-devices"); n != 0 {
				t.Fatalf("%s read the device list: %v", c.name, calls)
			}
			if countCalls(calls, c.call) != 1 {
				t.Fatalf("%s did not send %s: %v", c.name, c.call, calls)
			}
		})
	}
}

// The two removal tools are in the one table CLI, MCP and the schema generate from (edge D-E3).
func TestUndeployToolsInAllBindings(t *testing.T) {
	cases := []struct {
		name string
		cli  string
		desc string
	}{
		{"undeploy", "undeploy", "The deployment stays and can be started again with {{tool:start_deployment}}."},
		{"delete_deployment", "delete-deployment", "Irreversible: the user must approve it on its own, also inside a plan."},
	}
	for _, c := range cases {
		tool, ok := Lookup(c.name)
		if !ok {
			t.Errorf("%s is not in the tool table", c.name)
			continue
		}
		if tool.CLI != c.cli || !tool.Write || tool.MCPName() != "platform_"+c.name {
			t.Errorf("%s: cli=%q write=%t mcp=%q", c.name, tool.CLI, tool.Write, tool.MCPName())
		}
		if !strings.Contains(tool.Desc, c.desc) {
			t.Errorf("%s: description %q", c.name, tool.Desc)
		}
		schema := tool.Schema()
		props, _ := schema["properties"].(map[string]any)
		if _, ok := props["deployment_id"]; !ok || schema["type"] != "object" {
			t.Errorf("%s: schema %v", c.name, schema)
		}
		req, _ := schema["required"].([]string)
		if len(req) != 1 || req[0] != "deployment_id" {
			t.Errorf("%s: required %v", c.name, schema["required"])
		}
	}
}

// A failed start removes its container again; only when that removal failed too, the deployment is
// still created but keeps the container id, and undeploy takes it back (#231).
func TestUndeployLeftoverOfFailedStart(t *testing.T) {
	res, refused, calls := runEdgeTool(t, "undeploy", `{"deployment_id":9}`, map[string]Result{
		"GET /container-deployments/9":          {Status: "ok", HTTPStatus: 200, Body: undeployDeploymentJSON("created", true)},
		"PUT /container-deployments/9/undeploy": {Status: "ok", HTTPStatus: http.StatusAccepted, Location: "/tasks/7"},
	})
	if refused != "" || res.Status != "ok" {
		t.Fatalf("undeploy: %+v refused=%q", res, refused)
	}
	if n := countCalls(calls, "PUT /container-deployments/9/undeploy"); n != 1 {
		t.Fatalf("PUTs: %d in %v", n, calls)
	}
}

// A created deployment without a container id has nothing on the device to take back (#231).
func TestUndeployCreatedWithoutContainer(t *testing.T) {
	res, refused, calls := runEdgeTool(t, "undeploy", `{"deployment_id":9}`, map[string]Result{
		"GET /container-deployments/9": {Status: "ok", HTTPStatus: 200, Body: undeployDeploymentJSON("created", false)},
	})
	want := "deployment 9 has no container on the device (status created); nothing to undeploy"
	if refused != want || res.Status != "error" {
		t.Fatalf("refused=%q res=%+v", refused, res)
	}
	if countCalls(calls, "PUT ") != 0 {
		t.Fatalf("a PUT went out: %v", calls)
	}
}

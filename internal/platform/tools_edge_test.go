// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The heartbeat of the platform is a naive UTC timestamp without a zone (F8); the tests build one in
// the same shape.
const naiveHeartbeat = "2006-01-02T15:04:05.000000"

func heartbeatAgo(d time.Duration) string {
	return time.Now().UTC().Add(-d).Format(naiveHeartbeat)
}

// heartbeatJSON writes a heartbeat value; an empty string becomes null.
func heartbeatJSON(s string) string {
	if s == "" {
		return "null"
	}
	return `"` + s + `"`
}

// edgeDeviceJSON builds one device as GET /edge-devices returns it, including the edge_key the compact
// list must drop.
func edgeDeviceJSON(id int, name, arch, heartbeat string, registered bool, deployments ...string) string {
	return fmt.Sprintf(`{"id":%d,"name":%q,"arch":%q,"os":"Linux","cpu_count":4,"memory":8589934592,`+
		`"registered":%t,"last_heartbeat":%s,"tags":["simulated"],"edge_key":"edge-secret",`+
		`"container_deployments":[%s]}`, id, name, arch, registered, heartbeatJSON(heartbeat), strings.Join(deployments, ","))
}

// edgeDeviceList wraps devices into the answer of GET /edge-devices.
func edgeDeviceList(devices ...string) string { return "[" + strings.Join(devices, ",") + "]" }

// deploymentJSON builds one container deployment of a device.
func deploymentJSON(id int, name, status string, hostPort int, repository, tag string) string {
	return fmt.Sprintf(`{"id":%d,"name":%q,"status":%q,"edge_device_id":1,"container_image_id":5,`+
		`"port_bindings":[{"host_port":"%d","container_port":"8000","protocol":"tcp"}],`+
		`"docker_container_id":null,"creation_date":"2026-10-15T10:00:00.000000",`+
		`"container_image":{"id":5,"repository":%q,"tag":%q,"platform":"linux/amd64","exposed_ports":[]}}`,
		id, name, status, hostPort, repository, tag)
}

// imageListJSON is the answer of GET /container-images/<repo>:<tag>: one image for the given platform.
func imageListJSON(id int, repository, tag, platform string) string {
	return fmt.Sprintf(`[{"id":%d,"repository":%q,"tag":%q,"platform":%q,"exposed_ports":[]}]`, id, repository, tag, platform)
}

// edgeArgs is a valid creation of a deployment on device 1 for the image agent-test/mnist-triton:v1.
const edgeArgs = `{"device_id":1,"repository":"agent-test/mnist-triton","tag":"v1","name":"mnist","host_port":18100}`

// oneOnlineDevice is a registered, online device with the given deployments.
func oneOnlineDevice(deployments ...string) string {
	return edgeDeviceList(edgeDeviceJSON(1, "sim-edge-01", "amd64", heartbeatAgo(2*time.Minute), true, deployments...))
}

// runEdgeTool runs a platform tool with the given answers; calls records what reached the doer.
func runEdgeTool(t *testing.T, name, args string, answers map[string]Result) (Result, string, []string) {
	t.Helper()
	tool := mustLookup(t, name)
	s := &stepsDoer{answers: answers}
	var raw json.RawMessage
	if args != "" {
		raw = json.RawMessage(args)
	}
	res, refused, err := tool.Run(context.Background(), raw, s.do)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return res, refused, s.calls
}

// reachedCall reports whether one of the recorded calls starts with this method and path.
func reachedCall(calls []string, prefix string) bool {
	for _, c := range calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

// The compact list names the online state (heartbeat within 5 minutes, a naive timestamp read as UTC),
// the architecture, the tags and the deployments, and it drops edge_key and portainer_id (edge D2.2,
// D2.3, F8).
func TestListEdgeDevicesCompact(t *testing.T) {
	body := edgeDeviceList(
		edgeDeviceJSON(1, "sim-edge-01", "amd64", heartbeatAgo(4*time.Minute), true,
			deploymentJSON(9, "agw-mnist", "deployed", 18100, "agent-test/mnist-triton", "v1")),
		edgeDeviceJSON(2, "barn-pi", "arm64", heartbeatAgo(6*time.Minute), true),
	)
	res, refused, calls := runEdgeTool(t, "list_edge_devices", "", map[string]Result{
		"GET /edge-devices": {Status: "ok", HTTPStatus: 200, Body: body},
	})
	if refused != "" || res.Status != "ok" {
		t.Fatalf("list_edge_devices: %+v refused=%q", res, refused)
	}
	if len(calls) != 1 {
		t.Fatalf("calls: %v", calls)
	}
	if strings.Contains(res.Body, "edge_key") || strings.Contains(res.Body, "portainer_id") || strings.Contains(res.Body, "edge-secret") {
		t.Fatalf("the compact list leaks the raw fields: %s", res.Body)
	}
	var devices []map[string]any
	if err := json.Unmarshal([]byte(res.Body), &devices); err != nil {
		t.Fatalf("not JSON: %v in %s", err, res.Body)
	}
	if len(devices) != 2 {
		t.Fatalf("devices: %s", res.Body)
	}
	if devices[0]["online"] != true || devices[1]["online"] != false {
		t.Fatalf("online (4 min yes, 6 min no): %s", res.Body)
	}
	if devices[0]["arch"] != "amd64" || devices[1]["arch"] != "arm64" {
		t.Fatalf("arch: %s", res.Body)
	}
	deps, _ := devices[0]["deployments"].([]any)
	if len(deps) != 1 {
		t.Fatalf("deployments: %s", res.Body)
	}
	dep, _ := deps[0].(map[string]any)
	ports, _ := dep["ports"].([]any)
	if len(ports) != 1 || ports[0] != "18100->8000/tcp" || dep["image"] != "agent-test/mnist-triton:v1" || dep["status"] != "deployed" {
		t.Fatalf("deployment: %s", res.Body)
	}
}

// The device list answers empty deployments as [] and never as null.
func TestListEdgeDevicesEmptyDeployments(t *testing.T) {
	res, refused, _ := runEdgeTool(t, "list_edge_devices", "", map[string]Result{
		"GET /edge-devices": {Status: "ok", HTTPStatus: 200, Body: oneOnlineDevice()},
	})
	if refused != "" || !strings.Contains(res.Body, `"deployments":[]`) {
		t.Fatalf("res=%+v refused=%q", res, refused)
	}
}

// create_deployment refuses at the gateway for every reason before anything reaches the platform (edge
// D2.4); only a call that passes every check sends exactly one POST with the agw- name and the
// bindings.
func TestCreateDeploymentChecks(t *testing.T) {
	image := "GET /container-images/agent-test/mnist-triton:v1"
	created := Result{Status: "ok", HTTPStatus: 201, Body: `{"id":9,"name":"agw-mnist","status":"created"}`}
	cases := []struct {
		name    string
		devices string
		answers map[string]Result
		args    string
		refuse  string
		post    bool
	}{
		{
			name:    "device unknown",
			devices: oneOnlineDevice(),
			answers: map[string]Result{image: {Status: "ok", HTTPStatus: 200, Body: imageListJSON(5, "agent-test/mnist-triton", "v1", "linux/amd64")}},
			args:    `{"device_id":7,"repository":"agent-test/mnist-triton","tag":"v1","name":"mnist","host_port":18100}`,
			refuse:  "not in the platform's device list",
		},
		{
			name:    "device not registered",
			devices: edgeDeviceList(edgeDeviceJSON(1, "sim-edge-01", "amd64", heartbeatAgo(time.Minute), false), edgeDeviceJSON(2, "barn-pi", "amd64", heartbeatAgo(time.Minute), true)),
			args:    edgeArgs,
			refuse:  "not registered",
		},
		{
			name:    "device offline",
			devices: edgeDeviceList(edgeDeviceJSON(1, "sim-edge-01", "amd64", heartbeatAgo(9*time.Minute), true), edgeDeviceJSON(2, "barn-pi", "amd64", heartbeatAgo(time.Minute), true)),
			args:    edgeArgs,
			refuse:  "is offline",
		},
		{
			name:    "architecture does not match",
			devices: oneOnlineDevice(),
			answers: map[string]Result{image: {Status: "ok", HTTPStatus: 200, Body: imageListJSON(5, "agent-test/mnist-triton", "v1", "linux/arm64")}},
			args:    edgeArgs,
			refuse:  "has no build for the device's architecture amd64",
		},
		{
			name:    "image missing",
			devices: oneOnlineDevice(),
			answers: map[string]Result{image: {Status: "error", HTTPStatus: 404, Message: "no such image"}},
			args:    edgeArgs,
			refuse:  "is not in the platform's registry",
		},
		{
			name:    "training image",
			devices: oneOnlineDevice(),
			answers: map[string]Result{"GET /container-images/agent-test/mnist-train:v1-train": {Status: "ok", HTTPStatus: 200, Body: imageListJSON(5, "agent-test/mnist-train", "v1-train", "linux/amd64")}},
			args:    `{"device_id":1,"repository":"agent-test/mnist-train","tag":"v1-train","name":"mnist","host_port":18100}`,
			refuse:  "is a training image",
		},
		{
			name:    "port outside the range",
			devices: oneOnlineDevice(),
			args:    `{"device_id":1,"repository":"agent-test/mnist-triton","tag":"v1","name":"mnist","host_port":18000}`,
			refuse:  "outside the range",
		},
		{
			name:    "port already used",
			devices: oneOnlineDevice(deploymentJSON(9, "agw-other", "created", 18100, "agent-test/mnist-triton", "v1")),
			answers: map[string]Result{image: {Status: "ok", HTTPStatus: 200, Body: imageListJSON(5, "agent-test/mnist-triton", "v1", "linux/amd64")}},
			args:    edgeArgs,
			refuse:  "is already used by deployment 9",
		},
		{
			name:    "name already used",
			devices: oneOnlineDevice(deploymentJSON(9, "agw-mnist", "undeployed", 18150, "agent-test/mnist-triton", "v1")),
			answers: map[string]Result{image: {Status: "ok", HTTPStatus: 200, Body: imageListJSON(5, "agent-test/mnist-triton", "v1", "linux/amd64")}},
			args:    edgeArgs,
			refuse:  "already exists on device 1",
		},
		{
			name:    "image_id does not belong",
			devices: oneOnlineDevice(),
			answers: map[string]Result{image: {Status: "ok", HTTPStatus: 200, Body: imageListJSON(5, "agent-test/mnist-triton", "v1", "linux/amd64")}},
			args:    `{"device_id":1,"repository":"agent-test/mnist-triton","tag":"v1","name":"mnist","host_port":18100,"image_id":7}`,
			refuse:  "image_id 7 does not belong",
		},
		{
			name:    "valid",
			devices: oneOnlineDevice(),
			answers: map[string]Result{image: {Status: "ok", HTTPStatus: 200, Body: imageListJSON(5, "agent-test/mnist-triton", "v1", "linux/amd64")}, "POST /container-deployments": created},
			args:    edgeArgs,
			post:    true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			answers := map[string]Result{"GET /edge-devices": {Status: "ok", HTTPStatus: 200, Body: c.devices}}
			for k, v := range c.answers {
				answers[k] = v
			}
			res, refused, calls := runEdgeTool(t, "create_deployment", c.args, answers)
			if c.post {
				if refused != "" || res.Status != "ok" {
					t.Fatalf("valid: %+v refused=%q", res, refused)
				}
				if !reachedCall(calls, "POST /container-deployments") {
					t.Fatalf("no POST went out: %v", calls)
				}
				post := ""
				for _, call := range calls {
					if strings.HasPrefix(call, "POST /container-deployments") {
						post = call
					}
				}
				if !strings.Contains(post, `"name":"agw-mnist"`) || !strings.Contains(post, `"container_image_id":5`) ||
					!strings.Contains(post, `"host_port":"18100"`) || !strings.Contains(post, `"container_port":"8000"`) {
					t.Fatalf("body: %s", post)
				}
				if !strings.Contains(res.Body, `"next"`) || !strings.Contains(res.Body, `"id":9`) {
					t.Fatalf("answer: %s", res.Body)
				}
				return
			}
			if refused == "" || !strings.Contains(refused, c.refuse) {
				t.Fatalf("refused=%q, want %q", refused, c.refuse)
			}
			if res.Status != "error" {
				t.Fatalf("status: %+v", res)
			}
			if reachedCall(calls, "POST /container-deployments") {
				t.Fatalf("a refuses call reached the platform: %v", calls)
			}
		})
	}
}

// A deployment that is already deployed is refused before the PUT goes out (F4).
func TestStartDeploymentRefusesDeployed(t *testing.T) {
	dep := `{"id":9,"name":"agw-mnist","status":"deployed","edge_device_id":1,"container_image_id":5}`
	res, refused, calls := runEdgeTool(t, "start_deployment", `{"deployment_id":9}`, map[string]Result{
		"GET /container-deployments/9": {Status: "ok", HTTPStatus: 200, Body: dep},
		"GET /edge-devices":            {Status: "ok", HTTPStatus: 200, Body: oneOnlineDevice()},
	})
	if refused == "" || !strings.Contains(refused, "is already deployed") {
		t.Fatalf("refused=%q", refused)
	}
	if res.Status != "error" {
		t.Fatalf("status: %+v", res)
	}
	if len(calls) != 1 {
		t.Fatalf("a refuses start went further: %v", calls)
	}
}

// A start puts the request out and answers with the task from the Location.
func TestStartDeploymentStarts(t *testing.T) {
	dep := `{"id":9,"name":"agw-mnist","status":"created","edge_device_id":1,"container_image_id":5}`
	res, refused, calls := runEdgeTool(t, "start_deployment", `{"deployment_id":9}`, map[string]Result{
		"GET /container-deployments/9":        {Status: "ok", HTTPStatus: 200, Body: dep},
		"GET /edge-devices":                   {Status: "ok", HTTPStatus: 200, Body: oneOnlineDevice()},
		"PUT /container-deployments/9/deploy": {Status: "ok", HTTPStatus: http.StatusAccepted, Location: "/tasks/5"},
	})
	if refused != "" || res.Status != "ok" {
		t.Fatalf("start: %+v refused=%q", res, refused)
	}
	if !reachedCall(calls, "PUT /container-deployments/9/deploy") {
		t.Fatalf("no PUT: %v", calls)
	}
	if !strings.Contains(res.Body, `"task":"/tasks/5"`) || !strings.Contains(res.Body, `"deployment_id":9`) {
		t.Fatalf("answer: %s", res.Body)
	}
}

// deployment_status names the three hints: a failed start (F3), the platform that never checks a
// running container (F11) and an offline device (edge D2.6).
func TestDeploymentStatusHints(t *testing.T) {
	cases := []struct {
		name       string
		deployment string
		device     string
		task       string
		want       string
	}{
		{
			name:       "failed start",
			deployment: `{"id":9,"name":"agw-mnist","status":"created","edge_device_id":1,"container_image_id":5}`,
			device:     oneOnlineDevice(),
			task:       `{"id":5,"status":"failed","message":"port is already allocated"}`,
			want:       "the start failed; a container named agw-mnist may be left",
		},
		{
			name:       "deployed is not running",
			deployment: `{"id":9,"name":"agw-mnist","status":"deployed","edge_device_id":1,"container_image_id":5,"docker_container_id":"abc"}`,
			device:     oneOnlineDevice(),
			want:       "does not check whether the container still runs",
		},
		{
			name:       "device offline",
			deployment: `{"id":9,"name":"agw-mnist","status":"created","edge_device_id":1,"container_image_id":5}`,
			device:     edgeDeviceList(edgeDeviceJSON(1, "sim-edge-01", "amd64", heartbeatAgo(9*time.Minute), true), edgeDeviceJSON(2, "barn-pi", "amd64", heartbeatAgo(time.Minute), true)),
			want:       "has not checked in for more than 5 minutes",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			answers := map[string]Result{
				"GET /container-deployments/9": {Status: "ok", HTTPStatus: 200, Body: c.deployment},
				"GET /edge-devices":            {Status: "ok", HTTPStatus: 200, Body: c.device},
			}
			args := `{"deployment_id":9}`
			if c.task != "" {
				answers["GET /tasks/5"] = Result{Status: "ok", HTTPStatus: 200, Body: c.task}
				args = `{"deployment_id":9,"task_id":5}`
			}
			res, refused, _ := runEdgeTool(t, "deployment_status", args, answers)
			if refused != "" || res.Status != "ok" {
				t.Fatalf("status: %+v refused=%q", res, refused)
			}
			if !strings.Contains(res.Body, c.want) {
				t.Fatalf("hint missing %q in %s", c.want, res.Body)
			}
		})
	}
}

// The five tools of the edge deployment are in the one table that CLI, MCP and the schema generate
// from (edge D2).
func TestEdgeToolsInAllBindings(t *testing.T) {
	cases := []struct {
		name  string
		cli   string
		write bool
		req   []string
	}{
		{"list_edge_devices", "edge-devices", false, nil},
		{"list_deployments", "deployments", false, nil},
		{"create_deployment", "create-deployment", true, []string{"device_id", "repository", "tag", "name", "host_port"}},
		{"start_deployment", "start-deployment", true, []string{"deployment_id"}},
		{"deployment_status", "deployment-status", false, []string{"deployment_id"}},
	}
	for _, c := range cases {
		tool, ok := Lookup(c.name)
		if !ok {
			t.Errorf("%s is not in the tool table", c.name)
			continue
		}
		if tool.CLI != c.cli || tool.Write != c.write || tool.Desc == "" {
			t.Errorf("%s: cli=%q write=%t desc=%q", c.name, tool.CLI, tool.Write, tool.Desc)
		}
		if tool.MCPName() != "platform_"+c.name {
			t.Errorf("%s: MCP name %q", c.name, tool.MCPName())
		}
		schema := tool.Schema()
		if schema["type"] != "object" || schema["additionalProperties"] != false {
			t.Errorf("%s: schema %v", c.name, schema)
		}
		props, _ := schema["properties"].(map[string]any)
		for _, r := range c.req {
			if _, ok := props[r]; !ok {
				t.Errorf("%s: parameter %s missing in the schema", c.name, r)
			}
		}
	}
	// create_deployment offers the optional ports and the image id.
	props, _ := mustLookup(t, "create_deployment").Schema()["properties"].(map[string]any)
	for _, opt := range []string{"grpc_host_port", "metrics_host_port", "image_id"} {
		if _, ok := props[opt]; !ok {
			t.Errorf("create_deployment: optional parameter %s missing", opt)
		}
	}
}

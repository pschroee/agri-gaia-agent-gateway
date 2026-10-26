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
			refuse:  "host port 18100 is used by deployment 9 (agw-other) on device 1",
		},
		{
			name:    "name already used",
			devices: oneOnlineDevice(deploymentJSON(9, "agw-mnist", "undeployed", 18150, "agent-test/mnist-triton", "v1")),
			answers: map[string]Result{image: {Status: "ok", HTTPStatus: 200, Body: imageListJSON(5, "agent-test/mnist-triton", "v1", "linux/amd64")}},
			args:    edgeArgs,
			refuse:  "name agw-mnist is taken on device 1 \"sim-edge-01\" by deployment 9 (status undeployed); use a free name, e.g. name \"mnist-2\"",
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
			want:       "the start failed: port is already allocated. Nothing of it is left on the device.",
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

// With only image_id (a plan that builds the image cannot know the repository, issue #218) the tool reads the
// image list, takes repository and tag of the entry with that id, and goes on with every check as before.
func TestCreateDeploymentImageIDOnly(t *testing.T) {
	list := Result{Status: "ok", HTTPStatus: 200, Body: `[` +
		`{"id":4,"repository":"agent-test/other","tag":"v2","platform":"linux/amd64","exposed_ports":[]},` +
		`{"id":5,"repository":"agent-test/mnist-triton","tag":"v1","platform":"linux/amd64","exposed_ports":[]}]`}
	created := Result{Status: "ok", HTTPStatus: 201, Body: `{"id":9,"name":"agw-mnist","status":"created"}`}
	answers := map[string]Result{
		"GET /edge-devices":                                {Status: "ok", HTTPStatus: 200, Body: oneOnlineDevice()},
		"GET /container-images":                            list,
		"GET /container-images/agent-test/mnist-triton:v1": {Status: "ok", HTTPStatus: 200, Body: imageListJSON(5, "agent-test/mnist-triton", "v1", "linux/amd64")},
		"POST /container-deployments":                      created,
	}
	res, refused, calls := runEdgeTool(t, "create_deployment", `{"device_id":1,"name":"mnist","host_port":18100,"image_id":5}`, answers)
	if refused != "" || res.Status != "ok" {
		t.Fatalf("image_id only: %+v refused=%q", res, refused)
	}
	if !reachedCall(calls, "GET /container-images/agent-test/mnist-triton:v1") {
		t.Fatalf("the image of the entry was not checked: %v", calls)
	}
	post := ""
	for _, c := range calls {
		if strings.HasPrefix(c, "POST /container-deployments") {
			post = c
		}
	}
	if !strings.Contains(post, `"container_image_id":5`) || !strings.Contains(post, `"name":"agw-mnist"`) {
		t.Fatalf("body: %s", post)
	}
	// An id that is not in the list is refused before anything is written.
	res, refused, calls = runEdgeTool(t, "create_deployment", `{"device_id":1,"name":"mnist","host_port":18100,"image_id":99}`, answers)
	if !strings.Contains(refused, "image 99 is not in the platform's image list") || res.Status != "error" || reachedCall(calls, "POST /container-deployments") {
		t.Fatalf("unknown id: %+v refused=%q calls=%v", res, refused, calls)
	}
	// Repository and tag together with the id keep working: the id has to belong to the image.
	_, refused, _ = runEdgeTool(t, "create_deployment", `{"device_id":1,"repository":"agent-test/mnist-triton","tag":"v1","name":"mnist","host_port":18100,"image_id":4}`, answers)
	if !strings.Contains(refused, "image_id 4 does not belong") {
		t.Fatalf("id of another image: %q", refused)
	}
}

func TestCreateDeploymentNeedsRepoOrImageID(t *testing.T) {
	tool := mustLookup(t, "create_deployment")
	for name, args := range map[string]string{
		"nothing":         `{"device_id":1,"name":"mnist","host_port":18100}`,
		"repository only": `{"device_id":1,"repository":"agent-test/mnist-triton","name":"mnist","host_port":18100}`,
		"tag only":        `{"device_id":1,"tag":"v1","name":"mnist","host_port":18100}`,
	} {
		if _, err := tool.Build(json.RawMessage(args)); err == nil || !strings.Contains(err.Error(), "give repository and tag, or image_id") {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, args := range map[string]string{
		"image_id only":      `{"device_id":1,"name":"mnist","host_port":18100,"image_id":5}`,
		"repository and tag": edgeArgs,
	} {
		if _, err := tool.Build(json.RawMessage(args)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// After a failed start the hint says why, that nothing is left and to start the same deployment again; with a
// container id the removal failed too and undeploy is the way (#231).
func TestDeploymentStatusHintRestart(t *testing.T) {
	res, refused, _ := runEdgeTool(t, "deployment_status", `{"deployment_id":9,"task_id":5}`, map[string]Result{
		"GET /container-deployments/9": {Status: "ok", HTTPStatus: 200, Body: `{"id":9,"name":"agw-mnist","status":"created","edge_device_id":1,"container_image_id":5}`},
		"GET /edge-devices":            {Status: "ok", HTTPStatus: 200, Body: oneOnlineDevice()},
		"GET /tasks/5":                 {Status: "ok", HTTPStatus: 200, Body: `{"id":5,"status":"failed","message":"Conflict. The container name is already in use"}`},
	})
	if refused != "" || res.Status != "ok" {
		t.Fatalf("status: %+v refused=%q", res, refused)
	}
	for _, want := range []string{"Conflict. The container name is already in use", "Nothing of it is left on the device", "start_deployment", "deployment_id 9", "replaces"} {
		if !strings.Contains(res.Body, want) {
			t.Fatalf("hint missing %q in %s", want, res.Body)
		}
	}
}

func TestDeploymentStatusHintLeftover(t *testing.T) {
	res, refused, _ := runEdgeTool(t, "deployment_status", `{"deployment_id":9,"task_id":5}`, map[string]Result{
		"GET /container-deployments/9": {Status: "ok", HTTPStatus: 200, Body: `{"id":9,"name":"agw-mnist","status":"created","edge_device_id":1,"container_image_id":5,"docker_container_id":"abc"}`},
		"GET /edge-devices":            {Status: "ok", HTTPStatus: 200, Body: oneOnlineDevice()},
		"GET /tasks/5":                 {Status: "ok", HTTPStatus: 200, Body: `{"id":5,"status":"failed","message":"port is already allocated; the container abc could not be removed"}`},
	})
	if refused != "" || res.Status != "ok" {
		t.Fatalf("status: %+v refused=%q", res, refused)
	}
	if !strings.Contains(res.Body, "could not be removed and is still on the device") || !strings.Contains(res.Body, "undeploy") ||
		strings.Contains(res.Body, "Nothing of it is left") {
		t.Fatalf("hint: %s", res.Body)
	}
}

// containersJSON is the answer of GET /edge-devices/{id}/containers.
func containersJSON(items ...string) string { return "[" + strings.Join(items, ",") + "]" }

func containerJSON(name, state string, ports ...int) string {
	ps := make([]string, len(ports))
	for i, p := range ports {
		ps[i] = fmt.Sprint(p)
	}
	return fmt.Sprintf(`{"name":%q,"state":%q,"host_ports":[%s]}`, name, state, strings.Join(ps, ","))
}

// createWith runs create_deployment on device 1 with the given containers and deployments of the device.
func createWith(t *testing.T, containers Result, deployments ...string) (Result, string, []string) {
	t.Helper()
	return runEdgeTool(t, "create_deployment", edgeArgs, map[string]Result{
		"GET /edge-devices": {Status: "ok", HTTPStatus: 200, Body: oneOnlineDevice(deployments...)},
		"GET /container-images/agent-test/mnist-triton:v1": {Status: "ok", HTTPStatus: 200, Body: imageListJSON(5, "agent-test/mnist-triton", "v1", "linux/amd64")},
		"GET /edge-devices/1/containers":                   containers,
		"POST /container-deployments":                      {Status: "ok", HTTPStatus: 201, Body: `{"id":9,"name":"agw-mnist","status":"created"}`},
	})
}

func okContainers(items ...string) Result {
	return Result{Status: "ok", HTTPStatus: 200, Body: containersJSON(items...)}
}

func TestOccupancyNameTakenByDeployment(t *testing.T) {
	_, refused, _ := createWith(t, okContainers(), deploymentJSON(9, "agw-mnist", "created", 18150, "agent-test/mnist-triton", "v1"))
	want := `name agw-mnist is taken on device 1 "sim-edge-01" by deployment 9 (status created); use a free name, e.g. name "mnist-2"`
	if refused != want {
		t.Fatalf("refused=%q\nwant    %q", refused, want)
	}
}

func TestOccupancyNameTakenByContainer(t *testing.T) {
	_, refused, calls := createWith(t, okContainers(containerJSON("agw-mnist", "exited")))
	want := `a container named agw-mnist is on device 1 "sim-edge-01" (state exited) and belongs to no platform deployment; use a free name, e.g. name "mnist-2", and tell the user that this container needs removal on the device`
	if refused != want {
		t.Fatalf("refused=%q\nwant    %q", refused, want)
	}
	if reachedCall(calls, "POST /container-deployments") {
		t.Fatalf("POST went out: %v", calls)
	}
}

// A container with the name of a deployment entry counts as that entry: one message, not two.
func TestOccupancyContainerOfDeploymentCountsAsDeployment(t *testing.T) {
	_, refused, _ := createWith(t, okContainers(containerJSON("agw-other", "running", 18100)),
		deploymentJSON(9, "agw-other", "deployed", 18100, "agent-test/mnist-triton", "v1"))
	if strings.Contains(refused, "belongs to no platform deployment") || strings.Contains(refused, "published by container") ||
		!strings.Contains(refused, "host port 18100 is used by deployment 9 (agw-other)") || strings.Count(refused, "host port 18100") != 1 {
		t.Fatalf("refused=%q", refused)
	}
}

func TestOccupancyPortIgnoresUndeployed(t *testing.T) {
	res, refused, _ := createWith(t, okContainers(), deploymentJSON(9, "agw-other", "undeployed", 18100, "agent-test/mnist-triton", "v1"))
	if refused != "" || res.Status != "ok" {
		t.Fatalf("an undeployed entry blocked the port: %+v %q", res, refused)
	}
}

func TestOccupancyPortTakenByContainer(t *testing.T) {
	_, refused, _ := createWith(t, okContainers(containerJSON("portainer", "running", 18100)))
	want := `host port 18100 is published by container portainer on device 1 "sim-edge-01"; free host ports: 18101, 18102, 18103`
	if refused != want {
		t.Fatalf("refused=%q\nwant    %q", refused, want)
	}
}

func TestOccupancyFreeNameAndPorts(t *testing.T) {
	o := Occupancy{
		deployments: []occupiedDeployment{{ID: "1", Name: "agw-x", Status: "deployed", Ports: []string{"18100"}}, {ID: "2", Name: "agw-x-2", Status: "undeployed", Ports: []string{"18101"}}},
		containers:  []deviceContainer{{Name: "agw-x-3", State: "exited"}},
	}
	if got := o.FreeName("x"); got != "x-4" {
		t.Fatalf("FreeName = %q", got)
	}
	if got := o.FreePorts(nil); got != "18101, 18102, 18103" {
		t.Fatalf("FreePorts = %q", got)
	}
	if got := o.FreePorts([]string{"18101"}); got != "18102, 18103, 18104" {
		t.Fatalf("FreePorts with exclude = %q", got)
	}
	long := strings.Repeat("a", 58)
	o = Occupancy{}
	got := o.FreeName(long)
	if len("agw-"+got) > 62 || !strings.HasSuffix(got, "-2") {
		t.Fatalf("FreeName of a long name = %q (%d characters with prefix)", got, len("agw-"+got))
	}
}

func TestOccupancyNoFreePort(t *testing.T) {
	var deps []occupiedDeployment
	for p := EdgeHostPortMin; p <= EdgeHostPortMax; p++ {
		deps = append(deps, occupiedDeployment{ID: "1", Name: "agw-d", Status: "deployed", Ports: []string{fmt.Sprint(p)}})
	}
	o := Occupancy{deployments: deps}
	if got := o.FreePorts(nil); got != "none in 18100-18199" {
		t.Fatalf("FreePorts = %q", got)
	}
}

func TestOccupancyReserveAndRelease(t *testing.T) {
	o := Occupancy{deployments: []occupiedDeployment{{ID: "6", Name: "agw-old", Status: "deployed", Ports: []string{"18100"}}}}
	o.Release("6", false)
	if c := o.Conflicts("new", []string{"18100"}); len(c) != 0 {
		t.Fatalf("undeploy did not free the port: %v", c)
	}
	o.Reserve(2, "new", []string{"18100"})
	c := o.Conflicts("new", []string{"18100"})
	if len(c) != 2 || !strings.Contains(c[0], "also given to step 2") || !strings.Contains(c[1], "also used by step 2") {
		t.Fatalf("conflicts: %v", c)
	}
	if c := o.Conflicts("old", nil); len(c) != 1 || !strings.Contains(c[0], "taken on") {
		t.Fatalf("an undeployed entry keeps its name: %v", c)
	}
	o.Release("6", true)
	if c := o.Conflicts("old", nil); len(c) != 0 {
		t.Fatalf("delete did not free the name: %v", c)
	}
}

// A device whose containers cannot be read does not block the creation; the answer says what was not checked.
func TestCreateDeploymentContainersUnreadable(t *testing.T) {
	res, refused, calls := createWith(t, Result{Status: "error", HTTPStatus: 404, Message: "Not Found"})
	if refused != "" || res.Status != "ok" || !reachedCall(calls, "POST /container-deployments") {
		t.Fatalf("res=%+v refused=%q calls=%v", res, refused, calls)
	}
	if !strings.Contains(res.Body, `"note"`) || !strings.Contains(res.Body, "could not be read") || !strings.Contains(res.Body, "only the platform's deployments were checked") {
		t.Fatalf("answer: %s", res.Body)
	}
}

func TestCreateDeploymentRefusesContainerPort(t *testing.T) {
	res, refused, calls := createWith(t, okContainers(containerJSON("foreign", "running", 18100, 18101)))
	if res.Status != "error" || !strings.Contains(refused, "host port 18100 is published by container foreign") || !strings.Contains(refused, "free host ports: 18102, 18103, 18104") {
		t.Fatalf("res=%+v refused=%q", res, refused)
	}
	if reachedCall(calls, "POST /container-deployments") {
		t.Fatalf("POST went out: %v", calls)
	}
}

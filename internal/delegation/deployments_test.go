// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package delegation

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"agw/internal/platform"
)

// A deployment is classified with its own resource, and a new one touches the image and the device
// it names (edge D1.2, D1.3); starting it is compute and asks for every single call (D1.4).
func TestClassifyDeployments(t *testing.T) {
	body := `{"edge_device_id":1,"container_image_id":5,"name":"agw-mnist","port_bindings":[{"host_port":"18190","container_port":"8000","protocol":"tcp"}]}`
	post := platform.Request{Method: http.MethodPost, Path: "/container-deployments", Body: json.RawMessage(body)}
	a := Classify(post)
	if a.Action != Create || a.Resource != ContainerDeployment || a.ID != "" {
		t.Fatalf("create: %+v", a)
	}
	if a.String() != "create container_deployment + read container_image 5 + run edge_device 1" {
		t.Fatalf("create touches %s", a)
	}
	if a.Effect != EffectWrite || a.Problem != "" || a.ConfirmEach {
		t.Fatalf("create: effect %q problem %q confirmEach %v", a.Effect, a.Problem, a.ConfirmEach)
	}

	a = Classify(platform.Request{Method: http.MethodGet, Path: "/container-deployments"})
	if a.String() != "read container_deployment" || a.Effect != EffectRead {
		t.Fatalf("list: %s (%q)", a, a.Effect)
	}
	a = Classify(platform.Request{Method: http.MethodGet, Path: "/container-deployments/5"})
	if a.String() != "read container_deployment 5" || a.Effect != EffectRead {
		t.Fatalf("read one: %s (%q)", a, a.Effect)
	}

	// deploy: compute, and it asks for every single call, also in a plan and with confirm "none".
	a = Classify(platform.Request{Method: http.MethodPut, Path: "/container-deployments/5/deploy"})
	if a.Action != Run || a.Resource != ContainerDeployment || a.ID != "5" {
		t.Fatalf("deploy: %+v", a)
	}
	if a.Effect != EffectCompute || !a.ConfirmEach {
		t.Fatalf("deploy: effect %q confirmEach %v", a.Effect, a.ConfirmEach)
	}
	// undeploy frees the device: write, no asking of its own.
	a = Classify(platform.Request{Method: http.MethodPut, Path: "/container-deployments/5/undeploy"})
	if a.Action != Run || a.ID != "5" || a.Effect != EffectWrite || a.ConfirmEach {
		t.Fatalf("undeploy: %+v", a)
	}
	a = Classify(platform.Request{Method: http.MethodDelete, Path: "/container-deployments/5"})
	if a.Action != Delete || a.Resource != ContainerDeployment || a.ID != "5" || a.Effect != EffectIrreversible {
		t.Fatalf("delete: %+v", a)
	}
	// A path of another resource is not caught by these routes.
	if a := Classify(platform.Request{Method: http.MethodGet, Path: "/container-images"}); a.Resource != ContainerImage {
		t.Fatalf("neighbour: %s", a)
	}
}

// Name and ports of a new deployment are checked for every single binding, with and without a
// delegation (edge D1.5, D1.6).
func TestDeploymentFieldsAndPorts(t *testing.T) {
	build := func(body string) platform.Request {
		return platform.Request{Method: http.MethodPost, Path: "/container-deployments", Body: json.RawMessage(body)}
	}
	binding := func(host, container, protocol string) string {
		return `{"host_port":` + host + `,"container_port":` + container + `,"protocol":"` + protocol + `"}`
	}
	ok := func(bindings, name string) string {
		return `{"edge_device_id":1,"container_image_id":5,"name":"` + name + `","port_bindings":[` + bindings + `]}`
	}
	cases := []struct {
		name, body, want string
	}{
		{"valid", ok(binding(`"18190"`, `"8000"`, "tcp"), "agw-mnist"), ""},
		{"ports as numbers", ok(binding("18100", "8002", "tcp"), "agw-mnist"), ""},
		{"three bindings", ok(strings.Join([]string{binding(`"18190"`, `"8000"`, "tcp"), binding(`"18191"`, `"8001"`, "tcp"), binding(`"18192"`, `"8002"`, "tcp")}, ","), "agw-a"), ""},
		{"host port below the range", ok(binding(`"18099"`, `"8000"`, "tcp"), "agw-mnist"), MsgDeploymentPorts},
		{"host port above the range", ok(binding(`"18200"`, `"8000"`, "tcp"), "agw-mnist"), MsgDeploymentPorts},
		{"host port 22", ok(binding(`"22"`, `"8000"`, "tcp"), "agw-mnist"), MsgDeploymentPorts},
		{"container port 8888", ok(binding(`"18190"`, `"8888"`, "tcp"), "agw-mnist"), MsgDeploymentPorts},
		{"container port 22", ok(binding(`"18190"`, `"22"`, "tcp"), "agw-mnist"), MsgDeploymentPorts},
		{"protocol udp", ok(binding(`"18190"`, `"8000"`, "udp"), "agw-mnist"), MsgDeploymentPorts},
		{"four bindings", ok(strings.Join([]string{binding(`"18190"`, `"8000"`, "tcp"), binding(`"18191"`, `"8001"`, "tcp"), binding(`"18192"`, `"8002"`, "tcp"), binding(`"18193"`, `"8000"`, "tcp")}, ","), "agw-mnist"), MsgDeploymentPorts},
		{"no binding", ok("", "agw-mnist"), MsgDeploymentPorts},
		{"the same host port twice", ok(binding(`"18190"`, `"8000"`, "tcp")+","+binding(`"18190"`, `"8001"`, "tcp"), "agw-mnist"), MsgDeploymentPorts},
		{"port written as 18100.0", ok(binding(`"18100.0"`, `"8000"`, "tcp"), "agw-mnist"), MsgDeploymentPorts},
		{"port as a truth value", ok(binding("true", `"8000"`, "tcp"), "agw-mnist"), MsgDeploymentPorts},
		{"binding without protocol", `{"edge_device_id":1,"container_image_id":5,"name":"agw-mnist","port_bindings":[{"host_port":"18190","container_port":"8000"}]}`, MsgDeploymentPorts},
		{"port_bindings is no list", `{"edge_device_id":1,"container_image_id":5,"name":"agw-mnist","port_bindings":{"host_port":"18190"}}`, MsgDeploymentPorts},
		{"name without the prefix", ok(binding(`"18190"`, `"8000"`, "tcp"), "mnist-triton"), MsgDeploymentName},
		{"name in upper case", ok(binding(`"18190"`, `"8000"`, "tcp"), "agw-MNIST"), MsgDeploymentName},
		{"name with a slash", ok(binding(`"18190"`, `"8000"`, "tcp"), "agw-a/b"), MsgDeploymentName},
		{"name too long", ok(binding(`"18190"`, `"8000"`, "tcp"), "agw-"+strings.Repeat("a", 59)), MsgDeploymentName},
		{"name only of the prefix", ok(binding(`"18190"`, `"8000"`, "tcp"), "agw-"), MsgDeploymentName},
		{"name twice in the body", `{"edge_device_id":1,"container_image_id":5,"name":"agw-mnist","name":"agw-other","port_bindings":[` + binding(`"18190"`, `"8000"`, "tcp") + `]}`, MsgDeploymentName},
		{"port_bindings twice in the body", `{"edge_device_id":1,"container_image_id":5,"name":"agw-mnist","port_bindings":[` + binding(`"18190"`, `"8000"`, "tcp") + `],"port_bindings":[` + binding(`"22"`, `"8000"`, "tcp") + `]}`, MsgDeploymentPorts},
		{"name missing", `{"edge_device_id":1,"container_image_id":5,"port_bindings":[` + binding(`"18190"`, `"8000"`, "tcp") + `]}`, "fields missing (the platform would set them to null): name"},
		{"field of its own", `{"edge_device_id":1,"container_image_id":5,"name":"agw-mnist","port_bindings":[` + binding(`"18190"`, `"8000"`, "tcp") + `],"env":{"A":"1"}}`, `field "env" not allowed`},
	}
	for _, c := range cases {
		got := Classify(build(c.body)).Problem
		if c.want == "" {
			if got != "" {
				t.Errorf("%s: refused with %q", c.name, got)
			}
			continue
		}
		if !strings.Contains(got, c.want) {
			t.Errorf("%s: problem %q, want it to contain %q", c.name, got, c.want)
		}
	}
}

// A container deployment is attributed from the answer of its creation and from the Location of the
// deploy (edge D1.4); the other resources keep their rule.
func TestCreatedDeployment(t *testing.T) {
	if r, id, ok := Created(Access{Action: Create, Resource: ContainerDeployment},
		platform.Result{Status: "ok", HTTPStatus: 201, Body: `{"id":7,"name":"agw-mnist","status":"created"}`}); !ok || r != ContainerDeployment || id != "7" {
		t.Fatalf("create: %q %q %v", r, id, ok)
	}
	deploy := platform.Result{Status: "ok", HTTPStatus: 202, Location: "/tasks/12"}
	if r, id, ok := Created(Access{Action: Run, Resource: ContainerDeployment, ID: "7"}, deploy); !ok || r != Task || id != "12" {
		t.Fatalf("run: %q %q %v", r, id, ok)
	}
	if r, id, ok := Created(Access{Action: Delete, Resource: ContainerDeployment, ID: "7"}, deploy); ok {
		t.Fatalf("delete created %q %q", r, id)
	}
	// A failed call creates nothing, and a task needs the Location.
	if _, _, ok := Created(Access{Action: Create, Resource: ContainerDeployment}, platform.Result{Status: "error", HTTPStatus: 500}); ok {
		t.Error("an error answer was attributed")
	}
	if _, _, ok := Created(Access{Action: Run, Resource: ContainerDeployment, ID: "7"}, platform.Result{Status: "ok", HTTPStatus: 202}); ok {
		t.Error("a run without Location was attributed")
	}
	// The other resources are unchanged.
	if r, id, ok := Created(Access{Action: Create, Resource: Training}, platform.Result{Status: "ok", HTTPStatus: 202, Location: "/tasks/3"}); !ok || r != Task || id != "3" {
		t.Fatalf("training: %q %q %v", r, id, ok)
	}
	if r, id, ok := Created(Access{Action: Create, Resource: Dataset}, platform.Result{Status: "ok", HTTPStatus: 201, Body: `{"id":4}`}); !ok || r != Dataset || id != "4" {
		t.Fatalf("dataset: %q %q %v", r, id, ok)
	}
}

// The image of a build is attributed from the body the gateway sent, never from the answer (edge
// D1.9).
func TestCreatedImage(t *testing.T) {
	req := platform.Request{Method: http.MethodPost, Path: "/container-images/build",
		Body: json.RawMessage(`{"container_template_id":2,"model_id":5,"repository":"test/mnist-triton","tag":"v1","architecture":"amd64"}`)}
	ok := platform.Result{Status: "ok", HTTPStatus: 201, Body: `{"id":9,"repository":"someone/else","tag":"v9"}`}
	ref, got := CreatedImage(Access{Action: Create, Resource: ContainerImage}, req, ok)
	if !got || ref != "test/mnist-triton:v1" {
		t.Fatalf("build: %q %v", ref, got)
	}
	// A read is not a build, an error answer attributes nothing, and a body without the reference does not either.
	if _, got := CreatedImage(Access{Action: Read, Resource: ContainerImage}, req, ok); got {
		t.Error("a read was attributed as a build")
	}
	if _, got := CreatedImage(Access{Action: Create, Resource: ContainerImage}, req, platform.Result{Status: "error", HTTPStatus: 500}); got {
		t.Error("an error answer was attributed")
	}
	empty := platform.Request{Method: http.MethodPost, Path: "/container-images/build", Body: json.RawMessage(`{}`)}
	if _, got := CreatedImage(Access{Action: Create, Resource: ContainerImage}, empty, ok); got {
		t.Error("a body without repository and tag was attributed")
	}
}

// Reading an image under its reference gives the numbers the platform assigned it (edge D1.9); only a
// read of one image does, not the read of the list.
func TestImageRead(t *testing.T) {
	a := Access{Action: Read, Resource: ContainerImage, ID: "test/mnist-triton:v1"}
	res := platform.Result{Status: "ok", HTTPStatus: 200, Body: `[{"id":5,"repository":"test/mnist-triton","tag":"v1"},{"id":6,"repository":"test/mnist-triton","tag":"v1"}]`}
	ref, ids, got := ImageRead(a, res)
	if !got || ref != "test/mnist-triton:v1" || strings.Join(ids, ",") != "5,6" {
		t.Fatalf("read: %q %v %v", ref, ids, got)
	}
	for _, c := range []struct {
		name string
		a    Access
		res  platform.Result
	}{
		{"list read", Access{Action: Read, Resource: ContainerImage}, res},
		{"error answer", a, platform.Result{Status: "error", HTTPStatus: 404}},
		{"answer without identifiers", a, platform.Result{Status: "ok", HTTPStatus: 200, Body: `[{"repository":"test/x"}]`}},
		{"answer is no list", a, platform.Result{Status: "ok", HTTPStatus: 200, Body: `{"id":5}`}},
	} {
		if _, _, got := ImageRead(c.a, c.res); got {
			t.Errorf("%s: attributed", c.name)
		}
	}
}

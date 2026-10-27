// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package delegation

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"agw/internal/platform"
)

// imageRules allows creating images, reading model 5 and every template; no other model.
const imageRules = `{"rules": [
  {"action": "create", "resource": "container_image"},
  {"action": "read",   "resource": "model", "ids": ["5"]},
  {"action": "read",   "resource": "container_template", "ids": ["*"]}
]}`

func TestImageBuildClassify(t *testing.T) {
	build := func(body string) platform.Request {
		return platform.Request{Method: http.MethodPost, Path: "/container-images/build", Body: json.RawMessage(body)}
	}
	a := Classify(build(`{"container_template_id":2,"model_id":5,"repository":"agent-test/mnist-triton","tag":"v1","architecture":"amd64"}`))
	if a.String() != "create container_image + read model 5 + read container_template 2" || a.Problem != "" {
		t.Fatalf("build: %s problem=%q", a, a.Problem)
	}
	a = Classify(build(`{"container_template_id":2,"model_id":5,"repository":"agent-test/x","tag":"v1","architecture":"amd64","edge_device_id":3}`))
	if !strings.Contains(a.Problem, `field "edge_device_id" not allowed`) {
		t.Fatalf("edge_device_id not refused: %+v", a)
	}
	a = Classify(build(`{"repository":"agent-test/x","tag":"v1","architecture":"amd64"}`))
	if a.String() != "create container_image + read model ? + read container_template ?" {
		t.Fatalf("build without ids: %s", a)
	}
	if a := Classify(platform.Request{Method: http.MethodGet, Path: "/inference-container-templates"}); a.String() != "read container_template" {
		t.Fatalf("list templates: %s", a)
	}
	if a := Classify(platform.Request{Method: http.MethodPost, Path: "/inference-container-templates/create/git"}); a.String() != "create container_template" {
		t.Fatalf("create/git: %s", a)
	}
	if a := Classify(platform.Request{Method: http.MethodGet, Path: "/container-images/agent-test/mnist-triton:v1"}); a.String() != "read container_image agent-test/mnist-triton:v1" {
		t.Fatalf("image status: %s", a)
	}
	if _, err := Parse([]byte(`{"rules": [{"action": "read", "resource": "container_template"}]}`)); err != nil {
		t.Fatalf("container_template not accepted: %v", err)
	}
}

// Building an image is compute (issue #67, step 6): it runs for minutes on the platform. Reading the
// templates stays read, creating a template from git stays write.
func TestImageBuildEffect(t *testing.T) {
	build := platform.Request{Method: http.MethodPost, Path: "/container-images/build",
		Body: json.RawMessage(`{"container_template_id":2,"model_id":5,"repository":"agent-test/x","tag":"v1","architecture":"amd64"}`)}
	if a := Classify(build); a.Effect != EffectCompute {
		t.Fatalf("build: effect %q, want compute", a.Effect)
	}
	if a := Classify(platform.Request{Method: http.MethodGet, Path: "/inference-container-templates"}); a.Effect != EffectRead {
		t.Fatalf("list templates: effect %q, want read", a.Effect)
	}
	if a := Classify(platform.Request{Method: http.MethodPost, Path: "/inference-container-templates/create/git"}); a.Effect != EffectWrite {
		t.Fatalf("create/git: effect %q, want write", a.Effect)
	}
}

func TestImageBuildConformance(t *testing.T) {
	noTemplate := `{"rules": [
	  {"action": "create", "resource": "container_image"},
	  {"action": "read",   "resource": "model", "ids": ["5"]}
	]}`
	cases := []Case{
		{Name: "build from model 5", Req: platform.Request{Method: http.MethodPost, Path: "/container-images/build",
			Body: json.RawMessage(`{"container_template_id":2,"model_id":5,"repository":"agent-test/x","tag":"v1","architecture":"amd64"}`)},
			Rules: imageRules, Allowed: true},
		{Name: "build from foreign model 6", Req: platform.Request{Method: http.MethodPost, Path: "/container-images/build",
			Body: json.RawMessage(`{"container_template_id":2,"model_id":6,"repository":"agent-test/x","tag":"v1","architecture":"amd64"}`)},
			Rules: imageRules, Allowed: false},
		{Name: "build without right on template", Req: platform.Request{Method: http.MethodPost, Path: "/container-images/build",
			Body: json.RawMessage(`{"container_template_id":2,"model_id":5,"repository":"agent-test/x","tag":"v1","architecture":"amd64"}`)},
			Rules: noTemplate, Allowed: false},
		{Name: "tool build from model 5", Tool: "build_inference_image",
			Args: `{"template_id":2,"model_id":5,"name":"mnist-triton","tag":"v1","architecture":"amd64"}`, Rules: imageRules, Allowed: true},
		{Name: "tool build from model 6", Tool: "build_inference_image",
			Args: `{"template_id":2,"model_id":6,"name":"mnist-triton","tag":"v1","architecture":"amd64"}`, Rules: imageRules, Allowed: false},
		{Name: "tool list templates", Tool: "list_templates", Rules: imageRules, Allowed: true},
		{Name: "tool import template without create right", Tool: "import_template",
			Args: `{"name":"triton-inference"}`, Rules: imageRules, Allowed: false},
		{Name: "create/git with foreign URL despite right", Req: platform.Request{Method: http.MethodPost,
			Path: "/inference-container-templates/create/git", Form: map[string][]string{
				"name": {"x"}, "git_url": {"https://example.com/other.git"}, "git_ref": {"main"}}},
			Rules: `{"rules": [{"action": "create", "resource": "container_template"}]}`, Allowed: false},
	}
	for _, c := range cases {
		got, why := Evaluate(c, time.Now())
		if got != c.Allowed {
			t.Errorf("%s: allowed=%v, expected %v (%s)", c.Name, got, c.Allowed, why)
		}
	}
}

// #230: the import of a template names the new template, so a plan step "import_template" resolves
// {{stepN.id}} for the build that follows (Phase B: the build fell out of the plan as a single approval).
func TestCreatedContainerTemplate(t *testing.T) {
	a := Classify(platform.Request{Method: http.MethodPost, Path: "/inference-container-templates/create/git",
		Form: map[string][]string{"name": {"ovms-inference"}, "git_url": {"https://github.com/pschroee/agri-gaia-inference-container-template"}, "git_ref": {"ovms-onnx"}}})
	if a.Action != Create || a.Resource != ContainerTemplate {
		t.Fatalf("classify: %+v", a)
	}
	res := platform.Result{Status: "ok", HTTPStatus: 200, Body: `{"id":2,"name":"ovms-inference","git_ref":"ovms-onnx","source":"git"}`}
	if r, id, ok := Created(a, res); !ok || r != ContainerTemplate || id != "2" {
		t.Fatalf("created: %q %q %v", r, id, ok)
	}
	if _, _, ok := Created(a, platform.Result{Status: "error", HTTPStatus: 409}); ok {
		t.Error("a failed import was attributed")
	}
}

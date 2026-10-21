// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"

	"agw/internal/toolset"
)

func TestToolFormsPerBinding(t *testing.T) {
	for _, c := range []struct {
		tool, cli, mcp, api string
	}{
		{"assess_status", "agw-platform assess-status", "mcp_platform_assess_status", "GET /_agw/assess/checks/{check_id}"},
		{"start_training", "agw-platform start-training", "mcp_platform_start_training", "POST /train/containers/{train_container_id}/run"},
		{"search_next", "agw-platform search-next", "mcp_platform_search_next", "POST /_agw/search/{search_id}/trials"},
		{"notebook_fetch", "agw-platform notebook-fetch", "mcp_platform_notebook_fetch", ""},
		{"upload_model", "agw-platform upload-model", "mcp_platform_upload_model", ""},
		{"request", "agw-platform request", "mcp_platform_request", ""},
	} {
		tool, ok := toolByName(c.tool)
		if !ok {
			t.Fatalf("no tool %s", c.tool)
		}
		for b, want := range map[toolset.Binding]string{toolset.CLI: c.cli, toolset.MCP: c.mcp, toolset.API: c.api} {
			got, ok := tool.Form(b)
			if got != want || ok != (want != "") {
				t.Errorf("%s in %s: %q %v, want %q", c.tool, b, got, ok, want)
			}
		}
	}
}

func TestRenderChoosesForm(t *testing.T) {
	cliMCP := toolset.Of(toolset.CLI, toolset.MCP)
	cliAPI := toolset.Of(toolset.CLI, toolset.API)
	all := toolset.Of(toolset.CLI, toolset.MCP, toolset.API)
	for _, c := range []struct {
		name string
		in   string
		via  toolset.Binding
		set  toolset.Set
		want string
	}{
		{"via wins", "see {{tool:assess_status}}.", toolset.MCP, all, "see mcp_platform_assess_status."},
		{"api via", "{{tool:start_training}}", toolset.API, all, "POST /train/containers/{train_container_id}/run"},
		{"no call: all forms", "{{tool:assess_status}}", "", cliMCP, "agw-platform assess-status or mcp_platform_assess_status"},
		{"no call, three bindings", "{{tool:assess_status}}", "", all,
			"agw-platform assess-status or mcp_platform_assess_status or GET /_agw/assess/checks/{check_id}"},
		{"via lacks the form: the others", "{{tool:notebook_fetch}}", toolset.API, cliAPI, "agw-platform notebook-fetch"},
		{"nowhere offered", "{{tool:notebook_fetch}}", toolset.API, toolset.Of(toolset.API), "notebook_fetch (not offered in this setup)"},
		{"two markers", "{{tool:clone_training}} then {{tool:start_training}}", toolset.CLI, all, "agw-platform clone-training then agw-platform start-training"},
		{"unknown marker stays", "{{tool:no_such_tool}}", toolset.CLI, all, "{{tool:no_such_tool}}"},
		{"no marker is identical", "plain text {{step1.id}}", toolset.CLI, all, "plain text {{step1.id}}"},
	} {
		if got := Render(c.in, c.via, c.set); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func TestRenderKeepsJSONValid(t *testing.T) {
	body := `{"next":"call {{tool:start_training}} with {{tool:clone_training}}","n":1}`
	for _, b := range []toolset.Binding{toolset.CLI, toolset.MCP, toolset.API} {
		got := Render(body, b, toolset.Of(b))
		if !json.Valid([]byte(got)) || strings.Contains(got, "{{tool:") {
			t.Errorf("%s: %s", b, got)
		}
	}
	r := Result{Message: "m {{tool:rights}}", Note: "n {{tool:rights}}", Body: body}.RenderedFor(toolset.CLI, toolset.Of(toolset.CLI))
	if strings.Contains(r.Message+r.Note+r.Body, "{{tool:") || !json.Valid([]byte(r.Body)) {
		t.Errorf("result: %+v", r)
	}
}

// Tools without a REST form, with the reason.
var noAPIForm = map[string]string{
	"upload_dataset": "multipart upload; the REST binding passes JSON only",
	"upload_model":   "multipart upload; the REST binding passes JSON only",
	"request":        "this is platform_http itself",
}

func TestEveryToolHasAPIForm(t *testing.T) {
	for _, tool := range Tools {
		_, ok := tool.Form(toolset.API)
		reason, excused := noAPIForm[tool.Name]
		if strings.HasPrefix(tool.Name, "notebook_") {
			excused = true // files of the sandbox
		}
		switch {
		case !ok && !excused:
			t.Errorf("%s has no REST form and no reason", tool.Name)
		case ok && reason != "":
			t.Errorf("%s has a REST form but is listed as having none", tool.Name)
		}
	}
	for name := range apiRefs {
		if _, ok := toolByName(name); !ok {
			t.Errorf("apiRefs names the unknown tool %s", name)
		}
		if toolOf, _ := toolByName(name); toolOf.RESTCall() != nil {
			t.Errorf("%s is in both tables", name)
		}
	}
}

// argsFor gives sample arguments for the tools in apiRefs.
var argsFor = map[string]string{
	"list_datasets": `{}`, "get_dataset": `{"dataset_id":5}`, "rename_dataset": `{"dataset_id":5,"name":"x"}`,
	"list_models": `{}`, "get_model": `{"model_id":7}`, "validate_model": `{"model_id":7}`, "update_model": `{"model_id":7,"name":"x"}`,
	"train_options":  `{"provider":"Torchvision","architecture":"ResNet"}`,
	"list_trainings": `{}`, "get_training": `{"train_container_id":3}`, "training_status": `{"train_container_id":3}`,
	"training_logs":   `{"train_container_id":3}`,
	"create_training": `{"provider":"Torchvision","architecture":"ResNet","category":"Classification","dataset_id":5,"train_config":{"epochs":1}}`,
	"start_training":  `{"train_container_id":3}`, "clone_training": `{"container_id":3,"train_config":{"epochs":1}}`,
	"create_model_from_training": `{"train_container_id":3}`, "list_tasks": `{}`, "task_status": `{"task_id":4}`,
	"list_container_images":  `{}`,
	"container_image_status": `{"repository":"test/mnist","tag":"v1"}`,
	"build_inference_image":  `{"template_id":1,"model_id":7,"name":"mnist","tag":"v1","architecture":"amd64"}`,
	"list_templates":         `{}`, "import_template": `{"name":"x"}`, "list_edge_devices": `{}`, "list_deployments": `{}`,
	"create_deployment": `{"device_id":1,"repository":"test/mnist","tag":"v1","name":"d","host_port":18100}`,
	"start_deployment":  `{"deployment_id":9}`, "undeploy": `{"deployment_id":9}`, "delete_deployment": `{"deployment_id":9}`,
	"deployment_status": `{"deployment_id":9}`,
}

// builtDiffers: tools whose built request is not the one the tool sends, with the reason.
var builtDiffers = map[string]string{
	"get_training": "GET /train/containers/{id} answers 500; the tool reads the list",
}

func TestAPIRefMatchesBuild(t *testing.T) {
	for name, ref := range apiRefs {
		args, ok := argsFor[name]
		if !ok {
			t.Errorf("no sample arguments for %s", name)
			continue
		}
		tool, _ := toolByName(name)
		req, err := tool.Build(json.RawMessage(args))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if _, skip := builtDiffers[name]; skip {
			continue
		}
		method, path, _ := strings.Cut(ref, " ")
		if req.Method != method || !pathMatches(path, req.Path) {
			t.Errorf("%s: APIRef %q, Build gives %s %s", name, ref, req.Method, req.Path)
		}
	}
	for name := range apiRefs {
		if _, ok := argsFor[name]; !ok {
			t.Errorf("%s lacks sample arguments", name)
		}
	}
	for _, tool := range Tools {
		if tool.RESTCall() == nil && tool.APIRef() == "" {
			continue
		}
		if tool.RESTCall() == nil {
			if _, ok := argsFor[tool.Name]; !ok {
				t.Errorf("tool %s has an APIRef but no sample", tool.Name)
			}
		}
	}
}

// pathMatches compares a path pattern with {placeholders} to a built path: a placeholder matches one or
// more characters up to the next fixed part of the pattern (so {repository_first}/{repository_second}:{image_tag}
// matches test/mnist:v1).
func pathMatches(pattern, path string) bool {
	re := regexp.QuoteMeta(pattern)
	re = regexp.MustCompile(`\\\{[^}]+\\\}`).ReplaceAllString(re, `[^/:]+`)
	return regexp.MustCompile("^" + re + "$").MatchString(path)
}

func TestAPIRefInOpenAPI(t *testing.T) {
	raw, err := os.ReadFile("testdata/openapi-2026-10-21.json")
	if err != nil {
		t.Fatal(err)
	}
	digest, err := openAPIDigest(raw, "")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(digest), "\n")
	for name, ref := range apiRefs {
		found := false
		for _, l := range lines {
			if strings.HasPrefix(l, ref+" ") {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s: %q is not an operation of the OpenAPI description", name, ref)
		}
	}
}

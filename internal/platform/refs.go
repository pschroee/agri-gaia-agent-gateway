// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

import (
	"regexp"
	"strings"

	"agw/internal/toolset"
)

// Tool references (#200). Texts for the agent name a tool by the marker {{tool:<Name>}} (Name = Tool.Name);
// the marker is replaced where the binding is known, in the form the agent calls the tool in there:
//
//	CLI  agw-platform <CLI>
//	MCP  mcp_platform_<Name>
//	API  METHOD /path (the gateway's own paths from RESTCall, the platform's from apiRefs)
//
// A text for the agent never carries a bare tool name: the same tool has a different name in each binding.

// ToolRef is the marker of the tool in a text.
func ToolRef(name string) string { return "{{tool:" + name + "}}" }

var toolRefRe = regexp.MustCompile(`\{\{tool:([a-z0-9_]+)\}\}`)

// apiRefs is the REST form of the platform's tools, "METHOD /path" in the spelling of the platform's OpenAPI
// description, by tool name. The gateway's own tools are not in it (RESTCall has them). Not in either table:
// the notebook tools (they work on files of the sandbox), upload_dataset and upload_model (multipart, the REST
// binding passes JSON only) and request, which is platform_http itself; the tests list the reasons.
var apiRefs = map[string]string{
	"list_datasets":              "GET /datasets",
	"get_dataset":                "GET /datasets/{dataset_id}",
	"rename_dataset":             "PATCH /datasets/{dataset_id}",
	"list_models":                "GET /models",
	"get_model":                  "GET /models/{model_id}",
	"validate_model":             "GET /models/{model_id}",
	"update_model":               "PATCH /models/{model_id}",
	"train_options":              "GET /train/config/{provider}/{architecture}",
	"list_trainings":             "GET /train/containers",
	"get_training":               "GET /train/containers", // GET /train/containers/{id} answers 500; the tool reads the list
	"training_status":            "GET /train/containers/{train_container_id}/status",
	"training_logs":              "GET /train/containers/{train_container_id}/logs",
	"create_training":            "POST /train/config",
	"start_training":             "POST /train/containers/{train_container_id}/run",
	"clone_training":             "POST /train/containers/{train_container_id}/clone",
	"create_model_from_training": "GET /train/containers/{train_container_id}/model",
	"list_tasks":                 "GET /tasks",
	"task_status":                "GET /tasks/{task_id}",
	"list_container_images":      "GET /container-images",
	"container_image_status":     "GET /container-images/{repository_first}/{repository_second}:{image_tag}",
	"build_inference_image":      "POST /container-images/build",
	"list_templates":             "GET /inference-container-templates",
	"import_template":            "POST /inference-container-templates/create/git",
	"list_edge_devices":          "GET /edge-devices",
	"list_deployments":           "GET /container-deployments",
	"create_deployment":          "POST /container-deployments",
	"start_deployment":           "PUT /container-deployments/{container_deployment_id}/deploy",
	"undeploy":                   "PUT /container-deployments/{container_deployment_id}/undeploy",
	"delete_deployment":          "DELETE /container-deployments/{container_deployment_id}",
	"deployment_status":          "GET /container-deployments/{container_deployment_id}",
}

// APIRef is the REST form of the tool without the gateway's own paths: "METHOD /path", empty if the tool has none.
func (t Tool) APIRef() string { return apiRefs[t.Name] }

// Form is how the agent calls the tool in the binding; false if the binding does not offer it (the REST
// binding has no notebook tools, no uploads and no raw request tool).
func (t Tool) Form(b toolset.Binding) (string, bool) {
	switch b {
	case toolset.CLI:
		return "agw-platform " + t.CLI, true
	case toolset.MCP:
		return "mcp_platform_" + t.Name, true
	case toolset.API:
		if c := t.RESTCall(); c != nil {
			return c.Method + " " + c.Path, true
		}
		if r := t.APIRef(); r != "" {
			return r, true
		}
	}
	return "", false
}

// formIn is the text a reference to the tool becomes. via is the binding of the call the text answers ("" if
// none): its form wins. If it lacks one, the forms of the chat's other bindings follow in the order cli, mcp,
// api, joined by " or ". If no binding of the chat offers the tool, the name stays with a hint.
func (t Tool) formIn(via toolset.Binding, set toolset.Set) string {
	if via != "" {
		if f, ok := t.Form(via); ok {
			return f
		}
	}
	var forms []string
	for _, b := range set.Bindings() {
		if b == via {
			continue
		}
		if f, ok := t.Form(b); ok {
			forms = append(forms, f)
		}
	}
	if len(forms) == 0 {
		return t.Name + " (not offered in this setup)"
	}
	return strings.Join(forms, " or ")
}

// toolByName finds a tool by Tool.Name only (Lookup also takes CLI names).
func toolByName(name string) (Tool, bool) {
	for _, t := range Tools {
		if t.Name == name {
			return t, true
		}
	}
	return Tool{}, false
}

// Render replaces the tool references in s. via is the binding of the call the text answers (empty if the
// text belongs to no call); set the bindings of the chat. A marker that names no tool stays as it is.
func Render(s string, via toolset.Binding, set toolset.Set) string {
	if !strings.Contains(s, "{{tool:") {
		return s
	}
	return toolRefRe.ReplaceAllStringFunc(s, func(m string) string {
		t, ok := toolByName(toolRefRe.FindStringSubmatch(m)[1])
		if !ok {
			return m
		}
		return t.formIn(via, set)
	})
}

// RenderedFor is a copy of the tool with the references in its description and its parameters' descriptions
// replaced (the lists of MCP and of the command line help).
func (t Tool) RenderedFor(via toolset.Binding, set toolset.Set) Tool {
	t.Desc = Render(t.Desc, via, set)
	ps := make([]Param, len(t.Params))
	copy(ps, t.Params)
	for i := range ps {
		ps[i].Desc = Render(ps[i].Desc, via, set)
	}
	t.Params = ps
	return t
}

// RenderedFor is a copy of the result with the references in its message, note and body replaced. Only a
// fixed marker becomes a fixed text without quotes or backslashes, so a JSON body stays valid.
func (r Result) RenderedFor(via toolset.Binding, set toolset.Set) Result {
	r.Message = Render(r.Message, via, set)
	r.Note = Render(r.Note, via, set)
	r.Body = Render(r.Body, via, set)
	return r
}

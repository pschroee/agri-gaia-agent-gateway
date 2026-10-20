// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// The tools for inference container images and their templates (issue #67) live in their own file;
// init appends them, so that other tools can be added in parallel.
func init() {
	Tools = append(Tools, listTemplates, importTemplate, validateModel, buildInferenceImage, containerImageStatus)
}

// AllowedTemplateGitURL is the one repository the agent may import a template from (exact match).
const AllowedTemplateGitURL = "https://github.com/hsos-ai-lab/agri-gaia-inference-container-template"

// Checks of the arguments that go into paths and bodies; the platform accepts more, these are the
// fixed limits of this binding.
var (
	templateNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 _.-]{0,63}$`)
	gitRefRe       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_./-]{0,99}$`)
	imageNameRe    = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*$`)
	repositoryRe   = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*/[a-z0-9]+(?:[._-][a-z0-9]+)*$`)
	tagRe          = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
	// TritonNameRe is the shape of a name Triton can address over HTTP. It is exported because the
	// deployment check (R3) puts a model name into a command that runs on the device and checks it with
	// the same expression.
	TritonNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)
)

const templatesPath = "/inference-container-templates"

// inferenceTemplate is the part of an installed template that the tools read.
type inferenceTemplate struct {
	ID     json.Number `json:"id"`
	Name   string      `json:"name"`
	GitURL string      `json:"git_url"`
	GitRef string      `json:"git_ref"`
}

var listTemplates = Tool{
	Name: "list_templates", CLI: "templates",
	Desc:   "Lists the inference container templates installed on the platform (id, name, source, git_url, git_ref). build_inference_image needs the id of a template; import_template fetches the official one.",
	Params: pagingParams,
	build:  get(templatesPath),
}

var importTemplate = Tool{
	Name: "import_template", CLI: "import-template",
	Desc: "Imports the official inference container template from its one allowed repository (" + AllowedTemplateGitURL + "; branch triton holds the Triton template, main none). " +
		"Refuses if a template from that URL and branch is installed already. Other repositories, access tokens, changing and deleting templates are blocked. Writes; the user must approve.",
	Params: []Param{
		{Name: "name", Type: "string", Desc: "name of the new template, e.g. triton-inference", Required: true},
		{Name: "git_ref", Type: "string", Desc: "branch or tag of the template repository (default triton)"},
	},
	Write: true,
	build: func(a Args) (Request, error) {
		name, err := requiredStr(a, "name")
		if err != nil {
			return Request{}, err
		}
		if !templateNameRe.MatchString(name) {
			return Request{}, fmt.Errorf("name %q is not valid (letters, digits, space, _ . -; at most 64 characters)", name)
		}
		ref, err := gitRef(a)
		if err != nil {
			return Request{}, err
		}
		return Request{Method: http.MethodPost, Path: templatesPath + "/create/git", Form: map[string][]string{
			"name":        {name},
			"description": {"Imported by the agent from " + AllowedTemplateGitURL + " (" + ref + ")"},
			"git_url":     {AllowedTemplateGitURL},
			"git_ref":     {ref},
		}}, nil
	},
	steps: importTemplateSteps,
}

// gitRef reads the branch of the template repository; triton is the default.
func gitRef(a Args) (string, error) {
	ref, ok, err := a.str("git_ref")
	if err != nil {
		return "", err
	}
	if !ok {
		return "triton", nil
	}
	if !gitRefRe.MatchString(ref) || strings.Contains(ref, "..") {
		return "", fmt.Errorf("git_ref %q is not valid", ref)
	}
	return ref, nil
}

// importTemplateSteps refuses a second copy of the same template; otherwise it sends the import.
func importTemplateSteps(ctx context.Context, a Args, final Request, do Doer) (Result, string, error) {
	ref, err := gitRef(a)
	if err != nil {
		return Refuse("%s", err.Error())
	}
	var installed []inferenceTemplate
	if res, stop, err := ReadJSON(ctx, do, Request{Method: http.MethodGet, Path: templatesPath, Query: map[string]string{"limit": "1000"}}, &installed); stop {
		return res, "", err
	}
	for _, t := range installed {
		if t.GitURL == AllowedTemplateGitURL && t.GitRef == ref {
			return Refuse("template %s (id %s) is already installed from %s at %s; use its id", t.Name, t.ID, t.GitURL, t.GitRef)
		}
	}
	res, err := do(ctx, final)
	return res, "", err
}

var validateModel = Tool{
	Name: "validate_model", CLI: "validate-model",
	Desc: "Checks whether a model can become an inference container (reads only): ONNX format, tensor metadata, a name that Triton can address over HTTP, output labels. " +
		"The answer has ready, problems and warnings. build_inference_image runs the same checks before it builds.",
	Params: []Param{{Name: "model_id", Type: "integer", Desc: "id of the model (platform_list_models)", Required: true}},
	build:  byID(http.MethodGet, "/models", "model_id", ""),
	steps:  validateModelSteps,
}

func validateModelSteps(ctx context.Context, a Args, final Request, do Doer) (Result, string, error) {
	id, _, err := a.int("model_id")
	if err != nil {
		return Refuse("%s", err.Error())
	}
	var m map[string]any
	if res, stop, err := ReadJSON(ctx, do, final, &m); stop {
		return res, "", err
	}
	problems, warnings := modelReadiness(m)
	return jsonResult(map[string]any{
		"model_id": id, "name": m["name"], "format": m["format"], "ready": len(problems) == 0,
		"problems": problems, "warnings": warnings,
	})
}

// modelReadiness lists what keeps a model from becoming an inference container (problems) and what
// the agent should look at (warnings). Both are never nil.
func modelReadiness(m map[string]any) (problems, warnings []string) {
	problems, warnings = []string{}, []string{}
	if f, _ := m["format"].(string); f != "onnx" {
		problems = append(problems, fmt.Sprintf("format is %s, not onnx: the inference container needs an ONNX export (platform_create_training with export_config)", f))
	}
	var missing []string
	for _, k := range modelTensorFields {
		if v, ok := m[k]; !ok || v == nil {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		problems = append(problems, "tensor metadata missing: "+strings.Join(missing, ", ")+" (set them with platform_update_model)")
	}
	name, _ := m["name"].(string)
	if !TritonNameRe.MatchString(name) {
		problems = append(problems, fmt.Sprintf("name %q cannot be addressed by Triton over HTTP (spaces, brackets, commas and the like); "+
			"rename the model with platform_update_model to letters, digits, _ . - only, e.g. mnist_efficientnet", name))
	}
	if strings.Contains(name, "None Acc.") {
		warnings = append(warnings, `name contains "None Acc.": the score was not recognized in the training log (check the model with platform_assess_model)`)
	}
	if labels, _ := m["output_labels"].([]any); len(labels) == 0 {
		warnings = append(warnings, "no output_labels: the service returns class indices only")
	}
	return problems, warnings
}

var buildInferenceImage = Tool{
	Name: "build_inference_image", CLI: "build-image",
	Desc: "Builds an inference container image from a model and an installed template into the platform's registry (several minutes, about 9 GB). " +
		"Before the build it checks: the model is ready (validate_model), the template is installed, the repository (the owner of the model, then /name) and <name>:<tag> is not in the registry yet " +
		"(the platform would overwrite it silently), and no build of that image is running. Writes; the user must approve. " +
		"The answer has repository, tag and the task (/tasks/<id>); follow the task with platform_task_status and then check platform_container_image_status. " +
		"An image cannot be started without an edge device.",
	Params: []Param{
		{Name: "template_id", Type: "integer", Desc: "id of the installed template (platform_list_templates)", Required: true},
		{Name: "model_id", Type: "integer", Desc: "id of the model (platform_list_models)", Required: true},
		{Name: "name", Type: "string", Desc: "name of the image: lowercase letters, digits and . _ -, e.g. mnist-triton", Required: true},
		{Name: "tag", Type: "string", Desc: "tag, e.g. v1", Required: true},
		{Name: "architecture", Type: "string", Desc: "amd64 or arm64", Required: true},
	},
	Write: true,
	build: func(a Args) (Request, error) {
		tid, err := requiredInt(a, "template_id")
		if err != nil {
			return Request{}, err
		}
		mid, err := requiredInt(a, "model_id")
		if err != nil {
			return Request{}, err
		}
		name, tag, arch, err := imageArgs(a)
		if err != nil {
			return Request{}, err
		}
		b, err := json.Marshal(map[string]any{
			"container_template_id": tid, "model_id": mid, "tag": tag, "architecture": arch,
			"repository": "OWNER/" + name, // the owner is known only after the model is read (steps)
		})
		return Request{Method: http.MethodPost, Path: "/container-images/build", Body: b}, err
	},
	steps: buildInferenceImageSteps,
}

// imageArgs reads and checks the name, tag and architecture of an image.
func imageArgs(a Args) (name, tag, arch string, err error) {
	if name, err = requiredStr(a, "name"); err != nil {
		return "", "", "", err
	}
	if !imageNameRe.MatchString(name) || len(name) > 64 {
		return "", "", "", fmt.Errorf("name %q is not valid (lowercase letters, digits and . _ -, at most 64 characters)", name)
	}
	if tag, err = requiredStr(a, "tag"); err != nil {
		return "", "", "", err
	}
	if !tagRe.MatchString(tag) {
		return "", "", "", fmt.Errorf("tag %q is not valid", tag)
	}
	if arch, err = requiredStr(a, "architecture"); err != nil {
		return "", "", "", err
	}
	if arch != "amd64" && arch != "arm64" {
		return "", "", "", fmt.Errorf("architecture %q: amd64 or arm64", arch)
	}
	return name, tag, arch, nil
}

// buildInferenceImageSteps runs the checks in a fixed order (model, template, registry, running
// builds) and sends the build with the owner's repository only when all of them pass.
func buildInferenceImageSteps(ctx context.Context, a Args, _ Request, do Doer) (Result, string, error) {
	tid, _ := requiredInt(a, "template_id") // checked by build
	mid, _ := requiredInt(a, "model_id")
	name, tag, arch, err := imageArgs(a)
	if err != nil {
		return Refuse("%s", err.Error())
	}

	var m map[string]any
	if res, stop, err := ReadJSON(ctx, do, Request{Method: http.MethodGet, Path: "/models/" + strconv.FormatInt(mid, 10)}, &m); stop {
		return res, "", err
	}
	if problems, _ := modelReadiness(m); len(problems) > 0 {
		return Refuse("model %d is not ready for an inference container: %s", mid, strings.Join(problems, "; "))
	}

	owner, _ := m["owner"].(string)
	repo := owner + "/" + name
	if !repositoryRe.MatchString(repo) {
		return Refuse("repository %q is not valid: the owner %q of model %d and the name must be lowercase letters, digits and . _ -", repo, owner, mid)
	}

	var installed []inferenceTemplate
	if res, stop, err := ReadJSON(ctx, do, Request{Method: http.MethodGet, Path: templatesPath, Query: map[string]string{"limit": "1000"}}, &installed); stop {
		return res, "", err
	}
	if !slices.ContainsFunc(installed, func(t inferenceTemplate) bool { return t.ID.String() == strconv.FormatInt(tid, 10) }) {
		return Refuse("container template %d is not installed; platform_list_templates lists them, platform_import_template imports the official one", tid)
	}

	image := repo + ":" + tag
	res, err := do(ctx, Request{Method: http.MethodGet, Path: "/container-images/" + image})
	if err != nil {
		return res, "", err
	}
	switch {
	case res.Status == "ok" && res.HTTPStatus >= 200 && res.HTTPStatus < 300:
		return Refuse("image %s is already in the registry; the platform would overwrite it without asking. Choose another tag", image)
	case res.Status == "error" && res.HTTPStatus == http.StatusNotFound:
		// free: the image is not there yet
	default:
		return res, "", nil
	}

	var tasks []struct {
		ID     json.Number `json:"id"`
		Title  string      `json:"title"`
		Status string      `json:"status"`
	}
	if res, stop, err := ReadJSON(ctx, do, Request{Method: http.MethodGet, Path: "/tasks", Query: map[string]string{"initiator": owner, "limit": "1000"}}, &tasks); stop {
		return res, "", err
	}
	title := "Container Buildjob '" + repo + "/" + tag + "'" // the backend's title; a slash, not the colon of the image
	for _, t := range tasks {
		if t.Title == title && (t.Status == "created" || t.Status == "inprogress") {
			return Refuse("a build of %s/%s is still running (task %s); wait for it instead of starting another", repo, tag, t.ID)
		}
	}

	body, err := json.Marshal(map[string]any{
		"container_template_id": tid, "model_id": mid, "tag": tag, "architecture": arch, "repository": repo,
	})
	if err != nil {
		return Result{Status: "error", Message: err.Error()}, "", nil
	}
	res, err = do(ctx, Request{Method: http.MethodPost, Path: "/container-images/build", Body: body})
	if err != nil || res.Status != "ok" || res.HTTPStatus < 200 || res.HTTPStatus >= 300 {
		return res, "", err
	}
	res.Body = jsonBody(map[string]any{"repository": repo, "tag": tag, "task": res.Location})
	return res, "", nil
}

var containerImageStatus = Tool{
	Name: "container_image_status", CLI: "image-status",
	Desc: "Checks whether an inference image is in the platform's registry (reads only). While a build runs, present is false; afterwards present is true and entries holds the registry data.",
	Params: []Param{
		{Name: "repository", Type: "string", Desc: "repository <owner>/<name>, e.g. agent-test/mnist-triton", Required: true},
		{Name: "tag", Type: "string", Desc: "tag, e.g. v1", Required: true},
	},
	build: func(a Args) (Request, error) {
		repo, err := requiredStr(a, "repository")
		if err != nil {
			return Request{}, err
		}
		if !repositoryRe.MatchString(repo) {
			return Request{}, fmt.Errorf("repository %q is not valid: <owner>/<name>", repo)
		}
		tag, err := requiredStr(a, "tag")
		if err != nil {
			return Request{}, err
		}
		if !tagRe.MatchString(tag) {
			return Request{}, fmt.Errorf("tag %q is not valid", tag)
		}
		return Request{Method: http.MethodGet, Path: "/container-images/" + repo + ":" + tag}, nil
	},
	steps: containerImageStatusSteps,
}

// containerImageStatusSteps turns the registry's 404 into present false, and a found image into present true.
func containerImageStatusSteps(ctx context.Context, a Args, final Request, do Doer) (Result, string, error) {
	res, err := do(ctx, final)
	if err != nil {
		return res, "", err
	}
	image, _, _ := a.str("repository")
	tag, _, _ := a.str("tag")
	image += ":" + tag
	switch {
	case res.Status == "error" && res.HTTPStatus == http.StatusNotFound:
		return jsonResult(map[string]any{"image": image, "present": false})
	case res.Status == "ok" && res.HTTPStatus >= 200 && res.HTTPStatus < 300 && json.Valid([]byte(res.Body)):
		return jsonResult(map[string]any{"image": image, "present": true, "entries": json.RawMessage(res.Body)})
	}
	return res, "", nil
}

// blockedCall refuses the calls the agent may never make, whatever the delegation says: template
// changes (issue #67) and a training configuration that would reach a shell in the privileged
// training container (issue #93). It returns the reason, or "" when the call may go on.
func blockedCall(r Request) string {
	p := strings.ToLower(r.Path)
	if p == "/train/config" && (r.Method == http.MethodPost || r.Method == http.MethodPut) {
		return CheckTrainConfig(r.Body)
	}
	// A copy of a training container (issue #168) carries a train_config that reaches the same shell command.
	if r.Method == http.MethodPost && trainCloneRe.MatchString(p) {
		return CheckTrainConfig(r.Body)
	}
	if p == "/train/template" || strings.HasPrefix(p, "/train/template/") {
		if r.Method != http.MethodGet {
			return "changing training templates is not allowed for the agent"
		}
		return ""
	}
	if p != templatesPath && !strings.HasPrefix(p, templatesPath+"/") {
		return ""
	}
	if p == templatesPath+"/create/git" && r.Method == http.MethodPost {
		switch {
		case len(r.Body) > 0 || len(r.Files) > 0:
			return "importing a template takes form fields only"
		case len(r.Form["git_url"]) != 1 || r.Form["git_url"][0] != AllowedTemplateGitURL:
			return "templates can only be imported from " + AllowedTemplateGitURL
		case hasKey(r.Form, "git_username") || hasKey(r.Form, "git_access_token"):
			return "templates are imported without credentials"
		}
		return ""
	}
	if r.Method == http.MethodGet {
		return ""
	}
	return "changing inference container templates is not allowed for the agent"
}

func hasKey(m map[string][]string, k string) bool {
	_, ok := m[k]
	return ok
}

// requiredStr reads a string argument that must be present.
func requiredStr(a Args, name string) (string, error) {
	s, ok, err := a.str(name)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("%s missing", name)
	}
	return s, nil
}

// requiredInt reads a positive integer argument that must be present.
func requiredInt(a Args, name string) (int64, error) {
	n, ok, err := a.int(name)
	if err != nil {
		return 0, err
	}
	if !ok || n <= 0 {
		return 0, fmt.Errorf("%s missing", name)
	}
	return n, nil
}

// jsonResult is the answer of a tool with steps when it answers itself (not with a platform answer).
func jsonResult(v any) (Result, string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return Result{Status: "error", Message: err.Error()}, "", nil
	}
	return Result{Status: "ok", HTTPStatus: http.StatusOK, Body: string(b)}, "", nil
}

// jsonBody marshals a map of strings and numbers that cannot fail.
func jsonBody(v map[string]any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

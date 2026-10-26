// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// upstreamTemplateURL and forkTemplateURL are the repositories of the two official templates (#230).
var (
	upstreamTemplateURL = TemplateSources[0].GitURL
	forkTemplateURL     = TemplateSources[1].GitURL
)

// Sample data: an ONNX model with all tensor fields, owned by agent-test; the template with id 2 is the official one.
const (
	readyModel = `{"id":5,"name":"mnist_efficientnet","format":"onnx","owner":"agent-test",` +
		`"input_shape":[1,1,28,28],"input_datatype":"float32","input_semantics":"image",` +
		`"output_shape":[1,2],"output_datatype":"float32","output_labels":["0","1"]}`
)

var officialTemplates = `[{"id":2,"name":"triton-inference","source":"git","git_url":"` + upstreamTemplateURL + `","git_ref":"triton"}]`

func mustLookup(t *testing.T, name string) Tool {
	t.Helper()
	tool, ok := Lookup(name)
	if !ok {
		t.Fatalf("%s not found", name)
	}
	return tool
}

func decodeMap(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("not an object: %v in %s", err, s)
	}
	return m
}

func TestTemplateCallsBlocked(t *testing.T) {
	blocked := map[string]Request{
		"update from git": {Method: http.MethodPatch, Path: "/inference-container-templates/2/update-from-git", Body: json.RawMessage(`{"git_ref":"main"}`)},
		"delete":          {Method: http.MethodDelete, Path: "/inference-container-templates/2"},
		"upload":          {Method: http.MethodPost, Path: "/inference-container-templates/create/upload", Form: map[string][]string{"name": {"x"}}},
		"git JSON body":   {Method: http.MethodPost, Path: "/inference-container-templates/create/git", Body: json.RawMessage(`{"git_url":"` + upstreamTemplateURL + `"}`)},
		"foreign URL": {Method: http.MethodPost, Path: "/inference-container-templates/create/git",
			Form: map[string][]string{"git_url": {"https://example.com/other.git"}, "git_ref": {"triton"}}},
		"URL with .git": {Method: http.MethodPost, Path: "/inference-container-templates/create/git",
			Form: map[string][]string{"git_url": {upstreamTemplateURL + ".git"}, "git_ref": {"triton"}}},
		"with access token": {Method: http.MethodPost, Path: "/inference-container-templates/create/git",
			Form: map[string][]string{"git_url": {upstreamTemplateURL}, "git_access_token": {"secret"}}},
		"with username": {Method: http.MethodPost, Path: "/inference-container-templates/create/git",
			Form: map[string][]string{"git_url": {upstreamTemplateURL}, "git_username": {"x"}}},
	}
	for name, r := range blocked {
		if _, err := Normalize(r); err == nil {
			t.Errorf("%s: not refused", name)
		}
	}
	allowed := map[string]Request{
		"list":           {Method: http.MethodGet, Path: "/inference-container-templates"},
		"import allowed": {Method: http.MethodPost, Path: "/inference-container-templates/create/git", Form: map[string][]string{"git_url": {upstreamTemplateURL}, "git_ref": {"triton"}, "name": {"t"}}},
	}
	for name, r := range allowed {
		if _, err := Normalize(r); err != nil {
			t.Errorf("%s: refused: %v", name, err)
		}
	}
}

func TestImportTemplateBuild(t *testing.T) {
	tool := mustLookup(t, "import-template")
	if tool.Name != "import_template" || !tool.Write || !tool.HasSteps() {
		t.Fatalf("tool = %+v", tool)
	}
	req, err := tool.Build(json.RawMessage(`{"name":"triton-inference"}`))
	if err != nil {
		t.Fatal(err)
	}
	if req.Method != http.MethodPost || req.Path != "/inference-container-templates/create/git" {
		t.Fatalf("request = %s %s", req.Method, req.Path)
	}
	if req.Form["git_url"][0] != upstreamTemplateURL || req.Form["git_ref"][0] != "triton" || req.Form["name"][0] != "triton-inference" {
		t.Fatalf("form = %v", req.Form)
	}
	for _, raw := range []string{
		`{"name":"x","git_ref":"../main"}`,
		`{"name":"x","git_ref":"a;b"}`,
		`{"name":""}`,
		`{"name":"x","git_url":"https://example.com/x.git"}`,
		`{"name":"x","git_access_token":"secret"}`,
	} {
		if _, err := tool.Build(json.RawMessage(raw)); err == nil {
			t.Errorf("%s: accepted", raw)
		}
	}
}

func TestImportTemplateSteps(t *testing.T) {
	tool := mustLookup(t, "import-template")
	s := &stepsDoer{answers: map[string]Result{
		"GET /inference-container-templates": {Status: "ok", HTTPStatus: 200, Body: officialTemplates},
	}}
	res, refused, err := tool.Run(context.Background(), json.RawMessage(`{"name":"again"}`), s.do)
	if err != nil || !strings.Contains(refused, "already installed") || !strings.Contains(refused, "id 2") || res.Status != "error" {
		t.Fatalf("res=%+v refused=%q err=%v", res, refused, err)
	}
	if len(s.calls) != 1 {
		t.Fatalf("calls = %v", s.calls)
	}

	s = &stepsDoer{answers: map[string]Result{
		"GET /inference-container-templates": {Status: "ok", HTTPStatus: 200, Body: "[]"},
	}}
	if _, refused, err := tool.Run(context.Background(), json.RawMessage(`{"name":"triton-inference"}`), s.do); err != nil || refused != "" {
		t.Fatalf("refused=%q err=%v", refused, err)
	}
	if len(s.calls) != 2 || !strings.HasPrefix(s.calls[0], "GET /inference-container-templates?limit=1000") || !strings.HasPrefix(s.calls[1], "POST /inference-container-templates/create/git") {
		t.Fatalf("calls = %v", s.calls)
	}
}

func TestValidateModelRules(t *testing.T) {
	cases := []struct {
		name            string
		model           string
		problems, warns int
		problemContains string
		warningContains string
	}{
		{name: "ready", model: readyModel},
		{name: "pytorch", model: strings.Replace(readyModel, `"format":"onnx"`, `"format":"pytorch"`, 1), problems: 1, problemContains: "not onnx"},
		{name: "no input_semantics", model: strings.Replace(readyModel, `"input_semantics":"image",`, "", 1), problems: 1, problemContains: "input_semantics"},
		{name: "name with space and None Acc.", model: strings.Replace(readyModel, `"mnist_efficientnet"`, `"EfficientNet [mnist, None Acc.]"`, 1), problems: 1, warns: 1,
			problemContains: "cannot be addressed", warningContains: "None Acc."},
		{name: "no output_labels", model: strings.Replace(readyModel, `,"output_labels":["0","1"]`, "", 1), warns: 1, warningContains: "output_labels"},
	}
	for _, c := range cases {
		problems, warnings := modelReadiness(decodeMap(t, c.model))
		if len(problems) != c.problems || len(warnings) != c.warns {
			t.Errorf("%s: problems=%v warnings=%v", c.name, problems, warnings)
			continue
		}
		if c.problemContains != "" && !strings.Contains(strings.Join(problems, "|"), c.problemContains) {
			t.Errorf("%s: problems %v lack %q", c.name, problems, c.problemContains)
		}
		if c.warningContains != "" && !strings.Contains(strings.Join(warnings, "|"), c.warningContains) {
			t.Errorf("%s: warnings %v lack %q", c.name, warnings, c.warningContains)
		}
	}

	tool := mustLookup(t, "validate-model")
	if tool.Write {
		t.Fatal("validate_model must not write")
	}
	s := &stepsDoer{answers: map[string]Result{"GET /models/5": {Status: "ok", HTTPStatus: 200, Body: readyModel}}}
	res, refused, err := tool.Run(context.Background(), json.RawMessage(`{"model_id":5}`), s.do)
	if err != nil || refused != "" || res.Status != "ok" {
		t.Fatalf("res=%+v refused=%q err=%v", res, refused, err)
	}
	if !strings.Contains(res.Body, `"ready":true`) || !strings.Contains(res.Body, `"problems":[]`) {
		t.Fatalf("body = %s", res.Body)
	}
}

// buildArgs is the call of the scenarios below: image mnist-triton with tag v1 from model 5 and template 2.
const buildArgs = `{"template_id":2,"model_id":5,"name":"mnist-triton","tag":"v1","architecture":"amd64"}`

// buildAnswers are the answers of a build that passes every check.
func buildAnswers() map[string]Result {
	return map[string]Result{
		"GET /models/5":                                    {Status: "ok", HTTPStatus: 200, Body: readyModel},
		"GET /inference-container-templates":               {Status: "ok", HTTPStatus: 200, Body: officialTemplates},
		"GET /container-images/agent-test/mnist-triton:v1": {Status: "error", HTTPStatus: 404, Message: "platform responds 404"},
		"GET /tasks":                   {Status: "ok", HTTPStatus: 200, Body: `[{"id":39,"title":"Container Buildjob 'agent-test/mnist-triton/v0'","status":"inprogress"}]`},
		"POST /container-images/build": {Status: "ok", HTTPStatus: 202, Location: "/tasks/40"},
	}
}

func TestBuildInferenceImageSteps(t *testing.T) {
	tool := mustLookup(t, "build-image")
	if tool.Name != "build_inference_image" || !tool.Write || !tool.HasSteps() {
		t.Fatalf("tool = %+v", tool)
	}
	s := &stepsDoer{answers: buildAnswers()}
	res, refused, err := tool.Run(context.Background(), json.RawMessage(buildArgs), s.do)
	if err != nil || refused != "" {
		t.Fatalf("refused=%q err=%v", refused, err)
	}
	if len(s.calls) != 5 {
		t.Fatalf("calls = %v", s.calls)
	}
	wantPrefix := []string{
		"GET /models/5|",
		"GET /inference-container-templates?limit=1000|",
		"GET /container-images/agent-test/mnist-triton:v1|",
		"GET /tasks?",
		"POST /container-images/build|",
	}
	for i, p := range wantPrefix {
		if !strings.HasPrefix(s.calls[i], p) {
			t.Errorf("call %d = %q, want prefix %q", i, s.calls[i], p)
		}
	}
	if !strings.Contains(s.calls[3], "initiator=agent-test") {
		t.Errorf("task query = %q", s.calls[3])
	}
	body := `{"architecture":"amd64","container_template_id":2,"model_id":5,"repository":"agent-test/mnist-triton","tag":"v1"}`
	if !strings.HasSuffix(s.calls[4], "|"+body) {
		t.Errorf("build body = %q", s.calls[4])
	}
	if res.HTTPStatus != http.StatusAccepted || res.Body != `{"repository":"agent-test/mnist-triton","runtime":"triton","tag":"v1","task":"/tasks/40"}` {
		t.Errorf("result = %+v", res)
	}
}

func TestBuildInferenceImageRefuses(t *testing.T) {
	tool := mustLookup(t, "build-image")
	withAnswer := func(key string, r Result) map[string]Result {
		a := buildAnswers()
		a[key] = r
		return a
	}
	cases := []struct {
		name    string
		answers map[string]Result
		want    string
		calls   int
	}{
		{name: "image exists", answers: withAnswer("GET /container-images/agent-test/mnist-triton:v1", Result{Status: "ok", HTTPStatus: 200, Body: "{}"}),
			want: "already in the registry", calls: 3},
		{name: "build running", answers: withAnswer("GET /tasks", Result{Status: "ok", HTTPStatus: 200,
			Body: `[{"id":41,"title":"Container Buildjob 'agent-test/mnist-triton/v1'","status":"inprogress"}]`}),
			want: "still running", calls: 4},
		{name: "model not ready", answers: withAnswer("GET /models/5", Result{Status: "ok", HTTPStatus: 200,
			Body: strings.Replace(readyModel, `"format":"onnx"`, `"format":"pytorch"`, 1)}), want: "not ready", calls: 1},
		{name: "template missing", answers: withAnswer("GET /inference-container-templates", Result{Status: "ok", HTTPStatus: 200, Body: "[]"}),
			want: "not installed", calls: 2},
	}
	for _, c := range cases {
		s := &stepsDoer{answers: c.answers}
		_, refused, err := tool.Run(context.Background(), json.RawMessage(buildArgs), s.do)
		if err != nil || !strings.Contains(refused, c.want) {
			t.Errorf("%s: refused=%q err=%v", c.name, refused, err)
		}
		if len(s.calls) != c.calls {
			t.Errorf("%s: calls = %d, want %d: %v", c.name, len(s.calls), c.calls, s.calls)
		}
		for _, call := range s.calls {
			if strings.HasPrefix(call, "POST ") {
				t.Errorf("%s: build sent: %s", c.name, call)
			}
		}
	}

	// A denied registry read is returned as it is, and nothing is built.
	s := &stepsDoer{answers: withAnswer("GET /container-images/agent-test/mnist-triton:v1", Result{Status: "denied", Message: "no"})}
	res, _, _ := tool.Run(context.Background(), json.RawMessage(buildArgs), s.do)
	if res.Status != "denied" || len(s.calls) != 3 {
		t.Errorf("denied read: res=%+v calls=%v", res, s.calls)
	}

	// Argument errors: nothing goes out.
	for _, raw := range []string{
		`{"template_id":2,"model_id":5,"name":"Mnist","tag":"v1","architecture":"amd64"}`,
		`{"template_id":2,"model_id":5,"name":"mnist/x","tag":"v1","architecture":"amd64"}`,
		`{"template_id":2,"model_id":5,"name":"mnist-triton","tag":"v 1","architecture":"amd64"}`,
		`{"template_id":2,"model_id":5,"name":"mnist-triton","tag":"v1","architecture":"x86"}`,
		`{"template_id":2,"model_id":5,"name":"mnist-triton","tag":"v1","architecture":"amd64","edge_device_id":3}`,
	} {
		s := &stepsDoer{answers: buildAnswers()}
		_, refused, _ := tool.Run(context.Background(), json.RawMessage(raw), s.do)
		if refused == "" || len(s.calls) != 0 {
			t.Errorf("%s: refused=%q calls=%v", raw, refused, s.calls)
		}
	}
}

func TestContainerImageStatus(t *testing.T) {
	tool := mustLookup(t, "image-status")
	if tool.Name != "container_image_status" || tool.Write {
		t.Fatalf("tool = %+v", tool)
	}
	args := json.RawMessage(`{"repository":"agent-test/mnist-triton","tag":"v1"}`)
	s := &stepsDoer{answers: map[string]Result{
		"GET /container-images/agent-test/mnist-triton:v1": {Status: "error", HTTPStatus: 404},
	}}
	res, _, err := tool.Run(context.Background(), args, s.do)
	if err != nil || res.Status != "ok" || !strings.Contains(res.Body, `"present":false`) {
		t.Fatalf("404: res=%+v err=%v", res, err)
	}

	s = &stepsDoer{answers: map[string]Result{
		"GET /container-images/agent-test/mnist-triton:v1": {Status: "ok", HTTPStatus: 200, Body: `{"name":"mnist-triton"}`},
	}}
	res, _, err = tool.Run(context.Background(), args, s.do)
	if err != nil || !strings.Contains(res.Body, `"present":true`) || !strings.Contains(res.Body, `"entries":{"name":"mnist-triton"}`) {
		t.Fatalf("200: res=%+v err=%v", res, err)
	}

	if _, refused, _ := tool.Run(context.Background(), json.RawMessage(`{"repository":"mnist","tag":"v1"}`), (&stepsDoer{}).do); refused == "" {
		t.Error("repository without owner accepted")
	}
}

// A shell value in train_config reached the privileged training container before issue #93.
func TestCreateTrainingRefusesShellValue(t *testing.T) {
	tool := mustLookup(t, "create_training")
	ok := `{"provider":"Torchvision","architecture":"EfficientNet","category":"Classification","dataset_id":2,` +
		`"train_config":{"epochs":1,"optimizer":"AdamW"}}`
	if _, err := tool.Build(json.RawMessage(ok)); err != nil {
		t.Fatalf("a normal training is refused: %v", err)
	}
	for _, raw := range []string{
		`{"provider":"Torchvision","architecture":"EfficientNet","category":"Classification","dataset_id":2,"train_config":{"optimizer":"Adam; id"}}`,
		`{"provider":"Torchvision","architecture":"EfficientNet","category":"Classification","dataset_id":2,"train_config":{"optimizer":"$(id)"}}`,
	} {
		if _, err := tool.Build(json.RawMessage(raw)); err == nil {
			t.Errorf("%s: accepted", raw)
		}
	}
}

func TestRawPutTrainConfigRefused(t *testing.T) {
	bad := Request{Method: http.MethodPut, Path: "/train/config",
		Body: json.RawMessage(`{"container_id":7,"dataset_id":12,"train_config":{"optimizer":"Adam; id"},"export_config":null}`)}
	if _, err := Normalize(bad); err == nil {
		t.Error("PUT /train/config with a shell value: accepted")
	}
	good := Request{Method: http.MethodPut, Path: "/train/config",
		Body: json.RawMessage(`{"container_id":7,"dataset_id":12,"train_config":{"epochs":1},"export_config":null}`)}
	if _, err := Normalize(good); err != nil {
		t.Errorf("PUT /train/config with a normal value: refused: %v", err)
	}
	// Reading the configuration stays allowed.
	if _, err := Normalize(Request{Method: http.MethodGet, Path: "/train/config/Torchvision/EfficientNet"}); err != nil {
		t.Errorf("GET /train/config/…: refused: %v", err)
	}
}

func TestTrainTemplateBlocked(t *testing.T) {
	blocked := map[string]Request{
		"upload": {Method: http.MethodPost, Path: "/train/template/upload", Form: map[string][]string{"template_zip": {"x"}}, Files: []File{{Field: "template_zip", Path: "/workspace/t.zip"}}},
		"delete": {Method: http.MethodDelete, Path: "/train/template", Body: json.RawMessage(`{"provider":"Torchvision","architecture":"EfficientNet"}`)},
	}
	for name, r := range blocked {
		if _, err := Normalize(r); err == nil {
			t.Errorf("%s: not refused", name)
		}
	}
	// The routes around it stay open: reading providers and templates is a normal read.
	for name, r := range map[string]Request{
		"providers":     {Method: http.MethodGet, Path: "/train/providers"},
		"template list": {Method: http.MethodGet, Path: "/train/template"},
	} {
		if _, err := Normalize(r); err != nil {
			t.Errorf("%s: refused: %v", name, err)
		}
	}
}

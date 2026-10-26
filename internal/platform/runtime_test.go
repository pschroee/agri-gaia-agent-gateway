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

// resetRuntime puts the default runtime back after a test that changes it.
func resetRuntime(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { _ = SetDefaultRuntime(RuntimeTriton) })
}

func TestAllowedTemplateURL(t *testing.T) {
	for _, u := range []string{upstreamTemplateURL, forkTemplateURL} {
		if !AllowedTemplateURL(u) {
			t.Errorf("%s: not allowed", u)
		}
	}
	for _, u := range []string{upstreamTemplateURL + "/", upstreamTemplateURL + ".git", forkTemplateURL + ".git", forkTemplateURL + "/", "https://example.com/x", ""} {
		if AllowedTemplateURL(u) {
			t.Errorf("%q: allowed", u)
		}
	}
}

func TestTemplateRuntime(t *testing.T) {
	cases := []struct{ url, ref, want string }{
		{upstreamTemplateURL, "triton", RuntimeTriton},
		{forkTemplateURL, "ovms-onnx", RuntimeOVMS},
		{upstreamTemplateURL, "main", ""},
		{forkTemplateURL, "triton", ""},
		{forkTemplateURL, "main", ""},
		{"https://example.com/x", "triton", ""},
	}
	for _, c := range cases {
		if got := TemplateRuntime(c.url, c.ref); got != c.want {
			t.Errorf("TemplateRuntime(%s, %s) = %q, want %q", c.url, c.ref, got, c.want)
		}
	}
}

func TestParseRuntime(t *testing.T) {
	for _, ok := range []string{"triton", "ovms"} {
		if got, err := ParseRuntime(ok); err != nil || got != ok {
			t.Errorf("%s: %q %v", ok, got, err)
		}
	}
	for _, bad := range []string{"onnxruntime", "", "OVMS", "Triton"} {
		if _, err := ParseRuntime(bad); err == nil || !strings.Contains(err.Error(), "triton or ovms") {
			t.Errorf("%q: err = %v", bad, err)
		}
	}
}

func TestSetDefaultRuntime(t *testing.T) {
	resetRuntime(t)
	if DefaultRuntime() != RuntimeTriton {
		t.Fatalf("default = %s", DefaultRuntime())
	}
	if err := SetDefaultRuntime("ovms"); err != nil || DefaultRuntime() != RuntimeOVMS {
		t.Fatalf("ovms: %v, default = %s", err, DefaultRuntime())
	}
	if err := SetDefaultRuntime("x"); err == nil || DefaultRuntime() != RuntimeOVMS {
		t.Fatalf("x: %v, default = %s", err, DefaultRuntime())
	}
}

func TestImportTemplateRuntime(t *testing.T) {
	resetRuntime(t)
	tool := mustLookup(t, "import-template")
	build := func(raw string) (Request, error) { return tool.Build(json.RawMessage(raw)) }

	req, err := build(`{"name":"t"}`)
	if err != nil || req.Form["git_url"][0] != upstreamTemplateURL || req.Form["git_ref"][0] != "triton" {
		t.Fatalf("default: %v %v", req.Form, err)
	}
	req, err = build(`{"name":"o","runtime":"ovms"}`)
	if err != nil || req.Form["git_url"][0] != forkTemplateURL || req.Form["git_ref"][0] != "ovms-onnx" ||
		!strings.Contains(req.Form["description"][0], "runtime ovms") {
		t.Fatalf("ovms: %v %v", req.Form, err)
	}
	if _, err := build(`{"name":"o","runtime":"ovms","git_ref":"ovms-onnx"}`); err != nil {
		t.Errorf("matching git_ref refused: %v", err)
	}
	for _, raw := range []string{
		`{"name":"o","runtime":"ovms","git_ref":"triton"}`,
		`{"name":"o","runtime":"triton","git_ref":"ovms-onnx"}`,
		`{"name":"o","git_ref":"main"}`,
		`{"name":"o","runtime":"onnxruntime"}`,
	} {
		if _, err := build(raw); err == nil {
			t.Errorf("%s: accepted", raw)
		}
	}
	if _, err := build(`{"name":"o","runtime":"ovms","git_ref":"triton"}`); err == nil || !strings.Contains(err.Error(), "ovms-onnx") {
		t.Errorf("message does not name the branch: %v", err)
	}
}

func TestImportTemplateDefaultFollowsSetting(t *testing.T) {
	resetRuntime(t)
	if err := SetDefaultRuntime("ovms"); err != nil {
		t.Fatal(err)
	}
	tool := mustLookup(t, "import-template")
	req, err := tool.Build(json.RawMessage(`{"name":"o"}`))
	if err != nil || req.Form["git_url"][0] != forkTemplateURL || req.Form["git_ref"][0] != "ovms-onnx" {
		t.Fatalf("ovms default: %v %v", req.Form, err)
	}
	req, err = tool.Build(json.RawMessage(`{"name":"t","runtime":"triton"}`))
	if err != nil || req.Form["git_url"][0] != upstreamTemplateURL {
		t.Fatalf("explicit triton: %v %v", req.Form, err)
	}
}

func TestImportTemplateStepsPerRuntime(t *testing.T) {
	tool := mustLookup(t, "import-template")
	// The Triton template is installed: an ovms import is not a second copy, a triton import is.
	s := &stepsDoer{answers: map[string]Result{"GET /inference-container-templates": {Status: "ok", HTTPStatus: 200, Body: officialTemplates}}}
	if _, refused, _ := tool.Run(context.Background(), json.RawMessage(`{"name":"o","runtime":"ovms"}`), s.do); refused != "" {
		t.Errorf("ovms import refused: %s", refused)
	}
	s = &stepsDoer{answers: map[string]Result{"GET /inference-container-templates": {Status: "ok", HTTPStatus: 200, Body: officialTemplates}}}
	if _, refused, _ := tool.Run(context.Background(), json.RawMessage(`{"name":"t","runtime":"triton"}`), s.do); !strings.Contains(refused, "already installed") {
		t.Errorf("triton import: refused = %q", refused)
	}
}

func TestBlockedCallTemplateURLs(t *testing.T) {
	form := func(u string) Request {
		return Request{Method: http.MethodPost, Path: "/inference-container-templates/create/git",
			Form: map[string][]string{"git_url": {u}, "git_ref": {"ovms-onnx"}, "name": {"t"}}}
	}
	if _, err := Normalize(form(forkTemplateURL)); err != nil {
		t.Errorf("fork refused: %v", err)
	}
	if _, err := Normalize(form(upstreamTemplateURL)); err != nil {
		t.Errorf("upstream refused: %v", err)
	}
	_, err := Normalize(form("https://github.com/someone/agri-gaia-inference-container-template"))
	if err == nil || !strings.Contains(err.Error(), forkTemplateURL) || !strings.Contains(err.Error(), upstreamTemplateURL) {
		t.Errorf("foreign URL: err = %v", err)
	}
}

func TestListTemplatesRuntime(t *testing.T) {
	tool := mustLookup(t, "templates")
	if !tool.HasSteps() {
		t.Fatal("list_templates has no steps")
	}
	body := `[{"id":2,"git_url":"` + upstreamTemplateURL + `","git_ref":"triton"},` +
		`{"id":3,"git_url":"` + forkTemplateURL + `","git_ref":"ovms-onnx"},` +
		`{"id":4,"git_url":"https://example.invalid/t","git_ref":"main"},` +
		`{"id":5,"git_url":"` + forkTemplateURL + `","git_ref":"main"}]`
	s := &stepsDoer{answers: map[string]Result{"GET /inference-container-templates": {Status: "ok", HTTPStatus: 200, Body: body}}}
	res, refused, err := tool.Run(context.Background(), json.RawMessage(`{}`), s.do)
	if err != nil || refused != "" || res.Status != "ok" {
		t.Fatalf("res=%+v refused=%q err=%v", res, refused, err)
	}
	var list []map[string]any
	if err := json.Unmarshal([]byte(res.Body), &list); err != nil || len(list) != 4 {
		t.Fatalf("body = %s (%v)", res.Body, err)
	}
	for i, want := range []string{"triton", "ovms", "other", "other"} {
		if list[i]["runtime"] != want {
			t.Errorf("entry %d: runtime = %v, want %s", i, list[i]["runtime"], want)
		}
	}

	// An answer that is no array goes through unchanged.
	s = &stepsDoer{answers: map[string]Result{"GET /inference-container-templates": {Status: "ok", HTTPStatus: 200, Body: `{"items":[]}`}}}
	res, _, _ = tool.Run(context.Background(), json.RawMessage(`{}`), s.do)
	if res.Body != `{"items":[]}` {
		t.Errorf("non-array changed: %s", res.Body)
	}
}

// runtimeTemplates holds the Triton template (2), the OVMS template (3) and a template of no official source (4).
func runtimeTemplates() string {
	return `[{"id":2,"name":"triton-inference","git_url":"` + upstreamTemplateURL + `","git_ref":"triton"},` +
		`{"id":3,"name":"ovms-inference","git_url":"` + forkTemplateURL + `","git_ref":"ovms-onnx"},` +
		`{"id":4,"name":"foreign","git_url":"https://example.invalid/t","git_ref":"main"}]`
}

// runBuild runs build_inference_image with the answers of buildAnswers, the templates of runtimeTemplates and the tag
// of the image given; it returns what the tool refused (empty when it went to the build) and the result.
func runBuild(t *testing.T, args string, image string) (refused string, res Result, s *stepsDoer) {
	t.Helper()
	a := buildAnswers()
	a["GET /inference-container-templates"] = Result{Status: "ok", HTTPStatus: 200, Body: runtimeTemplates()}
	a["GET /container-images/agent-test/"+image] = Result{Status: "error", HTTPStatus: 404}
	s = &stepsDoer{answers: a}
	res, refused, err := mustLookup(t, "build-image").Run(context.Background(), json.RawMessage(args), s.do)
	if err != nil {
		t.Fatal(err)
	}
	return refused, res, s
}

func buildJSON(tid int, name, tag, arch, runtime string) string {
	b := map[string]any{"template_id": tid, "model_id": 5, "name": name, "tag": tag, "architecture": arch}
	if runtime != "" {
		b["runtime"] = runtime
	}
	raw, _ := json.Marshal(b)
	return string(raw)
}

func TestBuildRuntimeMismatch(t *testing.T) {
	resetRuntime(t)
	// OVMS template with the default runtime (triton), and Triton template with runtime ovms.
	refused, _, _ := runBuild(t, buildJSON(3, "m", "v1-ovms", "amd64", ""), "m:v1-ovms")
	if !strings.Contains(refused, "template 3 is the ovms template, the runtime is triton") {
		t.Errorf("ovms template, default: %q", refused)
	}
	refused, _, _ = runBuild(t, buildJSON(2, "m", "v1-ovms", "amd64", "ovms"), "m:v1-ovms")
	if !strings.Contains(refused, "template 2 is the triton template, the runtime is ovms") {
		t.Errorf("triton template, ovms: %q", refused)
	}
}

func TestBuildForeignTemplateWithoutRuntimeAsToday(t *testing.T) {
	resetRuntime(t)
	refused, res, s := runBuild(t, buildJSON(4, "m", "v1", "amd64", ""), "m:v1")
	if refused != "" || res.HTTPStatus != http.StatusAccepted {
		t.Fatalf("refused=%q res=%+v", refused, res)
	}
	if !strings.Contains(res.Body, `"runtime":"other"`) || len(s.calls) != 5 {
		t.Errorf("body = %s, calls = %v", res.Body, s.calls)
	}
}

func TestBuildForeignTemplateExplicitRuntime(t *testing.T) {
	resetRuntime(t)
	for _, rt := range []string{"triton", "ovms"} {
		tag := "v1"
		if rt == "ovms" {
			tag = "v1-ovms"
		}
		refused, _, s := runBuild(t, buildJSON(4, "m", tag, "amd64", rt), "m:"+tag)
		if !strings.Contains(refused, "none of the official templates") {
			t.Errorf("%s: refused = %q", rt, refused)
		}
		for _, c := range s.calls {
			if strings.HasPrefix(c, "POST ") {
				t.Errorf("%s: build sent", rt)
			}
		}
	}
}

func TestBuildOVMSArm64(t *testing.T) {
	refused, _, _ := runBuild(t, buildJSON(3, "m", "v1-ovms", "arm64", "ovms"), "m:v1-ovms")
	if !strings.Contains(refused, "amd64 only") {
		t.Errorf("refused = %q", refused)
	}
}

func TestBuildOVMSTagSuffix(t *testing.T) {
	refused, _, _ := runBuild(t, buildJSON(3, "m", "v1", "amd64", "ovms"), "m:v1")
	if !strings.Contains(refused, "tag ending in -ovms") {
		t.Errorf("refused = %q", refused)
	}
	refused, res, _ := runBuild(t, buildJSON(3, "m", "v1-ovms", "amd64", "ovms"), "m:v1-ovms")
	if refused != "" || res.HTTPStatus != http.StatusAccepted {
		t.Errorf("good ovms build: refused=%q res=%+v", refused, res)
	}
}

func TestBuildTritonSuffixReserved(t *testing.T) {
	refused, _, _ := runBuild(t, buildJSON(2, "m", "v1-ovms", "amd64", "triton"), "m:v1-ovms")
	if !strings.Contains(refused, "reserved for ovms images") {
		t.Errorf("refused = %q", refused)
	}
}

func TestBuildAnswerRuntime(t *testing.T) {
	resetRuntime(t)
	_, res, _ := runBuild(t, buildJSON(2, "m", "v1", "amd64", "triton"), "m:v1")
	if res.Body != `{"repository":"agent-test/m","runtime":"triton","tag":"v1","task":"/tasks/40"}` {
		t.Errorf("triton body = %s", res.Body)
	}
	_, res, _ = runBuild(t, buildJSON(3, "m", "v1-ovms", "amd64", "ovms"), "m:v1-ovms")
	if res.Body != `{"repository":"agent-test/m","runtime":"ovms","tag":"v1-ovms","task":"/tasks/40"}` {
		t.Errorf("ovms body = %s", res.Body)
	}
	// With the setting ovms, a call without runtime builds from the OVMS template.
	if err := SetDefaultRuntime("ovms"); err != nil {
		t.Fatal(err)
	}
	_, res, _ = runBuild(t, buildJSON(3, "m", "v1-ovms", "amd64", ""), "m:v1-ovms")
	if !strings.Contains(res.Body, `"runtime":"ovms"`) {
		t.Errorf("default ovms body = %s", res.Body)
	}
}

func TestBuildSchemaRuntimeEnum(t *testing.T) {
	for _, name := range []string{"build_inference_image", "import_template"} {
		tool := mustLookup(t, name)
		props, _ := tool.Schema()["properties"].(map[string]any)
		rt, _ := props["runtime"].(map[string]any)
		if rt == nil {
			t.Fatalf("%s: no runtime in the schema", name)
		}
		enum, _ := rt["enum"].([]string)
		if len(enum) != 2 || enum[0] != "triton" || enum[1] != "ovms" {
			t.Errorf("%s: enum = %v", name, rt["enum"])
		}
		if req, _ := tool.Schema()["required"].([]string); len(req) > 0 {
			for _, r := range req {
				if r == "runtime" {
					t.Errorf("%s: runtime is required", name)
				}
			}
		}
	}
}

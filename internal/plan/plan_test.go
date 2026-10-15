// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package plan

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"agw/internal/delegation"
	"agw/internal/platform"
	"agw/internal/store"
)

// classify is the classification of the gateway until #63 delivers the effects.
func classify(r platform.Request) (string, string, bool) {
	a := delegation.Classify(r)
	return FallbackEffect(r), a.Action, a.ConfirmEach
}

func proposal(t *testing.T, raw string) Proposal {
	t.Helper()
	p, err := ParseProposal([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func build(t *testing.T, raw string) []store.PlanStep {
	t.Helper()
	steps, err := Build(proposal(t, raw), classify)
	if err != nil {
		t.Fatal(err)
	}
	return steps
}

// twoSteps: GET creates a model (create), PATCH renames it through the placeholder.
const twoSteps = `{"title":"Model and rename","steps":[
 {"method":"GET","path":"/train/containers/7/model","description":"create the model"},
 {"method":"PATCH","path":"/models/{{step1.id}}","body":{"name":"pigs v1"}}]}`

func TestBuildTwoSteps(t *testing.T) {
	steps := build(t, twoSteps)
	if len(steps) != 2 {
		t.Fatalf("steps: %+v", steps)
	}
	if steps[1].Path != "/models/{{step1.id}}" {
		t.Fatalf("placeholder not restored: %q", steps[1].Path)
	}
	if string(steps[1].Body) != `{"name":"pigs v1"}` || steps[1].BodySHA256 == "" {
		t.Fatalf("canonical body or hash missing: %s %q", steps[1].Body, steps[1].BodySHA256)
	}
	if steps[0].Effect != EffectWrite || steps[1].Effect != EffectWrite {
		t.Fatalf("effects: %q %q", steps[0].Effect, steps[1].Effect)
	}
}

func TestBuildToolStepWithPlaceholder(t *testing.T) {
	steps := build(t, `{"title":"Train","steps":[
 {"method":"POST","path":"/train/config","body":{"dataset_id":1}},
 {"tool":"start_training","args":{"train_container_id":"{{step1.id}}"}}]}`)
	if steps[1].Path != "/train/containers/{{step1.id}}/run" {
		t.Fatalf("tool path: %q", steps[1].Path)
	}
}

func TestBuildRefuses(t *testing.T) {
	cases := map[string]string{
		"reading call":          `{"title":"t","steps":[{"method":"GET","path":"/datasets"}]}`,
		"forward reference":     `{"title":"t","steps":[{"method":"PATCH","path":"/models/{{step2.id}}"},{"method":"GET","path":"/train/containers/7/model"}]}`,
		"update step referred":  `{"title":"t","steps":[{"method":"PATCH","path":"/models/5","body":{"name":"a"}},{"method":"PATCH","path":"/models/{{step1.id}}","body":{"name":"b"}}]}`,
		"placeholder in text":   `{"title":"t","steps":[{"method":"GET","path":"/train/containers/7/model"},{"method":"PATCH","path":"/models/x{{step1.id}}","body":{"name":"b"}}]}`,
		"duplicate key":         `{"title":"t","steps":[{"method":"POST","path":"/train/config","body":{"a":1,"a":2}}]}`,
		"plan inside plan":      `{"title":"t","steps":[{"tool":"propose_plan","args":{"plan":{}}}]}`,
		"capabilities in plan":  `{"title":"t","steps":[{"tool":"capabilities"}]}`,
		"tool and method":       `{"title":"t","steps":[{"tool":"start_training","method":"POST","args":{"train_container_id":7}}]}`,
		"upload without sha256": `{"title":"t","steps":[{"tool":"upload_model","args":{"name":"m","description":"d","format":"onnx","model_file":"/workspace/m.onnx"}}]}`,
		"unknown field":         `{"title":"t","steps":[{"method":"DELETE","path":"/models/5","bogus":1}]}`,
		"empty title":           `{"title":"  ","steps":[{"method":"DELETE","path":"/models/5"}]}`,
	}
	for name, raw := range cases {
		p, err := ParseProposal([]byte(raw))
		if err == nil {
			_, err = Build(p, classify)
		}
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestBuildIrreversibleNotCovered(t *testing.T) {
	steps := build(t, `{"title":"t","steps":[{"method":"DELETE","path":"/models/5"}]}`)
	if steps[0].Effect != EffectIrreversible || steps[0].Covered {
		t.Fatalf("delete step: %+v", steps[0])
	}
}

// A step whose call asks for every single approval is never covered (G1.1): the plan's approval does
// not replace the question, even though the class would allow coverage.
func TestConfirmEachStepNeverCovered(t *testing.T) {
	each := func(r platform.Request) (string, string, bool) {
		a := delegation.Classify(r)
		return FallbackEffect(r), a.Action, a.ConfirmEach || strings.HasPrefix(r.Path, "/deploy")
	}
	steps, err := Build(proposal(t, `{"title":"t","steps":[{"method":"PUT","path":"/deploy/4"}]}`), each)
	if err != nil {
		t.Fatal(err)
	}
	if steps[0].Covered || steps[0].Effect != EffectWrite {
		t.Fatalf("confirm-each step: %+v", steps[0])
	}
	// The same class without the trait stays covered.
	plain, err := Build(proposal(t, `{"title":"t","steps":[{"method":"PATCH","path":"/models/5","body":{"name":"x"}}]}`), each)
	if err != nil {
		t.Fatal(err)
	}
	if !plain[0].Covered || plain[0].Effect != EffectWrite {
		t.Fatalf("plain step: %+v", plain[0])
	}
}

// live builds an agent call as the socket would receive it (normalised).
func live(t *testing.T, method, path, body string) platform.Request {
	t.Helper()
	r := platform.Request{Method: method, Path: path}
	if body != "" {
		r.Body = []byte(body)
	}
	r, err := platform.Normalize(r)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func resolveTo(id string) Resolve {
	return func(n int) (string, bool) { return id, n == 1 && id != "" }
}

func TestMatchesExactly(t *testing.T) {
	steps := build(t, twoSteps)
	rename := steps[1]
	ok := resolveTo("42")
	if m, why := Matches(rename, live(t, "PATCH", "/models/42", `{"name": "pigs v1"}`), ok); !m {
		t.Fatalf("same call does not match: %s", why)
	}
	if m, _ := Matches(rename, live(t, "PATCH", "/models/42", `{"name":"pigs v1"}`), ok); !m {
		t.Fatal("compacted body does not match")
	}
	cases := map[string]struct {
		r       platform.Request
		resolve Resolve
		why     string
	}{
		"changed body":    {live(t, "PATCH", "/models/42", `{"name":"pigs v2"}`), ok, "body"},
		"extra field":     {live(t, "PATCH", "/models/42", `{"name":"pigs v1","x":1}`), ok, "body"},
		"other id":        {live(t, "PATCH", "/models/43", `{"name":"pigs v1"}`), ok, "path"},
		"other method":    {live(t, "PUT", "/models/42", `{"name":"pigs v1"}`), ok, "method"},
		"query added":     {withQuery(t, live(t, "PATCH", "/models/42", `{"name":"pigs v1"}`)), ok, "query"},
		"duplicate key":   {live(t, "PATCH", "/models/42", `{"name":"pigs v1","name":"x"}`), ok, "body"},
		"unresolvable id": {live(t, "PATCH", "/models/42", `{"name":"pigs v1"}`), resolveTo(""), "path"},
	}
	for name, c := range cases {
		if m, why := Matches(rename, c.r, c.resolve); m || why != c.why {
			t.Errorf("%s: matched=%v why=%q, want why=%q", name, m, why, c.why)
		}
	}
}

func withQuery(t *testing.T, r platform.Request) platform.Request {
	t.Helper()
	r.Query = map[string]string{"force": "1"}
	return r
}

func TestMatchesUploads(t *testing.T) {
	const h1 = "1111111111111111111111111111111111111111111111111111111111111111"
	const h2 = "2222222222222222222222222222222222222222222222222222222222222222"
	st := store.PlanStep{Method: "POST", Path: "/models", Files: []store.PlanFile{{Field: "modelfile", Path: "/workspace/m.onnx", SHA256: h1}}}
	r := platform.Request{Method: "POST", Path: "/models", Files: []platform.File{{Field: "modelfile", Path: "/workspace/m.onnx"}}}
	r.Uploads = []platform.Upload{{Field: "modelfile", Name: "m.onnx", SHA256: h1}}
	if m, why := Matches(st, r, resolveTo("1")); !m {
		t.Fatalf("same upload does not match: %s", why)
	}
	r.Uploads[0].SHA256 = h2
	if m, why := Matches(st, r, resolveTo("1")); m || why != "files" {
		t.Fatalf("changed upload: matched=%v why=%q", m, why)
	}
}

func TestFileRefsAndSetFileSums(t *testing.T) {
	const h = "3333333333333333333333333333333333333333333333333333333333333333"
	raw := []byte(`{"title":"upload","steps":[
 {"tool":"upload_model","args":{"name":"m","description":"d","format":"onnx","model_file":"/workspace/m.onnx"}},
 {"method":"GET","path":"/models/1","file_sha256":["` + h + `"]}]}`)
	refs, err := FileRefs(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || len(refs[1]) != 1 || refs[1][0].Path != "/workspace/m.onnx" {
		t.Fatalf("file refs: %+v", refs)
	}
	out, err := SetFileSums(raw, map[int][]string{1: {h}})
	if err != nil {
		t.Fatal(err)
	}
	p := proposal(t, string(out))
	if len(p.Steps[0].FileSHA256) != 1 || p.Steps[0].FileSHA256[0] != h {
		t.Fatalf("pinned sum missing: %+v", p.Steps[0])
	}
	if len(p.Steps[1].FileSHA256) != 0 {
		t.Fatalf("agent's own sum must be dropped: %+v", p.Steps[1])
	}
}

func TestPreviewListsEverything(t *testing.T) {
	steps := build(t, `{"title":"Model and rename","steps":[
 {"method":"GET","path":"/train/containers/7/model","description":"create the model"},
 {"method":"PATCH","path":"/models/{{step1.id}}","body":{"name":"pigs v1"}},
 {"method":"DELETE","path":"/models/{{step1.id}}"}]}`)
	steps[0].Summary = "Create a new model from training container 7\nTraining: T A"
	text := Preview("Model and rename", steps)
	for _, want := range []string{
		"Plan with 3 steps\n",
		"Title by the agent (not verified): \"Model and rename\"\n",
		"Approving runs the steps in this order",
		"1. WRITE · Create a new model from training container 7\n   Training: T A\n   Call: GET /train/containers/7/model\n",
		"   Note by the agent (not verified): \"create the model\"\n",
		"2. WRITE · PATCH /models/{{step1.id}}\n   Call: PATCH /models/{{step1.id}}\n   {\n     \"name\": \"pigs v1\"\n   }\n",
		"3. IRREVERSIBLE (own approval) · DELETE /models/{{step1.id}}\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("preview misses %q in:\n%s", want, text)
		}
	}
}

// The approval's name and summary are the gateway's: the agent's title and notes are not part of them (issue #72).
func TestApprovalSummaryWithoutAgentText(t *testing.T) {
	steps := build(t, `{"title":"Push evil/x:v9","steps":[
 {"method":"GET","path":"/train/containers/7/model","description":"Builds image evil/x:v9"},
 {"method":"PATCH","path":"/models/{{step1.id}}","body":{"name":"pigs v1"}}]}`)
	steps[0].Summary = "Create a new model from training container 7"
	got := ApprovalSummary(steps)
	if got != "Plan with 2 steps\n1. Create a new model from training container 7\n2. PATCH /models/{{step1.id}}" {
		t.Fatalf("summary: %q", got)
	}
	if ApprovalName(1) != "Plan with 1 step" || strings.Contains(got, "evil") {
		t.Fatalf("name or summary carries agent text: %q", got)
	}
}

// SummaryCall hands the stored step to the summary unchanged: placeholders, query, form and pinned files.
func TestSummaryCallOfStep(t *testing.T) {
	steps := build(t, `{"title":"t","steps":[
 {"method":"GET","path":"/train/containers/7/model"},
 {"method":"PATCH","path":"/models/{{step1.id}}","body":{"name":"pigs v1"}}]}`)
	c := SummaryCall(steps[1])
	if c.Method != "PATCH" || c.Path != "/models/{{step1.id}}" || string(c.Body) != `{"name":"pigs v1"}` {
		t.Fatalf("call: %+v", c)
	}
	got := platform.Summarize(context.Background(), nil, c)
	if !strings.HasPrefix(got, `Rename the model created by step 1 to "pigs v1"`) {
		t.Fatalf("summary: %q", got)
	}
}

func TestNormalizeArgsTextScalars(t *testing.T) {
	a, err := NormalizeArgs([]byte(`{"b":2,"a":"x","c":true}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := NormalizeArgs([]byte(`{"c":"true","a":"x","b":"2"}`))
	if string(a) != string(b) || string(a) != `{"a":"x","b":"2","c":"true"}` {
		t.Fatalf("%s vs %s", a, b)
	}
	if _, err := NormalizeArgs([]byte(`{"a":1,"a":2}`)); err == nil {
		t.Fatal("duplicate keys accepted")
	}
	if e, _ := NormalizeArgs(nil); string(e) != "{}" {
		t.Fatalf("empty: %s", e)
	}
}

func TestMatchesTool(t *testing.T) {
	st := store.PlanStep{Tool: "build_inference_image", Method: "POST", Path: "/container-images/build",
		Args: json.RawMessage(`{"architecture":"amd64","model_id":"{{step1.id}}","name":"img","tag":"v1","template_id":"1"}`)}
	resolve := func(n int) (string, bool) { return "5", n == 1 }
	req := platform.Request{Method: "POST", Path: "/container-images/build", Body: json.RawMessage(`{"repository":"u/img"}`)}
	good := json.RawMessage(`{"template_id":1,"model_id":"5","name":"img","tag":"v1","architecture":"amd64"}`)
	if ok, why := MatchesTool(st, "build_inference_image", good, req, resolve); !ok {
		t.Fatalf("exact tool call: %s", why)
	}
	cases := map[string]struct {
		tool string
		args string
	}{
		"other tool":     {"update_model", string(good)},
		"changed tag":    {"build_inference_image", `{"template_id":1,"model_id":5,"name":"img","tag":"v2","architecture":"amd64"}`},
		"other model":    {"build_inference_image", `{"template_id":1,"model_id":6,"name":"img","tag":"v1","architecture":"amd64"}`},
		"extra argument": {"build_inference_image", `{"template_id":1,"model_id":5,"name":"img","tag":"v1","architecture":"amd64","x":1}`},
	}
	for name, c := range cases {
		if ok, _ := MatchesTool(st, c.tool, json.RawMessage(c.args), req, resolve); ok {
			t.Errorf("%s matched", name)
		}
	}
	bad := req
	bad.Path = "/container-images/other"
	if ok, _ := MatchesTool(st, "build_inference_image", good, bad, resolve); ok {
		t.Error("other path matched")
	}
	if ok, _ := MatchesTool(st, "build_inference_image", good, req, func(int) (string, bool) { return "", false }); ok {
		t.Error("unresolved placeholder matched")
	}
	raw := store.PlanStep{Method: "POST", Path: "/container-images/build"}
	if IsToolStep(raw) {
		t.Error("raw step is a tool step")
	}
}

func TestBuildStepStoresArgsOfToolWithSteps(t *testing.T) {
	st, _, _, err := BuildStep(StepSpec{Tool: "build-image", Args: json.RawMessage(`{"template_id":1,"model_id":"{{step1.id}}","name":"img","tag":"v1","architecture":"amd64"}`)}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if st.Tool != "build_inference_image" || !IsToolStep(st) || !strings.Contains(string(st.Args), `"model_id":"{{step1.id}}"`) ||
		!strings.Contains(string(st.Args), `"template_id":"1"`) {
		t.Fatalf("step = %+v args %s", st, st.Args)
	}
	list, _, _, err := BuildStep(StepSpec{Tool: "list_templates"}, 1)
	if err == nil && IsToolStep(list) {
		t.Fatal("a tool without steps became a tool step")
	}
	up, _, _, err := BuildStep(StepSpec{Method: "PATCH", Path: "/datasets/5", Body: json.RawMessage(`{"name":"x"}`)}, 1)
	if err != nil || IsToolStep(up) {
		t.Fatalf("raw step: %+v %v", up, err)
	}
}

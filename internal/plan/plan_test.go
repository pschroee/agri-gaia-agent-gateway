// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package plan

import (
	"strings"
	"testing"

	"agw/internal/delegation"
	"agw/internal/platform"
	"agw/internal/store"
)

// classify is the classification of the gateway until #63 delivers the effects.
func classify(r platform.Request) (string, string) {
	return FallbackEffect(r), delegation.Classify(r).Action
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
	text := Preview("Model and rename", steps)
	for _, want := range []string{
		"Plan: Model and rename\n",
		"3 steps. Approving runs them in this order",
		"1. WRITE · GET /train/containers/7/model\n   create the model\n",
		"2. WRITE · PATCH /models/{{step1.id}}\n   {\n     \"name\": \"pigs v1\"\n   }\n",
		"3. IRREVERSIBLE (own approval) · DELETE /models/{{step1.id}}\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("preview misses %q in:\n%s", want, text)
		}
	}
}

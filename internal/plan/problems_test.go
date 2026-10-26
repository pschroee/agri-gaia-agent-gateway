// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package plan

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// problemsOf parses and builds a proposal like ProposePlan does and returns the joined problems.
func problemsOf(t *testing.T, raw string) (Problems, error) {
	t.Helper()
	p, perr := ParseProposal([]byte(raw), testLimits)
	var berr error
	if len(p.Steps) > 0 {
		_, berr = Build(p, classify)
	}
	err := Join(perr, berr)
	var ps Problems
	if err != nil && !errors.As(err, &ps) {
		t.Fatalf("not a Problems: %T %v", err, err)
	}
	return ps, err
}

// #218: one problem reads exactly as the single error did before.
func TestProblemsSingleKeepsOldText(t *testing.T) {
	var p Problems
	p.add(3, errors.New("step 3: reading calls do not belong in a plan"))
	if got := p.Error(); got != "step 3: reading calls do not belong in a plan" {
		t.Fatalf("one step problem: %q", got)
	}
	var q Problems
	q.add(0, errors.New("title must have 1 to 120 characters"))
	if got := q.Error(); got != "title must have 1 to 120 characters" {
		t.Fatalf("one plan problem: %q", got)
	}
	if (Problems{}).err() != nil {
		t.Fatal("no problems must be a nil error")
	}
}

func TestProblemsListAndCap(t *testing.T) {
	var p Problems
	for i := 21; i >= 1; i-- { // added backwards: the list is sorted by step
		p.add(i, fmt.Errorf("bad %d", i))
	}
	lines := strings.Split(p.Error(), "\n")
	if len(lines) != 1+20+1 {
		t.Fatalf("lines: %d\n%s", len(lines), p.Error())
	}
	if lines[0] != "plan refused, 21 problems; fix all of them and propose the whole plan once more:" {
		t.Fatalf("head: %q", lines[0])
	}
	if lines[1] != "- step 1: bad 1" || lines[20] != "- step 20: bad 20" || lines[21] != "- … and 1 more" {
		t.Fatalf("lines: %q %q %q", lines[1], lines[20], lines[21])
	}
}

func TestJoinMergesAndDropsDuplicates(t *testing.T) {
	var a, b Problems
	a.add(0, errors.New("title must have 1 to 120 characters"))
	a.add(2, errors.New("description longer than 200 characters"))
	b.add(2, errors.New("step 2: description longer than 200 characters"))
	b.add(1, errors.New("step 1: unknown tool \"x\""))
	err := Join(a, nil, b)
	var ps Problems
	if !errors.As(err, &ps) || len(ps) != 3 {
		t.Fatalf("joined: %v", err)
	}
	if ps[0].Step != 0 || ps[1].Step != 1 || ps[2].Step != 2 {
		t.Fatalf("order: %+v", ps)
	}
	if Join(nil, nil) != nil {
		t.Fatal("nothing to join must be nil")
	}
}

// A budget above the limit, an empty title, a long description and an unknown field of a step: one answer.
func TestParseProposalCollectsAll(t *testing.T) {
	raw := `{"title":"  ","mode":"autonomous","budget":{"max_auto_turns":80},"steps":[
 {"method":"POST","path":"/datasets","description":"` + strings.Repeat("x", 201) + `"},
 {"method":"POST","path":"/datasets","bogus":1}]}`
	p, err := ParseProposal([]byte(raw), testLimits)
	var ps Problems
	if !errors.As(err, &ps) || len(ps) != 4 {
		t.Fatalf("problems: %v", err)
	}
	if len(p.Steps) != 2 {
		t.Fatalf("the other steps must stay: %+v", p.Steps)
	}
	msg := err.Error()
	for _, want := range []string{"plan refused, 4 problems", "title must have 1 to 120", "max_auto_turns is 80, at most 50", "step 1: description longer than 200", "step 2: json: unknown field \"bogus\""} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing %q in\n%s", want, msg)
		}
	}
}

func TestUnwrapTextPlanStepsAndStep(t *testing.T) {
	step1 := `{"method":"POST","path":"/datasets"}`
	step2 := `{"tool":"start_training","args":{"train_container_id":7}}`
	asText := func(s string) string { b, _ := json.Marshal(s); return string(b) }
	forms := map[string]string{
		"plan":  asText(`{"title":"t","steps":[` + step1 + `,` + step2 + `]}`),
		"steps": `{"title":"t","steps":` + asText(`[`+step1+`,`+step2+`]`) + `}`,
		"step":  `{"title":"t","steps":[` + asText(step1) + `,` + step2 + `]}`,
	}
	for name, raw := range forms {
		p, err := ParseProposal([]byte(raw), testLimits)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if p.Title != "t" || len(p.Steps) != 2 || p.Steps[0].Path != "/datasets" || p.Steps[1].Tool != "start_training" {
			t.Errorf("%s: %+v", name, p)
		}
	}
	// A title or a description that looks like JSON text stays text.
	p, err := ParseProposal([]byte(`{"title":"{\"a\":1}","steps":[{"method":"POST","path":"/datasets","description":"[1]"}]}`), testLimits)
	if err != nil || p.Title != `{"a":1}` || p.Steps[0].Description != "[1]" {
		t.Fatalf("text kept: %+v %v", p, err)
	}
}

// Duplicate keys inside the text of a step are still refused: the text is unpacked as text, not decoded.
func TestUnwrapTextKeepsDuplicateKeys(t *testing.T) {
	inner, _ := json.Marshal(`{"method":"POST","path":"/train/config","body":{"a":1,"a":2}}`)
	_, err := ParseProposal([]byte(`{"title":"t","steps":[`+string(inner)+`]}`), testLimits)
	if err == nil || !strings.Contains(err.Error(), "duplicate key") {
		t.Fatalf("want the duplicate key refused, got %v", err)
	}
	plan, _ := json.Marshal(`{"title":"t","title":"u","steps":[{"method":"POST","path":"/datasets"}]}`)
	if _, err := ParseProposal(plan, testLimits); err == nil || !strings.Contains(err.Error(), "duplicate key") {
		t.Fatalf("want the duplicate key in the plan text refused, got %v", err)
	}
}

// A broken array leaves a lone string behind (the demo chat, attempt 1): the answer says what a step is.
func TestStepAsBareStringProblem(t *testing.T) {
	p, err := ParseProposal([]byte(`{"title":"t","steps":[{"method":"POST","path":"/datasets"},"create the model"]}`), testLimits)
	if err == nil || !strings.Contains(err.Error(), `step 2: a step is a JSON object {"tool": …, "args": {…}} or {"method": …, "path": …}, not the text "create the model"`) {
		t.Fatalf("got %v", err)
	}
	if len(p.Steps) != 2 {
		t.Fatalf("steps: %+v", p.Steps)
	}
	if _, berr := Build(p, classify); berr != nil {
		t.Fatalf("Build must skip the step that did not read, got %v", berr)
	}
}

// The demo plan with its mistakes: the CLI name with underscores resolves, an unknown tool and a reference to a
// step that only works on an object are both reported, with the step to write instead.
func TestBuildCollectsAllSteps(t *testing.T) {
	raw := `{"title":"Demo","steps":[
 {"tool":"model_from_training","args":{"train_container_id":7}},
 {"tool":"nonsense_tool","args":{}},
 {"tool":"update_model","args":{"model_id":"{{step1.id}}","name":"pigs"}},
 {"tool":"update_model","args":{"model_id":"{{step3.id}}","name":"pigs v2"}}]}`
	ps, err := problemsOf(t, raw)
	if err == nil || len(ps) != 2 {
		t.Fatalf("problems: %v", err)
	}
	if ps[0].Step != 2 || !strings.Contains(ps[0].Msg, `unknown tool "nonsense_tool"`) {
		t.Errorf("first: %+v", ps[0])
	}
	if ps[1].Step != 4 || !strings.Contains(ps[1].Msg, `{{step3.id}} refers to step 3, which does not create an object; step 3 works on the object of step 1, so write "{{step1.id}}"`) {
		t.Errorf("second: %+v", ps[1])
	}
	// Without the mistakes the alias is stored under the tool's own name.
	steps := build(t, `{"title":"t","steps":[{"tool":"model_from_training","args":{"train_container_id":7}}]}`)
	if steps[0].Tool != "create_model_from_training" {
		t.Errorf("stored tool: %q", steps[0].Tool)
	}
}

// A reference to a step that did not build is not a second problem.
func TestBuildSkipsRefsToFailedStep(t *testing.T) {
	ps, err := problemsOf(t, `{"title":"t","steps":[
 {"tool":"nonsense_tool"},
 {"tool":"update_model","args":{"model_id":"{{step1.id}}","name":"x"}}]}`)
	if err == nil || len(ps) != 1 || ps[0].Step != 1 {
		t.Fatalf("want the one problem of step 1, got %v", err)
	}
}

func TestPlanLookupAliases(t *testing.T) {
	for in, want := range map[string]string{
		"start_training":                   "start_training",
		"mcp_platform_start_training":      "start_training",
		"platform_start_training":          "start_training",
		"model_from_training":              "create_model_from_training",
		"mcp_platform_model_from_training": "create_model_from_training",
		"create-model-from-training":       "create_model_from_training",
		"build_image":                      "build_inference_image",
		" update_model ":                   "update_model",
		"mcp_platform_create_deployment":   "create_deployment",
		"start-deployment":                 "start_deployment",
	} {
		tool, ok := planLookup(in)
		if !ok || tool.Name != want {
			t.Errorf("%q: %q %v, want %q", in, tool.Name, ok, want)
		}
	}
	if _, ok := planLookup("nonsense_tool"); ok {
		t.Error("nonsense_tool found")
	}
	err := unknownToolError("train")
	if !strings.Contains(err.Error(), "did you mean") || !strings.Contains(err.Error(), "start_training") {
		t.Errorf("suggestions: %v", err)
	}
	if got := unknownToolError("zzzz").Error(); got != `unknown tool "zzzz"` {
		t.Errorf("no suggestion: %q", got)
	}
	if n := strings.Count(unknownToolError("training").Error(), ","); n > 2 {
		t.Errorf("at most three suggestions: %v", unknownToolError("training"))
	}
}

func TestUnknownArgumentListsParams(t *testing.T) {
	ps, err := problemsOf(t, `{"title":"t","steps":[{"tool":"update_model","args":{"model_id":1,"bogus":"x"}}]}`)
	if err == nil || len(ps) != 1 {
		t.Fatalf("problems: %v", err)
	}
	want := `step 1: unknown argument "bogus"; update_model takes: model_id, name, input_name, input_datatype, input_shape, input_semantics, output_name, output_datatype, output_shape, output_labels`
	if err.Error() != want {
		t.Errorf("got  %s\nwant %s", err.Error(), want)
	}
}

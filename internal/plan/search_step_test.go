// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package plan

import (
	"encoding/json"
	"strings"
	"testing"

	"agw/internal/delegation"
	"agw/internal/platform"
)

const searchTrainStep = `{"tool":"create_training","args":{"provider":"Torchvision","architecture":"EfficientNet","category":"Classification","dataset_id":1,"train_config":{"epochs":1}}}`

func searchStep(base, budget, path string) string {
	return `{"tool":"propose_search","args":{"title":"t","path":"` + path + `","base_container_id":` + base +
		`,"space":{"lr":{"type":"float","min":0.001,"max":0.1,"scale":"log"}},"budget":` + budget + `}}`
}

const okBudget = `{"max_runs":3,"max_compute_minutes":30}`

// A search step builds with the placeholder of a create_training step, stores the form with the placeholder
// restored, and its object is what later steps name (issue #244, E1, E3).
func TestPlanSearchStepBuilds(t *testing.T) {
	steps := build(t, `{"title":"t","steps":[`+searchTrainStep+`,`+searchStep(`"{{step1.id}}"`, okBudget, "template")+
		`,{"tool":"create_model_from_training","args":{"train_container_id":"{{step2.id}}"}}]}`)
	if len(steps) != 3 || !IsSearchStep(steps[1]) {
		t.Fatalf("steps: %+v", steps)
	}
	if !strings.Contains(string(steps[1].Form), `"base_container_id":["{{step1.id}}"]`) {
		t.Fatalf("the placeholder is not stored in the form: %s", steps[1].Form)
	}
	if !steps[1].Covered || steps[1].Method != "POST" || steps[1].Path != platform.SearchPrefix {
		t.Fatalf("search step: %+v", steps[1])
	}
	if runs, minutes, ok := SearchLimits(steps[1]); !ok || runs != 3 || minutes != 30 {
		t.Fatalf("limits: %d %d %v", runs, minutes, ok)
	}
}

// The step is matched on its form after the placeholder is resolved: the exact call matches, another space or
// another base does not.
func TestPlanSearchStepMatchesResolvedForm(t *testing.T) {
	steps := build(t, `{"title":"t","steps":[`+searchTrainStep+`,`+searchStep(`"{{step1.id}}"`, okBudget, "template")+`]}`)
	tool, _ := platform.Lookup("propose_search")
	call := func(base, budget string) platform.Request {
		args := `{"title":"t","path":"template","base_container_id":` + base +
			`,"space":{"lr":{"type":"float","min":0.001,"max":0.1,"scale":"log"}},"budget":` + budget + `}`
		r, err := tool.Build(json.RawMessage(args))
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	resolve := func(n int) (string, bool) { return "63", n == 1 }
	if ok, why := Matches(steps[1], call("63", okBudget), resolve); !ok {
		t.Fatalf("the exact call does not match: %s", why)
	}
	if ok, why := Matches(steps[1], call("64", okBudget), resolve); ok || why != "form" {
		t.Fatalf("another base matched: %v %s", ok, why)
	}
	if ok, _ := Matches(steps[1], call("63", `{"max_runs":4,"max_compute_minutes":30}`), resolve); ok {
		t.Fatal("another budget matched")
	}
}

// Refused at the proposal, all problems at once (issue #218): the notebook way, a missing limit, a base that is
// not a create_training step, a second search.
func TestPlanSearchStepValidates(t *testing.T) {
	cases := map[string]struct {
		steps string
		want  []string
	}{
		// The notebook way needs files, which a plan step cannot give for a search: refused, whichever check fires.
		"notebook path": {searchStep(`7`, okBudget, "notebook"), []string{"base_container_id belongs to path template"}},
		"no limits":     {searchStep(`7`, `{"max_runs":3}`, "template"), []string{"max_runs and budget.max_compute_minutes"}},
		"base not a training": {`{"tool":"split_dataset","args":{"dataset_id":1}},` + searchStep(`"{{step1.id}}"`, okBudget, "template"),
			[]string{"must name the step that creates the training container"}},
		"two searches": {searchStep(`7`, okBudget, "template") + `,` + searchStep(`8`, okBudget, "template"),
			[]string{"holds one training search"}},
		"all at once": {`{"tool":"split_dataset","args":{"dataset_id":1}},` + searchStep(`"{{step1.id}}"`, `{"max_runs":3}`, "template"),
			[]string{"max_compute_minutes", "must name the step that creates the training container"}},
	}
	for name, c := range cases {
		p := proposal(t, `{"title":"t","steps":[`+c.steps+`]}`)
		_, err := Build(p, classify)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		for _, w := range c.want {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("%s: %q missing in %v", name, w, err)
			}
		}
	}
}

// A placeholder anywhere else in a form is still refused.
func TestPlanSearchStepPlaceholderOnlyInBase(t *testing.T) {
	raw := `{"title":"t","steps":[` + searchTrainStep + `,{"tool":"propose_search","args":{"title":"{{step1.id}}","path":"template",` +
		`"base_container_id":"{{step1.id}}","space":{"lr":{"type":"float","min":0.001,"max":0.1}},"budget":` + okBudget + `}}]}`
	if _, err := Build(proposal(t, raw), classify); err == nil {
		t.Fatal("a placeholder in the title was accepted")
	}
}

// The trials of the search count as compute calls of the plan (issue #244, E4).
func TestPlanSearchStepComputeCount(t *testing.T) {
	steps := build(t, `{"title":"t","steps":[`+searchTrainStep+`,`+searchStep(`"{{step1.id}}"`, `{"max_runs":4,"max_compute_minutes":20}`, "template")+`]}`)
	if got := SearchRuns(steps); got != 4 {
		t.Errorf("search runs: %d", got)
	}
	if got := SearchMinutes(steps); got != 20 {
		t.Errorf("search minutes: %d", got)
	}
	if got, base := ComputeSteps(steps), ComputeSteps(steps[:1]); got != base+4 {
		t.Errorf("compute steps: %d, without the search %d", got, base)
	}
}

// An autonomous plan with a search gets the rights the trials need.
func TestPlanSearchStepGrant(t *testing.T) {
	steps := build(t, `{"title":"t","steps":[`+searchTrainStep+`,`+searchStep(`"{{step1.id}}"`, okBudget, "template")+`]}`)
	del, err := Grant(steps, SampleStep)
	if err != nil {
		t.Fatal(err)
	}
	var run, upd bool
	for _, r := range del.Rules {
		run = run || (r.Action == "run" && r.Resource == "training")
		upd = upd || (r.Action == "update" && r.Resource == "search")
	}
	if !run || !upd {
		t.Fatalf("rules: %+v", del.Rules)
	}
}

// B20 (demo 6 of 27.10.2026): the agent's own title and note of a search step are no part of the match – the plan
// said "Laeufe", the call "Läufe". Everything that decides rights or compute stays exact.
func TestPlanSearchStepIgnoresFreeText(t *testing.T) {
	step := `{"tool":"propose_search","args":{"title":"Suche Laeufe","note":"Genau 3 Laeufe","path":"template","base_container_id":"{{step1.id}}",` +
		`"space":{"lr":{"type":"float","min":0.001,"max":0.1,"scale":"log"}},"budget":` + okBudget + `}}`
	steps := build(t, `{"title":"t","steps":[`+searchTrainStep+`,`+step+`]}`)
	tool, _ := platform.Lookup("propose_search")
	call := func(title, note, budget, space string) platform.Request {
		args := `{"title":"` + title + `","note":"` + note + `","path":"template","base_container_id":63,"space":` + space + `,"budget":` + budget + `}`
		r, err := tool.Build(json.RawMessage(args))
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	space := `{"lr":{"type":"float","min":0.001,"max":0.1,"scale":"log"}}`
	resolve := func(n int) (string, bool) { return "63", n == 1 }
	if ok, why := Matches(steps[1], call("Suche Läufe", "Genau 3 Läufe, kein Abbruch", okBudget, space), resolve); !ok {
		t.Fatalf("other free text of the agent made the call differ: %s", why)
	}
	if ok, _ := Matches(steps[1], call("Suche Laeufe", "Genau 3 Laeufe", `{"max_runs":3,"max_compute_minutes":60}`, space), resolve); ok {
		t.Fatal("another compute budget matched")
	}
	if ok, _ := Matches(steps[1], call("Suche Laeufe", "Genau 3 Laeufe", okBudget, `{"lr":{"type":"float","min":0.001,"max":0.5,"scale":"log"}}`), resolve); ok {
		t.Fatal("another space matched")
	}
}

// A placeholder that stands alone in a form field other than base_container_id is refused with the cause, not with the
// text message (#254, E3).
func TestPlaceholderAloneInFormFieldMessage(t *testing.T) {
	err := checkFormSentinels(map[string][]string{"title": {sentinel(1)}})
	if err == nil || !strings.Contains(err.Error(), "only allowed as base_container_id of a training search step") || strings.Contains(err.Error(), "inside a text") {
		t.Fatalf("message: %v", err)
	}
	if err := checkFormSentinels(map[string][]string{searchBaseField: {sentinel(1)}}); err != nil {
		t.Fatalf("base_container_id: %v", err)
	}
}

// A placeholder with text around it keeps the text message (#254, E3).
func TestPlaceholderInsideTextMessage(t *testing.T) {
	err := checkFormSentinels(map[string][]string{"title": {"run " + sentinel(1)}})
	if err == nil || !strings.Contains(err.Error(), "inside a text") {
		t.Fatalf("message: %v", err)
	}
}

// B25 (Phase B #244–#251): the search step of a plan uses compute time – its trials train on the platform – and the
// card names it compute, not write. Its trials are counted once, by the budget, not once more for the step itself.
func TestPlanSearchStepEffectCompute(t *testing.T) {
	gateway := func(r platform.Request) (string, string, bool) {
		a := delegation.Classify(r)
		return string(a.Effect), a.Action, a.ConfirmEach
	}
	steps, err := Build(proposal(t, `{"title":"t","steps":[`+searchTrainStep+`,`+searchStep(`"{{step1.id}}"`, okBudget, "template")+`]}`), gateway)
	if err != nil {
		t.Fatal(err)
	}
	if steps[1].Effect != EffectCompute {
		t.Fatalf("effect of the search step: %q", steps[1].Effect)
	}
	if !steps[1].Covered {
		t.Fatalf("the search step must stay covered by the plan: %+v", steps[1])
	}
	// create_training (1) + the search's three trials (budget.max_runs).
	if n := ComputeSteps(steps); n != 1+3 {
		t.Fatalf("compute calls of the plan: %d", n)
	}
}

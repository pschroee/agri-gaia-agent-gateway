// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package exp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// benchPlan is a valid benchmark plan with one harmless and one attack cell.
func benchPlan(role string) *Plan {
	return &Plan{
		Purpose: PurposeBenchmark, Seed: 1, K: 1,
		Models: []Model{{ID: "deepseek/deepseek-flash", Role: role}},
		Cells: []Cell{
			{ID: "cli-service-no-" + role, Binding: "cli", Protection: "service", Model: role},
			{ID: "cli-service-yes-" + role, Binding: "cli", Protection: "service", Attack: true, Model: role},
		},
		Tasks:  []string{"t"},
		Limits: Limits{MaxCostUSD: 1, MaxTokens: 1000, MaxDuration: "30m"},
	}
}

func offlineTask(off *OfflineCheck) Task {
	t := taskWith(nil, map[string]any{"action": "read", "resource": "dataset"})
	t.Check = CheckSpec{Offline: off}
	return t
}

// BM1: a benchmark plan needs no minimum number of tasks, an example in it is refused like in a main plan.
func TestValidateBenchmarkPurpose(t *testing.T) {
	p := benchPlan("deepseek-flash")
	task := offlineTask(&OfflineCheck{Kind: OfflineSolution})
	task.ID = "t"
	if problems := Validate(p, []Task{task}, nil); len(problems) != 0 {
		t.Fatalf("one task in a benchmark plan: %v", problems)
	}
	task.Example = true
	if problems := Validate(p, []Task{task}, nil); !hasProblem(problems, "is an example and cannot be part of a benchmark plan") {
		t.Fatalf("example task: %v", problems)
	}
	task.Example = false
	ex := Manifest{ID: "fx", Kind: KindDataset, Owner: "primary", Example: true}
	if problems := Validate(p, []Task{task}, []Manifest{ex}); !hasProblem(problems, "cannot be part of a benchmark plan") {
		t.Fatalf("example manifest: %v", problems)
	}
	p.Purpose = "weekly"
	if problems := Validate(p, []Task{task}, nil); !hasProblem(problems, "purpose must be") {
		t.Fatalf("unknown purpose: %v", problems)
	}
}

// BM1: roles are free in a benchmark plan, a main plan keeps main/second and its main role.
func TestValidateRolesFree(t *testing.T) {
	task := offlineTask(&OfflineCheck{Kind: OfflineSolution})
	if problems := Validate(benchPlan("deepseek-flash"), []Task{task}, nil); len(problems) != 0 {
		t.Fatalf("deepseek-flash: %v", problems)
	}
	for _, bad := range []string{"Main", "1abc", "has_underscore", "", strings.Repeat("a", 33)} {
		p := benchPlan("ok")
		p.Models[0].Role = bad
		if problems := Validate(p, []Task{task}, nil); !hasProblem(problems, "role must match") {
			t.Fatalf("role %q must be refused: %v", bad, problems)
		}
	}
	main := benchPlan("deepseek-flash")
	main.Purpose = PurposePilot
	problems := Validate(main, []Task{task}, nil)
	if !hasProblem(problems, "role must be") || !hasProblem(problems, `no model has the role "main"`) {
		t.Fatalf("free role in a pilot plan: %v", problems)
	}
	second := benchPlan("second")
	second.Purpose = PurposePilot
	if problems := Validate(second, []Task{task}, nil); !hasProblem(problems, `no model has the role "main"`) {
		t.Fatalf("pilot without main: %v", problems)
	}
}

func TestValidateOfflineCheck(t *testing.T) {
	p := benchPlan("main")
	cases := []struct {
		name string
		off  *OfflineCheck
		want string // empty: valid
	}{
		{"clip", &OfflineCheck{Kind: OfflineClip, Fahrten: []string{"F017", "F1204"}, Fields: []string{"richtung", "auto", "kennzeichen"}}, ""},
		{"solution", &OfflineCheck{Kind: OfflineSolution}, ""},
		{"coding", &OfflineCheck{Kind: OfflineCoding}, ""},
		{"bad kind", &OfflineCheck{Kind: "video"}, "check.offline.kind must be"},
		{"bad trip", &OfflineCheck{Kind: OfflineClip, Fahrten: []string{"X1"}}, `"X1" is not a trip`},
		{"short trip", &OfflineCheck{Kind: OfflineClip, Fahrten: []string{"F17"}}, `"F17" is not a trip`},
		{"bad field", &OfflineCheck{Kind: OfflineClip, Fields: []string{"farbe"}}, `"farbe" is not one of`},
		{"fields on solution", &OfflineCheck{Kind: OfflineSolution, Fields: []string{"auto"}}, "belong to kind"},
	}
	for _, c := range cases {
		problems := Validate(p, []Task{offlineTask(c.off)}, nil)
		if c.want == "" && len(problems) != 0 {
			t.Errorf("%s: %v", c.name, problems)
		}
		if c.want != "" && !hasProblem(problems, c.want) {
			t.Errorf("%s: want %q, got %v", c.name, c.want, problems)
		}
	}
	// Without offline an empty check stays refused; with offline other checks may stand next to it.
	empty := offlineTask(nil)
	if problems := Validate(p, []Task{empty}, nil); !hasProblem(problems, "check is empty") {
		t.Fatalf("empty check: %v", problems)
	}
	both := offlineTask(&OfflineCheck{Kind: OfflineSolution})
	both.Check.Created = &Created{Resource: "dataset", Count: 1}
	if problems := Validate(p, []Task{both}, nil); len(problems) != 0 {
		t.Fatalf("offline next to created: %v", problems)
	}
	if !both.Check.HasOnline() || offlineTask(&OfflineCheck{Kind: OfflineSolution}).Check.HasOnline() {
		t.Fatal("HasOnline")
	}
}

func TestValidateAttackFixtureListedWrong(t *testing.T) {
	p := benchPlan("main")
	clean := Manifest{ID: "ds-clean", Kind: KindDataset, Owner: "primary"}
	evil := Manifest{ID: "ds-evil", Kind: KindDataset, Owner: "primary", Attack: true, Replaces: "ds-clean"}
	task := offlineTask(&OfflineCheck{Kind: OfflineSolution})
	task.Fixtures = []string{"ds-clean"}
	task.AttackFixtures = []string{"ds-evil"}
	if problems := Validate(p, []Task{task}, []Manifest{clean, evil}); len(problems) != 0 {
		t.Fatalf("valid pair: %v", problems)
	}

	// attack: true under fixtures:
	wrong := task
	wrong.Fixtures = []string{"ds-clean", "ds-evil"}
	if problems := Validate(p, []Task{wrong}, []Manifest{clean, evil}); !hasProblem(problems, "attack fixture must be listed under attack_fixtures") {
		t.Fatalf("attack under fixtures: %v", problems)
	}
	// attack_fixtures with cells
	limited := evil
	limited.Cells = []string{"cli-service-yes-main"}
	if problems := Validate(p, []Task{task}, []Manifest{clean, limited}); !hasProblem(problems, "must not limit cells") {
		t.Fatalf("attack fixture with cells: %v", problems)
	}
	// replaces on a fixture of another kind
	model := clean
	model.Kind = KindModel
	if problems := Validate(p, []Task{task}, []Manifest{model, evil}); !hasProblem(problems, "the kinds must match") {
		t.Fatalf("replaces across kinds: %v", problems)
	}
	// replaces of a fixture the task does not list
	other := evil
	other.Replaces = "ds-unlisted"
	if problems := Validate(p, []Task{task}, []Manifest{clean, other}); !hasProblem(problems, "which the task does not list") {
		t.Fatalf("replaces of an unlisted fixture: %v", problems)
	}
	// attack_fixtures entry whose manifest is not an attack manifest
	plain := evil
	plain.Attack = false
	plain.Replaces = ""
	if problems := Validate(p, []Task{task}, []Manifest{clean, plain}); !hasProblem(problems, "must have attack: true") {
		t.Fatalf("non-attack manifest: %v", problems)
	}
	// replaces without attack
	if problems := Validate(p, []Task{offlineTask(&OfflineCheck{Kind: OfflineSolution})}, []Manifest{plain, {ID: "ds-r", Kind: KindDataset, Owner: "primary", Replaces: "ds-clean"}}); !hasProblem(problems, "replaces is only for attack fixtures") {
		t.Fatalf("replaces without attack: %v", problems)
	}
	// no attack cell in the plan
	noAttack := benchPlan("main")
	noAttack.Cells = noAttack.Cells[:1]
	if problems := Validate(noAttack, []Task{task}, []Manifest{clean, evil}); !hasProblem(problems, "no cell of the plan has attack: true") {
		t.Fatalf("plan without attack cell: %v", problems)
	}
}

func TestValidateTaskInternetAndCollect(t *testing.T) {
	p := benchPlan("main")
	task := offlineTask(&OfflineCheck{Kind: OfflineSolution})
	task.Internet = InternetRequest
	task.Collect = []CollectSpec{{Resource: "dataset", Name: "garage-loesung"}}
	if problems := Validate(p, []Task{task}, nil); len(problems) != 0 {
		t.Fatalf("valid: %v", problems)
	}
	task.Internet = "on"
	task.Collect = []CollectSpec{{Resource: "model", Name: "x"}, {Resource: "dataset", Name: "../x"}, {Resource: "dataset"}}
	problems := Validate(p, []Task{task}, nil)
	for _, want := range []string{`internet must be "off" or "request"`, "collect[0].resource", "collect[1].name must be a plain name", "collect[2].name is empty"} {
		if !hasProblem(problems, want) {
			t.Errorf("want %q in %v", want, problems)
		}
	}
}

func writeBase(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, BaseFile), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

const baseYAML = `owner: primary
datasets:
  - name: garage-clips-train
    id: 812
    filecount: 141
    total_filesize: 2712345678
    files_sha256: abc
`

func TestLoadBaseSet(t *testing.T) {
	if b, err := LoadBaseSet(t.TempDir()); b != nil || err != nil {
		t.Fatalf("missing file: %v %v", b, err)
	}
	b, err := LoadBaseSet(writeBase(t, baseYAML))
	if err != nil {
		t.Fatal(err)
	}
	d, ok := b.Lookup("garage-clips-train")
	if !ok || d.ID != 812 || d.Filecount != 141 || d.TotalFilesize != 2712345678 || d.FilesSHA256 != "abc" {
		t.Fatalf("lookup: %+v %v", d, ok)
	}
	if b.IDs()["garage-clips-train"] != "812" {
		t.Fatalf("ids: %v", b.IDs())
	}
	if _, err := LoadBaseSet(writeBase(t, baseYAML+"bogus: 1\n")); err == nil {
		t.Fatal("unknown field must be refused")
	}
	dup := baseYAML + "  - name: garage-clips-train\n    id: 9\n"
	if _, err := LoadBaseSet(writeBase(t, dup)); err == nil || !strings.Contains(err.Error(), "listed twice") {
		t.Fatalf("duplicate: %v", err)
	}
	var nilSet *BaseSet
	if _, ok := nilSet.Lookup("x"); ok {
		t.Fatal("nil set")
	}
}

func TestCompareBase(t *testing.T) {
	want := BaseDataset{Name: "garage-clips-train", ID: 812, Filecount: 141, TotalFilesize: 2712345678}
	same := map[string]any{"name": "garage-clips-train", "filecount": float64(141), "total_filesize": float64(2712345678)}
	if got := CompareBase(want, same); len(got) != 0 {
		t.Fatalf("unchanged: %v", got)
	}
	changed := map[string]any{"name": "garage-clips-train", "filecount": float64(140), "total_filesize": float64(2712345678)}
	got := CompareBase(want, changed)
	if len(got) != 1 || got[0] != "filecount 141 → 140" {
		t.Fatalf("filecount: %v", got)
	}
	renamed := map[string]any{"name": "x", "filecount": float64(141), "total_filesize": float64(1)}
	if got := CompareBase(want, renamed); len(got) != 2 {
		t.Fatalf("name and size: %v", got)
	}
	nested := map[string]any{"name": "garage-clips-train", "metadata": map[string]any{"filecount": float64(141), "total_filesize": float64(2712345678)}}
	if got := CompareBase(want, nested); len(got) != 0 {
		t.Fatalf("nested metadata: %v", got)
	}
	if got := CompareBase(want, map[string]any{"name": "garage-clips-train"}); len(got) != 2 {
		t.Fatalf("missing fields: %v", got)
	}
}

func TestResolveBaseRef(t *testing.T) {
	b, err := LoadBaseSet(writeBase(t, baseYAML))
	if err != nil {
		t.Fatal(err)
	}
	read := map[string]any{"action": "read", "resource": "dataset", "ids": []any{"base:garage-clips-train"}}
	task := taskWith(nil, read)
	task.Check = CheckSpec{Offline: &OfflineCheck{Kind: OfflineSolution}}
	if got := problemText(validateTask(task, testPlan, nil, b)); got != "" {
		t.Fatalf("valid base ref: %s", got)
	}
	raw, err := ResolveFixtureRefs(task.Delegation, nil, b.IDs())
	if err != nil {
		t.Fatal(err)
	}
	var back struct {
		Rules []struct {
			IDs []string `json:"ids"`
		} `json:"rules"`
	}
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if len(back.Rules) != 1 || len(back.Rules[0].IDs) != 1 || back.Rules[0].IDs[0] != "812" {
		t.Fatalf("resolved: %s", raw)
	}

	unknown := taskWith(nil, map[string]any{"action": "read", "resource": "dataset", "ids": []any{"base:nope"}})
	if got := problemText(validateTask(unknown, testPlan, nil, b)); !strings.Contains(got, `base dataset "nope"`) {
		t.Fatalf("unknown name: %s", got)
	}
	if got := problemText(validateTask(task, testPlan, nil, nil)); !strings.Contains(got, BaseFile+" does not list") {
		t.Fatalf("without grundbestand.yaml: %s", got)
	}
	if _, err := ResolveFixtureRefs(unknown.Delegation, nil, b.IDs()); err == nil {
		t.Fatal("unknown name must not resolve")
	}
	for _, action := range []string{"update", "delete"} {
		w := taskWith(nil, map[string]any{"action": action, "resource": "dataset", "ids": []any{"base:garage-clips-train"}})
		if got := problemText(validateTask(w, testPlan, nil, b)); !strings.Contains(got, "reading only") {
			t.Fatalf("%s on a base dataset: %s", action, got)
		}
	}
	uses := taskWith(nil, map[string]any{"action": "read", "resource": "dataset"})
	uses.UsesBase = true
	if got := problemText(validateTask(uses, testPlan, nil, nil)); !strings.Contains(got, "uses_base") {
		t.Fatalf("uses_base without file: %s", got)
	}
}

// A pilot plan is not refused for an attack fixture that no task of the plan names (the material folder may hold the
// fixtures of other plans); a task that names one is refused.
func TestPilotProblemsOnlyUsedAttackFixtures(t *testing.T) {
	p := benchPlan("main")
	p.Purpose = PurposePilot
	unused := Manifest{ID: "ds-evil", Kind: KindDataset, Owner: "primary", Attack: true}
	task := offlineTask(&OfflineCheck{Kind: OfflineSolution})
	if problems := PilotProblems(p, []Task{task}, []Manifest{unused}, nil); len(problems) != 0 {
		t.Fatalf("unused attack fixture: %v", problems)
	}
	task.Fixtures = []string{"ds-evil"}
	if problems := PilotProblems(p, []Task{task}, []Manifest{unused}, nil); !hasProblem(problems, "attack fixture") {
		t.Fatalf("task names an attack fixture: %v", problems)
	}
	task.Fixtures = nil
	task.AttackFixtures = []string{"ds-evil"}
	if problems := PilotProblems(p, []Task{task}, []Manifest{unused}, nil); !hasProblem(problems, "attack fixture") {
		t.Fatalf("task names it under attack_fixtures: %v", problems)
	}
}

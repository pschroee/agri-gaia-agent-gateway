// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

// Package exp reads the experiment material of the master's thesis (issue #134, X9): the
// experiment plan, its tasks and the fixture manifests. The formats are the ones the runner (X1)
// and the fixture seeding (X4, issue #129) use; validation checks referential integrity and refuses
// example files in a main plan. Seeding, verification and removal live in fixtures.go and work
// against the platform, never through a chat.
package exp

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Purposes of a plan.
const (
	PurposeMain  = "main"
	PurposePilot = "pilot"
)

// Model roles. The second model only runs the cells for H1 and H2 (outline 7.1).
const (
	RoleMain   = "main"
	RoleSecond = "second"
)

// Plan is the content of plan.yaml or pilot.yaml. The cells are listed explicitly: the second
// model does not run every combination of the other factors.
type Plan struct {
	Purpose        string   `yaml:"purpose"`
	Seed           int64    `yaml:"seed"`
	K              int      `yaml:"k"`
	Models         []Model  `yaml:"models"`
	Cells          []Cell   `yaml:"cells"`
	Tasks          []string `yaml:"tasks"`
	Limits         Limits   `yaml:"limits"`
	Pass1Threshold float64  `yaml:"pass1_threshold"`
	BudgetEUR      string   `yaml:"budget_eur"`
}

// Model names one model of the plan. ID may be a placeholder such as <HAUPTMODELL> until the
// model is fixed (XE5).
type Model struct {
	ID   string `yaml:"id"`
	Role string `yaml:"role"`
}

// Cell is one cell of the plan.
type Cell struct {
	ID         string `yaml:"id"`
	Binding    string `yaml:"binding"`
	Protection string `yaml:"protection"`
	Attack     bool   `yaml:"attack"`
	Model      string `yaml:"model"` // role of the model, main or second
	// Trace: syscall trace of this cell (X7.6/#152): on | off. Empty means on.
	Trace string `yaml:"trace,omitempty"`
}

// TraceValue resolves the plan's trace field: only "off" disables it, everything else (including
// an empty field) is "on", the default for experiments.
func TraceValue(v string) string {
	if v == "off" {
		return "off"
	}
	return "on"
}

// Limits are the bounds per run (X3). MaxDuration is a Go duration such as 90m.
type Limits struct {
	MaxCostUSD  float64 `yaml:"max_cost_usd"`
	MaxTokens   int64   `yaml:"max_tokens"`
	MaxDuration string  `yaml:"max_duration"`
}

// Problem is one finding of Validate. Where names the file or object, Text the finding.
type Problem struct {
	Where string
	Text  string
}

func (p Problem) String() string { return p.Where + ": " + p.Text }

// LoadPlan reads plan.yaml, the tasks it lists and the fixture manifests from the directories
// aufgaben/ and fixtures/ below dir. Unknown YAML fields are an error, so that a typo does not
// silently drop a rule or a check.
func LoadPlan(dir string) (*Plan, []Task, []Manifest, error) {
	return loadPlanFile(filepath.Join(dir, "plan.yaml"), dir)
}

// LoadPlanFile reads a plan file (plan.yaml or pilot.yaml). The material directories
// aufgaben/ and fixtures/ are taken from the directory of the plan file.
func LoadPlanFile(path string) (*Plan, []Task, []Manifest, error) {
	return loadPlanFile(path, filepath.Dir(path))
}

func loadPlanFile(path, dir string) (*Plan, []Task, []Manifest, error) {
	p, err := loadYAML[Plan](path)
	if err != nil {
		return nil, nil, nil, err
	}
	var tasks []Task
	seen := make(map[string]bool, len(p.Tasks))
	for _, id := range p.Tasks {
		id = strings.TrimSpace(id)
		if id == "" {
			return nil, nil, nil, fmt.Errorf("%s: an entry of tasks is empty", filepath.Base(path))
		}
		if seen[id] {
			return nil, nil, nil, fmt.Errorf("%s: task %q is listed twice", filepath.Base(path), id)
		}
		seen[id] = true
		taskPath := filepath.Join(dir, "aufgaben", id+".yaml")
		t, err := loadYAML[Task](taskPath)
		if err != nil {
			return nil, nil, nil, err
		}
		if t.ID != id {
			return nil, nil, nil, fmt.Errorf("%s: id is %q, but the plan lists %q", taskPath, t.ID, id)
		}
		tasks = append(tasks, *t)
	}
	manifests, err := loadManifests(filepath.Join(dir, "fixtures"))
	if err != nil {
		return nil, nil, nil, err
	}
	return p, tasks, manifests, nil
}

// loadYAML reads one document and refuses unknown fields.
func loadYAML[T any](path string) (*T, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	var v T
	if err := dec.Decode(&v); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%s is empty", path)
		}
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &v, nil
}

// Verify checks the plan, its tasks and the fixture manifests. It collects every finding
// instead of stopping at the first one, so that the material can be fixed in one pass.
func Validate(p *Plan, ts []Task, ms []Manifest) []Problem {
	if p == nil {
		return []Problem{{"plan", "missing"}}
	}
	problems := validatePlan(p)
	fixtures := make(map[string]bool, len(ms))
	for _, m := range ms {
		fixtures[m.ID] = true
	}
	for _, t := range ts {
		problems = append(problems, validateTask(t, p, fixtures)...)
	}
	cells := make(map[string]bool, len(p.Cells))
	for _, c := range p.Cells {
		cells[c.ID] = true
	}
	for _, m := range ms {
		problems = append(problems, validateManifest(m, p.Purpose, cells)...)
	}
	return problems
}

// PilotProblems adds the rules of X7.1 to Validate: a pilot plan must not use an attack
// fixture and must not reuse a task of the main plan. mainTasks are the task IDs listed in plan.yaml
// next to the pilot plan.
func PilotProblems(p *Plan, ts []Task, ms []Manifest, mainTasks map[string]bool) []Problem {
	if p == nil || p.Purpose != PurposePilot {
		return nil
	}
	var out []Problem
	for _, m := range ms {
		if m.Attack {
			out = append(out, Problem{"fixture " + m.ID, "is an attack fixture and cannot be part of a pilot plan"})
		}
	}
	for _, t := range ts {
		if mainTasks[t.ID] {
			out = append(out, Problem{"task " + t.ID, "is also listed in a main plan; a pilot uses its own tasks"})
		}
	}
	return out
}

// MainTaskIDs reads the task IDs of plan.yaml in dir. It returns nil when the file does not exist,
// so that material without a main plan still validates.
func MainTaskIDs(dir string) (map[string]bool, error) {
	path := filepath.Join(dir, "plan.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	var p Plan
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	out := make(map[string]bool, len(p.Tasks))
	for _, id := range p.Tasks {
		out[strings.TrimSpace(id)] = true
	}
	return out, nil
}

func validatePlan(p *Plan) []Problem {
	var out []Problem
	add := func(format string, a ...any) { out = append(out, Problem{"plan", fmt.Sprintf(format, a...)}) }
	switch p.Purpose {
	case PurposeMain, PurposePilot:
	default:
		add("purpose must be %q or %q, not %q", PurposeMain, PurposePilot, p.Purpose)
	}
	if p.K < 1 {
		add("k must be at least 1, not %d", p.K)
	}
	if len(p.Models) == 0 {
		add("no model is listed")
	}
	roles := make(map[string]bool, len(p.Models))
	modelIDs := make(map[string]bool, len(p.Models))
	for i, m := range p.Models {
		where := fmt.Sprintf("model %d", i+1)
		if strings.TrimSpace(m.ID) == "" {
			add("%s: id is empty", where)
		} else if modelIDs[m.ID] {
			add("%s: id %q is listed twice", where, m.ID)
		}
		modelIDs[m.ID] = true
		switch m.Role {
		case RoleMain, RoleSecond:
			roles[m.Role] = true
		default:
			add("%s: role must be %q or %q, not %q", where, RoleMain, RoleSecond, m.Role)
		}
	}
	if !roles[RoleMain] {
		add("no model has the role %q", RoleMain)
	}
	if len(p.Cells) == 0 {
		add("no cell is listed; the cells are enumerated explicitly, not derived")
	}
	cellIDs := make(map[string]bool, len(p.Cells))
	for i, c := range p.Cells {
		where := fmt.Sprintf("cell %d", i+1)
		if strings.TrimSpace(c.ID) == "" {
			add("%s: id is empty", where)
		} else if cellIDs[c.ID] {
			add("%s: id %q is listed twice", where, c.ID)
		}
		cellIDs[c.ID] = true
		if !bindings[c.Binding] {
			add("%s: binding must be one of %s, not %q", where, strings.Join(bindingList(), ", "), c.Binding)
		}
		if !protections[c.Protection] {
			add("%s: protection must be one of %s, not %q", where, strings.Join(protectionList(), ", "), c.Protection)
		}
		if c.Model != "" && !roles[c.Model] {
			add("%s: model %q is not a role of a listed model", where, c.Model)
		}
		if c.Model == "" {
			add("%s: model is empty; name the role of the model, main or second", where)
		}
		if c.Trace != "" && c.Trace != "on" && c.Trace != "off" {
			add("%s: trace must be \"on\" or \"off\", not %q", where, c.Trace)
		}
	}
	if p.Limits.MaxCostUSD < 0 {
		add("limits.max_cost_usd must not be negative, not %v", p.Limits.MaxCostUSD)
	}
	if p.Limits.MaxTokens < 1 {
		add("limits.max_tokens must be at least 1, not %d", p.Limits.MaxTokens)
	}
	if p.Limits.MaxDuration == "" {
		add("limits.max_duration is empty")
	} else if d, err := time.ParseDuration(p.Limits.MaxDuration); err != nil {
		add("limits.max_duration is not a duration such as 90m: %v", err)
	} else if d <= 0 {
		add("limits.max_duration must be positive, not %s", d)
	}
	if p.Pass1Threshold < 0 || p.Pass1Threshold > 1 {
		add("pass1_threshold must be between 0 and 1, not %v", p.Pass1Threshold)
	}
	if strings.TrimSpace(p.BudgetEUR) == "" {
		add("budget_eur is empty; a placeholder such as <BUDGET_EUR> is allowed")
	}
	if p.Purpose == PurposeMain && len(p.Tasks) < 20 {
		add("a main plan needs at least 20 tasks, %d listed", len(p.Tasks))
	}
	return out
}

var (
	bindings    = map[string]bool{"mcp": true, "api": true, "cli": true, "cli+mcp": true}
	protections = map[string]bool{"none": true, "instruction": true, "service": true}
)

func bindingList() []string    { return []string{"mcp", "api", "cli", "cli+mcp"} }
func protectionList() []string { return []string{"none", "instruction", "service"} }

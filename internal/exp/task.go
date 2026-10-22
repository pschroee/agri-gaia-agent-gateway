// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package exp

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"agw/internal/delegation"
	"go.yaml.in/yaml/v3"
)

// Kinds of a task. An allowed task is solvable with exactly the delegated rights; a tempting
// task invites a shortcut that the delegation does not cover.
const (
	KindAllowed  = "allowed"
	KindTempting = "tempting"
)

// Task is the content of aufgaben/<id>.yaml.
type Task struct {
	ID         string         `yaml:"id"`
	Title      string         `yaml:"title"`
	Prompt     string         `yaml:"prompt"`
	Delegation map[string]any `yaml:"delegation"`
	Fixtures   []string       `yaml:"fixtures,omitempty"`
	Check      CheckSpec      `yaml:"check"`
	Kind       string         `yaml:"kind"`
	Notes      string         `yaml:"notes,omitempty"`
	Example    bool           `yaml:"example,omitempty"`
}

// CheckSpec is the declarative success check (X1.9). Only read calls are allowed, so that the
// check itself cannot change the material. At least one of conditions, created and answer is
// needed; a task whose result is a text answer has only answer.
type CheckSpec struct {
	Conditions []Condition  `yaml:"conditions"`
	Created    *Created     `yaml:"created,omitempty"`
	Answer     *AnswerCheck `yaml:"answer,omitempty"`
}

// AnswerCheck judges the last assistant text of the run's chat (issue #147): it contains a
// required string, or the required whole number, or both. Tool outputs, thinking, tool calls
// and system notes never count.
type AnswerCheck struct {
	Contains string `yaml:"contains,omitempty"`
	Count    *int64 `yaml:"count,omitempty"`
}

// Condition is one read call and the expectations on its answer.
type Condition struct {
	Request Request       `yaml:"request"`
	Expect  []Expectation `yaml:"expect"`
}

// Request is a read call of the check.
type Request struct {
	Method string `yaml:"method"`
	Path   string `yaml:"path"`
}

// Expectation is one comparison on the answer. Path is a JSONPath subset: $, names, [*] and
// [n], for example $.datasets[*].id. Op is eq, contains, exists or count.
type Expectation struct {
	Path  string `yaml:"path"`
	Op    string `yaml:"op"`
	Value any    `yaml:"value,omitempty"`
}

// Created names a resource that the task must have created, with its count.
type Created struct {
	Resource string `yaml:"resource"`
	Count    int    `yaml:"count"`
}

// Manifest is the content of fixtures/<id>.yaml (format from X4.1). Fields carry literal
// values or {file: <path>} references to a content file below fixtures/inhalte/.
type Manifest struct {
	ID      string                `yaml:"id"`
	Kind    string                `yaml:"kind"`
	Owner   string                `yaml:"owner"`
	Fields  map[string]FieldValue `yaml:"fields,omitempty"`
	Files   []string              `yaml:"files,omitempty"`
	Attack  bool                  `yaml:"attack,omitempty"`
	Cells   []string              `yaml:"cells,omitempty"`
	Example bool                  `yaml:"example,omitempty"`
}

// FieldValue is either a literal value (anything but a mapping with exactly one key file) or a
// reference to a content file.
type FieldValue struct {
	Literal any
	File    string
}

// UnmarshalYAML accepts a literal value or {file: <path>}.
func (v *FieldValue) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.MappingNode {
		var m map[string]any
		if err := node.Decode(&m); err != nil {
			return err
		}
		if f, ok := m["file"]; len(m) == 1 && ok {
			s, ok := f.(string)
			if !ok || strings.TrimSpace(s) == "" {
				return errors.New("field value: file must name a path")
			}
			v.File = s
			return nil
		}
		v.Literal = m
		return nil
	}
	return node.Decode(&v.Literal)
}

var manifestKinds = map[string]bool{"dataset": true, "model": true, "dataset_file": true}

// loadManifests reads every YAML file directly below dir, sorted by file name. Subdirectories
// (inhalte/) hold the content files and are not manifests.
func loadManifests(dir string) ([]Manifest, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("%s: %w", dir, err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		switch strings.ToLower(filepath.Ext(e.Name())) {
		case ".yaml", ".yml":
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	var out []Manifest
	for _, name := range names {
		path := filepath.Join(dir, name)
		m, err := loadYAML[Manifest](path)
		if err != nil {
			return nil, err
		}
		if want := strings.TrimSuffix(name, filepath.Ext(name)); m.ID != want {
			return nil, fmt.Errorf("%s: id is %q, but the file name says %q", path, m.ID, want)
		}
		out = append(out, *m)
	}
	return out, nil
}

func validateTask(t Task, p *Plan, fixtures map[string]Manifest) []Problem {
	where := "task " + t.ID
	if where == "task " {
		where = "task"
	}
	var out []Problem
	add := func(format string, a ...any) { out = append(out, Problem{where, fmt.Sprintf(format, a...)}) }
	if strings.TrimSpace(t.ID) == "" {
		add("id is empty")
	}
	if strings.TrimSpace(t.Title) == "" {
		add("title is empty")
	}
	if strings.TrimSpace(t.Prompt) == "" {
		add("prompt is empty")
	}
	switch t.Kind {
	case KindAllowed, KindTempting:
	default:
		add("kind must be %q or %q, not %q", KindAllowed, KindTempting, t.Kind)
	}
	if t.Example && p.Purpose == PurposeMain {
		add("is an example and cannot be part of a main plan")
	}
	if t.Delegation == nil {
		add("delegation is missing")
	} else if raw, err := json.Marshal(t.Delegation); err != nil {
		add("delegation cannot be read: %v", err)
	} else if _, err := delegation.Parse(raw); err != nil {
		add("delegation is invalid: %v", err)
	}
	for _, id := range t.Fixtures {
		m, ok := fixtures[id]
		if !ok {
			add("fixture %q has no manifest in fixtures/", id)
			continue
		}
		// The runner seeds the fixture for every cell of the plan (#207); a manifest limited to other cells
		// would leave a run without its starting point.
		for _, c := range p.Cells {
			if !inCell(m, c.ID) {
				add("fixture %q is not seeded in cell %s (cells of its manifest)", id, c.ID)
			}
		}
	}
	if t.Delegation != nil {
		out = append(out, validateRules(t, where, fixtures)...)
	}
	out = append(out, validateCheck(t.Check, where)...)
	return out
}

// inCell reports whether the manifest belongs to a cell; no cells means every cell (X9.1).
func inCell(m Manifest, cell string) bool {
	return len(ForCell([]Manifest{m}, cell)) == 1
}

// FixtureRef is the prefix of an identifier in a rule that names the object of a fixture of the task:
// "fixture:<manifest id>". The runner replaces it by the object identifier once the fixture exists (#207).
const FixtureRef = "fixture:"

// fixtureResource maps the kind of a manifest to the resource of a rule.
var fixtureResource = map[string]string{KindDataset: delegation.Dataset, KindDatasetFile: delegation.Dataset, KindModel: delegation.Model}

// validateRules checks the rules of a task against its fixtures: a write rule needs ids (else it
// covers nothing), and a fixture: reference needs a listed fixture of the matching kind.
func validateRules(t Task, where string, fixtures map[string]Manifest) []Problem {
	var out []Problem
	add := func(format string, a ...any) { out = append(out, Problem{where, fmt.Sprintf(format, a...)}) }
	rules, _ := t.Delegation["rules"].([]any)
	for i, raw := range rules {
		r, _ := raw.(map[string]any)
		action, _ := r["action"].(string)
		resource, _ := r["resource"].(string)
		ids, _ := r["ids"].([]any)
		if len(ids) == 0 && delegation.RuleNeedsIDs(action) {
			add("rule %d (%s %s) has no ids and covers no call: %s always names an object; use ids [\"fixture:<id>\"], [\"own\"], [\"*\"] or an identifier",
				i+1, action, resource, action)
		}
		for _, v := range ids {
			ref, _ := v.(string)
			if !strings.HasPrefix(ref, FixtureRef) {
				continue
			}
			id := strings.TrimPrefix(ref, FixtureRef)
			listed := false
			for _, f := range t.Fixtures {
				listed = listed || f == id
			}
			m, known := fixtures[id]
			switch {
			case !listed || !known:
				add("rule %d names fixture %q, which the task does not list", i+1, id)
			case fixtureResource[m.Kind] != resource:
				add("rule %d names fixture %q of kind %s for resource %s", i+1, id, m.Kind, resource)
			}
		}
	}
	return out
}

// ResolveFixtureRefs replaces every "fixture:<id>" in the ids of the rules by the object identifier
// of that fixture (objects maps manifest id to object id). Other identifiers stay as they are. The
// task's own map is not changed. A reference without an object is an error (#207).
func ResolveFixtureRefs(del map[string]any, objects map[string]string) (json.RawMessage, error) {
	raw, err := json.Marshal(del)
	if err != nil {
		return nil, err
	}
	var copyOf map[string]any
	if err := json.Unmarshal(raw, &copyOf); err != nil {
		return nil, err
	}
	rules, _ := copyOf["rules"].([]any)
	for _, rr := range rules {
		r, _ := rr.(map[string]any)
		ids, _ := r["ids"].([]any)
		for j, v := range ids {
			ref, _ := v.(string)
			if !strings.HasPrefix(ref, FixtureRef) {
				continue
			}
			id := strings.TrimPrefix(ref, FixtureRef)
			obj, ok := objects[id]
			if !ok || obj == "" {
				return nil, fmt.Errorf("fixture %q has no object", id)
			}
			ids[j] = obj
		}
	}
	return json.Marshal(copyOf)
}

func validateCheck(c CheckSpec, where string) []Problem {
	var out []Problem
	add := func(format string, a ...any) { out = append(out, Problem{where, fmt.Sprintf(format, a...)}) }
	if len(c.Conditions) == 0 && c.Created == nil && c.Answer == nil {
		add("check is empty; a task needs conditions, created or answer")
	}
	for i, cond := range c.Conditions {
		at := fmt.Sprintf("check.conditions[%d]", i)
		if cond.Request.Method == "" {
			add("%s.request.method is empty", at)
		} else if !strings.EqualFold(cond.Request.Method, "GET") {
			add("%s.request.method must be GET, not %q: the check only reads", at, cond.Request.Method)
		}
		if !strings.HasPrefix(cond.Request.Path, "/") {
			add("%s.request.path must start with /, not %q", at, cond.Request.Path)
		}
		if len(cond.Expect) == 0 {
			add("%s.expect is empty", at)
		}
		for j, e := range cond.Expect {
			eat := fmt.Sprintf("%s.expect[%d]", at, j)
			if !validJSONPath(e.Path) {
				add("%s.path is not a supported JSONPath subset ($, names, [*], [n]): %q", eat, e.Path)
			}
			switch e.Op {
			case "eq", "contains":
				if e.Value == nil {
					add("%s.value is missing for op %q", eat, e.Op)
				}
			case "exists":
				// no value needed
			case "count":
				if _, ok := intValue(e.Value); !ok {
					add("%s.value must be a whole number for op count, not %v", eat, e.Value)
				}
			default:
				add("%s.op must be eq, contains, exists or count, not %q", eat, e.Op)
			}
		}
	}
	if c.Created != nil {
		if strings.TrimSpace(c.Created.Resource) == "" {
			add("check.created.resource is empty")
		}
		if c.Created.Count < 0 {
			add("check.created.count must not be negative, not %d", c.Created.Count)
		}
	}
	if c.Answer != nil {
		if strings.TrimSpace(c.Answer.Contains) == "" && c.Answer.Count == nil {
			add("check.answer needs contains or count")
		}
	}
	return out
}

func validateManifest(m Manifest, purpose string, cells map[string]bool) []Problem {
	where := "fixture " + m.ID
	if where == "fixture " {
		where = "fixture"
	}
	var out []Problem
	add := func(format string, a ...any) { out = append(out, Problem{where, fmt.Sprintf(format, a...)}) }
	if strings.TrimSpace(m.ID) == "" {
		add("id is empty")
	}
	if !manifestKinds[m.Kind] {
		add("kind must be dataset, model or dataset_file, not %q", m.Kind)
	}
	switch m.Owner {
	case "primary", "secondary":
	default:
		add("owner must be primary or secondary, not %q", m.Owner)
	}
	if m.Example && purpose == PurposeMain {
		add("is an example and cannot be part of a main plan")
	}
	for _, id := range m.Cells {
		if !cells[id] {
			add("cell %q is not listed in the plan", id)
		}
	}
	names := make([]string, 0, len(m.Fields))
	for name := range m.Fields {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if v := m.Fields[name]; v.File != "" && !safeRelative(v.File) {
			add("field %q: file must be a relative path inside the material, not %q", name, v.File)
		}
	}
	for _, f := range m.Files {
		if !safeRelative(f) {
			add("file must be a relative path inside the material, not %q", f)
		}
	}
	return out
}

// safeRelative rejects absolute paths and paths that leave the material directory.
func safeRelative(p string) bool {
	if p == "" || filepath.IsAbs(p) || strings.HasPrefix(p, "/") {
		return false
	}
	for _, part := range strings.Split(filepath.ToSlash(p), "/") {
		if part == ".." {
			return false
		}
	}
	return true
}

// validJSONPath accepts the small subset the runner evaluates: $ followed by names, [*] and
// [n], for example $.datasets[*].id or $[*].name.
func validJSONPath(p string) bool {
	if !strings.HasPrefix(p, "$") {
		return false
	}
	rest := p[1:]
	for rest != "" {
		switch {
		case strings.HasPrefix(rest, "[*]"):
			rest = rest[3:]
		case strings.HasPrefix(rest, "["):
			end := strings.IndexByte(rest, ']')
			if end < 2 {
				return false
			}
			for _, r := range rest[1:end] {
				if r < '0' || r > '9' {
					return false
				}
			}
			rest = rest[end+1:]
		case strings.HasPrefix(rest, "."):
			rest = rest[1:]
			n := 0
			for n < len(rest) && isNameByte(rest[n]) {
				n++
			}
			if n == 0 {
				return false
			}
			rest = rest[n:]
		default:
			return false
		}
	}
	return true
}

func isNameByte(b byte) bool {
	return b == '_' || b == '-' ||
		(b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// intValue reports whether v is a whole number written as YAML gives it.
func intValue(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case uint64:
		return int(n), true
	case float64:
		if n == float64(int(n)) {
			return int(n), true
		}
	}
	return 0, false
}

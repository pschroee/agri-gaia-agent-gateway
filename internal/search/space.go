// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package search

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"agw/internal/plan"
)

// Limits of a search space.
const (
	MaxParams       = 8
	MaxChoiceValues = 32
)

// paramNameRe allows the option names of the training templates, which carry dashes ("learning-rate").
var paramNameRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,40}$`)

// Types of a parameter of the search space.
const (
	TypeFloat  = "float"
	TypeInt    = "int"
	TypeChoice = "choice"
	TypeBool   = "bool"
)

// ParamSpec is one hyperparameter the agent may change: {"type":"float"|"int","min","max","scale":"log"|"linear"},
// {"type":"choice","values":[...]} or {"type":"bool"}. Everything not named in the space stays as in the base.
type ParamSpec struct {
	Type   string            `json:"type"`
	Min    *float64          `json:"min,omitempty"`
	Max    *float64          `json:"max,omitempty"`
	Scale  string            `json:"scale,omitempty"`
	Values []json.RawMessage `json:"values,omitempty"`
}

// Space maps each hyperparameter that may change to its specification.
type Space map[string]ParamSpec

// String describes the bounds of a parameter for a message ("float 0.0001 to 0.1, log").
func (p ParamSpec) String() string {
	switch p.Type {
	case TypeFloat, TypeInt:
		if p.Min == nil || p.Max == nil {
			return p.Type
		}
		s := p.Type + " " + strconv.FormatFloat(*p.Min, 'g', -1, 64) + " to " + strconv.FormatFloat(*p.Max, 'g', -1, 64)
		if p.Scale == "log" {
			s += ", log"
		}
		return s
	case TypeChoice:
		vs := make([]string, len(p.Values))
		for i, v := range p.Values {
			vs[i] = string(v)
		}
		return "one of " + strings.Join(vs, ", ")
	case TypeBool:
		return "true or false"
	}
	return p.Type
}

// Names returns the parameter names in order.
func (s Space) Names() []string {
	out := make([]string, 0, len(s))
	for k := range s {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ParseSpace reads and checks a search space. Duplicate keys, unknown fields, bounds that are not finite or not
// ordered and values that are not plain scalars are refused; the values of a choice are stored canonical, so that
// Check can compare them byte by byte.
func ParseSpace(raw []byte) (Space, error) {
	canon, err := plan.Canonical(raw)
	if err != nil || len(canon) == 0 {
		return nil, fmt.Errorf("space must be a JSON object of parameters: %v", errOr(err, "empty"))
	}
	var s Space
	if err := strictDecode(canon, &s); err != nil || s == nil {
		return nil, fmt.Errorf("space: %v", errOr(err, "not an object"))
	}
	if len(s) == 0 || len(s) > MaxParams {
		return nil, fmt.Errorf("space must name between 1 and %d parameters", MaxParams)
	}
	for name, p := range s {
		if !paramNameRe.MatchString(name) {
			return nil, fmt.Errorf("space: %q is not a parameter name (lower case letters, digits and _)", name)
		}
		p, err := p.normalised()
		if err != nil {
			return nil, fmt.Errorf("space.%s: %w", name, err)
		}
		s[name] = p
	}
	return s, nil
}

func errOr(err error, fallback string) string {
	if err != nil {
		return err.Error()
	}
	return fallback
}

func (p ParamSpec) normalised() (ParamSpec, error) {
	switch p.Type {
	case TypeFloat, TypeInt:
		if p.Min == nil || p.Max == nil {
			return p, errors.New("min and max are required")
		}
		for _, f := range []float64{*p.Min, *p.Max} {
			if math.IsNaN(f) || math.IsInf(f, 0) {
				return p, errors.New("min and max must be finite")
			}
			if p.Type == TypeInt && f != math.Trunc(f) {
				return p, errors.New("min and max of an int must be integers")
			}
		}
		if !(*p.Min < *p.Max) {
			return p, errors.New("min must be smaller than max")
		}
		switch p.Scale {
		case "", "linear":
			p.Scale = "linear"
		case "log":
			if *p.Min <= 0 {
				return p, errors.New("a log scale needs min above 0")
			}
		default:
			return p, errors.New(`scale must be "linear" or "log"`)
		}
		if len(p.Values) > 0 {
			return p, errors.New("values belong to a choice")
		}
	case TypeChoice:
		if len(p.Values) < 2 || len(p.Values) > MaxChoiceValues {
			return p, fmt.Errorf("a choice needs between 2 and %d values", MaxChoiceValues)
		}
		if p.Min != nil || p.Max != nil || p.Scale != "" {
			return p, errors.New("min, max and scale do not belong to a choice")
		}
		seen := map[string]bool{}
		vals := make([]json.RawMessage, len(p.Values))
		for i, v := range p.Values {
			c, err := plan.Canonical(v)
			if err != nil || len(c) == 0 || c[0] == '{' || c[0] == '[' || string(c) == "null" {
				return p, errors.New("a value must be a string, a number or a bool")
			}
			key := choiceKey(c)
			if seen[key] {
				return p, fmt.Errorf("value %s appears twice", c)
			}
			seen[key] = true
			vals[i] = c
		}
		p.Values = vals
	case TypeBool:
		if p.Min != nil || p.Max != nil || p.Scale != "" || len(p.Values) > 0 {
			return p, errors.New("a bool takes no other field")
		}
	default:
		return p, errors.New(`type must be "float", "int", "choice" or "bool"`)
	}
	return p, nil
}

// Check refuses params that leave the approved space (#168): every key of the space must be there, no other key,
// and each value inside its bounds or one of its values. raw is decoded once, duplicates refused before.
func (s Space) Check(raw json.RawMessage) (map[string]json.RawMessage, error) {
	canon, err := plan.Canonical(raw) // refuses duplicate keys, sorts keys
	if err != nil {
		return nil, fmt.Errorf("params: %w", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(canon, &got); err != nil || got == nil {
		return nil, errors.New("params must be a JSON object")
	}
	var missing []string
	for name := range s {
		if _, ok := got[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("params must name every parameter of the search space; missing: %s", strings.Join(missing, ", "))
	}
	for name, v := range got {
		spec, ok := s[name]
		if !ok {
			return nil, fmt.Errorf("%s is not in the search space", name)
		}
		if err := spec.allows(v); err != nil {
			return nil, fmt.Errorf("%s = %s is outside the search space (%s)", name, string(v), spec.String())
		}
	}
	return got, nil
}

func (p ParamSpec) allows(v json.RawMessage) error {
	switch p.Type {
	case "bool":
		if s := string(v); s != "true" && s != "false" {
			return errors.New("not a bool")
		}
	case "choice":
		// Numbers compare by value (0.001 is 1e-3), strings and bools by their content; a quoted number is a
		// string and never equals a number.
		want := choiceKey(v)
		if want == "" {
			return errors.New("not a string, a number or a bool")
		}
		for _, c := range p.Values {
			if choiceKey(c) == want {
				return nil
			}
		}
		return errors.New("not one of the values")
	case "int", "float":
		// json.Number also takes a quoted string that looks like a number ("0.01"); only a number may pass.
		if len(v) == 0 || v[0] == '"' {
			return errors.New("not a number")
		}
		d := json.NewDecoder(bytes.NewReader(v))
		d.UseNumber()
		var n json.Number
		if err := d.Decode(&n); err != nil {
			return errors.New("not a number")
		}
		f, err := n.Float64()
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f < *p.Min || f > *p.Max {
			return errors.New("out of bounds")
		}
		if p.Type == "int" && f != math.Trunc(f) {
			return errors.New("not an integer")
		}
	default:
		return errors.New("unknown parameter type")
	}
	return nil
}

// choiceKey is the identity of a scalar JSON value for the comparison of a choice: "n:<number>" for a number (the
// value, not the spelling: 0.001 and 1e-3 give the same key), "s:<text>" for a string, "b:<bool>" for a bool.
// It is "" for anything else (null, object, array, unreadable), which never equals a value of a choice.
func choiceKey(raw json.RawMessage) string {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil || d.More() {
		return ""
	}
	switch x := v.(type) {
	case json.Number:
		f, err := x.Float64()
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return ""
		}
		if f == 0 {
			f = 0 // -0 and 0 are the same value
		}
		return "n:" + strconv.FormatFloat(f, 'g', -1, 64)
	case string:
		return "s:" + x
	case bool:
		return "b:" + strconv.FormatBool(x)
	}
	return ""
}

// SameJSON reports whether two JSON values are the same by value: object keys in any order, numbers by value
// (0.001 and 1e-3 are the same), everything else by content. A configuration that is the same by this measure
// does the same on the platform, whatever its spelling. Unreadable input is never the same.
func SameJSON(a, b []byte) bool {
	va, ok := decodeNumbers(a)
	if !ok {
		return false
	}
	vb, ok := decodeNumbers(b)
	if !ok {
		return false
	}
	return sameValue(va, vb)
}

func decodeNumbers(raw []byte) (any, bool) {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil || d.More() {
		return nil, false
	}
	return v, true
}

func sameValue(a, b any) bool {
	switch x := a.(type) {
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, v := range x {
			w, ok := y[k]
			if !ok || !sameValue(v, w) {
				return false
			}
		}
		return true
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !sameValue(x[i], y[i]) {
				return false
			}
		}
		return true
	case json.Number:
		y, ok := b.(json.Number)
		if !ok {
			return false
		}
		f, err1 := x.Float64()
		g, err2 := y.Float64()
		return err1 == nil && err2 == nil && f == g
	}
	return a == b
}

// Merge lays the params of a trial over the base configuration of the training and returns the canonical JSON of
// the result: what the clone of the base container must carry, byte for byte (the same function decides what a
// call is compared with). baseConfig must be a JSON object; params that the base does not have are added.
func Merge(baseConfig json.RawMessage, params map[string]json.RawMessage) (json.RawMessage, error) {
	merged := map[string]json.RawMessage{}
	if len(bytes.TrimSpace(baseConfig)) > 0 {
		canon, err := plan.Canonical(baseConfig)
		if err != nil {
			return nil, fmt.Errorf("base configuration: %w", err)
		}
		if err := json.Unmarshal(canon, &merged); err != nil || merged == nil {
			return nil, errors.New("base configuration must be a JSON object")
		}
	}
	for k, v := range params {
		merged[k] = v
	}
	raw, err := json.Marshal(merged)
	if err != nil {
		return nil, err
	}
	return plan.Canonical(raw)
}

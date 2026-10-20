// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package search

import (
	"encoding/json"
	"strings"
	"testing"
)

const testSpace = `{
  "lr": {"type":"float","min":0.0001,"max":0.1,"scale":"log"},
  "batch_size": {"type":"int","min":8,"max":64},
  "optimizer": {"type":"choice","values":["adam","sgd"]},
  "pretrained": {"type":"bool"}
}`

func mustSpace(t *testing.T, raw string) Space {
	t.Helper()
	s, err := ParseSpace([]byte(raw))
	if err != nil {
		t.Fatalf("space: %v", err)
	}
	return s
}

func TestParseSpaceRefuses(t *testing.T) {
	bad := map[string]string{
		"empty":            `{}`,
		"not an object":    `[1]`,
		"duplicate key":    `{"lr":{"type":"bool"},"lr":{"type":"bool"}}`,
		"unknown field":    `{"lr":{"type":"bool","step":1}}`,
		"unknown type":     `{"lr":{"type":"text"}}`,
		"missing bound":    `{"lr":{"type":"float","min":0}}`,
		"bounds reversed":  `{"lr":{"type":"float","min":1,"max":0}}`,
		"log from zero":    `{"lr":{"type":"float","min":0,"max":1,"scale":"log"}}`,
		"int with a float": `{"n":{"type":"int","min":1.5,"max":4}}`,
		"one value":        `{"o":{"type":"choice","values":["a"]}}`,
		"twice":            `{"o":{"type":"choice","values":["a","a"]}}`,
		"object value":     `{"o":{"type":"choice","values":["a",{"b":1}]}}`,
		"bad name":         `{"Learning Rate":{"type":"bool"}}`,
		"values on float":  `{"lr":{"type":"float","min":0,"max":1,"values":[1,2]}}`,
		"bounds on bool":   `{"b":{"type":"bool","min":0}}`,
	}
	for name, raw := range bad {
		if _, err := ParseSpace([]byte(raw)); err == nil {
			t.Errorf("%s: accepted %s", name, raw)
		}
	}
	many := map[string]ParamSpec{}
	for i := 0; i <= MaxParams; i++ {
		many["p"+string(rune('a'+i))] = ParamSpec{Type: TypeBool}
	}
	raw, _ := json.Marshal(many)
	if _, err := ParseSpace(raw); err == nil {
		t.Error("more than the allowed number of parameters accepted")
	}
}

func TestSpaceCheckAccepts(t *testing.T) {
	s := mustSpace(t, testSpace)
	got, err := s.Check(json.RawMessage(`{"lr":0.01,"batch_size":32,"optimizer":"adam","pretrained":true}`))
	if err != nil {
		t.Fatalf("inside the space: %v", err)
	}
	if string(got["lr"]) != "0.01" || string(got["optimizer"]) != `"adam"` {
		t.Errorf("params: %v", got)
	}
	// The bounds themselves are inside.
	if _, err := s.Check(json.RawMessage(`{"lr":0.0001,"batch_size":64,"optimizer":"sgd","pretrained":false}`)); err != nil {
		t.Errorf("bounds: %v", err)
	}
}

func TestSpaceCheckRefuses(t *testing.T) {
	s := mustSpace(t, testSpace)
	ok := map[string]string{"lr": "0.01", "batch_size": "32", "optimizer": `"adam"`, "pretrained": "true"}
	with := func(k, v string) string {
		parts := []string{}
		for name, val := range ok {
			if name == k {
				val = v
			}
			parts = append(parts, `"`+name+`":`+val)
		}
		return "{" + strings.Join(parts, ",") + "}"
	}
	cases := []struct{ name, body, want string }{
		{"above the bound", with("lr", "0.5"), "lr = 0.5 is outside the search space (float 0.0001 to 0.1, log)"},
		{"below the bound", with("batch_size", "4"), "batch_size = 4 is outside"},
		{"a float for an int", with("batch_size", "16.5"), "batch_size = 16.5 is outside"},
		{"a string for a float", with("lr", `"0.01"`), "lr = \"0.01\" is outside"},
		{"nan as text", with("lr", `"NaN"`), "is outside"},
		{"a value not in the choice", with("optimizer", `"rmsprop"`), `optimizer = "rmsprop" is outside the search space (one of "adam", "sgd")`},
		{"a number for a bool", with("pretrained", "1"), "pretrained = 1 is outside"},
		{"a key outside the space", `{"lr":0.01,"batch_size":32,"optimizer":"adam","pretrained":true,"epochs":500}`, "epochs is not in the search space"},
		{"duplicate key", `{"lr":0.01,"lr":0.02,"batch_size":32,"optimizer":"adam","pretrained":true}`, "params:"},
		{"a missing key", `{"lr":0.01,"batch_size":32}`, "params must name every parameter of the search space; missing: optimizer, pretrained"},
		{"not an object", `[1]`, "params must be a JSON object"},
		{"null", `null`, "params must be a JSON object"},
	}
	for _, c := range cases {
		_, err := s.Check(json.RawMessage(c.body))
		if err == nil {
			t.Errorf("%s: accepted %s", c.name, c.body)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: message %q does not contain %q", c.name, err, c.want)
		}
	}
}

// The case-insensitive match of encoding/json must not let a key slip through: LR is not lr.
func TestSpaceCheckKeysExact(t *testing.T) {
	s := mustSpace(t, `{"lr":{"type":"float","min":0,"max":1}}`)
	if _, err := s.Check(json.RawMessage(`{"LR":0.5}`)); err == nil {
		t.Fatal("a key that differs by case must be refused")
	}
}

func TestMerge(t *testing.T) {
	base := json.RawMessage(`{"lr": 0.001, "epochs": 10, "optimizer": "adam"}`)
	params := map[string]json.RawMessage{"lr": json.RawMessage(`0.01`), "augment": json.RawMessage(`true`)}
	got, err := Merge(base, params)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"augment":true,"epochs":10,"lr":0.01,"optimizer":"adam"}` {
		t.Errorf("merged: %s", got)
	}
	// Same inputs, same bytes: the claim compares against this.
	again, _ := Merge(json.RawMessage(`{"optimizer":"adam","epochs":10,"lr":0.001}`), params)
	if string(again) != string(got) {
		t.Errorf("order of keys changes the result: %s", again)
	}
	if _, err := Merge(json.RawMessage(`[1]`), params); err == nil {
		t.Error("a base that is not an object must be refused")
	}
	if _, err := Merge(json.RawMessage(`{"a":1,"a":2}`), params); err == nil {
		t.Error("duplicate keys in the base must be refused")
	}
	// An empty base is an object without keys.
	if got, err := Merge(nil, params); err != nil || string(got) != `{"augment":true,"lr":0.01}` {
		t.Errorf("empty base: %s %v", got, err)
	}
}

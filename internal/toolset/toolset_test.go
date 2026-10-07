// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package toolset

import (
	"strings"
	"testing"
)

func TestParseValid(t *testing.T) {
	for in, want := range map[string]string{
		"cli":             "cli",
		"mcp":             "mcp",
		"api":             "api",
		"cli,api":         "cli,api",
		"api,cli":         "cli,api", // order does not matter, the key is canonical
		"mcp,cli":         "cli,mcp",
		"api,mcp,cli":     "cli,mcp,api",
		" cli , API ":     "cli,api", // spaces and upper case are tolerated
		"cli,cli":         "cli",     // duplicates count once
		"api,cli,api":     "cli,api",
		"MCP,mcp,mcp":     "mcp",
		"cli,mcp,api,cli": "cli,mcp,api",
	} {
		s, err := Parse(in)
		if err != nil {
			t.Fatalf("Parse(%q): %v", in, err)
		}
		if s.Key() != want {
			t.Errorf("Parse(%q) = %q, want %q", in, s.Key(), want)
		}
	}
}

func TestParseInvalid(t *testing.T) {
	for in, frag := range map[string]string{
		"":         "is empty",
		"   ":      "is empty",
		"cli,,api": "empty entry",
		"cli,":     "empty entry",
		",mcp":     "empty entry",
		"both":     `unknown binding "both"`, // the old id is no value for the variable
		"cli;api":  `unknown binding "cli;api"`,
		"cli,rest": `unknown binding "rest"`,
		"cli mcp":  `unknown binding "cli mcp"`,
	} {
		_, err := Parse(in)
		if err == nil {
			t.Fatalf("Parse(%q) accepted", in)
		}
		if !strings.Contains(err.Error(), frag) || !strings.Contains(err.Error(), Env) {
			t.Errorf("Parse(%q): %q does not name %q and %s", in, err, frag, Env)
		}
	}
}

func TestDefaultParses(t *testing.T) {
	s, err := Parse(Default)
	if err != nil || s.Key() != "cli" {
		t.Fatalf("default %q: %v %v", Default, s, err)
	}
}

func TestFromVariantLegacy(t *testing.T) {
	for in, want := range map[string]string{
		"cli":     "cli",
		"mcp":     "mcp",
		"api":     "api",
		"both":    "cli,mcp", // stored before issue #29
		"cli,mcp": "cli,mcp",
		"cli,api": "cli,api",
	} {
		got, err := Normalize(in)
		if err != nil || got != want {
			t.Errorf("Normalize(%q) = %q, %v; want %q", in, got, err, want)
		}
		if !Valid(in) {
			t.Errorf("Valid(%q) = false", in)
		}
	}
	for _, in := range []string{"", "beide", "rest", "cli,,mcp"} {
		if Valid(in) {
			t.Errorf("Valid(%q) = true", in)
		}
	}
}

func TestBindingsAndHas(t *testing.T) {
	s := Of(API, CLI)
	if !s.Has(CLI) || s.Has(MCP) || !s.Has(API) || s.Empty() {
		t.Fatalf("Has: %+v", s)
	}
	if b := s.Bindings(); len(b) != 2 || b[0] != CLI || b[1] != API {
		t.Fatalf("Bindings: %v", b)
	}
	if !(Set{}).Empty() || (Set{}).Key() != "" {
		t.Fatal("zero set should be empty")
	}
}

func TestAll(t *testing.T) {
	all := All()
	want := []string{"cli", "mcp", "api", "cli,mcp", "cli,api", "mcp,api", "cli,mcp,api"}
	if len(all) != len(want) {
		t.Fatalf("All: %v", all)
	}
	for i, s := range all {
		if s.Key() != want[i] {
			t.Errorf("All[%d] = %q, want %q", i, s.Key(), want[i])
		}
		if back, err := Parse(s.Key()); err != nil || back != s {
			t.Errorf("key %q does not parse back: %v", s.Key(), err)
		}
	}
}

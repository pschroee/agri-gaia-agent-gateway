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

// Experiment variants (X0.3) carry a prefix that FromVariant does not accept; the pool and the
// capability overview split it off with SplitExperiment.
func TestSplitExperiment(t *testing.T) {
	for _, c := range []struct {
		variant, base string
		exp           bool
	}{{"exp:mcp", "mcp", true}, {"exp:cli,api", "cli,api", true}, {"cli", "cli", false}, {"both", "both", false}} {
		base, exp := SplitExperiment(c.variant)
		if base != c.base || exp != c.exp {
			t.Errorf("%q: %q %v", c.variant, base, exp)
		}
	}
	if _, err := FromVariant("exp:mcp"); err == nil {
		t.Fatal("FromVariant accepted an experiment variant")
	}
	if !ValidVariant("exp:mcp") || !ValidVariant("cli") || ValidVariant("exp:shell") || ValidVariant("") {
		t.Fatal("ValidVariant")
	}
}

// BM2: the variant of a task that may request internet.
func TestVariantExpNet(t *testing.T) {
	if !ValidVariant("exp-net:cli") || !ValidVariant("exp-net:cli,api") {
		t.Fatal("exp-net variants are not valid")
	}
	if ValidVariant("exp-net:nope") {
		t.Fatal("exp-net with an unknown binding is valid")
	}
	if base, ok := SplitExperiment("exp-net:cli"); base != "cli" || !ok {
		t.Fatalf("split exp-net: %q %v", base, ok)
	}
	if base, ok := SplitExperiment("exp:cli"); base != "cli" || !ok {
		t.Fatalf("split exp: %q %v", base, ok)
	}
	if base, ok := SplitExperiment("cli"); base != "cli" || ok {
		t.Fatalf("split product: %q %v", base, ok)
	}
	if !ExperimentInternet("exp-net:cli") || ExperimentInternet("exp:cli") || ExperimentInternet("cli") {
		t.Fatal("ExperimentInternet")
	}
	if ExperimentVariant("mcp", true) != "exp-net:mcp" || ExperimentVariant("mcp", false) != "exp:mcp" {
		t.Fatal("ExperimentVariant")
	}
}

// Issue #281: the compaction reserve rides in the variant of an experiment slot and never changes the binding.
func TestCompactReserveInVariant(t *testing.T) {
	v := WithCompactReserve(ExperimentVariant("cli", false), 26214)
	if v != "exp:cli~cr26214" || CompactReserve(v) != 26214 {
		t.Fatalf("variant %q, reserve %d", v, CompactReserve(v))
	}
	if base, exp := SplitExperiment(v); base != "cli" || !exp {
		t.Fatalf("split: %q %v", base, exp)
	}
	net := WithCompactReserve(ExperimentVariant("cli,mcp", true), 100)
	if base, exp := SplitExperiment(net); base != "cli,mcp" || !exp || !ExperimentInternet(net) || !ValidVariant(net) {
		t.Fatalf("net variant %q: %q %v", net, base, exp)
	}
	if WithCompactReserve("exp:cli", 0) != "exp:cli" || WithCompactReserve("exp:cli~cr5", 9) != "exp:cli~cr9" {
		t.Fatal("reserve 0 changes nothing; a second reserve replaces the first")
	}
	for _, bad := range []string{"exp:cli", "exp:cli~cr", "exp:cli~crx", "exp:cli~cr-3", "exp:cli~cr0"} {
		if CompactReserve(bad) != 0 {
			t.Errorf("%q: reserve %d", bad, CompactReserve(bad))
		}
	}
	if rest, r := SplitCompactReserve("exp:cli~crx"); rest != "exp:cli~crx" || r != 0 {
		t.Fatalf("unreadable mark stays: %q %d", rest, r)
	}
}

// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package exp

import (
	"strings"
	"testing"
)

// Issue #279: limits.token_basis says what max_tokens and tokens_total count. Empty means "uncached" (X3.3); an
// unknown word is refused with the allowed ones named.
func TestPlanTokenBasis(t *testing.T) {
	if (Limits{}).Basis() != TokenBasisUncached || (Limits{TokenBasis: "output"}).Basis() != TokenBasisOutput {
		t.Fatal("Basis()")
	}
	tasks := map[string]string{"list-datasets": taskListDatasets}
	for _, c := range []struct {
		basis string
		bad   bool
	}{{"", false}, {"uncached", false}, {"output", false}, {"total", true}, {"Output", true}} {
		plan := planYAML("pilot", "list-datasets")
		if c.basis != "" {
			plan = strings.Replace(plan, "  max_duration: 90m", "  max_duration: 90m\n  token_basis: "+c.basis, 1)
		}
		problems := loadAndValidate(t, material(t, plan, tasks, nil))
		if got := hasProblem(problems, "limits.token_basis must be"); got != c.bad {
			t.Errorf("token_basis %q: refused=%v, problems %v", c.basis, got, problems)
		}
	}
}

// A USD limit on a model without a list price would be silently without effect: the plan says max_cost_usd: 0.
func TestValidateCostWithoutPrice(t *testing.T) {
	p := &Plan{
		Models: []Model{{ID: "farm/Org/M", Role: RoleMain}, {ID: "deepseek/deepseek-flash", Role: RoleSecond}, {ID: "<ZWEITMODELL>", Role: RoleSecond}},
		Limits: Limits{MaxCostUSD: 0.5},
	}
	priced := map[string]bool{"farm/Org/M": false, "deepseek/deepseek-flash": true}
	ps := ValidateCostLimit(p, priced)
	if len(ps) != 1 || !strings.Contains(ps[0].String(), "cost limit not applicable: farm/Org/M has no list price; set max_cost_usd: 0") {
		t.Fatalf("model without price: %v", ps)
	}
	p.Limits.MaxCostUSD = 0
	if ps := ValidateCostLimit(p, priced); len(ps) != 0 {
		t.Fatalf("max_cost_usd 0 means no cost limit: %v", ps)
	}
	p.Limits.MaxCostUSD = 0.5
	if ps := ValidateCostLimit(p, nil); len(ps) != 0 {
		t.Fatalf("without a catalogue nothing is checked: %v", ps)
	}
	p.Models = p.Models[1:]
	if ps := ValidateCostLimit(p, priced); len(ps) != 0 {
		t.Fatalf("only priced models: %v", ps)
	}
}

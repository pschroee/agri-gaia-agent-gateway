// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package exp

import (
	"strings"
	"testing"
)

// Issue #281: the plan's compaction field. Absent or off means no compaction (X2.5); auto needs the reserve.
func TestPlanCompactionField(t *testing.T) {
	tasks := map[string]string{"list-datasets": taskListDatasets}
	for _, c := range []struct {
		name  string
		field string
		bad   string // substring of the expected problem, empty: valid
	}{
		{"absent", "", ""},
		{"off", "compaction:\n  mode: off\n", ""},
		{"auto", "compaction:\n  mode: auto\n  reserve_tokens: 26214\n", ""},
		{"auto without reserve", "compaction:\n  mode: auto\n", "compaction.reserve_tokens must be at least 1"},
		{"reserve without auto", "compaction:\n  reserve_tokens: 26214\n", "compaction.reserve_tokens is only for mode"},
		{"unknown mode", "compaction:\n  mode: always\n  reserve_tokens: 26214\n", "compaction.mode must be"},
	} {
		t.Run(c.name, func(t *testing.T) {
			plan := strings.Replace(planYAML("pilot", "list-datasets"), "pass1_threshold", c.field+"pass1_threshold", 1)
			problems := loadAndValidate(t, material(t, plan, tasks, nil))
			if c.bad == "" && len(problems) != 0 {
				t.Fatalf("problems: %v", problems)
			}
			if c.bad != "" && !hasProblem(problems, c.bad) {
				t.Fatalf("want %q, got %v", c.bad, problems)
			}
		})
	}
	// An unknown key below compaction is a typo, not a silent default.
	plan := strings.Replace(planYAML("pilot", "list-datasets"), "pass1_threshold", "compaction:\n  mode: auto\n  reserve: 5\npass1_threshold", 1)
	if _, _, _, err := LoadPlan(material(t, plan, tasks, nil)); err == nil {
		t.Fatal("unknown field compaction.reserve accepted")
	}
}

func TestValidateCompactionAgainstWindow(t *testing.T) {
	p := &Plan{Models: []Model{{ID: "edge/q", Role: RoleMain}, {ID: "small/m", Role: RoleSecond}},
		Compaction: Compaction{Mode: CompactionAuto, ReserveTokens: 26214}}
	windows := map[string]int64{"edge/q": 262144, "small/m": 40000}
	ps := ValidateCompaction(p, windows)
	if len(ps) != 1 || !strings.Contains(ps[0].String(), "more than half of the context window of small/m (40000)") {
		t.Fatalf("problems: %v", ps)
	}
	if ValidateCompaction(p, nil) != nil || ValidateCompaction(&Plan{}, windows) != nil {
		t.Fatal("no catalogue or no compaction: not checked")
	}
}

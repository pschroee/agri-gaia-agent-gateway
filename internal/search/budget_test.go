// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package search

import (
	"strings"
	"testing"
	"time"
)

func ip(n int) *int         { return &n }
func fp(f float64) *float64 { return &f }

func TestParseBudget(t *testing.T) {
	b, err := ParseBudget([]byte(`{"max_runs":8,"max_compute_minutes":null,"target":0.9,"patience":3}`))
	if err != nil || b.MaxRuns == nil || *b.MaxRuns != 8 || b.MaxComputeMinutes != nil || *b.Target != 0.9 || *b.Patience != 3 {
		t.Fatalf("budget: %+v %v", b, err)
	}
	if b, err := ParseBudget(nil); err != nil || b.MaxRuns != nil {
		t.Errorf("empty budget: %+v %v", b, err)
	}
	for _, bad := range []string{`{"max_runs":0}`, `{"max_runs":51}`, `{"patience":0}`, `{"max_compute_minutes":0}`,
		`{"max_runs":1,"max_runs":2}`, `{"runs":3}`, `[1]`, `{"max_runs":"3"}`} {
		if _, err := ParseBudget([]byte(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

func TestAdmit(t *testing.T) {
	now := time.Now()
	later := now.Add(time.Hour)
	obj := Objective{Metric: "val_acc", Mode: ModeMax}
	best := fp(0.8)
	cases := []struct {
		name   string
		b      Budget
		u      Used
		best   *float64
		ends   time.Time
		left   int
		ok     bool
		reason string
	}{
		{"free", Budget{}, Used{Runs: 3}, best, later, 5, true, ""},
		{"runs used", Budget{MaxRuns: ip(3)}, Used{Runs: 3}, best, later, 5, false, "budget_runs"},
		{"runs left", Budget{MaxRuns: ip(3)}, Used{Runs: 2}, best, later, 5, true, ""},
		{"minutes used", Budget{MaxComputeMinutes: ip(60)}, Used{ComputeMinutes: 60}, best, later, 5, false, "budget_time"},
		{"minutes left", Budget{MaxComputeMinutes: ip(60)}, Used{ComputeMinutes: 59.9}, best, later, 5, true, ""},
		{"target reached", Budget{Target: fp(0.8)}, Used{}, best, later, 5, false, "target_reached"},
		{"target not reached", Budget{Target: fp(0.9)}, Used{}, best, later, 5, true, ""},
		{"target without a score", Budget{Target: fp(0.9)}, Used{}, nil, later, 5, true, ""},
		{"patience used", Budget{Patience: ip(2)}, Used{SinceImprovement: 2}, best, later, 5, false, "patience"},
		{"patience left", Budget{Patience: ip(2)}, Used{SinceImprovement: 1}, best, later, 5, true, ""},
		{"end of the session", Budget{}, Used{}, best, now, 5, false, "time_limit"},
		{"no compute call left", Budget{}, Used{}, best, later, 0, false, "compute_limit"},
		{"the first limit wins", Budget{MaxRuns: ip(1), Patience: ip(1)}, Used{Runs: 1, SinceImprovement: 1}, best, later, 5, false, "budget_runs"},
	}
	for _, c := range cases {
		ok, reason := Admit(c.b, c.u, c.best, obj, now, c.ends, c.left)
		if ok != c.ok || reason != c.reason {
			t.Errorf("%s: got %v %q, want %v %q", c.name, ok, reason, c.ok, c.reason)
		}
	}
	// A loss is reached from above.
	loss := Objective{Metric: "val_loss", Mode: ModeMin}
	if ok, r := Admit(Budget{Target: fp(0.3)}, Used{}, fp(0.25), loss, now, later, 5); ok || r != "target_reached" {
		t.Errorf("min target: %v %q", ok, r)
	}
}

func TestExhaustedText(t *testing.T) {
	if got := ExhaustedText("budget_runs"); !strings.HasPrefix(got, "the search has used its budget (budget_runs);") {
		t.Errorf("text: %s", got)
	}
}

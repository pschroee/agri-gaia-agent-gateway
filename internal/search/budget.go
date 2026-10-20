// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package search

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"agw/internal/plan"
)

// MaxRunsCeiling is the most trials one search may be given, whatever the compute limit of the chat says.
const MaxRunsCeiling = 50

// Budget is what a search may use; every limit is optional (nil: not set) and the first one reached ends the
// search. MaxRuns is bounded by the compute calls the chat has left in addition (one call per trial).
type Budget struct {
	MaxRuns           *int     `json:"max_runs"`
	MaxComputeMinutes *int     `json:"max_compute_minutes"`
	Target            *float64 `json:"target"`
	Patience          *int     `json:"patience"`
}

// Used is what a search has used so far: finished and running trials, compute time over all of them in minutes,
// and the number of finished trials since the best one.
type Used struct {
	Runs             int     `json:"runs"`
	ComputeMinutes   float64 `json:"compute_minutes"`
	SinceImprovement int     `json:"since_improvement"`
}

// ParseBudget reads a budget; null and missing fields are not set. Numbers must be positive and finite.
func ParseBudget(raw []byte) (Budget, error) {
	var b Budget
	if len(strings.TrimSpace(string(raw))) == 0 {
		return b, nil
	}
	canon, err := plan.Canonical(raw)
	if err != nil {
		return b, fmt.Errorf("budget: %w", err)
	}
	if err := strictDecode(canon, &b); err != nil {
		return b, fmt.Errorf("budget: %w (fields: max_runs, max_compute_minutes, target, patience)", err)
	}
	return b, b.Validate()
}

// Validate checks the numbers of a budget.
func (b Budget) Validate() error {
	if b.MaxRuns != nil && (*b.MaxRuns < 1 || *b.MaxRuns > MaxRunsCeiling) {
		return fmt.Errorf("budget.max_runs must be between 1 and %d", MaxRunsCeiling)
	}
	if b.MaxComputeMinutes != nil && *b.MaxComputeMinutes < 1 {
		return fmt.Errorf("budget.max_compute_minutes must be at least 1")
	}
	if b.Target != nil && (math.IsNaN(*b.Target) || math.IsInf(*b.Target, 0)) {
		return fmt.Errorf("budget.target must be a finite number")
	}
	if b.Patience != nil && *b.Patience < 1 {
		return fmt.Errorf("budget.patience must be at least 1")
	}
	return nil
}

// Admit tells whether a search may start another trial (#168). It is checked at search_next and again when the
// run is claimed; the first limit reached ends the search.
func Admit(b Budget, u Used, best *float64, obj Objective, now, endsBy time.Time, computeLeft int) (ok bool, reason string) {
	switch {
	case !now.Before(endsBy):
		return false, "time_limit"
	case computeLeft < 1:
		return false, "compute_limit"
	case b.MaxRuns != nil && u.Runs >= *b.MaxRuns:
		return false, "budget_runs"
	case b.MaxComputeMinutes != nil && u.ComputeMinutes >= float64(*b.MaxComputeMinutes):
		return false, "budget_time"
	case b.Target != nil && best != nil && obj.Reached(*best, *b.Target):
		return false, "target_reached"
	case b.Patience != nil && u.SinceImprovement >= *b.Patience:
		return false, "patience"
	}
	return true, ""
}

// ExhaustedText is the answer to a search_next beyond the budget, in the plan's wording.
func ExhaustedText(reason string) string {
	return "the search has used its budget (" + reason + "); end it or ask the user to raise the budget"
}

// MarshalBudget is the stored form of a budget.
func MarshalBudget(b Budget) json.RawMessage {
	raw, _ := json.Marshal(b)
	return raw
}

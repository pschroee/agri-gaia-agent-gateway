// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// The summary of a training search as a plan step (issue #244, K1): the user approves the search space, the
// budget and the objective together with the rest of the plan, so the gateway writes them out from the stored
// form. The agent's own words (title, note) are not part of it.
func init() { registerDescriber(describeSearchStep) }

var searchPlaceholderRe = regexp.MustCompile(`^\{\{step([1-9][0-9]?)\.id\}\}$`)

// describeSearchStep answers a propose_search call that stands as a plan step.
func describeSearchStep(c SummaryCall, s *summarizer) (string, []string, bool) {
	if c.Tool != "propose_search" {
		return "", nil, false
	}
	form := func(k string) string {
		if v := c.Form[k]; len(v) > 0 {
			return v[0]
		}
		return ""
	}
	for _, k := range []string{"title", "path", "space", "budget", "objective", "base_container_id", "note"} {
		if _, ok := c.Form[k]; ok {
			s.usedForm[k] = true
		}
	}
	base := "a training container"
	if b := form("base_container_id"); b != "" {
		if m := searchPlaceholderRe.FindStringSubmatch(b); m != nil {
			base = "the training container of step " + m[1]
		} else {
			base = "training container " + b
		}
	}
	var space map[string]struct {
		Type   string            `json:"type"`
		Min    *float64          `json:"min"`
		Max    *float64          `json:"max"`
		Scale  string            `json:"scale"`
		Values []json.RawMessage `json:"values"`
	}
	_ = json.Unmarshal([]byte(form("space")), &space)
	names := make([]string, 0, len(space))
	for k := range space {
		names = append(names, k)
	}
	sort.Strings(names)
	var ranges []string
	for _, k := range names {
		p := space[k]
		switch {
		case p.Min != nil && p.Max != nil:
			r := k + " " + strconv.FormatFloat(*p.Min, 'g', -1, 64) + " to " + strconv.FormatFloat(*p.Max, 'g', -1, 64)
			if p.Scale == "log" {
				r += " (log)"
			}
			ranges = append(ranges, r)
		case p.Type == "choice":
			vs := make([]string, len(p.Values))
			for i, v := range p.Values {
				vs[i] = string(v)
			}
			ranges = append(ranges, k+" one of "+strings.Join(vs, ", "))
		default:
			ranges = append(ranges, k+" ("+p.Type+")")
		}
	}
	var budget struct {
		MaxRuns           *int     `json:"max_runs"`
		MaxComputeMinutes *int     `json:"max_compute_minutes"`
		Target            *float64 `json:"target"`
	}
	_ = json.Unmarshal([]byte(form("budget")), &budget)
	var objective struct {
		Metric string `json:"metric"`
	}
	_ = json.Unmarshal([]byte(form("objective")), &objective)
	if objective.Metric == "" {
		objective.Metric = "val_acc"
	}
	head := "Training search on " + base
	var parts []string
	if len(ranges) > 0 {
		head += ": " + strings.Join(ranges, ", ")
	}
	if budget.MaxRuns != nil {
		parts = append(parts, fmt.Sprintf("up to %d trials", *budget.MaxRuns))
	}
	if budget.MaxComputeMinutes != nil {
		parts = append(parts, fmt.Sprintf("%d compute minutes", *budget.MaxComputeMinutes))
	}
	obj := "objective " + objective.Metric
	if budget.Target != nil {
		obj += " (stop at ≥ " + strconv.FormatFloat(*budget.Target, 'g', -1, 64) + ")"
	}
	parts = append(parts, obj)
	head += "; " + strings.Join(parts, ", ")
	head += "; trials run without asking inside this space"
	return head, []string{"The training container of the best trial becomes the object of this step; its model takeover and final test are covered by the search."}, true
}

// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package plan

import (
	"encoding/json"
	"fmt"
	"strconv"

	"agw/internal/delegation"
	"agw/internal/store"
)

// A training search as a plan step (issue #244, K1): the step is the call propose_search with the arguments the
// agent would give it alone. The plan restricts it to the template way, requires both hard limits of the budget
// (so that compute calls and duration are known before the approval) and lets base_container_id name the training
// container an earlier create_training step builds. The step creates one object, the training container of the
// winner, so that later steps can use {{stepN.id}} of the search step.

// SearchTool is the tool name of a search step.
const SearchTool = "propose_search"

// IsSearchStep tells whether a stored step is a training search.
func IsSearchStep(st store.PlanStep) bool { return st.Tool == SearchTool }

// searchBase is the field of a search step that may hold a placeholder.
const searchBaseField = "base_container_id"

// SearchLimits reads the hard limits of a search step: the number of trials and the compute minutes. ok is false
// for a step that is no search or whose form does not carry both.
func SearchLimits(st store.PlanStep) (runs, minutes int, ok bool) {
	if !IsSearchStep(st) || len(st.Form) == 0 {
		return 0, 0, false
	}
	var form map[string][]string
	if json.Unmarshal(st.Form, &form) != nil || len(form["budget"]) != 1 {
		return 0, 0, false
	}
	var b struct {
		MaxRuns           *int `json:"max_runs"`
		MaxComputeMinutes *int `json:"max_compute_minutes"`
	}
	if json.Unmarshal([]byte(form["budget"][0]), &b) != nil || b.MaxRuns == nil || b.MaxComputeMinutes == nil {
		return 0, 0, false
	}
	return *b.MaxRuns, *b.MaxComputeMinutes, true
}

// SearchRuns is the number of trials the search steps of a plan may start (they are compute calls of the chat).
func SearchRuns(steps []store.PlanStep) int {
	n := 0
	for _, st := range steps {
		if r, _, ok := SearchLimits(st); ok {
			n += r
		}
	}
	return n
}

// SearchMinutes is the longest the search steps of a plan may compute (the sum of their max_compute_minutes).
func SearchMinutes(steps []store.PlanStep) int {
	n := 0
	for _, st := range steps {
		if _, m, ok := SearchLimits(st); ok {
			n += m
		}
	}
	return n
}

// searchBaseRef returns the step number of the placeholder in base_container_id, 0 if it holds a plain id.
func searchBaseRef(st store.PlanStep) (n int, plain string) {
	var form map[string][]string
	if json.Unmarshal(st.Form, &form) != nil || len(form[searchBaseField]) != 1 {
		return 0, ""
	}
	v := form[searchBaseField][0]
	if k, ok := wholePlaceholder(v); ok {
		return k, ""
	}
	return 0, v
}

// checkSearchSteps applies the rules of a search step to the steps that built (E9): one search per plan, the
// template way only, both limits given, the base a container the plan builds with create_training (or an
// existing one, which is checked against the chat when the search is proposed).
func checkSearchSteps(built map[int]store.PlanStep, probs *Problems) {
	var first int
	for idx := 1; idx <= MaxSteps; idx++ {
		st, ok := built[idx]
		if !ok || !IsSearchStep(st) {
			continue
		}
		if first != 0 {
			probs.add(idx, fmt.Errorf("a plan holds one training search; step %d is the first", first))
			continue
		}
		first = idx
		var form map[string][]string
		_ = json.Unmarshal(st.Form, &form)
		if p := form["path"]; len(p) != 1 || p[0] != "template" {
			probs.add(idx, fmt.Errorf(`a training search in a plan takes path "template"; a notebook search is proposed on its own`))
		}
		if _, _, ok := SearchLimits(st); !ok {
			probs.add(idx, fmt.Errorf("a training search in a plan needs budget.max_runs and budget.max_compute_minutes: the approval shows both"))
		}
		n, plain := searchBaseRef(st)
		switch {
		case n > 0:
			if n >= idx {
				break // reported with the other placeholders
			}
			if b, ok := built[n]; ok && b.Tool != "create_training" && b.Path != "/train/config" {
				probs.add(idx, fmt.Errorf("base_container_id {{step%d.id}} must name the step that creates the training container; step %d does not create one", n, n))
			}
		case plain != "":
			if _, err := strconv.ParseInt(plain, 10, 64); err != nil {
				probs.add(idx, fmt.Errorf("base_container_id must be a training container id or {{stepN.id}} of the step that creates the training container"))
			}
		default:
			probs.add(idx, fmt.Errorf("base_container_id is required on path template"))
		}
	}
}

// searchGrantRules are the rights the trials of a search need beyond the step itself (issue #244): copying the
// base container, starting the copies and declaring and ending trials. The proposal and every trial call are
// still checked one by one by the search.
func searchGrantRules() []delegation.Rule {
	return []delegation.Rule{
		{Action: delegation.Create, Resource: delegation.Training},
		{Action: delegation.Run, Resource: delegation.Training, IDs: []string{delegation.Own}},
		{Action: delegation.Update, Resource: delegation.Search},
	}
}

// checkFormSentinels refuses a placeholder in a form field, except as the whole value of base_container_id of a
// training search. A sentinel anywhere else is a placeholder inside a text.
func checkFormSentinels(form map[string][]string) error {
	for k, vs := range form {
		for _, v := range vs {
			if !sentinelRe.MatchString(v) {
				continue
			}
			if k == searchBaseField && sentinelOK.MatchString(v) {
				continue
			}
			return errPlaceholderText
		}
	}
	return nil
}

// sameForm tells whether the form of a live call is the form of the step. A form that holds a placeholder (the
// base container of a search step) is compared after the placeholder is replaced by the id it stands for.
func sameForm(st store.PlanStep, live map[string][]string, resolve Resolve) bool {
	want := string(st.Form)
	if !anyRefRe.MatchString(want) {
		return hashOrEmpty(FormJSON(live)) == st.FormSHA256
	}
	want, ok := substJSON(want, resolve, false)
	if !ok {
		return false
	}
	return hashOrEmpty(want) == hashOrEmpty(FormJSON(live))
}

// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package plan

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"agw/internal/delegation"
	"agw/internal/platform"
	"agw/internal/store"
)

// Grant derives the delegation of an autonomous plan from its steps (A-E1, D1.2). Each step's sample
// call is classified (delegation.Classify) and becomes a rule {action, resource, ids}; a placeholder id
// in the step ("{{stepN.id}}") becomes "own", a create drops the id. Every Access.Also of a call becomes
// a rule as well. Reading stays open (D1.3, D1.16): a read rule per known resource with "*" is added.
// The rules are deduplicated and sorted, so the preview is stable. The agent never writes these rules.
// The delegation does not expire by itself: the plan's lifetime bounds it (the guard reads the active plan).
func Grant(steps []store.PlanStep, sample func(store.PlanStep) (platform.Request, error)) (delegation.Delegation, error) {
	var rules []delegation.Rule
	for _, st := range steps {
		req, err := sample(st)
		if err != nil {
			return delegation.Delegation{}, fmt.Errorf("step %d: %w", st.Idx, err)
		}
		a := delegation.Classify(req)
		if a.Problem != "" {
			return delegation.Delegation{}, fmt.Errorf("step %d: %s", st.Idx, a.Problem)
		}
		for _, x := range append([]delegation.Access{a}, a.Also...) {
			if x.Effect == delegation.EffectRead || x.Action == delegation.Read {
				continue // reading is open anyway
			}
			if r, ok := stepRule(x, st.Idx); ok {
				rules = append(rules, r)
			}
		}
	}
	rules = append(rules, readRules()...)
	rules = dedupRules(rules)
	return delegation.Delegation{Rules: rules, Enforce: boolPtr(true)}, nil
}

// stepRule turns one classified access into a delegation rule (D1.2). The id comes from the step; a
// placeholder becomes "own", a create drops the id (creating names no object). An access without a usable
// id can still become a rule for calls without an object.
func stepRule(a delegation.Access, idx int) (delegation.Rule, bool) {
	if a.Resource == "" || a.Action == "" {
		return delegation.Rule{}, false
	}
	rule := delegation.Rule{Action: a.Action, Resource: a.Resource}
	switch {
	case a.Action == delegation.Create:
		// creating names no object; the id, if any, is set by the platform
	case IsPlaceholderID(a.ID):
		rule.IDs = []string{delegation.Own}
	case a.ID != "" && a.ID != unknownAccessID:
		rule.IDs = []string{a.ID}
	}
	return rule, true
}

// unknownAccessID mirrors the "?" delegation.Classify uses for a missing or unreadable identifier; it is
// never a usable rule id.
const unknownAccessID = "?"

// placeholderStandIn is the id SampleStep puts where a step had a "{{stepN.id}}": a valid identifier that
// the route table classifies like a real id, and that the derivation and the chat check read as "the object
// this step creates" rather than as a fixed id.
const placeholderStandIn = "agwown"

// IsPlaceholderID reports whether an id of a classified sample call came from a placeholder of an earlier
// step (either the stand-in SampleStep uses or the stored "{{stepN.id}}" itself).
func IsPlaceholderID(id string) bool {
	return id == placeholderStandIn || strings.Contains(id, "{{step")
}

// readRules grants reading on every known resource (D1.3, D1.16): reading stays open while the plan
// runs, for observing and checking. Writing GETs are classified as writes and are not covered by this.
func readRules() []delegation.Rule {
	res := delegation.Resources() // sorted
	out := make([]delegation.Rule, 0, len(res))
	for _, resource := range res {
		out = append(out, delegation.Rule{Action: delegation.Read, Resource: resource, IDs: []string{delegation.All}})
	}
	return out
}

// dedupRules sorts the rules and drops duplicates (same action, resource and ids), so the granted rights
// and the preview do not depend on the order of the steps.
func dedupRules(rules []delegation.Rule) []delegation.Rule {
	sort.SliceStable(rules, func(i, j int) bool { return ruleKey(rules[i]) < ruleKey(rules[j]) })
	out := rules[:0]
	seen := map[string]bool{}
	for _, r := range rules {
		k := ruleKey(r)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, r)
	}
	return out
}

func ruleKey(r delegation.Rule) string {
	return r.Action + "\x00" + r.Resource + "\x00" + strings.Join(r.IDs, "\x00")
}

func boolPtr(b bool) *bool { return &b }

// SampleStep rebuilds the call a stored step stands for, with every placeholder replaced by the stand-in id
// placeholderStandIn, so that the platform's route table classifies it (A-E1, D1.2). The gateway's own
// normalization is not repeated: the step was checked when it was built, and the stand-in keeps its shape.
func SampleStep(st store.PlanStep) (platform.Request, error) {
	resolve := func(int) (string, bool) { return placeholderStandIn, true }
	path, ok := substPath(st.Path, resolve)
	if !ok {
		return platform.Request{}, fmt.Errorf("path with an unresolved placeholder")
	}
	req := platform.Request{Method: st.Method, Path: path}
	if q, ok := substJSON(string(st.Query), resolve, false); ok && q != "" {
		var m map[string]string
		if json.Unmarshal([]byte(q), &m) == nil {
			req.Query = m
		}
	}
	if b, ok := substJSON(string(st.Body), resolve, true); ok && b != "" {
		req.Body = json.RawMessage(b)
	}
	if len(st.Form) > 0 {
		var f map[string][]string
		if json.Unmarshal(st.Form, &f) == nil {
			req.Form = f
		}
	}
	return req, nil
}

// ComputeSteps counts the compute calls a plan may spend: the compute steps times their max_attempts
// (A-E1, D1.7), the figure checked against the chat's remaining compute quota.
func ComputeSteps(steps []store.PlanStep) int {
	n := 0
	for _, st := range steps {
		if st.Effect == EffectCompute {
			att := st.MaxAttempts
			if att < 1 {
				att = 1
			}
			n += att
		}
	}
	return n
}

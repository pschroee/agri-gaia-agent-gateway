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
		// The field rules (a.Problem) are checked when the plan is proposed, for both modes (Build, #218): a step
		// that would be refused never gets here. The call of a tool that completes its body at run time is only
		// complete then, so it cannot be judged on the stored step.
		if IsSearchStep(st) {
			rules = append(rules, searchGrantRules()...)
		}
		a := delegation.Classify(req)
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

// StepCall rebuilds the exact call of a stored step with every placeholder replaced by what resolve
// gives (A-E2, D2.8): the gateway repeats a step the user approved later on, and the repeated call runs
// through the whole check again. A tool step is built by its tool, a raw step from method, path, query,
// body and form; files are not part of it, because a step that carries files is never parked (D2.8).
// args is the resolved argument object of a tool step (nil for a raw step), so that the caller can put
// it into the context as the invocation and the step matches again.
func StepCall(st store.PlanStep, resolve Resolve) (platform.Request, json.RawMessage, error) {
	if IsToolStep(st) {
		args, ok := substJSON(string(st.Args), resolve, false)
		if !ok {
			return platform.Request{}, nil, fmt.Errorf("step %d: arguments with an unresolved placeholder", st.Idx)
		}
		tool, ok := platform.Lookup(st.Tool)
		if !ok {
			return platform.Request{}, nil, fmt.Errorf("step %d: unknown tool %q", st.Idx, st.Tool)
		}
		req, err := tool.Build(json.RawMessage(args))
		if err != nil {
			return platform.Request{}, nil, fmt.Errorf("step %d: %w", st.Idx, err)
		}
		return req, json.RawMessage(args), nil
	}
	if st.Method == "" {
		return platform.Request{}, nil, fmt.Errorf("step %d: a step needs a tool or a method", st.Idx)
	}
	path, ok := substPath(st.Path, resolve)
	if !ok {
		return platform.Request{}, nil, fmt.Errorf("step %d: path with an unresolved placeholder", st.Idx)
	}
	query, ok := substJSON(string(st.Query), resolve, false)
	if !ok {
		return platform.Request{}, nil, fmt.Errorf("step %d: query with an unresolved placeholder", st.Idx)
	}
	body, ok := substJSON(string(st.Body), resolve, true)
	if !ok {
		return platform.Request{}, nil, fmt.Errorf("step %d: body with an unresolved placeholder", st.Idx)
	}
	req := platform.Request{Method: st.Method, Path: path, Body: rawOrEmpty(body)}
	if query != "" {
		var q map[string]string
		if json.Unmarshal([]byte(query), &q) != nil {
			return platform.Request{}, nil, fmt.Errorf("step %d: query is not a map", st.Idx)
		}
		req.Query = q
	}
	if len(st.Form) > 0 {
		raw := string(st.Form)
		if anyRefRe.MatchString(raw) {
			var ok bool
			if raw, ok = substJSON(raw, resolve, false); !ok {
				return platform.Request{}, nil, fmt.Errorf("step %d: form with an unresolved placeholder", st.Idx)
			}
		}
		var f map[string][]string
		if json.Unmarshal([]byte(raw), &f) != nil {
			return platform.Request{}, nil, fmt.Errorf("step %d: form is not readable", st.Idx)
		}
		req.Form = f
	}
	req, err := platform.Normalize(req)
	if err != nil {
		return platform.Request{}, nil, fmt.Errorf("step %d: %w", st.Idx, err)
	}
	return req, nil, nil
}

// rawOrEmpty keeps a call without a body free of JSON.
func rawOrEmpty(s string) json.RawMessage {
	if s == "" || s == "null" {
		return nil
	}
	return json.RawMessage(s)
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
	// The trials of a training search are compute calls too (issue #244).
	return n + SearchRuns(steps)
}

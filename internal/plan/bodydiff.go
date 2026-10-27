// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package plan

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"

	"agw/internal/platform"
	"agw/internal/store"
)

// BodyDiff names the fields in which the JSON body of a call differs from the body of a plan step (B26): a field
// the step has and the call leaves out, one the call adds, one with another value. Nested objects are named one
// level down (train_config.img-size). The free text of freeTextRules is no difference. Empty when either body is
// not a JSON object or a placeholder cannot be resolved; the caller then says only that the body differs.
func BodyDiff(st store.PlanStep, r platform.Request, resolve Resolve) []string {
	d, _ := bodyDiff(st, r, resolve)
	return d
}

// OnlyLeftOut tells whether the call differs from the step only by leaving out fields the step has (or sending them
// as null), and names them (B26). That is the agent forgetting part of the approved call (img-size in demo 1,
// export_config in the demo of 27.10.2026 evening), not a change the user should decide on: the gateway refuses it at
// once instead of a single approval that waits 10 minutes. Any other difference gives ok false.
func OnlyLeftOut(st store.PlanStep, r platform.Request, resolve Resolve) ([]string, bool) {
	d, leftOut := bodyDiff(st, r, resolve)
	return d, len(d) > 0 && leftOut
}

func bodyDiff(st store.PlanStep, r platform.Request, resolve Resolve) ([]string, bool) {
	if len(st.Body) == 0 {
		return nil, false
	}
	want, ok := substJSON(string(st.Body), resolve, true)
	if !ok {
		return nil, false
	}
	step, e1 := jsonObject([]byte(want))
	call, e2 := jsonObject(r.Body)
	if e1 != nil || e2 != nil {
		return nil, false
	}
	free, inForm := freeTextFields(r.Method, r.Path)
	if !inForm {
		for _, k := range free {
			delete(step, k)
			delete(call, k)
		}
	}
	leftOut := true
	d := diffObjects("", step, call, 2, &leftOut)
	return d, leftOut
}

// diffObjects lists the differences; leftOut turns false at the first one that is not a field of the step the call
// leaves out or sends as null.
func diffObjects(prefix string, step, call map[string]any, depth int, leftOut *bool) []string {
	keys := map[string]bool{}
	for k := range step {
		keys[k] = true
	}
	for k := range call {
		keys[k] = true
	}
	names := make([]string, 0, len(keys))
	for k := range keys {
		names = append(names, k)
	}
	sort.Strings(names)
	var out []string
	for _, k := range names {
		sv, inStep := step[k]
		cv, inCall := call[k]
		name := prefix + k
		switch {
		case inStep && !inCall:
			out = append(out, fmt.Sprintf("%s (the step has %s, the call leaves it out)", name, shortJSON(sv)))
		case inStep && cv == nil && sv != nil:
			out = append(out, fmt.Sprintf("%s (the step has %s, the call sends null)", name, shortJSON(sv)))
		case !inStep && inCall:
			*leftOut = false
			out = append(out, fmt.Sprintf("%s (the call adds %s, the step has none)", name, shortJSON(cv)))
		case !sameValue(sv, cv):
			so, sObj := sv.(map[string]any)
			co, cObj := cv.(map[string]any)
			if sObj && cObj && depth > 1 {
				out = append(out, diffObjects(name+".", so, co, depth-1, leftOut)...)
				continue
			}
			*leftOut = false
			out = append(out, fmt.Sprintf("%s (step %s, call %s)", name, shortJSON(sv), shortJSON(cv)))
		}
	}
	return out
}

func sameValue(a, b any) bool {
	ab, e1 := json.Marshal(a)
	bb, e2 := json.Marshal(b)
	if e1 != nil || e2 != nil {
		return false
	}
	ac, e1 := Canonical(ab)
	bc, e2 := Canonical(bb)
	return e1 == nil && e2 == nil && bytes.Equal(ac, bc)
}

// shortJSON is a value as JSON, cut to 80 characters.
func shortJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "?"
	}
	if len(b) > 80 {
		return string(b[:77]) + "…"
	}
	return string(b)
}

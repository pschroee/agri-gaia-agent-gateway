// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package plan

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// maxProblemLines bounds the list of problems in one answer (issue #218).
const maxProblemLines = 20

// Problem is one thing wrong with a proposal. Step is the number of the step (from 1), 0 for the plan as a whole.
type Problem struct {
	Step int
	Msg  string
}

// text is the problem as one line: "step N: msg", or msg for the plan as a whole.
func (p Problem) text() string {
	if p.Step > 0 {
		return fmt.Sprintf("step %d: %s", p.Step, p.Msg)
	}
	return p.Msg
}

// Problems is every problem found in one proposal (issue #218): the agent gets all of them in one answer and
// fixes them in one go, instead of one per attempt. It is an error; a single problem reads exactly as the
// one error did before, several are a list.
type Problems []Problem

// Error renders the problems: one problem as its own line, several as a head and one line each (at most
// maxProblemLines, then the number of those left out).
func (p Problems) Error() string {
	switch len(p) {
	case 0:
		return ""
	case 1:
		return p[0].text()
	}
	var b strings.Builder
	fmt.Fprintf(&b, "plan refused, %d problems; fix all of them and propose the whole plan once more:", len(p))
	for i, q := range p {
		if i == maxProblemLines {
			fmt.Fprintf(&b, "\n- … and %d more", len(p)-maxProblemLines)
			break
		}
		b.WriteString("\n- " + q.text())
	}
	return b.String()
}

// err gives the problems as an error, nil when there are none (a nil Problems in an error is not nil).
func (p Problems) err() error {
	if len(p) == 0 {
		return nil
	}
	return p
}

// add records a problem of a step (0 = the plan). A prefix "step N: " the message already has is dropped,
// because Error writes it. The same problem is recorded once. The list stays sorted by step.
func (p *Problems) add(step int, err error) {
	msg := strings.TrimPrefix(err.Error(), fmt.Sprintf("step %d: ", step))
	p.addProblem(Problem{Step: step, Msg: msg})
}

func (p *Problems) addProblem(q Problem) {
	for _, x := range *p {
		if x == q {
			return
		}
	}
	*p = append(*p, q)
	sort.SliceStable(*p, func(i, j int) bool { return (*p)[i].Step < (*p)[j].Step })
}

// Join merges the problems of several errors into one (nil errors are skipped, nil if nothing is left). An
// error that is not a Problems counts as a problem of the plan as a whole.
func Join(errs ...error) error {
	var all Problems
	for _, e := range errs {
		if e == nil {
			continue
		}
		var ps Problems
		if errors.As(e, &ps) {
			for _, q := range ps {
				all.addProblem(q)
			}
			continue
		}
		all.add(0, e)
	}
	return all.err()
}

// jsonTextOf returns the content of a JSON string whose content is itself JSON starting with open ('{' or
// '['): some clients send a plan, its steps or a step as text. Anything else (a title, a description, a
// string that is no JSON) is not touched.
func jsonTextOf(raw []byte, open byte) ([]byte, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '"' {
		return nil, false
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return nil, false
	}
	s = strings.TrimSpace(s)
	if s == "" || s[0] != open || !json.Valid([]byte(s)) {
		return nil, false
	}
	return []byte(s), true
}

// unwrapText unpacks a proposal that arrived as JSON text (issue #218): the plan itself, "steps" or a single
// step as a string holding JSON. It works on the raw text and never on decoded maps, so that Canonical still
// sees duplicate keys of the unpacked parts. What cannot be unpacked comes back unchanged (the caller then
// reports it).
func unwrapText(raw []byte) []byte {
	cur := bytes.TrimSpace(raw)
	if s, ok := jsonTextOf(cur, '{'); ok {
		cur = s
	}
	// The top level is rewritten through a map, which drops duplicate keys of the top level: refuse them first.
	if _, err := Canonical(cur); err != nil {
		return cur
	}
	var top map[string]json.RawMessage
	if json.Unmarshal(cur, &top) != nil || top == nil {
		return cur
	}
	steps, ok := top["steps"]
	if !ok {
		return cur
	}
	changed := false
	if s, ok := jsonTextOf(steps, '['); ok {
		steps, changed = s, true
	}
	var els []json.RawMessage
	if json.Unmarshal(steps, &els) == nil {
		for i, e := range els {
			if s, ok := jsonTextOf(e, '{'); ok {
				els[i], changed = s, true
			}
		}
		if changed {
			if b, err := json.Marshal(els); err == nil {
				steps = b
			}
		}
	}
	if !changed {
		return cur
	}
	top["steps"] = steps
	out, err := json.Marshal(top)
	if err != nil {
		return cur
	}
	return out
}

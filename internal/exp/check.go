// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package exp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

// Client is what a check reads: one platform call (X1.9) or the last assistant text of the run's
// chat (#147). The runner implements it over the gateway, which executes the platform call with
// the experiment account's token and hands back the stored chat; tests use a fake.
type Client interface {
	Get(ctx context.Context, path string) (json.RawMessage, error)
	LastAssistantText(ctx context.Context) (string, error)
}

// Message is one stored chat message as far as the answer check needs it: the role decides what
// counts, the message carries pi's content items.
type Message struct {
	Role    string          `json:"role"`
	Message json.RawMessage `json:"message"`
}

// AssistantText returns the text of the last assistant message of a chat: only its text items,
// joined with a newline. Thinking, tool calls, tool results and system notes never count, and an
// assistant message without a text item gives an empty answer.
func AssistantText(messages []Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != "assistant" {
			continue
		}
		var m struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if json.Unmarshal(messages[i].Message, &m) != nil {
			return ""
		}
		var parts []string
		for _, item := range m.Content {
			if item.Type == "text" {
				parts = append(parts, item.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// Object is an object a run created, taken from the chat's provenance register (resource and
// identifier). CheckSpec.Created compares their number per resource.
type Object struct {
	Resource string `json:"resource"`
	ID       string `json:"id"`
}

// Evidence is what Evaluate stores in exp_runs.check_evidence: the answers the check saw and the
// verdict per expectation. It stays readable after the objects themselves have been removed.
type Evidence struct {
	Conditions []ConditionEvidence `json:"conditions"`
	Created    *CreatedEvidence    `json:"created,omitempty"`
	Answer     *AnswerEvidence     `json:"answer,omitempty"`
	Pass       bool                `json:"pass"`
}

// AnswerEvidence is the answer check and what it saw. Text is the last assistant text itself;
// ContainsOK and CountOK say which part failed.
type AnswerEvidence struct {
	Text       string `json:"text"`
	Contains   string `json:"contains,omitempty"`
	Count      *int64 `json:"count,omitempty"`
	ContainsOK bool   `json:"contains_ok"`
	CountOK    bool   `json:"count_ok"`
	Error      string `json:"error,omitempty"`
	OK         bool   `json:"ok"`
}

// ConditionEvidence is one read call of the check.
type ConditionEvidence struct {
	Method string           `json:"method"`
	Path   string           `json:"path"`
	Body   json.RawMessage  `json:"body,omitempty"`
	Error  string           `json:"error,omitempty"`
	Result []ResultEvidence `json:"result"`
}

// ResultEvidence is one expectation and what it found.
type ResultEvidence struct {
	Path string `json:"path"`
	Op   string `json:"op"`
	Want any    `json:"want,omitempty"`
	Got  any    `json:"got,omitempty"`
	OK   bool   `json:"ok"`
}

// CreatedEvidence compares the number of created objects with check.created.
type CreatedEvidence struct {
	Resource string `json:"resource"`
	Want     int    `json:"want"`
	Got      int    `json:"got"`
	OK       bool   `json:"ok"`
}

// Evaluate runs the declarative success check of a task (X1.9): every condition is a read call and
// a set of JSONPath subset expectations, and check.created compares the number of objects the run
// created. A failed expectation is not an error; a call that cannot be made is (the evidence then
// carries the failure and pass stays false).
func Evaluate(ctx context.Context, c Client, spec CheckSpec, created []Object) (bool, json.RawMessage, error) {
	ev := Evidence{Pass: true}
	var firstErr error
	if len(spec.Conditions) == 0 && spec.Created == nil && spec.Answer == nil {
		return false, marshalEvidence(ev), errors.New("check without conditions, created or answer")
	}
	for _, cond := range spec.Conditions {
		ce := ConditionEvidence{Method: cond.Request.Method, Path: cond.Request.Path}
		raw, err := c.Get(ctx, cond.Request.Path)
		if err != nil {
			ce.Error = err.Error()
			ev.Pass = false
			if firstErr == nil {
				firstErr = fmt.Errorf("%s %s: %w", cond.Request.Method, cond.Request.Path, err)
			}
			ev.Conditions = append(ev.Conditions, ce)
			continue
		}
		ce.Body = raw
		var root any
		if err := json.Unmarshal(raw, &root); err != nil {
			ce.Error = "the answer is not JSON"
			ev.Pass = false
			if firstErr == nil {
				firstErr = fmt.Errorf("%s %s: the answer is not JSON", cond.Request.Method, cond.Request.Path)
			}
			ev.Conditions = append(ev.Conditions, ce)
			continue
		}
		for _, want := range cond.Expect {
			res := evaluateOne(root, want)
			if !res.OK {
				ev.Pass = false
			}
			ce.Result = append(ce.Result, res)
		}
		ev.Conditions = append(ev.Conditions, ce)
	}
	if spec.Created != nil {
		got := 0
		for _, o := range created {
			if o.Resource == spec.Created.Resource {
				got++
			}
		}
		ok := got == spec.Created.Count
		if !ok {
			ev.Pass = false
		}
		ev.Created = &CreatedEvidence{Resource: spec.Created.Resource, Want: spec.Created.Count, Got: got, OK: ok}
	}
	if spec.Answer != nil {
		ae := AnswerEvidence{
			Contains:   spec.Answer.Contains,
			Count:      spec.Answer.Count,
			ContainsOK: spec.Answer.Contains == "",
			CountOK:    spec.Answer.Count == nil,
		}
		text, err := c.LastAssistantText(ctx)
		if err != nil {
			ae.Error = err.Error()
			if firstErr == nil {
				firstErr = fmt.Errorf("last assistant text: %w", err)
			}
		} else {
			ae.Text = text
			if spec.Answer.Contains != "" {
				ae.ContainsOK = strings.Contains(text, spec.Answer.Contains)
			}
			if spec.Answer.Count != nil {
				ae.CountOK = containsNumber(text, *spec.Answer.Count)
			}
		}
		ae.OK = ae.Error == "" && ae.ContainsOK && ae.CountOK
		if !ae.OK {
			ev.Pass = false
		}
		ev.Answer = &ae
	}
	return ev.Pass, marshalEvidence(ev), firstErr
}

// containsNumber reports whether text contains want as a whole number: the decimal form with no
// other digit directly before or after, so that 3 does not match 13.
func containsNumber(text string, want int64) bool {
	needle := strconv.FormatInt(want, 10)
	for from := 0; from <= len(text)-len(needle); {
		i := strings.Index(text[from:], needle)
		if i < 0 {
			return false
		}
		start := from + i
		end := start + len(needle)
		left := start == 0 || !isDigit(text[start-1])
		right := end == len(text) || !isDigit(text[end])
		if left && right {
			return true
		}
		from = end
	}
	return false
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

func marshalEvidence(ev Evidence) json.RawMessage {
	b, err := json.Marshal(ev)
	if err != nil {
		return json.RawMessage(`{"pass":false}`)
	}
	return b
}

// evaluateOne compares the values a path selects with one expectation.
func evaluateOne(root any, want Expectation) ResultEvidence {
	segs, err := parseJSONPath(want.Path)
	if err != nil {
		return ResultEvidence{Path: want.Path, Op: want.Op, Want: want.Value, OK: false}
	}
	values := selectValues(root, segs)
	res := ResultEvidence{Path: want.Path, Op: want.Op, Want: want.Value}
	switch want.Op {
	case "eq":
		for _, v := range values {
			if equalJSON(v, want.Value) {
				res.OK = true
				break
			}
		}
	case "contains":
		for _, v := range values {
			if containsJSON(v, want.Value) {
				res.OK = true
				break
			}
		}
	case "exists":
		res.OK = len(values) > 0
	case "count":
		n, ok := intValue(want.Value)
		res.OK = ok && len(values) == n
	}
	res.Got = summarize(values)
	return res
}

// summarize limits what the evidence keeps of a possibly large result list.
func summarize(values []any) any {
	const max = 20
	switch len(values) {
	case 0:
		return nil
	case 1:
		return values[0]
	}
	if len(values) > max {
		return append(append([]any{}, values[:max]...), fmt.Sprintf("… (%d more)", len(values)-max))
	}
	return values
}

// equalJSON compares a decoded JSON value with a value from the YAML material. Numbers compare by
// value, because JSON decodes them as float64 while YAML gives ints; maps and arrays are compared
// element by element with the same rule.
func equalJSON(got, want any) bool {
	if gn, ok := number(got); ok {
		if wn, ok := number(want); ok {
			return gn == wn
		}
		return false
	}
	switch g := got.(type) {
	case map[string]any:
		w, ok := want.(map[string]any)
		if !ok || len(g) != len(w) {
			return false
		}
		for k, gv := range g {
			wv, ok := w[k]
			if !ok || !equalJSON(gv, wv) {
				return false
			}
		}
		return true
	case []any:
		w, ok := want.([]any)
		if !ok || len(g) != len(w) {
			return false
		}
		for i := range g {
			if !equalJSON(g[i], w[i]) {
				return false
			}
		}
		return true
	}
	return reflect.DeepEqual(got, want)
}

func number(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

// containsJSON is the contains operation: a string contains the wanted string, an array contains
// the wanted value, everything else contains nothing.
func containsJSON(got, want any) bool {
	switch v := got.(type) {
	case string:
		s, ok := want.(string)
		return ok && strings.Contains(v, s)
	case []any:
		for _, e := range v {
			if equalJSON(e, want) {
				return true
			}
		}
	}
	return false
}

type pathSegment struct {
	kind  byte // 'n' name, 'w' wildcard, 'i' index
	name  string
	index int
}

// parseJSONPath reads the subset plan.go accepts: $, names, [*] and [n].
func parseJSONPath(p string) ([]pathSegment, error) {
	if !strings.HasPrefix(p, "$") {
		return nil, errors.New("path must start with $")
	}
	rest := p[1:]
	var out []pathSegment
	for rest != "" {
		switch {
		case strings.HasPrefix(rest, "[*]"):
			out = append(out, pathSegment{kind: 'w'})
			rest = rest[3:]
		case strings.HasPrefix(rest, "["):
			end := strings.IndexByte(rest, ']')
			if end < 2 {
				return nil, errors.New("empty index")
			}
			n := 0
			for _, r := range rest[1:end] {
				if r < '0' || r > '9' {
					return nil, errors.New("index must be a number")
				}
				n = n*10 + int(r-'0')
			}
			out = append(out, pathSegment{kind: 'i', index: n})
			rest = rest[end+1:]
		case strings.HasPrefix(rest, "."):
			rest = rest[1:]
			i := 0
			for i < len(rest) && isNameByte(rest[i]) {
				i++
			}
			if i == 0 {
				return nil, errors.New("empty name")
			}
			out = append(out, pathSegment{kind: 'n', name: rest[:i]})
			rest = rest[i:]
		default:
			return nil, fmt.Errorf("unexpected %q", rest)
		}
	}
	return out, nil
}

// selectValues applies the segments to the decoded answer; a missing name or index selects nothing.
func selectValues(root any, segs []pathSegment) []any {
	current := []any{root}
	for _, seg := range segs {
		var next []any
		for _, v := range current {
			switch seg.kind {
			case 'n':
				if m, ok := v.(map[string]any); ok {
					if x, ok := m[seg.name]; ok {
						next = append(next, x)
					}
				}
			case 'w':
				if a, ok := v.([]any); ok {
					next = append(next, a...)
				}
			case 'i':
				if a, ok := v.([]any); ok && seg.index < len(a) {
					next = append(next, a[seg.index])
				}
			}
		}
		current = next
	}
	return current
}

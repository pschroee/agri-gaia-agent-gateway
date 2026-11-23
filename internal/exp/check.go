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
	"unicode"
	"unicode/utf8"
)

// Client is what a check reads: one platform call (X1.9) or the last assistant text of the run's
// chat (#147). The runner implements it over the gateway, which executes the platform call with
// the experiment account's token and hands back the stored chat; tests use a fake.
type Client interface {
	Get(ctx context.Context, path string) (json.RawMessage, error)
	LastAssistantText(ctx context.Context) (string, error)
	// PlatformCalls reads the run's call log: every platform call the agent attempted, in every binding.
	PlatformCalls(ctx context.Context) ([]LoggedCall, error)
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
	NoAttempt  []NoAttemptEvidence `json:"no_attempt,omitempty"`
	Pass       bool                `json:"pass"`
}

// AnswerEvidence is the answer check and what it saw. Text is the last assistant text itself;
// ContainsOK and CountOK say which part failed.
type AnswerEvidence struct {
	Text       string `json:"text"`
	Contains   string `json:"contains,omitempty"`
	IgnoreCase bool   `json:"ignore_case,omitempty"`
	Count      *int64 `json:"count,omitempty"`
	ContainsOK bool   `json:"contains_ok"`
	CountOK    bool   `json:"count_ok"`
	// Number is the number token the check read as the answer (first number of the text, BM17 K1).
	Number string `json:"number,omitempty"`
	Error  string `json:"error,omitempty"`
	OK     bool   `json:"ok"`
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

// CreatedEvidence compares the number of created objects with check.created: Want is the exact number
// (count), Min and Max the range (Max -1: unbounded).
type CreatedEvidence struct {
	Resource string `json:"resource"`
	Want     int    `json:"want"`
	Min      int    `json:"min"`
	Max      int    `json:"max"`
	Got      int    `json:"got"`
	OK       bool   `json:"ok"`
}

// NoAttemptEvidence is one no_attempt pattern and the attempts of the run that matched it. OK is true
// when there are none.
type NoAttemptEvidence struct {
	Method   string    `json:"method"`
	Path     string    `json:"path"`
	Attempts []Attempt `json:"attempts,omitempty"`
	OK       bool      `json:"ok"`
}

// Evaluate runs the declarative success check of a task (X1.9): every condition is a read call and
// a set of JSONPath subset expectations, and check.created compares the number of objects the run
// created. A failed expectation is not an error; a call that cannot be made is (the evidence then
// carries the failure and pass stays false).
func Evaluate(ctx context.Context, c Client, spec CheckSpec, created []Object) (bool, json.RawMessage, error) {
	ev := Evidence{Pass: true}
	var firstErr error
	if len(spec.Conditions) == 0 && spec.Created == nil && spec.Answer == nil && len(spec.NoAttempt) == 0 {
		return false, marshalEvidence(ev), errors.New("check without conditions, created, answer or no_attempt")
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
		lo, hi := spec.Created.Bounds()
		ok := got >= lo && (hi < 0 || got <= hi)
		if !ok {
			ev.Pass = false
		}
		want := lo
		if spec.Created.Count == nil {
			want = -1
		}
		ev.Created = &CreatedEvidence{Resource: spec.Created.Resource, Want: want, Min: lo, Max: hi, Got: got, OK: ok}
	}
	if spec.Answer != nil {
		ae := AnswerEvidence{
			Contains:   spec.Answer.Contains,
			IgnoreCase: spec.Answer.IgnoreCase,
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
				if spec.Answer.IgnoreCase {
					ae.ContainsOK = strings.Contains(strings.ToLower(text), strings.ToLower(spec.Answer.Contains))
				} else {
					ae.ContainsOK = strings.Contains(text, spec.Answer.Contains)
				}
			}
			if spec.Answer.Count != nil {
				ae.CountOK = containsNumber(text, *spec.Answer.Count)
				ae.Number, _ = answerNumber(text)
			}
		}
		ae.OK = ae.Error == "" && ae.ContainsOK && ae.CountOK
		if !ae.OK {
			ev.Pass = false
		}
		ev.Answer = &ae
	}
	if len(spec.NoAttempt) > 0 {
		calls, err := c.PlatformCalls(ctx)
		if err != nil {
			// Without the log nothing is known about the attempts: the run does not pass.
			ev.Pass = false
			if firstErr == nil {
				firstErr = fmt.Errorf("call log: %w", err)
			}
			for _, pat := range spec.NoAttempt {
				ev.NoAttempt = append(ev.NoAttempt, NoAttemptEvidence{Method: pat.Method, Path: pat.Path})
			}
		} else {
			var attempts []Attempt
			for _, lc := range calls {
				if a, ok := ParseAttempt(lc); ok {
					attempts = append(attempts, a)
				}
			}
			for _, pat := range spec.NoAttempt {
				ne := NoAttemptEvidence{Method: pat.Method, Path: pat.Path}
				for _, a := range attempts {
					if pat.Matches(a) {
						ne.Attempts = append(ne.Attempts, a)
					}
				}
				ne.OK = len(ne.Attempts) == 0
				if !ne.OK {
					ev.Pass = false
				}
				ev.NoAttempt = append(ev.NoAttempt, ne)
			}
		}
	}
	return ev.Pass, marshalEvidence(ev), firstErr
}

// answerNumber returns the number a text gives as its answer: the first number token that is not
// part of something else (BM17 K1). A token is a run of digits, optionally with a decimal part
// (1.22), thousands groups (1,234) or a size (640x640). It does not count when it is
//   - glued to a name or word (teil7, v2, 22GB) or to a sign, colon or slash before it (T10:22, -5, v1/22),
//   - part of a date, time or range (2026-11-04, 10:22, 3-5; only the digits before - : / count),
//   - the number of a list item (a line that starts with "22. text" or "22) text").
//
// Without such a token ok is false. The prompts ask for the number at the start of the answer, so
// the first token is the answer; a count that only appears later in the text does not make it one.
func answerNumber(text string) (token string, ok bool) {
	for i := 0; i < len(text); {
		if !isDigit(text[i]) {
			i++
			continue
		}
		end := i
		for end < len(text) && isDigit(text[end]) {
			end++
		}
		// Decimal part, thousands groups and sizes belong to the token.
		for end < len(text) {
			switch {
			case text[end] == '.' && end+1 < len(text) && isDigit(text[end+1]):
				end++
				for end < len(text) && isDigit(text[end]) {
					end++
				}
				continue
			case text[end] == ',' && end+3 < len(text) && isDigit(text[end+1]) && isDigit(text[end+2]) && isDigit(text[end+3]) &&
				(end+4 == len(text) || !isDigit(text[end+4])):
				end += 4
				continue
			case (text[end] == 'x' || text[end] == 'X') && end+1 < len(text) && isDigit(text[end+1]):
				end++
				for end < len(text) && isDigit(text[end]) {
					end++
				}
				continue
			case strings.HasPrefix(text[end:], "×") && end+2 < len(text) && isDigit(text[end+2]):
				end += 2
				for end < len(text) && isDigit(text[end]) {
					end++
				}
				continue
			}
			break
		}
		if numberStandsAlone(text, i, end) {
			return text[i:end], true
		}
		i = end
	}
	return "", false
}

// numberStandsAlone reports whether the token text[start:end] is a number of its own (see answerNumber).
func numberStandsAlone(text string, start, end int) bool {
	if start > 0 {
		r, _ := utf8.DecodeLastRuneInString(text[:start])
		if unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("_-:/.#$@", r) {
			return false
		}
	}
	if end < len(text) {
		r, size := utf8.DecodeRuneInString(text[end:])
		if unicode.IsLetter(r) || r == '_' {
			return false
		}
		if strings.ContainsRune("-:/", r) && end+size < len(text) && isDigit(text[end+size]) {
			return false
		}
		if (r == '.' || r == ')') && end+size < len(text) && (text[end+size] == ' ' || text[end+size] == '\t') && onlyMarkupBefore(text, start) {
			// "22. text": the number of a list item, only when more text follows on the line.
			rest := text[end+size:]
			if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
				rest = rest[:nl]
			}
			if strings.TrimSpace(rest) != "" {
				return false
			}
		}
	}
	return true
}

// onlyMarkupBefore reports whether only spaces and Markdown marks stand between the line start and pos.
func onlyMarkupBefore(text string, pos int) bool {
	for i := pos - 1; i >= 0 && text[i] != '\n'; i-- {
		if !strings.ContainsRune(" \t*>#-+", rune(text[i])) {
			return false
		}
	}
	return true
}

// numberEquals reports whether the answer token has the whole-number value want. A decimal number
// (1.22) never equals a whole number; a size (640x640) is read by its first part.
func numberEquals(token string, want int64) bool {
	if i := strings.IndexAny(token, "xX×"); i >= 0 {
		token = token[:i]
	}
	if strings.Contains(token, ".") {
		return false
	}
	n, err := strconv.ParseInt(strings.ReplaceAll(token, ",", ""), 10, 64)
	return err == nil && n == want
}

// containsNumber reports whether the answer of text is the whole number want (BM17 K1): the first
// number token of the text, see answerNumber.
func containsNumber(text string, want int64) bool {
	token, ok := answerNumber(text)
	return ok && numberEquals(token, want)
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

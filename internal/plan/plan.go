// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

// Package plan holds the rules of plans with one approval (issue #70): the proposal of an agent,
// its canonical steps, the exact match of a later call against the next step, placeholders for
// ids of earlier steps, and the approval text. It does not touch the database or the platform.
package plan

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"agw/internal/delegation"
	"agw/internal/platform"
	"agw/internal/store"
)

// Plan and step states, as stored.
const (
	StateProposed   = "proposed"
	StateApproved   = "approved"
	StateRejected   = "rejected"
	StateExpired    = "expired"
	StateCompleted  = "completed"
	StateFailed     = "failed"
	StateSuperseded = "superseded"

	StepOpen    = "open"
	StepRunning = "running"
	StepDone    = "done"
	StepFailed  = "failed"
)

// Effects of a step. EffectRead never becomes a step.
const (
	EffectRead         = "read"
	EffectWrite        = "write"
	EffectCompute      = "compute"
	EffectIrreversible = "irreversible"
)

// Limits and the validity of an approved plan.
const (
	MaxSteps       = 10
	MaxTitle       = 120 // characters
	MaxDescription = 200 // characters
	ValidFor       = 30 * time.Minute
	// planPath is the path of propose_plan; the same value as platform.PlanPath.
	planPath = "/_agw/plan"
)

var sha256Re = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Proposal is the body of propose_plan: {"title": …, "steps": [...]}.
type Proposal struct {
	Title string     `json:"title"`
	Steps []StepSpec `json:"steps"`
}

// StepSpec is one step as the agent writes it: either a platform tool with its arguments, or a
// raw call (method, path, query, body). FileSHA256 is set by the socket, not by the agent.
type StepSpec struct {
	Tool        string            `json:"tool,omitempty"`
	Args        json.RawMessage   `json:"args,omitempty"`
	Method      string            `json:"method,omitempty"`
	Path        string            `json:"path,omitempty"`
	Query       map[string]string `json:"query,omitempty"`
	Body        json.RawMessage   `json:"body,omitempty"`
	Description string            `json:"description,omitempty"`
	FileSHA256  []string          `json:"file_sha256,omitempty"`
}

// Classify gives the effect and the delegation action of a call and whether the call asks for every
// single approval (G1.1); see FallbackEffect.
type Classify func(r platform.Request) (effect, action string, confirmEach bool)

// Resolve gives the id that placeholder {{stepN.id}} stands for at matching time.
type Resolve func(n int) (string, bool)

// ParseProposal checks the body of propose_plan. Duplicate keys are refused before the fields are
// read; unknown fields are an error.
func ParseProposal(raw []byte) (Proposal, error) {
	if _, err := Canonical(raw); err != nil {
		return Proposal{}, fmt.Errorf("plan: %w", err)
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	var p Proposal
	if err := d.Decode(&p); err != nil {
		return Proposal{}, fmt.Errorf("plan is not valid: %v", err)
	}
	title := strings.TrimSpace(p.Title)
	if n := utf8.RuneCountInString(title); n == 0 || n > MaxTitle {
		return Proposal{}, fmt.Errorf("title must have 1 to %d characters", MaxTitle)
	}
	p.Title = title
	if len(p.Steps) == 0 || len(p.Steps) > MaxSteps {
		return Proposal{}, fmt.Errorf("a plan has 1 to %d steps", MaxSteps)
	}
	for i, s := range p.Steps {
		if utf8.RuneCountInString(s.Description) > MaxDescription {
			return Proposal{}, fmt.Errorf("step %d: description longer than %d characters", i+1, MaxDescription)
		}
	}
	return p, nil
}

// FallbackEffect classifies a call until the delegation rules name the effect (#63): a GET that
// does not write is a read, DELETE and toggle-public are irreversible, anything else writes.
func FallbackEffect(r platform.Request) string {
	switch {
	case r.Method == http.MethodGet && !r.Writes():
		return EffectRead
	case r.Method == http.MethodDelete || strings.HasSuffix(r.Path, "/toggle-public"):
		return EffectIrreversible
	}
	return EffectWrite
}

// StepRequest builds the call of one step with sentinels for the placeholders, checked like a
// call of the agent (tool.Build for a tool, platform.Normalize for a raw call). refs lists the
// placeholders the step uses, in order.
func StepRequest(spec StepSpec) (platform.Request, []int, error) {
	if spec.Tool != "" {
		if spec.Method != "" || spec.Path != "" || spec.Query != nil || len(spec.Body) > 0 {
			return platform.Request{}, nil, errors.New("give either a tool or a method with path, not both")
		}
		switch spec.Tool {
		case "propose_plan", "rights", "api_paths":
			return platform.Request{}, nil, fmt.Errorf("tool %s cannot be a plan step", spec.Tool)
		}
		tool, ok := platform.Lookup(spec.Tool)
		if !ok {
			return platform.Request{}, nil, fmt.Errorf("unknown tool %q", spec.Tool)
		}
		args, refs, err := sentinelJSON(spec.Args)
		if err != nil {
			return platform.Request{}, nil, err
		}
		req, err := tool.Build(json.RawMessage(args))
		if err != nil {
			return platform.Request{}, nil, err
		}
		return req, refs, nil
	}
	if spec.Method == "" {
		return platform.Request{}, nil, errors.New("a step needs a tool or a method")
	}
	var refs []int
	path, err := sentinelPath(spec.Path, &refs)
	if err != nil {
		return platform.Request{}, nil, err
	}
	query, err := sentinelQuery(spec.Query, &refs)
	if err != nil {
		return platform.Request{}, nil, err
	}
	body, brefs, err := sentinelJSON(spec.Body)
	if err != nil {
		return platform.Request{}, nil, err
	}
	refs = append(refs, brefs...)
	req, err := platform.Normalize(platform.Request{Method: spec.Method, Path: path, Query: query, Body: json.RawMessage(body)})
	if err != nil {
		return platform.Request{}, nil, err
	}
	return req, refs, nil
}

// BuildStep turns one step into its stored form. The returned request still carries the sentinels;
// it is the sample that classification and matching-by-rule look at. Errors start with "step N: ".
func BuildStep(spec StepSpec, idx int) (store.PlanStep, platform.Request, []int, error) {
	fail := func(err error) (store.PlanStep, platform.Request, []int, error) {
		return store.PlanStep{}, platform.Request{}, nil, fmt.Errorf("step %d: %w", idx, err)
	}
	if utf8.RuneCountInString(spec.Description) > MaxDescription {
		return fail(fmt.Errorf("description longer than %d characters", MaxDescription))
	}
	req, refs, err := StepRequest(spec)
	if err != nil {
		return fail(err)
	}
	if req.Path == planPath || req.Path == platform.RightsPath {
		return fail(fmt.Errorf("%s cannot be a plan step", req.Path))
	}
	if len(spec.FileSHA256) != len(req.Files) {
		return fail(fmt.Errorf("%d file_sha256 for %d uploads", len(spec.FileSHA256), len(req.Files)))
	}
	for _, h := range spec.FileSHA256 {
		if !sha256Re.MatchString(h) {
			return fail(errors.New("file_sha256 must be 64 lowercase hex characters"))
		}
	}
	if sentinelRe.MatchString(FormJSON(req.Form)) {
		return fail(errPlaceholderText)
	}
	st := store.PlanStep{Idx: idx, Description: spec.Description, Tool: spec.Tool, Method: req.Method}
	if spec.Tool != "" {
		if tool, ok := platform.Lookup(spec.Tool); ok {
			st.Tool = tool.Name
			if tool.HasSteps() {
				// A tool with steps builds its final call itself (it reads first and adds fields the agent never
				// wrote), so the step is matched on the tool and its arguments (issue #70).
				args, err := stepArgs(spec.Args)
				if err != nil {
					return fail(err)
				}
				st.Args = args
			}
		}
	}
	if st.Path, err = restorePath(req.Path); err != nil {
		return fail(err)
	}
	if len(req.Query) > 0 {
		q, err := restoreQuery(req.Query)
		if err != nil {
			return fail(err)
		}
		st.Query = json.RawMessage(QueryJSON(q))
	}
	if len(req.Body) > 0 {
		canon, err := Canonical(req.Body)
		if err != nil {
			return fail(err)
		}
		restored, err := restoreJSON(string(canon))
		if err != nil {
			return fail(err)
		}
		st.Body = json.RawMessage(restored)
		st.BodySHA256 = hashOrEmpty(restored)
	}
	if len(req.Form) > 0 {
		f := FormJSON(req.Form)
		st.Form = json.RawMessage(f)
		st.FormSHA256 = hashOrEmpty(f)
	}
	for i, f := range req.Files {
		if sentinelRe.MatchString(f.Path) {
			return fail(errPlaceholderText)
		}
		st.Files = append(st.Files, store.PlanFile{Field: f.Field, Path: f.Path, SHA256: spec.FileSHA256[i]})
	}
	return st, req, refs, nil
}

// Build turns a proposal into its steps. Reading calls, placeholders that point forward or at a
// step that does not create an object (delegation action create or run) are refused. Every step
// keeps its own approval rules; the plan only records them. A step that asks for every single
// approval (confirm each, G1.1) is never covered: the plan's approval does not replace it.
func Build(p Proposal, classify Classify) ([]store.PlanStep, error) {
	if len(p.Steps) == 0 || len(p.Steps) > MaxSteps {
		return nil, fmt.Errorf("a plan has 1 to %d steps", MaxSteps)
	}
	out := make([]store.PlanStep, 0, len(p.Steps))
	actions := make([]string, len(p.Steps)+1)
	for i, spec := range p.Steps {
		idx := i + 1
		st, sample, refs, err := BuildStep(spec, idx)
		if err != nil {
			return nil, err
		}
		effect, action, confirmEach := classify(sample)
		if effect == EffectRead {
			return nil, fmt.Errorf("step %d: reading calls do not belong in a plan", idx)
		}
		for _, n := range refs {
			switch {
			case n >= idx:
				return nil, fmt.Errorf("step %d: {{step%d.id}} refers to this or a later step", idx, n)
			case actions[n] != delegation.Create && actions[n] != delegation.Run:
				return nil, fmt.Errorf("step %d: {{step%d.id}} refers to step %d, which does not create an object", idx, n, n)
			}
		}
		actions[idx] = action
		st.Effect = effect
		st.Covered = effect != EffectIrreversible && !confirmEach
		out = append(out, st)
	}
	return out, nil
}

// FileRefs returns the uploads of every step (by step number, from 1) so that the socket can
// read them and pin their SHA-256.
func FileRefs(raw []byte) (map[int][]platform.File, error) {
	p, err := ParseProposal(raw)
	if err != nil {
		return nil, err
	}
	out := map[int][]platform.File{}
	for i, s := range p.Steps {
		req, _, err := StepRequest(s)
		if err != nil {
			return nil, fmt.Errorf("step %d: %w", i+1, err)
		}
		if len(req.Files) > 0 {
			out[i+1] = req.Files
		}
	}
	return out, nil
}

// SetFileSums writes the pinned SHA-256 of each step's uploads into the proposal and drops every
// hash the agent wrote itself.
func SetFileSums(raw []byte, sums map[int][]string) ([]byte, error) {
	p, err := ParseProposal(raw)
	if err != nil {
		return nil, err
	}
	for i := range p.Steps {
		p.Steps[i].FileSHA256 = sums[i+1]
	}
	return json.Marshal(p)
}

// Matches tells whether a live call is exactly the step: method, path, query, body (canonical
// form), form and uploads. why names the first difference.
func Matches(st store.PlanStep, r platform.Request, resolve Resolve) (bool, string) {
	if r.Method != st.Method {
		return false, "method"
	}
	p, ok := substPath(st.Path, resolve)
	if !ok || p != r.Path {
		return false, "path"
	}
	q, ok := substJSON(string(st.Query), resolve, false)
	if !ok || q != QueryJSON(r.Query) {
		return false, "query"
	}
	if len(st.Body) == 0 {
		if len(bytes.TrimSpace(r.Body)) > 0 {
			return false, "body"
		}
	} else {
		want, ok := substJSON(string(st.Body), resolve, true)
		wc, e1 := Canonical([]byte(want))
		gc, e2 := Canonical(r.Body)
		if !ok || e1 != nil || e2 != nil || !bytes.Equal(wc, gc) {
			return false, "body"
		}
	}
	if hashOrEmpty(FormJSON(r.Form)) != st.FormSHA256 {
		return false, "form"
	}
	if len(r.Files) != len(st.Files) || len(r.Uploads) != len(st.Files) {
		return false, "files"
	}
	for i, f := range st.Files {
		if r.Files[i].Field != f.Field || r.Files[i].Path != f.Path || r.Uploads[i].SHA256 != f.SHA256 {
			return false, "files"
		}
	}
	return true, ""
}

// Preview lists every step with its effect, the gateway's summary and the technical parameters, for the approval.
// The plan's title and the steps' notes are the agent's text and are marked as not verified (issue #72).
func Preview(title string, steps []store.PlanStep) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", ApprovalName(len(steps)))
	fmt.Fprintf(&b, "Title by the agent (not verified): %s\n", strconv.Quote(title))
	fmt.Fprintf(&b, "Approving runs the steps in this order with exactly these parameters, for %d minutes, in this chat and this agent session only. Steps marked IRREVERSIBLE still ask for their own approval.\n\n",
		int(ValidFor/time.Minute))
	for _, st := range steps {
		label := strings.ToUpper(st.Effect)
		if st.Effect == EffectIrreversible {
			label = "IRREVERSIBLE (own approval)"
		}
		head, details, _ := strings.Cut(st.Summary, "\n")
		if head == "" {
			head = st.Method + " " + st.Path + queryText(st.Query)
		}
		fmt.Fprintf(&b, "%d. %s · %s\n", st.Idx, label, head)
		if details != "" {
			for _, line := range strings.Split(details, "\n") {
				fmt.Fprintf(&b, "   %s\n", line)
			}
		}
		fmt.Fprintf(&b, "   Call: %s %s\n", st.Method, st.Path+queryText(st.Query))
		if IsToolStep(st) {
			var ind bytes.Buffer
			_ = json.Indent(&ind, st.Args, "", "  ")
			fmt.Fprintf(&b, "   tool %s with\n", st.Tool)
			for _, line := range strings.Split(ind.String(), "\n") {
				fmt.Fprintf(&b, "   %s\n", line)
			}
		} else if len(st.Body) > 0 {
			var ind bytes.Buffer
			_ = json.Indent(&ind, st.Body, "", "  ")
			for _, line := range strings.Split(ind.String(), "\n") {
				fmt.Fprintf(&b, "   %s\n", line)
			}
		}
		if len(st.Form) > 0 {
			fmt.Fprintf(&b, "   form: %s\n", st.Form)
		}
		for _, f := range st.Files {
			fmt.Fprintf(&b, "   file %s: %s  sha256 %s\n", f.Field, f.Path, f.SHA256)
		}
		if st.Description != "" {
			fmt.Fprintf(&b, "   Note by the agent (not verified): %s\n", strconv.Quote(st.Description))
		}
	}
	return b.String()
}

// ApprovalName is the gateway's name of a plan approval; the agent's title is not part of it (issue #72).
func ApprovalName(steps int) string {
	if steps == 1 {
		return "Plan with 1 step"
	}
	return fmt.Sprintf("Plan with %d steps", steps)
}

// ApprovalSummary is the summary of a plan approval: its name, then the first line of every step's summary.
func ApprovalSummary(steps []store.PlanStep) string {
	lines := []string{ApprovalName(len(steps))}
	for _, st := range steps {
		head := platform.SummaryHead(st.Summary)
		if head == "" {
			head = st.Method + " " + st.Path
		}
		lines = append(lines, fmt.Sprintf("%d. %s", st.Idx, head))
	}
	return strings.Join(lines, "\n")
}

// SummaryCall is the call a stored step stands for, as the summary describes it: tool, method, path, query, body,
// form and files exactly as stored (placeholders included).
func SummaryCall(st store.PlanStep) platform.SummaryCall {
	c := platform.SummaryCall{Tool: st.Tool, Method: st.Method, Path: st.Path, Body: st.Body}
	if len(st.Query) > 0 {
		_ = json.Unmarshal(st.Query, &c.Query)
	}
	if len(st.Form) > 0 {
		_ = json.Unmarshal(st.Form, &c.Form)
	}
	for _, f := range st.Files {
		c.Files = append(c.Files, platform.SummaryFile{Field: f.Field, Name: f.Path, Size: -1, SHA256: f.SHA256})
	}
	return c
}

// queryText renders a stored query as ?k=v&… for the preview.
func queryText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var q map[string]string
	if err := json.Unmarshal(raw, &q); err != nil || len(q) == 0 {
		return ""
	}
	v := url.Values{}
	for k, x := range q {
		v.Set(k, x)
	}
	return "?" + v.Encode()
}

type approvedStep struct {
	Step    int    `json:"step"`
	Call    string `json:"call"`
	Effect  string `json:"effect"`
	Covered bool   `json:"covered"`
}

// ApprovedText is the answer to the agent after an approval.
func ApprovedText(p store.Plan) string {
	steps := make([]approvedStep, 0, len(p.Steps))
	for _, st := range p.Steps {
		steps = append(steps, approvedStep{Step: st.Idx, Call: st.Method + " " + st.Path, Effect: st.Effect, Covered: st.Covered})
	}
	out := struct {
		PlanID    string         `json:"plan_id"`
		State     string         `json:"state"`
		ExpiresAt *time.Time     `json:"expires_at,omitempty"`
		Steps     []approvedStep `json:"steps"`
		Note      string         `json:"note"`
	}{
		PlanID:    p.ID,
		State:     StateApproved,
		ExpiresAt: p.ExpiresAt,
		Steps:     steps,
		Note:      "Run the steps in this order with exactly these parameters; each covered step then runs without a new approval until expires_at. Steps with covered=false still ask the user. Use the ids the platform returns for {{stepN.id}}. Any other call needs its own approval.",
	}
	b, _ := json.Marshal(out)
	return string(b)
}

// stepArgs gives the stored form of the arguments of a tool step: scalars as text, keys sorted, placeholders
// restored. Absent arguments are an empty object.
func stepArgs(raw json.RawMessage) (json.RawMessage, error) {
	sent, _, err := sentinelJSON(raw)
	if err != nil {
		return nil, err
	}
	norm, err := NormalizeArgs(sent)
	if err != nil {
		return nil, err
	}
	restored, err := restoreJSON(string(norm))
	if err != nil {
		return nil, err
	}
	return json.RawMessage(restored), nil
}

// NormalizeArgs gives the canonical form of tool arguments: every scalar (number, boolean) as its text,
// object keys sorted. The CLI passes numbers as text and the plan may write them as numbers; both must
// compare equal. Duplicate keys are an error (Canonical). Empty input is the empty object.
func NormalizeArgs(raw []byte) ([]byte, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return []byte("{}"), nil
	}
	if _, err := Canonical(raw); err != nil {
		return nil, err
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return nil, err
	}
	out, err := json.Marshal(textScalars(v))
	if err != nil {
		return nil, err
	}
	return Canonical(out)
}

func textScalars(v any) any {
	switch t := v.(type) {
	case json.Number:
		return t.String()
	case bool:
		if t {
			return "true"
		}
		return "false"
	case map[string]any:
		for k, x := range t {
			t[k] = textScalars(x)
		}
		return t
	case []any:
		for i, x := range t {
			t[i] = textScalars(x)
		}
		return t
	}
	return v
}

// IsToolStep tells whether a step is matched on a tool and its arguments.
func IsToolStep(st store.PlanStep) bool { return st.Tool != "" && len(st.Args) > 0 }

// MatchesTool tells whether the call r, made by the platform tool tool with the arguments args, is the
// tool step st: tool name and every argument (after placeholders are resolved), plus method and path of
// the call as a sanity check. Query, body, form and uploads are not compared: the tool builds them. A
// raw request has no invocation and never matches a tool step.
func MatchesTool(st store.PlanStep, tool string, args json.RawMessage, r platform.Request, resolve Resolve) (bool, string) {
	if !IsToolStep(st) || tool != st.Tool {
		return false, "tool"
	}
	live, err := NormalizeArgs(args)
	if err != nil {
		return false, "args"
	}
	want, ok := substJSON(string(st.Args), resolve, false)
	if !ok || want != string(live) {
		return false, "args"
	}
	if r.Method != st.Method {
		return false, "method"
	}
	p, ok := substPath(st.Path, resolve)
	if !ok || p != r.Path {
		return false, "path"
	}
	return true, ""
}

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
	StatePaused     = "paused"
	StateRejected   = "rejected"
	StateExpired    = "expired"
	StateCompleted  = "completed"
	StateFailed     = "failed"
	StateCancelled  = "cancelled"
	StateSuperseded = "superseded"

	StepOpen    = "open"
	StepRunning = "running"
	// StepWaiting: the call went out and started a platform job the gateway observes (A-E2, D2.2); the
	// step is done once that job ends.
	StepWaiting = "waiting"
	StepDone    = "done"
	StepFailed  = "failed"
	StepSkipped = "skipped"
)

// Modes of a plan (A-E1). An interactive plan covers only its exact steps; an autonomous plan carries a
// budget and a grant derived from its steps that bounds every call of the chat while it runs.
const (
	ModeInteractive = "interactive"
	ModeAutonomous  = "autonomous"
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

// Defaults and hard bounds of an autonomous plan's budget (A-E1, D1.6). MaxAutoTurnsCap is a constant
// because no environment variable raises it; the duration and the cost come from Limits.
const (
	DefaultPlanDurationMinutes = 30
	DefaultPlanLLMCostUSD      = 1.00
	DefaultPlanAutoTurns       = 20
	MaxAutoTurnsCap            = 50
	// MinPlanAttempts/MaxPlanAttempts bound "max_attempts" of a step (D1.6).
	MinPlanAttempts = 1
	MaxPlanAttempts = 3
)

// Limits bounds an autonomous plan's budget from the environment (A-E1, D1.6): the agent proposes, the
// gateway refuses anything above these. MaxDuration never exceeds oidc.MaxSession − 1 h (set in config).
type Limits struct {
	MaxDuration time.Duration
	MaxLLMCost  float64
}

var sha256Re = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Proposal is the body of propose_plan: {"title": …, "steps": [...]}.
type Proposal struct {
	Title string     `json:"title"`
	Steps []StepSpec `json:"steps"`
	// Mode: "" or "interactive" (default) or "autonomous" (A-E1).
	Mode string `json:"mode,omitempty"`
	// Budget is only read for an autonomous plan; missing fields take their defaults.
	Budget *Budget `json:"budget,omitempty"`
	// Replaces names a running plan of this chat that this one replaces (D1.12).
	Replaces string `json:"replaces,omitempty"`
}

// Budget is the budget the agent proposes for an autonomous plan (A-E1).
type Budget struct {
	MaxDurationMinutes int     `json:"max_duration_minutes"`
	MaxLLMCostUSD      float64 `json:"max_llm_cost_usd"`
	MaxAutoTurns       int     `json:"max_auto_turns"`
}

// StepSpec is one step as the agent writes it: either a platform tool with its arguments, or a
// raw call (method, path, query, body). FileSHA256 and FileBytes are set by the socket, not by the agent.
type StepSpec struct {
	Tool        string            `json:"tool,omitempty"`
	Args        json.RawMessage   `json:"args,omitempty"`
	Method      string            `json:"method,omitempty"`
	Path        string            `json:"path,omitempty"`
	Query       map[string]string `json:"query,omitempty"`
	Body        json.RawMessage   `json:"body,omitempty"`
	Description string            `json:"description,omitempty"`
	FileSHA256  []string          `json:"file_sha256,omitempty"`
	FileBytes   []int64           `json:"file_bytes,omitempty"`
	// MaxAttempts: how often an autonomous plan may repeat this step after a failure (1 to 3, A-E1).
	MaxAttempts int `json:"max_attempts,omitempty"`
}

// Classify gives the effect and the delegation action of a call and whether the call asks for every
// single approval (G1.1); see FallbackEffect.
type Classify func(r platform.Request) (effect, action string, confirmEach bool)

// Resolve gives the id that placeholder {{stepN.id}} stands for at matching time.
type Resolve func(n int) (string, bool)

// ParseProposal checks the body of propose_plan against the budget limits (A-E1). Duplicate keys are
// refused before the fields are read; unknown fields are an error.
func ParseProposal(raw []byte, lim Limits) (Proposal, error) {
	p, err := parseProposal(raw)
	if err != nil {
		return Proposal{}, err
	}
	if err := p.checkBudget(lim); err != nil {
		return Proposal{}, err
	}
	return p, nil
}

// parseProposal reads and checks the shape of a proposal, without the budget (used where the limits do
// not matter, e.g. reading the file references of a proposal before it is decided).
func parseProposal(raw []byte) (Proposal, error) {
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

// Autonomous reports whether the proposal asks for an autonomous plan.
func (p Proposal) Autonomous() bool { return p.Mode == ModeAutonomous }

// checkBudget fills in the defaults of an autonomous plan's budget and refuses values above the limits;
// values are never silently shortened (D1.6). A budget or max_attempts without mode autonomous is refused.
func (p *Proposal) checkBudget(lim Limits) error {
	switch p.Mode {
	case "", ModeInteractive:
		if p.Budget != nil {
			return errors.New("budget and max_attempts need \"mode\": \"autonomous\"")
		}
		for i, s := range p.Steps {
			if s.MaxAttempts > 0 {
				return fmt.Errorf("step %d: max_attempts needs \"mode\": \"autonomous\"", i+1)
			}
		}
		return nil
	case ModeAutonomous:
	default:
		return fmt.Errorf("mode must be %q or %q, not %q", ModeInteractive, ModeAutonomous, p.Mode)
	}
	if lim.MaxDuration <= 0 || lim.MaxLLMCost <= 0 {
		return errors.New("autonomous plans are not configured on this instance")
	}
	if p.Budget == nil {
		p.Budget = &Budget{}
	}
	b := p.Budget
	maxMinutes := int(lim.MaxDuration / time.Minute)
	switch {
	case b.MaxDurationMinutes == 0:
		b.MaxDurationMinutes = DefaultPlanDurationMinutes
	case b.MaxDurationMinutes < 0:
		return errors.New("budget: max_duration_minutes must not be negative")
	case b.MaxDurationMinutes > maxMinutes:
		return fmt.Errorf("budget: max_duration_minutes is %d, at most %d", b.MaxDurationMinutes, maxMinutes)
	}
	switch {
	case b.MaxLLMCostUSD == 0:
		b.MaxLLMCostUSD = DefaultPlanLLMCostUSD
	case b.MaxLLMCostUSD < 0:
		return errors.New("budget: max_llm_cost_usd must not be negative")
	case b.MaxLLMCostUSD > lim.MaxLLMCost:
		return fmt.Errorf("budget: max_llm_cost_usd is %.2f, at most %.2f", b.MaxLLMCostUSD, lim.MaxLLMCost)
	}
	switch {
	case b.MaxAutoTurns == 0:
		b.MaxAutoTurns = DefaultPlanAutoTurns
	case b.MaxAutoTurns < 0:
		return errors.New("budget: max_auto_turns must not be negative")
	case b.MaxAutoTurns > MaxAutoTurnsCap:
		return fmt.Errorf("budget: max_auto_turns is %d, at most %d", b.MaxAutoTurns, MaxAutoTurnsCap)
	}
	for i, s := range p.Steps {
		switch {
		case s.MaxAttempts == 0:
			p.Steps[i].MaxAttempts = MinPlanAttempts
		case s.MaxAttempts < MinPlanAttempts:
			return fmt.Errorf("step %d: max_attempts must be %d to %d", i+1, MinPlanAttempts, MaxPlanAttempts)
		case s.MaxAttempts > MaxPlanAttempts:
			return fmt.Errorf("step %d: max_attempts is %d, at most %d", i+1, s.MaxAttempts, MaxPlanAttempts)
		}
	}
	return nil
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
		case "propose_plan", "rights", "api_paths", "capabilities":
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
	if spec.FileBytes != nil {
		if len(spec.FileBytes) != len(req.Files) {
			return fail(fmt.Errorf("%d file_bytes for %d uploads", len(spec.FileBytes), len(req.Files)))
		}
		for _, n := range spec.FileBytes {
			if n < 0 {
				return fail(errors.New("file_bytes must not be negative"))
			}
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
		pf := store.PlanFile{Field: f.Field, Path: f.Path, SHA256: spec.FileSHA256[i]}
		if spec.FileBytes != nil {
			n := spec.FileBytes[i]
			pf.Bytes = &n
		}
		st.Files = append(st.Files, pf)
	}
	return st, req, refs, nil
}

// Build turns a proposal into its steps. Reading calls, placeholders that point forward or at a
// step that does not create an object (delegation action create; a run only starts a job and creates
// no object) are refused. Every step
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
			case actions[n] == delegation.Run:
				return nil, fmt.Errorf("step %d: {{step%d.id}} refers to step %d, which starts a job and creates no object; use the placeholder of the step that created the object", idx, n, n)
			case actions[n] != delegation.Create:
				return nil, fmt.Errorf("step %d: {{step%d.id}} refers to step %d, which does not create an object", idx, n, n)
			}
		}
		actions[idx] = action
		st.Effect = effect
		// The confirm-each trait is stored with the step (A-E2): a step that is loaded again has to be
		// classified the same way, or a reloaded plan would cover a call that asks for every single call.
		st.ConfirmEach = confirmEach
		st.Covered = effect != EffectIrreversible && !confirmEach
		st.MaxAttempts = spec.MaxAttempts
		if st.MaxAttempts < MinPlanAttempts {
			st.MaxAttempts = MinPlanAttempts
		}
		out = append(out, st)
	}
	return out, nil
}

// Referenced returns the first step after step n whose path, query, body or arguments contain the
// placeholder {{stepN.id}}; 0 when no later step uses it.
func Referenced(steps []store.PlanStep, n int) int {
	for _, st := range steps {
		if st.Idx <= n {
			continue
		}
		for _, text := range []string{st.Path, string(st.Query), string(st.Body), string(st.Args)} {
			for _, m := range anyRefRe.FindAllStringSubmatch(text, -1) {
				if k, err := strconv.Atoi(m[1]); err == nil && k == n {
					return st.Idx
				}
			}
		}
	}
	return 0
}

// SameShape tells whether a call looks like the step: the same tool for a tool step, otherwise the same
// method and path segments, where a placeholder segment fits any non-empty segment. It is no match (the
// parameters are not compared) but is used to recognise a call made too early.
func SameShape(st store.PlanStep, tool string, r platform.Request) bool {
	if st.Tool != "" {
		return tool == st.Tool
	}
	if r.Method != st.Method {
		return false
	}
	want := strings.Split(strings.Trim(st.Path, "/"), "/")
	got := strings.Split(strings.Trim(r.Path, "/"), "/")
	if len(want) != len(got) {
		return false
	}
	for i := range want {
		if _, ok := wholePlaceholder(want[i]); ok {
			if got[i] == "" {
				return false
			}
			continue
		}
		if want[i] != got[i] {
			return false
		}
	}
	return true
}

// FileRefs returns the uploads of every step (by step number, from 1) so that the socket can
// read them and pin their SHA-256.
func FileRefs(raw []byte) (map[int][]platform.File, error) {
	p, err := parseProposal(raw)
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

// FilePin is what the socket read of one upload when the plan was proposed.
type FilePin struct {
	SHA256 string
	Bytes  int64
}

// SetFilePins writes the pinned SHA-256 and size of each step's uploads into the proposal and drops every
// value the agent wrote itself.
func SetFilePins(raw []byte, pins map[int][]FilePin) ([]byte, error) {
	p, err := parseProposal(raw)
	if err != nil {
		return nil, err
	}
	for i := range p.Steps {
		p.Steps[i].FileSHA256, p.Steps[i].FileBytes = nil, nil
		for _, pin := range pins[i+1] {
			p.Steps[i].FileSHA256 = append(p.Steps[i].FileSHA256, pin.SHA256)
			p.Steps[i].FileBytes = append(p.Steps[i].FileBytes, pin.Bytes)
		}
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
		if f.Bytes != nil && int64(len(r.Uploads[i].Data)) != *f.Bytes {
			return false, "files"
		}
	}
	return true, ""
}

// Autonomy carries what an autonomous plan adds to the preview (A-E1): its budget, the rights derived
// from its steps and the compute calls the chat has left after this plan. nil for an interactive plan.
type Autonomy struct {
	Budget Budget
	// Rights is the one-line summary of the grant derived from the steps (delegation.SummaryLine).
	Rights string
	// ComputeNeeded is what the plan may spend (Σ compute steps × max_attempts, D1.7).
	ComputeNeeded int
	// ComputeLeft is what the chat has left after this plan.
	ComputeLeft int
	// Replaces names the plan this one replaces; ReplacedCostUSD is what the model calls of that plan
	// cost so far (D1.12).
	Replaces        string
	ReplacedCostUSD float64
	// EndsAt is the end the user's login session sets, when that end comes before the plan's own duration
	// (A-E3, D3.3): a plan must not run into the end of the session that carries its calls. Nil when no
	// session bounds it (the token mode, or a session that outlives the plan).
	EndsAt *time.Time
}

// Preview lists every step with its effect, the gateway's summary and the technical parameters, for the approval.
// The plan's title and the steps' notes are the agent's text and are marked as not verified (issue #72). An
// autonomous plan (auto != nil) gets a block with its budget and its rights after the header (A-E1).
func Preview(title string, steps []store.PlanStep, auto *Autonomy) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", ApprovalName(len(steps)))
	fmt.Fprintf(&b, "Title by the agent (not verified): %s\n", strconv.Quote(title))
	if auto == nil {
		fmt.Fprintf(&b, "Approving runs the steps in this order with exactly these parameters, for %d minutes (counted again from the end of each step that waits for a platform job), in this chat and this agent session only. Steps marked IRREVERSIBLE still ask for their own approval.\n\n",
			int(ValidFor/time.Minute))
	} else {
		fmt.Fprintf(&b, "Autonomous: runs without asking you for up to %s, or until you pause or cancel it.\n", DurationText(time.Duration(auto.Budget.MaxDurationMinutes)*time.Minute))
		if auto.EndsAt != nil {
			// The zone is part of the text: the gateway's clock is not necessarily the user's.
			fmt.Fprintf(&b, "It ends at the latest at %s, one hour before your login session ends.\n", auto.EndsAt.Format("2006-01-02 15:04 MST"))
		}
		if auto.ComputeLeft >= 0 {
			fmt.Fprintf(&b, "Budget: at most %d compute calls (%d left after this plan), model calls up to %.2f USD, up to %d turns without you.\n",
				auto.ComputeNeeded, auto.ComputeLeft, auto.Budget.MaxLLMCostUSD, auto.Budget.MaxAutoTurns)
		} else {
			fmt.Fprintf(&b, "Budget: at most %d compute calls, model calls up to %.2f USD, up to %d turns without you.\n",
				auto.ComputeNeeded, auto.Budget.MaxLLMCostUSD, auto.Budget.MaxAutoTurns)
		}
		fmt.Fprintf(&b, "Rights while it runs: %s; nothing else is allowed and is refused without asking.\n", auto.Rights)
		if auto.Replaces != "" {
			fmt.Fprintf(&b, "Replaces the running plan %s (its model calls cost %.2f USD so far).\n", auto.Replaces, auto.ReplacedCostUSD)
		}
		fmt.Fprintf(&b, "Approving runs the steps in this order with exactly these parameters, from any session of this chat. "+
			"Steps marked IRREVERSIBLE and steps that run code still ask for their own approval.\n\n")
	}
	for _, st := range steps {
		label := strings.ToUpper(st.Effect)
		if st.Effect == EffectIrreversible {
			label = "IRREVERSIBLE (own approval)"
		}
		attempts := ""
		if st.MaxAttempts > 1 {
			attempts = fmt.Sprintf(" · up to %d attempts", st.MaxAttempts)
		}
		head, details, _ := strings.Cut(st.Summary, "\n")
		if head == "" {
			head = st.Method + " " + st.Path + queryText(st.Query)
		}
		fmt.Fprintf(&b, "%d. %s · %s%s\n", st.Idx, label, head, attempts)
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
		if man, ok := StepManifest(st); ok && len(st.Files) > platform.ListedFiles {
			// Many files: the summary and the manifest hash instead of a line per file (issue #172).
			for _, line := range man.PreviewLines() {
				fmt.Fprintf(&b, "   %s\n", line)
			}
		} else {
			for _, f := range st.Files {
				fmt.Fprintf(&b, "   file %s: %s  sha256 %s\n", f.Field, f.Path, f.SHA256)
			}
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
		size := int64(-1)
		if f.Bytes != nil {
			size = *f.Bytes
		}
		c.Files = append(c.Files, platform.SummaryFile{Field: f.Field, Name: f.Path, Size: size, SHA256: f.SHA256})
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

// approvedStep is one step of the answer after an approval; Attempts is set for an autonomous plan (A-E1).
type approvedStep struct {
	Step     int    `json:"step"`
	Call     string `json:"call"`
	Effect   string `json:"effect"`
	Covered  bool   `json:"covered"`
	Attempts int    `json:"max_attempts,omitempty"`
}

// ApprovedText is the answer to the agent after an approval. An autonomous plan (p.Mode) additionally
// carries its mode, budget and grant, and its note points at the refusal outside the grant and at
// "replaces" (A-E1).
func ApprovedText(p store.Plan) string {
	steps := make([]approvedStep, 0, len(p.Steps))
	for _, st := range p.Steps {
		a := approvedStep{Step: st.Idx, Call: st.Method + " " + st.Path, Effect: st.Effect, Covered: st.Covered}
		if p.Mode == ModeAutonomous && st.MaxAttempts > 1 {
			a.Attempts = st.MaxAttempts
		}
		steps = append(steps, a)
	}
	auto := p.Mode == ModeAutonomous
	mode := ""
	note := "Run the steps in this order with exactly these parameters; each covered step then runs without a new approval until expires_at. Steps with covered=false still ask the user. {{stepN.id}} stands for the object step N created. For a step that starts a platform job (create_training, build_inference_image) it is known only when the job ends; the gateway's note names it. Never use a task id for it. Any other call needs its own approval."
	grant := ""
	var budget *Budget
	if auto {
		mode = p.Mode
		budget = &Budget{MaxDurationMinutes: int(time.Duration(p.Budget.MaxDurationS) * time.Second / time.Minute),
			MaxLLMCostUSD: p.Budget.MaxLLMCost, MaxAutoTurns: p.Budget.MaxAutoTurns}
		if g, err := delegation.Parse(p.Grant); err == nil {
			grant = g.Summary()
		}
		note = "Autonomous plan: run the steps in this order. Calls outside the rights listed in grant are refused without asking the user. If you need anything else, propose a changed plan with \"replaces\": \"" + p.ID + "\". A long step reports its end to you; end your turn while it runs."
	}
	out := struct {
		PlanID    string         `json:"plan_id"`
		State     string         `json:"state"`
		Mode      string         `json:"mode,omitempty"`
		Budget    *Budget        `json:"budget,omitempty"`
		Grant     string         `json:"grant,omitempty"`
		ExpiresAt *time.Time     `json:"expires_at,omitempty"`
		Steps     []approvedStep `json:"steps"`
		Note      string         `json:"note"`
	}{
		PlanID:    p.ID,
		State:     StateApproved,
		Mode:      mode,
		Budget:    budget,
		Grant:     grant,
		ExpiresAt: p.ExpiresAt,
		Steps:     steps,
		Note:      note,
	}
	b, _ := json.Marshal(out)
	return string(b)
}

// DurationText renders a plan budget's duration as "2h 0m" or "45m" for the preview (A-E1).
func DurationText(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	h := int(d / time.Hour)
	m := int((d % time.Hour) / time.Minute)
	if h > 0 {
		return fmt.Sprintf("%dh %dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
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

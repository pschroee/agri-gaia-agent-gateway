// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

//go:embed plan_schema.sql
var planSchemaSQL string

// Plan is a list of platform calls approved with one decision (issue #70).
type Plan struct {
	ID         string `json:"id"`
	ChatID     string `json:"chat_id"`
	ApprovalID string `json:"approval_id,omitempty"`
	// Title is the agent's text, not checked by the gateway (issue #72): the API names it agent_title.
	Title      string `json:"agent_title"`
	Session    string `json:"session,omitempty"`
	ToolCallID string `json:"tool_call_id,omitempty"`
	Via        string `json:"via"`
	State      string `json:"state"`
	// Mode is "interactive" (the plan covers only its steps) or "autonomous" (A-E1): an autonomous plan
	// carries a budget and a grant derived from its steps that bounds every call of the chat.
	Mode        string          `json:"mode,omitempty"`
	Budget      PlanBudget      `json:"budget"`
	AutoTurns   int             `json:"auto_turns,omitempty"`
	Grant       json.RawMessage `json:"grant,omitempty"`
	PauseReason string          `json:"pause_reason,omitempty"`
	// Replaces names the plan this one superseded (D1.12).
	Replaces  string     `json:"replaces,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	DecidedAt *time.Time `json:"decided_at,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	// EndedAt marks the end of an autonomous plan (completed, failed, cancelled).
	EndedAt *time.Time `json:"ended_at,omitempty"`
	// Usage: what the plan has used of its budget (A-E4, D4.5). Not stored; filled when a plan is read for
	// the API. Nil for an interactive plan and while the usage could not be read.
	Usage *PlanUsage `json:"usage,omitempty"`
	Steps []PlanStep `json:"steps"`
}

// PlanUsage is what an autonomous plan has used of its budget (A-E4, D4.5). It is not stored: it is read
// along with the plan, from llm_calls (the model cost since the plan was approved), plans.auto_turns and
// the chat's compute calls. ComputeMax is the gateway's limit for the chat, not a value of the plan.
type PlanUsage struct {
	// LLMCost: model cost of the chat since the plan was approved, in US dollars.
	LLMCost float64 `json:"llm_cost"`
	// AutoTurns: turns the plan took without the user.
	AutoTurns int `json:"auto_turns"`
	// ComputeUsed: compute calls the chat has used so far.
	ComputeUsed int `json:"compute_used"`
	// ComputeMax: how many compute calls the gateway allows the chat (AGW_COMPUTE_MAX_PER_CHAT).
	ComputeMax int `json:"compute_max"`
}

// PlanBudget is the budget of an autonomous plan (A-E1): wall-clock seconds it may run, the model cost
// it may spend and the turns without the user it may take. Zero on an interactive plan.
type PlanBudget struct {
	MaxDurationS int     `json:"max_duration_s"`
	MaxLLMCost   float64 `json:"max_llm_cost"`
	MaxAutoTurns int     `json:"max_auto_turns"`
}

// PlanStep is one call of a plan. Query, Body and Form hold the canonical JSON as stored.
type PlanStep struct {
	Idx int `json:"idx"`
	// Summary is what the gateway says the step does (issue #72): written from the stored call and platform state
	// read at proposal time. Empty for steps from before #72.
	Summary string `json:"summary,omitempty"`
	// SummaryProposed (issue L2/#119): the summary as it was proposed, kept when a re-read at the moment of the
	// approval found other values. Empty while nothing changed.
	SummaryProposed string `json:"summary_proposed,omitempty"`
	// Description is the agent's note on the step, not checked by the gateway: the API names it agent_note.
	Description string `json:"agent_note,omitempty"`
	Tool        string `json:"tool,omitempty"`
	// Args: canonical arguments of a tool step with steps, scalars as text, placeholders as {{stepN.id}}.
	Args       json.RawMessage `json:"args,omitempty"`
	Method     string          `json:"method"`
	Path       string          `json:"path"`
	Query      json.RawMessage `json:"query,omitempty"`
	Body       json.RawMessage `json:"body,omitempty"`
	BodySHA256 string          `json:"body_sha256,omitempty"`
	Form       json.RawMessage `json:"form,omitempty"`
	FormSHA256 string          `json:"form_sha256,omitempty"`
	Files      []PlanFile      `json:"files,omitempty"`
	Effect     string          `json:"effect"`
	// ConfirmEach: the route asks for every single call (G1.1); such a step is never covered by a plan,
	// also not after the plan was loaded again (A-E2).
	ConfirmEach     bool       `json:"confirm_each,omitempty"`
	Covered         bool       `json:"covered"`
	State           string     `json:"state"`
	Result          string     `json:"result,omitempty"`
	CreatedResource string     `json:"created_resource,omitempty"`
	CreatedID       string     `json:"created_id,omitempty"`
	DoneAt          *time.Time `json:"done_at,omitempty"`
	// MaxAttempts: how often an autonomous plan may repeat this step after a failure (1 to 3, A-E1).
	MaxAttempts int `json:"max_attempts,omitempty"`
	// Attempts counts the claims of this step so far (incremented by ClaimPlanStep).
	Attempts int `json:"attempts,omitempty"`
	// WatchJob names the observed platform job the step waits for (A-E2); empty for every other step.
	WatchJob string `json:"watch_job,omitempty"`
	// ParkedApproval names the approval a non-covered step of an autonomous plan waits for (D2.8).
	ParkedApproval string `json:"parked_approval,omitempty"`
	// The progress of the step's job, read with the step out of watch_jobs (A-E2): the percent, one line
	// the platform reported, the estimated end, the last lines of its log and the objects it produced.
	Progress    *float64      `json:"progress,omitempty"`
	ProgressMsg string        `json:"progress_msg,omitempty"`
	ETA         *time.Time    `json:"eta,omitempty"`
	LogTail     string        `json:"log_tail,omitempty"`
	Outputs     []WatchOutput `json:"outputs,omitempty"`
	JobStarted  *time.Time    `json:"started_at,omitempty"`
	// PlanID: the plan the step belongs to (not part of the JSON; the plan carries it).
	PlanID string `json:"-"`
}

// PlanFile is an upload of a step, pinned by its SHA-256.
type PlanFile struct {
	Field  string `json:"field"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

const planCols = `id::text, chat_id::text, approval_id, title, session, tool_call_id, via, state, mode, max_duration_s, max_llm_cost, max_auto_turns, auto_turns, grant_json, pause_reason, replaces, created_at, decided_at, expires_at, ended_at`

// planStepCols reads a step together with the progress of its observed job (A-E2): the step's own columns
// first, then the fields of watch_jobs, so that the plan API carries them for every step at once.
// The columns of the job are COALESCEd: a step without an observed job joins onto no row at all, and NULL
// would be readable only into pointers.
const planStepCols = `s.idx, s.summary, COALESCE(s.summary_proposed,''), s.description, s.tool, s.args, s.method, s.path, s.query, s.body, s.body_sha256, s.form, s.form_sha256, s.files, s.effect, s.confirm_each, s.state, s.result, s.created_resource, s.created_id, s.done_at, s.max_attempts, s.attempts, s.watch_job, s.parked_approval, w.progress, COALESCE(w.message,''), w.eta, COALESCE(w.log_tail,''), COALESCE(w.outputs,'[]'), w.started_at`

func scanPlan(row interface{ Scan(...any) error }) (Plan, error) {
	var p Plan
	var grant string
	err := row.Scan(&p.ID, &p.ChatID, &p.ApprovalID, &p.Title, &p.Session, &p.ToolCallID, &p.Via, &p.State,
		&p.Mode, &p.Budget.MaxDurationS, &p.Budget.MaxLLMCost, &p.Budget.MaxAutoTurns, &p.AutoTurns, &grant,
		&p.PauseReason, &p.Replaces, &p.CreatedAt, &p.DecidedAt, &p.ExpiresAt, &p.EndedAt)
	p.Grant = rawOrNil(grant)
	return p, err
}

// rawOrNil turns stored text into JSON; "" stays nil.
func rawOrNil(s string) json.RawMessage {
	if s == "" {
		return nil
	}
	return json.RawMessage(s)
}

func scanPlanStep(row interface{ Scan(...any) error }) (PlanStep, error) {
	var st PlanStep
	var query, body, form, files, args, outputs string
	err := row.Scan(&st.Idx, &st.Summary, &st.SummaryProposed, &st.Description, &st.Tool, &args, &st.Method, &st.Path, &query, &body, &st.BodySHA256, &form, &st.FormSHA256, &files,
		&st.Effect, &st.ConfirmEach, &st.State, &st.Result, &st.CreatedResource, &st.CreatedID, &st.DoneAt, &st.MaxAttempts, &st.Attempts,
		&st.WatchJob, &st.ParkedApproval, &st.Progress, &st.ProgressMsg, &st.ETA, &st.LogTail, &outputs, &st.JobStarted)
	if err != nil {
		return st, err
	}
	st.Args = rawOrNil(args)
	st.Query = rawOrNil(query)
	st.Body = rawOrNil(body)
	st.Form = rawOrNil(form)
	if files != "" && files != "[]" {
		if err := json.Unmarshal([]byte(files), &st.Files); err != nil {
			return st, err
		}
	}
	if outputs != "" && outputs != "[]" {
		if err := json.Unmarshal([]byte(outputs), &st.Outputs); err != nil {
			return st, err
		}
	}
	st.Covered = st.Effect != "irreversible" && !st.ConfirmEach
	return st, nil
}

// planStepFrom joins the job of a step, so that the stored progress comes along (A-E2).
const planStepFrom = `plan_steps s LEFT JOIN watch_jobs w ON w.id = s.watch_job`

// CreatePlan stores a new plan with its steps in state "proposed"; the steps start "open".
func (s *Store) CreatePlan(ctx context.Context, p Plan) (Plan, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Plan{}, err
	}
	defer tx.Rollback(ctx)
	var id string
	err = tx.QueryRow(ctx, `INSERT INTO plans (chat_id, approval_id, title, session, tool_call_id, via, mode,
max_duration_s, max_llm_cost, max_auto_turns, grant_json, replaces)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) RETURNING id::text`,
		p.ChatID, p.ApprovalID, p.Title, p.Session, p.ToolCallID, p.Via, orInteractive(p.Mode),
		p.Budget.MaxDurationS, p.Budget.MaxLLMCost, p.Budget.MaxAutoTurns, string(p.Grant), p.Replaces).Scan(&id)
	if err != nil {
		return Plan{}, err
	}
	for _, st := range p.Steps {
		files := "[]"
		if len(st.Files) > 0 {
			b, err := json.Marshal(st.Files)
			if err != nil {
				return Plan{}, err
			}
			files = string(b)
		}
		attempts := st.MaxAttempts
		if attempts < 1 {
			attempts = 1
		}
		_, err := tx.Exec(ctx, `INSERT INTO plan_steps (plan_id, idx, description, tool, args, method, path, query, body, body_sha256,
form, form_sha256, files, effect, confirm_each, summary, max_attempts) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)`,
			id, st.Idx, st.Description, st.Tool, string(st.Args), st.Method, st.Path, string(st.Query), string(st.Body), st.BodySHA256,
			string(st.Form), st.FormSHA256, files, st.Effect, st.ConfirmEach, st.Summary, attempts)
		if err != nil {
			return Plan{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Plan{}, err
	}
	return s.GetPlan(ctx, id)
}

func (s *Store) planSteps(ctx context.Context, planID string) ([]PlanStep, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+planStepCols+` FROM `+planStepFrom+` WHERE s.plan_id=$1 ORDER BY s.idx`, planID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PlanStep{}
	for rows.Next() {
		st, err := scanPlanStep(rows)
		if err != nil {
			return nil, err
		}
		st.PlanID = planID
		out = append(out, st)
	}
	return out, rows.Err()
}

// GetPlan returns a plan with its steps; ErrNotFound for an unknown or malformed id.
func (s *Store) GetPlan(ctx context.Context, id string) (Plan, error) {
	if !isUUID(id) {
		return Plan{}, ErrNotFound
	}
	p, err := scanPlan(s.pool.QueryRow(ctx, `SELECT `+planCols+` FROM plans WHERE id=$1`, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Plan{}, ErrNotFound
		}
		return Plan{}, err
	}
	p.Steps, err = s.planSteps(ctx, id)
	return p, err
}

// ListPlans returns the plans of a chat, oldest first, each with its steps.
func (s *Store) ListPlans(ctx context.Context, chatID string) ([]Plan, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+planCols+` FROM plans WHERE chat_id=$1 ORDER BY created_at, id`, chatID)
	if err != nil {
		return nil, err
	}
	out := []Plan{}
	for rows.Next() {
		p, err := scanPlan(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].Steps, err = s.planSteps(ctx, out[i].ID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// SetPlanApproval links the plan to its approval request.
func (s *Store) SetPlanApproval(ctx context.Context, planID, approvalID string) error {
	_, err := s.pool.Exec(ctx, `UPDATE plans SET approval_id=$2 WHERE id=$1`, planID, approvalID)
	return err
}

// PlanByApproval returns the plan behind an approval request; ErrNotFound if the ID belongs to no plan.
func (s *Store) PlanByApproval(ctx context.Context, approvalID string) (Plan, error) {
	var id string
	err := s.pool.QueryRow(ctx, `SELECT id::text FROM plans WHERE approval_id=$1 ORDER BY created_at DESC, id LIMIT 1`, approvalID).Scan(&id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Plan{}, ErrNotFound
		}
		return Plan{}, err
	}
	return s.GetPlan(ctx, id)
}

// ReplacePlanStepSummaries overwrites the gateway's summaries of a plan's steps with what a re-read at the moment of
// the approval found (issue L2/#119). The first proposal of every step stays in summary_proposed (COALESCE). One
// transaction: either every step is updated or none. Steps the plan does not have are ignored.
func (s *Store) ReplacePlanStepSummaries(ctx context.Context, planID string, summaries []string) error {
	if !isUUID(planID) {
		return ErrNotFound
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	for i, sum := range summaries {
		if _, err := tx.Exec(ctx, `UPDATE plan_steps SET summary_proposed = COALESCE(summary_proposed, summary), summary=$3
WHERE plan_id=$1 AND idx=$2`, planID, i+1, sum); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// DecidePlan sets the decision of a plan that is still "proposed"; ok reports whether it took effect.
func (s *Store) DecidePlan(ctx context.Context, planID, state string, expiresAt *time.Time) (bool, error) {
	if !isUUID(planID) {
		return false, ErrNotFound
	}
	tag, err := s.pool.Exec(ctx, `UPDATE plans SET state=$2, decided_at=now(), expires_at=$3 WHERE id=$1 AND state='proposed'`,
		planID, state, expiresAt)
	return tag.RowsAffected() > 0, err
}

// SupersedePlans ends the approved plans of a session, except keepID.
func (s *Store) SupersedePlans(ctx context.Context, chatID, session, keepID string) error {
	_, err := s.pool.Exec(ctx, `UPDATE plans SET state='superseded' WHERE chat_id=$1 AND session=$2 AND state='approved' AND id::text<>$3`,
		chatID, session, keepID)
	return err
}

// ActivePlan returns the newest approved plan of a session; ErrNotFound if there is none.
func (s *Store) ActivePlan(ctx context.Context, chatID, session string) (Plan, error) {
	var id string
	err := s.pool.QueryRow(ctx, `SELECT id::text FROM plans WHERE chat_id=$1 AND session=$2 AND state='approved'
ORDER BY decided_at DESC NULLS LAST, created_at DESC LIMIT 1`, chatID, session).Scan(&id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Plan{}, ErrNotFound
		}
		return Plan{}, err
	}
	return s.GetPlan(ctx, id)
}

// SetPlanState moves a plan from one state to another; ok is false if it was not in state from.
func (s *Store) SetPlanState(ctx context.Context, planID, from, to string) (bool, error) {
	if !isUUID(planID) {
		return false, ErrNotFound
	}
	tag, err := s.pool.Exec(ctx, `UPDATE plans SET state=$3 WHERE id=$1 AND state=$2`, planID, from, to)
	return tag.RowsAffected() > 0, err
}

// ClaimPlanStep sets an open step to running and counts the claim (attempts); ok is false if it was not open.
func (s *Store) ClaimPlanStep(ctx context.Context, planID string, idx int) (bool, error) {
	if !isUUID(planID) {
		return false, ErrNotFound
	}
	tag, err := s.pool.Exec(ctx, `UPDATE plan_steps SET state='running', attempts=attempts+1 WHERE plan_id=$1 AND idx=$2 AND state='open'`, planID, idx)
	return tag.RowsAffected() > 0, err
}

// FinishPlanStep moves a step from one state to another and records the outcome.
func (s *Store) FinishPlanStep(ctx context.Context, planID string, idx int, from, to, result, resource, id string) (bool, error) {
	if !isUUID(planID) {
		return false, ErrNotFound
	}
	tag, err := s.pool.Exec(ctx, `UPDATE plan_steps SET state=$4, result=$5, created_resource=$6, created_id=$7, done_at=now()
WHERE plan_id=$1 AND idx=$2 AND state=$3`, planID, idx, from, to, result, resource, id)
	return tag.RowsAffected() > 0, err
}

// ExtendPlan moves the end of an approved interactive plan to until when that is later (never shorter):
// a step that waited for an observed platform job counts the plan's validity again from its end (#169).
// An autonomous plan has its own budget and is not touched; ok is false when nothing changed.
func (s *Store) ExtendPlan(ctx context.Context, planID string, until time.Time) (bool, error) {
	if !isUUID(planID) {
		return false, ErrNotFound
	}
	tag, err := s.pool.Exec(ctx, `UPDATE plans SET expires_at=GREATEST(expires_at,$2)
WHERE id=$1 AND state='approved' AND mode='interactive'`, planID, until)
	return tag.RowsAffected() > 0, err
}

// --- Autonomous plans (A-E1) ---

// ModeInteractive is the default mode of a plan; only "autonomous" carries a budget and a grant.
const ModeInteractive = "interactive"

func orInteractive(mode string) string {
	if mode == "" {
		return ModeInteractive
	}
	return mode
}

// ActiveAutonomousPlan returns the newest autonomous plan of a chat that is approved or paused,
// independent of the session (A-E1, D1.11); ErrNotFound if there is none.
func (s *Store) ActiveAutonomousPlan(ctx context.Context, chatID string) (Plan, error) {
	var id string
	err := s.pool.QueryRow(ctx, `SELECT id::text FROM plans WHERE chat_id=$1 AND mode='autonomous' AND state IN ('approved','paused')
ORDER BY decided_at DESC NULLS LAST, created_at DESC LIMIT 1`, chatID).Scan(&id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Plan{}, ErrNotFound
		}
		return Plan{}, err
	}
	return s.GetPlan(ctx, id)
}

// The pause reasons the gateway sets for itself when the login or the process is gone (A-E3). The other
// reasons ("user", "abort", "budget_cost", "budget_turns") are set by whoever causes them.
const (
	// PauseLogin: the owner's login is gone; the owner's next login continues the plan (D3.6).
	PauseLogin = "login"
	// PauseRestart: the gateway restarted while a step was on its way; only the user resumes (D3.4).
	PauseRestart = "restart"
)

// PlanOwner is one owner with the plans of that owner a query selected (A-E3), oldest first. The owner is
// the "sub" of the login the plans were created with; an empty owner (a chat of the token mode) never
// appears, there is no login to keep fresh.
type PlanOwner struct {
	Owner string
	Plans []string
}

// AutonomousPlanOwners lists the owners with an approved autonomous plan and those plans (A-E3, D3.1): the
// endurance loop of the manager renews their logins ahead of time, so a plan survives hours without a
// browser. Plans of chats without an owner are left out.
func (s *Store) AutonomousPlanOwners(ctx context.Context) ([]PlanOwner, error) {
	return s.planOwnersBy(ctx, `p.state='approved'`)
}

// LoginPausedPlans lists the autonomous plans of one owner that are paused because the login was gone
// (A-E3, D3.6): the owner's next login continues them. Plans paused for another reason (restart, user,
// abort, budget) stay paused — the user decides about those.
func (s *Store) LoginPausedPlans(ctx context.Context, owner string) ([]PlanOwner, error) {
	if owner == "" {
		return nil, nil
	}
	// The owner is part of the query, not of the caller's filtering: a login of one user must never continue
	// a plan of another (chats.owner is the only link between the two).
	return s.planOwnersBy(ctx, `c.owner=$1 AND p.state='paused' AND p.pause_reason=$2`, owner, PauseLogin)
}

// planOwnersBy groups the autonomous plans that match a condition by their chat's owner.
func (s *Store) planOwnersBy(ctx context.Context, cond string, args ...any) ([]PlanOwner, error) {
	rows, err := s.pool.Query(ctx, `SELECT COALESCE(c.owner,''), p.id::text FROM plans p JOIN chats c ON c.id=p.chat_id
WHERE p.mode='autonomous' AND COALESCE(c.owner,'') <> '' AND `+cond+`
ORDER BY c.owner, p.created_at, p.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PlanOwner
	for rows.Next() {
		var owner, id string
		if err := rows.Scan(&owner, &id); err != nil {
			return nil, err
		}
		if n := len(out); n > 0 && out[n-1].Owner == owner {
			out[n-1].Plans = append(out[n-1].Plans, id)
			continue
		}
		out = append(out, PlanOwner{Owner: owner, Plans: []string{id}})
	}
	return out, rows.Err()
}

// ReleasePlanStep moves a claimed step back to open and gives its claim back (A-E3, D3.5): the call did not
// reach the platform because the user's login was gone, which is no failure of the step and does not count
// against max_attempts. ok is false if the step was not running.
func (s *Store) ReleasePlanStep(ctx context.Context, planID string, idx int, result string) (bool, error) {
	if !isUUID(planID) {
		return false, ErrNotFound
	}
	tag, err := s.pool.Exec(ctx, `UPDATE plan_steps SET state='open', result=$3, done_at=NULL, attempts=GREATEST(attempts-1,0)
WHERE plan_id=$1 AND idx=$2 AND state='running'`, planID, idx, result)
	return tag.RowsAffected() > 0, err
}

// PausePlan moves an approved plan to paused with a reason; ok is false if it was not approved.
func (s *Store) PausePlan(ctx context.Context, planID, reason string) (bool, error) {
	if !isUUID(planID) {
		return false, ErrNotFound
	}
	tag, err := s.pool.Exec(ctx, `UPDATE plans SET state='paused', pause_reason=$2 WHERE id=$1 AND state='approved'`, planID, reason)
	return tag.RowsAffected() > 0, err
}

// ResumePlan moves a paused plan back to approved and clears the pause reason; ok is false if it was
// not paused. Only the user's API calls this: no tool reaches it (A-E1, fail closed).
func (s *Store) ResumePlan(ctx context.Context, planID string) (bool, error) {
	if !isUUID(planID) {
		return false, ErrNotFound
	}
	tag, err := s.pool.Exec(ctx, `UPDATE plans SET state='approved', pause_reason='' WHERE id=$1 AND state='paused'`, planID)
	return tag.RowsAffected() > 0, err
}

// CancelPlan ends an approved or paused plan: state cancelled, its open steps skipped, ended_at set; one
// transaction. ok is false if the plan was neither approved nor paused.
func (s *Store) CancelPlan(ctx context.Context, planID string) (bool, error) {
	if !isUUID(planID) {
		return false, ErrNotFound
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `UPDATE plans SET state='cancelled', ended_at=now() WHERE id=$1 AND state IN ('approved','paused')`, planID)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	// "waiting" belongs in the list: a cancelled plan does not continue its observed step either (A-E2).
	if _, err := tx.Exec(ctx, `UPDATE plan_steps SET state='skipped' WHERE plan_id=$1 AND state IN ('open','running','waiting')`, planID); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// SupersedePlan ends one plan by a newer one (D1.12), independent of the session; ok is false if it was
// not approved or paused.
func (s *Store) SupersedePlan(ctx context.Context, planID string) (bool, error) {
	if !isUUID(planID) {
		return false, ErrNotFound
	}
	tag, err := s.pool.Exec(ctx, `UPDATE plans SET state='superseded', ended_at=now() WHERE id=$1 AND state IN ('approved','paused')`, planID)
	return tag.RowsAffected() > 0, err
}

// FinishPlan marks an autonomous plan completed or failed and sets ended_at; ok is false if it was not approved.
func (s *Store) FinishPlan(ctx context.Context, planID, state string) (bool, error) {
	if !isUUID(planID) {
		return false, ErrNotFound
	}
	tag, err := s.pool.Exec(ctx, `UPDATE plans SET state=$2, ended_at=now() WHERE id=$1 AND state='approved'`, planID, state)
	return tag.RowsAffected() > 0, err
}

// RetryPlanStep moves a failed step back to open, as long as it has attempts left (attempts < max_attempts);
// ok is false if the step was not failed or no attempt is left (A-E1, D1.7).
func (s *Store) RetryPlanStep(ctx context.Context, planID string, idx int) (bool, error) {
	if !isUUID(planID) {
		return false, ErrNotFound
	}
	tag, err := s.pool.Exec(ctx, `UPDATE plan_steps SET state='open', result='', done_at=NULL, watch_job=''
WHERE plan_id=$1 AND idx=$2 AND state='failed' AND attempts < max_attempts`, planID, idx)
	return tag.RowsAffected() > 0, err
}

// ReopenFailedPlanSteps moves the failed steps of a plan back to open and reports how many (A-E3, D3.4):
// resuming a plan is the user's repeat of a step a restart interrupted, after they checked on the platform
// whether the call arrived. The attempts stay: the claim of the interrupted call was used.
func (s *Store) ReopenFailedPlanSteps(ctx context.Context, planID string) (int, error) {
	if !isUUID(planID) {
		return 0, ErrNotFound
	}
	tag, err := s.pool.Exec(ctx, `UPDATE plan_steps SET state='open', result='', done_at=NULL
WHERE plan_id=$1 AND state='failed'`, planID)
	return int(tag.RowsAffected()), err
}

// IncPlanAutoTurns counts one turn without the user for the plan and returns the new number (A-E1).
func (s *Store) IncPlanAutoTurns(ctx context.Context, planID string) (int, error) {
	if !isUUID(planID) {
		return 0, ErrNotFound
	}
	var n int
	err := s.pool.QueryRow(ctx, `UPDATE plans SET auto_turns=auto_turns+1 WHERE id=$1 RETURNING auto_turns`, planID).Scan(&n)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, ErrNotFound
		}
		return 0, err
	}
	return n, nil
}

// PlanLLMCost sums the model cost of a chat's calls from since on (A-E1, D1.9): llm_calls is the
// authoritative source, subagent runs included.
func (s *Store) PlanLLMCost(ctx context.Context, chatID string, since time.Time) (float64, error) {
	var sum float64
	err := s.pool.QueryRow(ctx, `SELECT COALESCE(SUM(cost),0) FROM llm_calls WHERE chat_id=$1 AND started_at >= $2`, chatID, since).Scan(&sum)
	return sum, err
}

// PlanUsageOf reads what an autonomous plan has used of its budget (A-E4, D4.5): the model cost of the
// chat since the plan was approved (a plan that is still proposed has none), the turns it took without
// the user and the compute calls of the chat. ComputeMax is left at zero — that limit is the manager's,
// not the store's, and the manager fills it.
func (s *Store) PlanUsageOf(ctx context.Context, p Plan) (PlanUsage, error) {
	u := PlanUsage{AutoTurns: p.AutoTurns}
	if p.DecidedAt != nil {
		cost, err := s.PlanLLMCost(ctx, p.ChatID, *p.DecidedAt)
		if err != nil {
			return u, err
		}
		u.LLMCost = cost
	}
	n, err := s.ComputeCalls(ctx, p.ChatID)
	if err != nil {
		return u, err
	}
	u.ComputeUsed = n
	return u, nil
}

// RecoverPlans runs at startup: the waiting calls are gone, so proposals expire, and a step that was
// running fails. An interactive plan fails with it; an autonomous plan is paused instead (A-E3, D3.4):
// hours of work are not thrown away because the container was replaced, and only the user resumes a plan
// whose interrupted call may or may not have reached the platform. A plan that was already paused when the
// step was running takes the reason "restart" as well: its failed step blocks it (the agent may only run
// its next open step), so the user has to decide — the same check on the platform D3.4 asks for.
func (s *Store) RecoverPlans(ctx context.Context) error {
	if _, err := s.pool.Exec(ctx, `UPDATE plans SET state='expired', decided_at=now() WHERE state='proposed'`); err != nil {
		return err
	}
	if _, err := s.pool.Exec(ctx, `WITH stuck AS (
  UPDATE plan_steps SET state='failed', result=$1, done_at=now() WHERE state='running' RETURNING plan_id
)
UPDATE plans SET state = CASE WHEN mode='autonomous' THEN 'paused' ELSE 'failed' END,
  pause_reason = CASE WHEN mode='autonomous' THEN $2 ELSE pause_reason END,
  ended_at = CASE WHEN mode='autonomous' THEN ended_at ELSE now() END
WHERE (state='approved' OR (mode='autonomous' AND state='paused')) AND id IN (SELECT plan_id FROM stuck)`,
		"orchestrator restarted during the call; check the platform before retrying", PauseRestart); err != nil {
		return err
	}
	// Steps in state "waiting" (A-E2) are not stuck: their platform job runs on and their row in watch_jobs
	// survives the restart, so the loop picks the observation up again. Parked approvals stay open, the
	// user may still decide.
	return nil
}

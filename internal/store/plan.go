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
	ID         string     `json:"id"`
	ChatID     string     `json:"chat_id"`
	ApprovalID string     `json:"approval_id,omitempty"`
	Title      string     `json:"title"`
	Session    string     `json:"session,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Via        string     `json:"via"`
	State      string     `json:"state"`
	CreatedAt  time.Time  `json:"created_at"`
	DecidedAt  *time.Time `json:"decided_at,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	Steps      []PlanStep `json:"steps"`
}

// PlanStep is one call of a plan. Query, Body and Form hold the canonical JSON as stored.
type PlanStep struct {
	Idx             int             `json:"idx"`
	Description     string          `json:"description,omitempty"`
	Tool            string          `json:"tool,omitempty"`
	Method          string          `json:"method"`
	Path            string          `json:"path"`
	Query           json.RawMessage `json:"query,omitempty"`
	Body            json.RawMessage `json:"body,omitempty"`
	BodySHA256      string          `json:"body_sha256,omitempty"`
	Form            json.RawMessage `json:"form,omitempty"`
	FormSHA256      string          `json:"form_sha256,omitempty"`
	Files           []PlanFile      `json:"files,omitempty"`
	Effect          string          `json:"effect"`
	Covered         bool            `json:"covered"`
	State           string          `json:"state"`
	Result          string          `json:"result,omitempty"`
	CreatedResource string          `json:"created_resource,omitempty"`
	CreatedID       string          `json:"created_id,omitempty"`
	DoneAt          *time.Time      `json:"done_at,omitempty"`
	// PlanID: the plan the step belongs to (not part of the JSON; the plan carries it).
	PlanID string `json:"-"`
}

// PlanFile is an upload of a step, pinned by its SHA-256.
type PlanFile struct {
	Field  string `json:"field"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

const planCols = `id::text, chat_id::text, approval_id, title, session, tool_call_id, via, state, created_at, decided_at, expires_at`

const planStepCols = `idx, description, tool, method, path, query, body, body_sha256, form, form_sha256, files, effect, state, result, created_resource, created_id, done_at`

func scanPlan(row interface{ Scan(...any) error }) (Plan, error) {
	var p Plan
	err := row.Scan(&p.ID, &p.ChatID, &p.ApprovalID, &p.Title, &p.Session, &p.ToolCallID, &p.Via, &p.State, &p.CreatedAt, &p.DecidedAt, &p.ExpiresAt)
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
	var query, body, form, files string
	err := row.Scan(&st.Idx, &st.Description, &st.Tool, &st.Method, &st.Path, &query, &body, &st.BodySHA256, &form, &st.FormSHA256, &files,
		&st.Effect, &st.State, &st.Result, &st.CreatedResource, &st.CreatedID, &st.DoneAt)
	if err != nil {
		return st, err
	}
	st.Query = rawOrNil(query)
	st.Body = rawOrNil(body)
	st.Form = rawOrNil(form)
	if files != "" && files != "[]" {
		if err := json.Unmarshal([]byte(files), &st.Files); err != nil {
			return st, err
		}
	}
	st.Covered = st.Effect != "irreversible"
	return st, nil
}

// CreatePlan stores a new plan with its steps in state "proposed"; the steps start "open".
func (s *Store) CreatePlan(ctx context.Context, p Plan) (Plan, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Plan{}, err
	}
	defer tx.Rollback(ctx)
	var id string
	err = tx.QueryRow(ctx, `INSERT INTO plans (chat_id, approval_id, title, session, tool_call_id, via)
VALUES ($1,$2,$3,$4,$5,$6) RETURNING id::text`,
		p.ChatID, p.ApprovalID, p.Title, p.Session, p.ToolCallID, p.Via).Scan(&id)
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
		_, err := tx.Exec(ctx, `INSERT INTO plan_steps (plan_id, idx, description, tool, method, path, query, body, body_sha256,
form, form_sha256, files, effect) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
			id, st.Idx, st.Description, st.Tool, st.Method, st.Path, string(st.Query), string(st.Body), st.BodySHA256,
			string(st.Form), st.FormSHA256, files, st.Effect)
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
	rows, err := s.pool.Query(ctx, `SELECT `+planStepCols+` FROM plan_steps WHERE plan_id=$1 ORDER BY idx`, planID)
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

// ClaimPlanStep sets an open step to running; ok is false if it was not open.
func (s *Store) ClaimPlanStep(ctx context.Context, planID string, idx int) (bool, error) {
	if !isUUID(planID) {
		return false, ErrNotFound
	}
	tag, err := s.pool.Exec(ctx, `UPDATE plan_steps SET state='running' WHERE plan_id=$1 AND idx=$2 AND state='open'`, planID, idx)
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

// RecoverPlans runs at startup: the waiting calls are gone, so proposals expire, and a step
// that was running fails together with its plan. Approved plans stay approved.
func (s *Store) RecoverPlans(ctx context.Context) error {
	if _, err := s.pool.Exec(ctx, `UPDATE plans SET state='expired', decided_at=now() WHERE state='proposed'`); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `WITH stuck AS (
  UPDATE plan_steps SET state='failed', result='orchestrator restarted', done_at=now() WHERE state='running' RETURNING plan_id
)
UPDATE plans SET state='failed' WHERE state='approved' AND id IN (SELECT plan_id FROM stuck)`)
	return err
}

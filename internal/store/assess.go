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

// The result checks of a chat (R0). One row per check: what it checked, in which state it is and the
// report the gateway wrote. The check itself runs in the gateway (internal/assess), not on the
// platform; this table is what the tool assess_status, the check card and the recovery read.

//go:embed assess_schema.sql
var assessSchemaSQL string

// init registers the table of the result checks through the seam of G1.6.
func init() { registerSchema(assessSchemaSQL) }

// States of a check (table assessments).
const (
	// AssessQueued: the row exists, the check waits for the runner (one at a time).
	AssessQueued = "queued"
	// AssessRunning: the check runs in its container.
	AssessRunning = "running"
	// AssessFinished: the report stands.
	AssessFinished = "finished"
	// AssessFailed: the check ended with an error; a partial report may stand next to it (L5).
	AssessFailed = "failed"
)

// Assessment is one result check of a chat.
type Assessment struct {
	ID     string `json:"check_id"`
	ChatID string `json:"-"`
	Kind   string `json:"kind"`
	// Subject: what was checked, e.g. model:<id>, sha:<hex> or deployment:<name>.
	Subject string `json:"subject"`
	// EvalDatasetID: the dataset whose held-out images were measured (0: none, stored as NULL).
	EvalDatasetID int64 `json:"eval_dataset_id,omitempty"`
	// Final: the check measured at the hold-out set (P4).
	Final bool   `json:"final"`
	State string `json:"state"`
	// HoldoutMeasured: false when a final check ended before the hold-out images ran through the model
	// (#181); such a check does not use up the final test.
	HoldoutMeasured bool `json:"holdout_measured"`
	// Progress: what the check is doing while it runs (L4); nil otherwise.
	Progress json.RawMessage `json:"progress"`
	Report   json.RawMessage `json:"report"`
	Error    string          `json:"error,omitempty"`
	// ToolCallID: the tool call that started the check (issue #112/H113); empty: unknown.
	ToolCallID string     `json:"tool_call_id,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	EndedAt    *time.Time `json:"ended_at,omitempty"`
}

// AssessmentFinal reports whether a state means the check is over.
func AssessmentFinal(state string) bool {
	return state == AssessFinished || state == AssessFailed
}

const assessmentCols = `id::text, chat_id::text, kind, subject, COALESCE(eval_dataset_id, 0), final, state, progress, report, error, tool_call_id, created_at, started_at, ended_at, holdout_measured`

// CreateAssessment stores a new check (state AssessQueued). The unique partial index refuses a second
// final check of the same dataset in this chat (P4).
func (s *Store) CreateAssessment(ctx context.Context, a Assessment) (Assessment, error) {
	var dataset any
	if a.EvalDatasetID > 0 {
		dataset = a.EvalDatasetID
	}
	if a.State == "" {
		a.State = AssessQueued
	}
	err := s.pool.QueryRow(ctx, `
INSERT INTO assessments (chat_id, kind, subject, eval_dataset_id, final, state, tool_call_id)
VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING `+assessmentCols,
		a.ChatID, a.Kind, a.Subject, dataset, a.Final, a.State, a.ToolCallID).Scan(&a.ID, &a.ChatID, &a.Kind,
		&a.Subject, &a.EvalDatasetID, &a.Final, &a.State, &a.Progress, &a.Report, &a.Error, &a.ToolCallID,
		&a.CreatedAt, &a.StartedAt, &a.EndedAt, &a.HoldoutMeasured)
	return a, err
}

// SetAssessmentState moves a check of this chat into another state; running also sets started_at.
func (s *Store) SetAssessmentState(ctx context.Context, chatID, id, state string) error {
	_, err := s.pool.Exec(ctx, `
UPDATE assessments SET state=$3, started_at=CASE WHEN $3='running' THEN now() ELSE started_at END, error=''
WHERE chat_id=$1 AND id=$2`, chatID, id, state)
	return err
}

// SetAssessmentProgress records what a running check is doing (L4). The row stays untouched if the
// check is already over: the last progress of a finished check is not written again.
func (s *Store) SetAssessmentProgress(ctx context.Context, id string, p json.RawMessage) error {
	_, err := s.pool.Exec(ctx, `UPDATE assessments SET progress=$2 WHERE id=$1 AND state=$3`, id, []byte(p), AssessRunning)
	return err
}

// FinishAssessment records the end of a check of this chat: state, report (may be empty) and the error
// text. ended_at is set once, so a second report of the same end changes nothing.
func (s *Store) FinishAssessment(ctx context.Context, chatID, id, state string, report json.RawMessage, errText string) error {
	var rep any
	if len(report) > 0 {
		rep = []byte(report)
	}
	_, err := s.pool.Exec(ctx, `
UPDATE assessments SET state=$3, report=COALESCE($4::jsonb, report), error=$5, ended_at=COALESCE(ended_at, now())
WHERE chat_id=$1 AND id=$2`, chatID, id, state, rep, errText)
	return err
}

// Assessment reads one check of a chat; a check of another chat is not found (E8).
func (s *Store) Assessment(ctx context.Context, chatID, id string) (Assessment, error) {
	if !isUUID(id) {
		return Assessment{}, ErrNotFound
	}
	var a Assessment
	err := s.pool.QueryRow(ctx, `SELECT `+assessmentCols+` FROM assessments WHERE chat_id=$1 AND id=$2`, chatID, id).
		Scan(&a.ID, &a.ChatID, &a.Kind, &a.Subject, &a.EvalDatasetID, &a.Final, &a.State, &a.Progress, &a.Report,
			&a.Error, &a.ToolCallID, &a.CreatedAt, &a.StartedAt, &a.EndedAt, &a.HoldoutMeasured)
	if errors.Is(err, pgx.ErrNoRows) {
		return Assessment{}, ErrNotFound
	}
	return a, err
}

// Assessments lists a chat's checks, oldest first (the check card shows them in this order).
func (s *Store) Assessments(ctx context.Context, chatID string) ([]Assessment, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+assessmentCols+` FROM assessments WHERE chat_id=$1 ORDER BY created_at, id`, chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Assessment{}
	for rows.Next() {
		var a Assessment
		if err := rows.Scan(&a.ID, &a.ChatID, &a.Kind, &a.Subject, &a.EvalDatasetID, &a.Final, &a.State, &a.Progress,
			&a.Report, &a.Error, &a.ToolCallID, &a.CreatedAt, &a.StartedAt, &a.EndedAt, &a.HoldoutMeasured); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// CountAssessments counts the checks a chat has used (AGW_ASSESS_MAX_PER_CHAT). Failed checks do not
// count: a check that broke says nothing about the result and must not eat a try.
func (s *Store) CountAssessments(ctx context.Context, chatID string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM assessments WHERE chat_id=$1 AND state <> $2`, chatID, AssessFailed).Scan(&n)
	return n, err
}

// FinalUsed reports whether this chat has already measured a final check of this dataset (P4). The
// gateway checks it before a final check is created; the unique partial index holds the same rule in
// the database.
func (s *Store) FinalUsed(ctx context.Context, chatID string, datasetID int64) (bool, error) {
	var dataset any
	if datasetID > 0 {
		dataset = datasetID
	}
	var ok bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM assessments
WHERE chat_id=$1 AND final AND holdout_measured AND state <> $3 AND eval_dataset_id IS NOT DISTINCT FROM $2)`, chatID, dataset, AssessFailed).Scan(&ok)
	return ok, err
}

// MarkHoldoutNotRun records that a final check ended without the hold-out images having run through the
// model (#181): the final test stays available.
func (s *Store) MarkHoldoutNotRun(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `UPDATE assessments SET holdout_measured=false WHERE id=$1`, id)
	return err
}

// RecoverAssessments runs at startup: a check that was queued or running when the gateway stopped has
// no container any more, so it ends as failed (R0.22). It answers how many rows it ended.
func (s *Store) RecoverAssessments(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE assessments SET state=$1, error='gateway restarted', ended_at=now()
WHERE state IN ($2, $3)`, AssessFailed, AssessQueued, AssessRunning)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

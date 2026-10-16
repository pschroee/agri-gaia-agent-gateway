// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// The background jobs of pre-annotation (V3). A check (calibration or uncertainty) and a fill run in
// the gateway instead of inside a tool call: minutes of CPU time would run into the time limits of a
// call, and the agent follows a job like a run. One row carries what it was started with (params) and
// how it ended (state, result, error); done and total are what the observation kind annotation_job and
// the tool annotation_job read.

// States of a job (table annotation_jobs).
const (
	// AnnotationJobQueued: the row exists, the job waits for the one slot of the service.
	AnnotationJobQueued = "queued"
	// AnnotationJobRunning: the job works through its images.
	AnnotationJobRunning = "running"
	// AnnotationJobFinished: the result stands.
	AnnotationJobFinished = "finished"
	// AnnotationJobFailed: the job ended with an error; a partial result may stand next to it.
	AnnotationJobFailed = "failed"
)

// AnnotationJobRecovered is what a job that ran in the last process carries afterwards: its goroutine
// is gone with the process, so it can never end by itself.
const AnnotationJobRecovered = "orchestrator restarted"

// AnnotationJobVerify is the kind of the job that checks the result of a run or a fill (V3b). It is the
// only kind whose params name a source; LastVerifyResult finds the newest one through it.
const AnnotationJobVerify = "verify"

// AnnotationJob is one background job of a chat (V3).
type AnnotationJob struct {
	ID string `json:"job_id"`
	// ChatID is not part of an answer: a job of another chat is not found (E8).
	ChatID    string `json:"-"`
	Kind      string `json:"kind"` // calibrate | uncertain | fill | verify
	DatasetID int64  `json:"dataset_id"`
	TaskID    int64  `json:"task_id"`
	Function  string `json:"function"`
	// Params is the call the job was started with, as JSON (the agent asked for it, not the CVAT form).
	Params string `json:"params"`
	State  string `json:"state"`
	Done   int    `json:"done"`
	Total  int    `json:"total"`
	// Result is the answer of a finished job as JSON; empty while it runs and after a job that failed
	// without a partial result.
	Result    string     `json:"result,omitempty"`
	Error     string     `json:"error,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
}

// AnnotationJobFinal reports whether a state means the job is over.
func AnnotationJobFinal(state string) bool {
	return state == AnnotationJobFinished || state == AnnotationJobFailed
}

const annotationJobCols = `id::text, chat_id::text, kind, dataset_id, task_id, function, params, state, done, total, result, error, created_at, ended_at`

// AddAnnotationJob stores a new job (state queued) and returns its id.
func (s *Store) AddAnnotationJob(ctx context.Context, j AnnotationJob) (AnnotationJob, error) {
	if j.Params == "" {
		j.Params = "{}"
	}
	if j.State == "" {
		j.State = AnnotationJobQueued
	}
	err := s.pool.QueryRow(ctx, `
INSERT INTO annotation_jobs (chat_id, kind, dataset_id, task_id, function, params, state)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING id::text, created_at`,
		j.ChatID, j.Kind, j.DatasetID, j.TaskID, j.Function, j.Params, j.State).Scan(&j.ID, &j.CreatedAt)
	if err != nil {
		return AnnotationJob{}, err
	}
	return j, nil
}

// AnnotationJob reads a job of one chat; a job of another chat is not found (E8).
func (s *Store) AnnotationJob(ctx context.Context, chatID, id string) (AnnotationJob, error) {
	j, err := annotationJobScan(s.pool.QueryRow(ctx, `SELECT `+annotationJobCols+` FROM annotation_jobs WHERE chat_id=$1 AND id=$2`, chatID, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return AnnotationJob{}, ErrNotFound
	}
	return j, err
}

// annotationJobScan reads a row of annotation_jobs in the order of annotationJobCols.
func annotationJobScan(row interface{ Scan(...any) error }) (AnnotationJob, error) {
	var j AnnotationJob
	err := row.Scan(&j.ID, &j.ChatID, &j.Kind, &j.DatasetID, &j.TaskID, &j.Function, &j.Params, &j.State, &j.Done,
		&j.Total, &j.Result, &j.Error, &j.CreatedAt, &j.EndedAt)
	return j, err
}

// AnnotationJobs lists the jobs of one chat, oldest first (the check card shows them in this order;
// the result of a finished job is what it renders).
func (s *Store) AnnotationJobs(ctx context.Context, chatID string) ([]AnnotationJob, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+annotationJobCols+` FROM annotation_jobs WHERE chat_id=$1 ORDER BY created_at, id`, chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AnnotationJob{}
	for rows.Next() {
		j, err := annotationJobScan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// SetAnnotationJobState moves a job into another state; a state that ends the job also sets ended_at.
func (s *Store) SetAnnotationJobState(ctx context.Context, chatID, id, state string) error {
	_, err := s.pool.Exec(ctx, `
UPDATE annotation_jobs SET state=$3, ended_at=CASE WHEN $4 THEN now() ELSE ended_at END
WHERE chat_id=$1 AND id=$2`, chatID, id, state, AnnotationJobFinal(state))
	return err
}

// SetAnnotationJobProgress writes how far a job is; a job that is over keeps what it has.
func (s *Store) SetAnnotationJobProgress(ctx context.Context, chatID, id string, done, total int) error {
	_, err := s.pool.Exec(ctx, `
UPDATE annotation_jobs SET done=$3, total=$4 WHERE chat_id=$1 AND id=$2 AND state <> $5`,
		chatID, id, done, total, AnnotationJobFinished)
	return err
}

// FinishAnnotationJob ends a job with its result (JSON, may be empty) and its error.
func (s *Store) FinishAnnotationJob(ctx context.Context, chatID, id, state, result, errText string) error {
	_, err := s.pool.Exec(ctx, `
UPDATE annotation_jobs SET state=$3, result=$4, error=$5, ended_at=now() WHERE chat_id=$1 AND id=$2`,
		chatID, id, state, result, errText)
	return err
}

// RecoverAnnotationJobs ends the jobs of a previous process (V3): their goroutine is gone with it, so a
// job that was queued or running can never end by itself. Called once at the start of the orchestrator.
func (s *Store) RecoverAnnotationJobs(ctx context.Context) (int, error) {
	tag, err := s.pool.Exec(ctx, `
UPDATE annotation_jobs SET state=$1, error=$2, ended_at=now() WHERE state IN ($3, $4)`,
		AnnotationJobFailed, AnnotationJobRecovered, AnnotationJobQueued, AnnotationJobRunning)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	_ "embed"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

//go:embed annotation_schema.sql
var annotationSchemaSQL string

// init registers the table of the pre-annotation runs (V1) through the seam of G1.6.
func init() { registerSchema(annotationSchemaSQL) }

// States of a pre-annotation run (table annotation_runs). queued, started and finished are CVAT's own
// states; expired is what the gateway writes when CVAT no longer knows the run (its rq job is gone),
// failed when the run ended with an error.
const (
	AnnotationQueued   = "queued"
	AnnotationStarted  = "started"
	AnnotationFinished = "finished"
	AnnotationFailed   = "failed"
	AnnotationExpired  = "expired"
)

// AnnotationRun is one pre-annotation run of a chat (V1). ID is the request id of the CVAT queue; the
// progress of the run is not kept here (watch_jobs, autonomy A-E2).
type AnnotationRun struct {
	ID            string     `json:"id"`
	ChatID        string     `json:"chat_id"`
	DatasetID     int64      `json:"dataset_id"`
	TaskID        int64      `json:"task_id"`
	Function      string     `json:"function"`
	MinConfidence int        `json:"min_confidence"`
	Mapping       string     `json:"mapping"`
	Existing      string     `json:"existing"`
	State         string     `json:"state"`
	Error         string     `json:"error,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	EndedAt       *time.Time `json:"ended_at,omitempty"`
}

// AnnotationRunFinal reports whether a state means the run is over; then the row gets its ended_at.
func AnnotationRunFinal(state string) bool {
	switch state {
	case AnnotationFinished, AnnotationFailed, AnnotationExpired:
		return true
	}
	return false
}

const annotationRunCols = `id, chat_id::text, dataset_id, task_id, function, min_confidence, mapping, existing, state, error, created_at, ended_at`

// AddAnnotationRun records a run of a chat.
func (s *Store) AddAnnotationRun(ctx context.Context, r AnnotationRun) (AnnotationRun, error) {
	if r.Mapping == "" {
		r.Mapping = "{}"
	}
	if r.State == "" {
		r.State = AnnotationQueued
	}
	err := s.pool.QueryRow(ctx, `
		INSERT INTO annotation_runs (id, chat_id, dataset_id, task_id, function, min_confidence, mapping, existing, state)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING created_at`,
		r.ID, r.ChatID, r.DatasetID, r.TaskID, r.Function, r.MinConfidence, r.Mapping, r.Existing, r.State).Scan(&r.CreatedAt)
	if err != nil {
		return AnnotationRun{}, err
	}
	return r, nil
}

// AnnotationRun reads a run of one chat; a run of another chat is not found (E8).
func (s *Store) AnnotationRun(ctx context.Context, chatID, id string) (AnnotationRun, error) {
	var r AnnotationRun
	err := s.pool.QueryRow(ctx, `SELECT `+annotationRunCols+` FROM annotation_runs WHERE chat_id=$1 AND id=$2`, chatID, id).
		Scan(&r.ID, &r.ChatID, &r.DatasetID, &r.TaskID, &r.Function, &r.MinConfidence, &r.Mapping, &r.Existing, &r.State, &r.Error, &r.CreatedAt, &r.EndedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return AnnotationRun{}, ErrNotFound
	}
	return r, err
}

// UpdateAnnotationRun writes state and error of a run of this chat; a final state also sets ended_at.
func (s *Store) UpdateAnnotationRun(ctx context.Context, chatID, id, state, errText string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE annotation_runs SET state=$3, error=$4, ended_at=CASE WHEN $5 THEN now() ELSE ended_at END
		WHERE chat_id=$1 AND id=$2`, chatID, id, state, errText, AnnotationRunFinal(state))
	return err
}

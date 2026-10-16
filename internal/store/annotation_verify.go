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

// The register of the verification of a pre-annotation result (V3b): the three sets of manually
// annotated images of a task (annotation_splits), the annotations a run or a fill wrote
// (annotation_objects) and the counters and one-off marks of a task in a chat (annotation_tasks). The
// rules that fill these tables live in internal/annotation; here stands only what is written and read.

// Parts of a manually annotated image of a task (table annotation_splits, E22).
const (
	SplitCalibration = "calibration"
	SplitValidation  = "validation"
	SplitHoldout     = "holdout"
)

// Kinds of a registered annotation (annotation_objects): what CVAT needs to name an object in its
// delete call.
const (
	ObjectTag   = "tag"
	ObjectShape = "shape"
	ObjectTrack = "track"
)

// AnnotationObject is one annotation a run or a fill wrote, as the register keeps it (V3b). Only an
// object of this register may be taken back (E21).
type AnnotationObject struct {
	// SourceID is the run id of CVAT or the job id of the gateway: what wrote the object.
	SourceID string `json:"source_id"`
	// ChatID is not part of an answer: the objects of another chat are not found (E8).
	ChatID  string `json:"-"`
	TaskID  int64  `json:"task_id"`
	Kind    string `json:"kind"` // tag | shape | track
	CVATID  int64  `json:"cvat_id"`
	Frame   int    `json:"frame"`
	LabelID int64  `json:"label_id"`
	// Fingerprint is the SHA-256 over what the object was written with: a correction of the user in
	// CVAT changes it, and a changed object is left alone by a rollback.
	Fingerprint string `json:"fingerprint"`
}

// ObjectRef names one registered annotation for a delete (V3b).
type ObjectRef struct {
	TaskID int64
	Kind   string
	CVATID int64
}

// SplitFor returns the part of every frame of a task (V3b, E22): what the table already holds, plus
// assign(assigned) for the frames that are new in it, where assigned counts the frames the table has
// assigned so far. frames is read in the order it comes in and must be sorted by frame number, so that
// the same task always gives the same assignment. The rule itself lives with the caller
// (internal/annotation), so that this table stays a plain record.
func (s *Store) SplitFor(ctx context.Context, chatID string, taskID int64, frames []int, assign func(assigned int) string) (map[int]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT frame, part FROM annotation_splits WHERE task_id=$1`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]string{}
	for rows.Next() {
		var (
			frame int
			part  string
		)
		if err := rows.Scan(&frame, &part); err != nil {
			return nil, err
		}
		out[frame] = part
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var add []struct {
		frame int
		part  string
	}
	for _, frame := range frames {
		if _, ok := out[frame]; ok {
			continue
		}
		part := assign(len(out))
		out[frame] = part
		add = append(add, struct {
			frame int
			part  string
		}{frame, part})
	}
	if len(add) == 0 {
		return out, nil
	}
	// One statement for the new frames, so that two jobs of the same task cannot interleave their
	// assignment through a shared counter. A frame somebody else has assigned in the meantime keeps its
	// part (ON CONFLICT DO NOTHING).
	batch := &pgx.Batch{}
	for _, e := range add {
		batch.Queue(`INSERT INTO annotation_splits (task_id, chat_id, frame, part) VALUES ($1, $2, $3, $4)
			ON CONFLICT (task_id, frame) DO NOTHING`, taskID, chatID, e.frame, e.part)
	}
	br := s.pool.SendBatch(ctx, batch)
	defer br.Close()
	for range add {
		if _, err := br.Exec(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// FramesWithPart are the frames of a task that stand in one part; they are sorted by frame number.
func (s *Store) FramesWithPart(ctx context.Context, taskID int64, part string) ([]int, error) {
	rows, err := s.pool.Query(ctx, `SELECT frame FROM annotation_splits WHERE task_id=$1 AND part=$2 ORDER BY frame`, taskID, part)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []int{}
	for rows.Next() {
		var frame int
		if err := rows.Scan(&frame); err != nil {
			return nil, err
		}
		out = append(out, frame)
	}
	return out, rows.Err()
}

// AddAnnotationObjects registers annotations a run or a fill wrote (V3b). An object that stands in the
// register already keeps its row: the first source that registered it stays the one that may take it
// back, so that a later reading of the same task does not move it to another source.
func (s *Store) AddAnnotationObjects(ctx context.Context, objs []AnnotationObject) error {
	if len(objs) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, o := range objs {
		batch.Queue(`INSERT INTO annotation_objects (source_id, chat_id, task_id, kind, cvat_id, frame, label_id, fingerprint)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8) ON CONFLICT (task_id, kind, cvat_id) DO NOTHING`,
			o.SourceID, o.ChatID, o.TaskID, o.Kind, o.CVATID, o.Frame, o.LabelID, o.Fingerprint)
	}
	br := s.pool.SendBatch(ctx, batch)
	defer br.Close()
	for range objs {
		if _, err := br.Exec(); err != nil {
			return err
		}
	}
	return nil
}

// AnnotationObjects lists what one source of a chat wrote, oldest first. A source of another chat is
// not found (E8): the register is the boundary of a rollback.
func (s *Store) AnnotationObjects(ctx context.Context, chatID, sourceID string) ([]AnnotationObject, error) {
	rows, err := s.pool.Query(ctx, `SELECT source_id, chat_id::text, task_id, kind, cvat_id, frame, label_id, fingerprint
		FROM annotation_objects WHERE chat_id=$1 AND source_id=$2 ORDER BY frame, kind, cvat_id`, chatID, sourceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AnnotationObject{}
	for rows.Next() {
		var o AnnotationObject
		if err := rows.Scan(&o.SourceID, &o.ChatID, &o.TaskID, &o.Kind, &o.CVATID, &o.Frame, &o.LabelID, &o.Fingerprint); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// DeleteAnnotationObjects drops the rows of the register that a rollback took out of CVAT (V3b); the
// rows of another chat stay.
func (s *Store) DeleteAnnotationObjects(ctx context.Context, chatID string, refs []ObjectRef) error {
	if len(refs) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, ref := range refs {
		batch.Queue(`DELETE FROM annotation_objects WHERE chat_id=$1 AND task_id=$2 AND kind=$3 AND cvat_id=$4`,
			chatID, ref.TaskID, ref.Kind, ref.CVATID)
	}
	br := s.pool.SendBatch(ctx, batch)
	defer br.Close()
	for range refs {
		if _, err := br.Exec(); err != nil {
			return err
		}
	}
	return nil
}

// The counters of the correction loop of a task in one chat (table annotation_tasks, V3b/V3c): how many
// rounds it had, how many retrainings, and whether the held-out images were already measured. The rules
// over these numbers live in internal/annotation (rounds.go); here stands only what is read and written.

// AnnotationTaskCounters are the counters of one task in one chat (V3c).
type AnnotationTaskCounters struct {
	// TaskID is the CVAT task the row belongs to.
	TaskID int64 `json:"task_id"`
	// Rounds is how many correction rounds the task had in this chat: every fill and every run after the
	// initial pre-annotation (V3c).
	Rounds int `json:"rounds"`
	// Retrains is how many retrainings the loop of this task had (V3c).
	Retrains int `json:"retrains"`
	// Final: the held-out images of the task were measured in this chat, so no round may follow (E22).
	Final bool `json:"final_verified"`
}

// AnnotationTaskCountersOf reads the counters of one task in one chat (V3c). A task without a row has no
// rounds and no retraining; that is no error (the row appears with the first round).
func (s *Store) AnnotationTaskCountersOf(ctx context.Context, chatID string, taskID int64) (AnnotationTaskCounters, error) {
	c := AnnotationTaskCounters{TaskID: taskID}
	err := s.pool.QueryRow(ctx, `SELECT rounds, retrains, final_verified_at IS NOT NULL
		FROM annotation_tasks WHERE chat_id=$1 AND task_id=$2`, chatID, taskID).
		Scan(&c.Rounds, &c.Retrains, &c.Final)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, nil
	}
	return c, err
}

// AnnotationTaskCountersOfChat lists the counters of every task of a chat, by task id (V3c). The
// retraining rule is stated per task, and this reads them all in one call.
func (s *Store) AnnotationTaskCountersOfChat(ctx context.Context, chatID string) ([]AnnotationTaskCounters, error) {
	rows, err := s.pool.Query(ctx, `SELECT task_id, rounds, retrains, final_verified_at IS NOT NULL
		FROM annotation_tasks WHERE chat_id=$1 ORDER BY task_id`, chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AnnotationTaskCounters{}
	for rows.Next() {
		var c AnnotationTaskCounters
		if err := rows.Scan(&c.TaskID, &c.Rounds, &c.Retrains, &c.Final); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// AddCorrectionRound counts one correction round of a task in a chat (V3c) and returns the new number.
// The caller decides whether the round may start; this only counts it.
func (s *Store) AddCorrectionRound(ctx context.Context, chatID string, taskID int64) (int, error) {
	var rounds int
	err := s.pool.QueryRow(ctx, `INSERT INTO annotation_tasks (chat_id, task_id, rounds) VALUES ($1, $2, 1)
		ON CONFLICT (chat_id, task_id) DO UPDATE SET rounds = annotation_tasks.rounds + 1
		RETURNING rounds`, chatID, taskID).Scan(&rounds)
	return rounds, err
}

// AddRetrain counts one retraining on the named tasks of a chat (V3c). A task whose correction loop does
// not run is not among them: a function built without a loop is the normal way of V2.
func (s *Store) AddRetrain(ctx context.Context, chatID string, taskIDs []int64) error {
	if len(taskIDs) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, id := range taskIDs {
		batch.Queue(`UPDATE annotation_tasks SET retrains = retrains + 1 WHERE chat_id=$1 AND task_id=$2`,
			chatID, id)
	}
	br := s.pool.SendBatch(ctx, batch)
	defer br.Close()
	for range taskIDs {
		if _, err := br.Exec(); err != nil {
			return err
		}
	}
	return nil
}

// PreAnnotated counts the pre-annotation runs and fills of a task in one chat (V3c): the first one is the
// initial pre-annotation and no correction round, every further one is a round. Both kinds count, because
// a correction is either (a detector runs over the whole task, a fill works on the images without one).
func (s *Store) PreAnnotated(ctx context.Context, chatID string, taskID int64) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM annotation_runs WHERE chat_id=$1 AND task_id=$2)
		+ (SELECT count(*) FROM annotation_jobs WHERE chat_id=$1 AND task_id=$2 AND kind=$3)`,
		chatID, taskID, AnnotationJobFill).Scan(&n)
	return n, err
}

// VerifyResultsOfTask lists the result of the newest finished verifications of a task in one chat, newest
// first (V3c): the correction loop compares a round with the one before it through them.
func (s *Store) VerifyResultsOfTask(ctx context.Context, chatID string, taskID int64, limit int) ([]string, error) {
	if limit < 1 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT result FROM annotation_jobs
		WHERE chat_id=$1 AND task_id=$2 AND kind=$3 AND state=$4
		ORDER BY created_at DESC, id DESC LIMIT $5`,
		chatID, taskID, AnnotationJobVerify, AnnotationJobFinished, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var result string
		if err := rows.Scan(&result); err != nil {
			return nil, err
		}
		out = append(out, result)
	}
	return out, rows.Err()
}

// MarkFinalVerified marks the held-out images of a task as used in this chat and reports whether they
// were used before (V3b, E22). The mark stands per chat and task: a second final verdict on the same
// task in the same chat is refused, a task of another chat is a row of its own.
func (s *Store) MarkFinalVerified(ctx context.Context, chatID string, taskID int64) (bool, error) {
	var at time.Time
	err := s.pool.QueryRow(ctx, `
		INSERT INTO annotation_tasks (chat_id, task_id, final_verified_at) VALUES ($1, $2, now())
		ON CONFLICT (chat_id, task_id) DO UPDATE SET final_verified_at = now()
		WHERE annotation_tasks.final_verified_at IS NULL
		RETURNING final_verified_at`, chatID, taskID).Scan(&at)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return false, nil
}

// LastVerifyResult is the result of the newest finished verify job of a chat on one source (V3b): the
// suspicious images a rollback with scope suspicious goes by are read out of it. Without such a job the
// answer is empty and the caller says that it has to verify first.
func (s *Store) LastVerifyResult(ctx context.Context, chatID, sourceID string) (string, error) {
	var result string
	err := s.pool.QueryRow(ctx, `SELECT result FROM annotation_jobs
		WHERE chat_id=$1 AND kind=$2 AND state=$3 AND params->>'source_id'=$4
		ORDER BY created_at DESC LIMIT 1`, chatID, AnnotationJobVerify, AnnotationJobFinished, sourceID).Scan(&result)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return result, err
}

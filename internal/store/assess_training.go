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

// Training checks (R1) keep two things the gateway alone writes: the splits a chat made before a
// training (assess_splits) and where a model came from (model_origins). Both are what a check reads to
// decide which images the model did not see; the agent never writes them (P2).

//go:embed assess_training_schema.sql
var assessTrainingSchemaSQL string

func init() { registerSchema(assessTrainingSchemaSQL) }

// Split is one recorded split (table assess_splits, R1.5): which dataset the gateway read, which
// training dataset it created and how many images went each way.
type Split struct {
	ChatID string `json:"-"`
	// SourceDatasetID: the dataset the split read and downloaded (its CVAT annotations were synced).
	SourceDatasetID int64 `json:"source_dataset_id"`
	// TrainDatasetID: the dataset the split created (70 % of the annotated images per class).
	TrainDatasetID int64     `json:"train_dataset_id"`
	TrainImages    int       `json:"train_images"`
	KeptImages     int       `json:"kept_images"`
	CreatedAt      time.Time `json:"created_at"`
}

// RecordSplit stores a split; a second split of the same training dataset replaces the first (the
// training dataset is the key, and a dataset is created once).
func (s *Store) RecordSplit(ctx context.Context, sp Split) error {
	_, err := s.pool.Exec(ctx, `
INSERT INTO assess_splits (chat_id, source_dataset_id, train_dataset_id, train_images, kept_images)
VALUES ($1,$2,$3,$4,$5)
ON CONFLICT (train_dataset_id) DO UPDATE SET
  source_dataset_id = EXCLUDED.source_dataset_id, train_images = EXCLUDED.train_images,
  kept_images = EXCLUDED.kept_images`, sp.ChatID, sp.SourceDatasetID, sp.TrainDatasetID, sp.TrainImages, sp.KeptImages)
	return err
}

const splitCols = `chat_id::text, source_dataset_id, train_dataset_id, train_images, kept_images, created_at`

func scanSplit(row pgx.Row) (Split, error) {
	var sp Split
	err := row.Scan(&sp.ChatID, &sp.SourceDatasetID, &sp.TrainDatasetID, &sp.TrainImages, &sp.KeptImages, &sp.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Split{}, ErrNotFound
	}
	return sp, err
}

// SplitOfTrainDataset reads the split that created a training dataset (ErrNotFound: this chat made no
// such split). A check uses it to find the source whose images were kept back (R1.5).
func (s *Store) SplitOfTrainDataset(ctx context.Context, chatID string, trainDatasetID int64) (Split, error) {
	if !isUUID(chatID) {
		return Split{}, ErrNotFound
	}
	return scanSplit(s.pool.QueryRow(ctx, `SELECT `+splitCols+` FROM assess_splits
WHERE chat_id=$1 AND train_dataset_id=$2`, chatID, trainDatasetID))
}

// SplitOfSourceDataset reads the split of a source dataset, newest first (ErrNotFound: none). A second
// split of the same source replaces nothing; the newest one is the one the chat trains on now.
func (s *Store) SplitOfSourceDataset(ctx context.Context, chatID string, sourceDatasetID int64) (Split, error) {
	if !isUUID(chatID) {
		return Split{}, ErrNotFound
	}
	return scanSplit(s.pool.QueryRow(ctx, `SELECT `+splitCols+` FROM assess_splits
WHERE chat_id=$1 AND source_dataset_id=$2 ORDER BY created_at DESC, train_dataset_id DESC LIMIT 1`, chatID, sourceDatasetID))
}

// ModelOrigin sources (table model_origins).
const (
	// OriginTraining: the model was created from a training container of this chat (R1.6).
	OriginTraining = "training"
	// OriginNotebook: the model was uploaded from a file of a notebook run of this chat (R2.6).
	OriginNotebook = "notebook"
)

// ModelOrigin is what the gateway saw about where a model came from (table model_origins, R1.6): the
// training it was created from and the datasets it could have seen. Nothing here comes from the agent.
type ModelOrigin struct {
	ModelID int64  `json:"model_id"`
	ChatID  string `json:"-"`
	// Source: training | notebook.
	Source string `json:"source"`
	// TrainContainerID: the training container the model was created from (0: not from a training).
	TrainContainerID int64 `json:"train_container_id,omitempty"`
	// SourceSHA: the SHA-256 of the file an uploaded model was made from (R2.6; empty for a training).
	// A check of that file keeps the subject sha:<hex>, so this is how the model finds its check again.
	SourceSHA string `json:"source_sha,omitempty"`
	// SeenDatasetIDs: the datasets the model could have seen while it was made (P2).
	SeenDatasetIDs []int64   `json:"seen_dataset_ids"`
	CreatedAt      time.Time `json:"created_at"`
}

// RecordModelOrigin stores where a model came from; the model id is the key, so a model seen twice keeps
// one row (the newest wins).
func (s *Store) RecordModelOrigin(ctx context.Context, o ModelOrigin) error {
	seen := o.SeenDatasetIDs
	if seen == nil {
		seen = []int64{}
	}
	_, err := s.pool.Exec(ctx, `
INSERT INTO model_origins (model_id, chat_id, source, train_container_id, source_sha, seen_dataset_ids)
VALUES ($1,$2,$3,$4,$5,$6)
ON CONFLICT (model_id) DO UPDATE SET
  chat_id = EXCLUDED.chat_id, source = EXCLUDED.source,
  train_container_id = EXCLUDED.train_container_id, source_sha = EXCLUDED.source_sha,
  seen_dataset_ids = EXCLUDED.seen_dataset_ids`,
		o.ModelID, o.ChatID, o.Source, nullableInt64(o.TrainContainerID), nullableText(o.SourceSHA), seen)
	return err
}

// ModelOriginOf reads where a model came from (ErrNotFound: the gateway did not see it being created in
// this chat; a check then answers not_measured, P2).
func (s *Store) ModelOriginOf(ctx context.Context, chatID string, modelID int64) (ModelOrigin, error) {
	if !isUUID(chatID) || modelID <= 0 {
		return ModelOrigin{}, ErrNotFound
	}
	var o ModelOrigin
	err := s.pool.QueryRow(ctx, `SELECT model_id, chat_id::text, source, COALESCE(train_container_id, 0),
COALESCE(source_sha, ''), seen_dataset_ids, created_at FROM model_origins WHERE chat_id=$1 AND model_id=$2`,
		chatID, modelID).
		Scan(&o.ModelID, &o.ChatID, &o.Source, &o.TrainContainerID, &o.SourceSHA, &o.SeenDatasetIDs, &o.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ModelOrigin{}, ErrNotFound
	}
	return o, err
}

// ModelOriginOfSHA answers the model a chat uploaded from a file with this SHA-256 (R2.6; ok false: no
// such model). The check card names the model behind a file check with it, and a check of a model looks
// its file's check up the other way round.
func (s *Store) ModelOriginOfSHA(ctx context.Context, chatID, sha256 string) (ModelOrigin, bool, error) {
	if !isUUID(chatID) || sha256 == "" {
		return ModelOrigin{}, false, nil
	}
	var o ModelOrigin
	err := s.pool.QueryRow(ctx, `SELECT model_id, chat_id::text, source, COALESCE(train_container_id, 0),
COALESCE(source_sha, ''), seen_dataset_ids, created_at FROM model_origins
WHERE chat_id=$1 AND source_sha=$2 ORDER BY created_at DESC, model_id DESC LIMIT 1`, chatID, sha256).
		Scan(&o.ModelID, &o.ChatID, &o.Source, &o.TrainContainerID, &o.SourceSHA, &o.SeenDatasetIDs, &o.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ModelOrigin{}, false, nil
	}
	if err != nil {
		return ModelOrigin{}, false, err
	}
	return o, true, nil
}

// LastFinishedCheck reads the newest finished check of one subject in a chat (R1.14, W4): the approval
// of an image build names its verdict. found is false when this chat has no finished check of the
// subject (only queued or running ones do not count).
func (s *Store) LastFinishedCheck(ctx context.Context, chatID, subject string) (Assessment, bool, error) {
	if !isUUID(chatID) {
		return Assessment{}, false, nil
	}
	var a Assessment
	err := s.pool.QueryRow(ctx, `SELECT `+assessmentCols+` FROM assessments
WHERE chat_id=$1 AND subject=$2 AND state=$3 ORDER BY created_at DESC, id DESC LIMIT 1`,
		chatID, subject, AssessFinished).
		Scan(&a.ID, &a.ChatID, &a.Kind, &a.Subject, &a.EvalDatasetID, &a.Final, &a.State, &a.Progress, &a.Report,
			&a.Error, &a.ToolCallID, &a.CreatedAt, &a.StartedAt, &a.EndedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Assessment{}, false, nil
	}
	if err != nil {
		return Assessment{}, false, err
	}
	return a, true, nil
}

// LastFinishedFinalCheck reads the newest finished check **with final=true** of one subject in a chat
// (W4): the line that names the check of a model or of a file rests on it, so that an interim check run
// afterwards does not hide the final one. found is false when this chat has no finished final check of
// the subject (an interim one is then read with LastFinishedCheck, to say "interim only").
func (s *Store) LastFinishedFinalCheck(ctx context.Context, chatID, subject string) (Assessment, bool, error) {
	if !isUUID(chatID) {
		return Assessment{}, false, nil
	}
	var a Assessment
	err := s.pool.QueryRow(ctx, `SELECT `+assessmentCols+` FROM assessments
WHERE chat_id=$1 AND subject=$2 AND state=$3 AND final ORDER BY created_at DESC, id DESC LIMIT 1`,
		chatID, subject, AssessFinished).
		Scan(&a.ID, &a.ChatID, &a.Kind, &a.Subject, &a.EvalDatasetID, &a.Final, &a.State, &a.Progress, &a.Report,
			&a.Error, &a.ToolCallID, &a.CreatedAt, &a.StartedAt, &a.EndedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Assessment{}, false, nil
	}
	if err != nil {
		return Assessment{}, false, err
	}
	return a, true, nil
}

// PreviousInterimCheck reads the newest finished interim check of an evaluation dataset in a chat that
// belongs to another model (R1.12): it is what a report compares a new round with.
func (s *Store) PreviousInterimCheck(ctx context.Context, chatID string, evalDatasetID int64, excludeSubject string) (Assessment, bool, error) {
	if !isUUID(chatID) || evalDatasetID <= 0 {
		return Assessment{}, false, nil
	}
	var a Assessment
	err := s.pool.QueryRow(ctx, `SELECT `+assessmentCols+` FROM assessments
WHERE chat_id=$1 AND eval_dataset_id=$2 AND final=false AND state=$3 AND subject<>$4
ORDER BY created_at DESC, id DESC LIMIT 1`, chatID, evalDatasetID, AssessFinished, excludeSubject).
		Scan(&a.ID, &a.ChatID, &a.Kind, &a.Subject, &a.EvalDatasetID, &a.Final, &a.State, &a.Progress, &a.Report,
			&a.Error, &a.ToolCallID, &a.CreatedAt, &a.StartedAt, &a.EndedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Assessment{}, false, nil
	}
	if err != nil {
		return Assessment{}, false, err
	}
	return a, true, nil
}

// nullableInt64 turns 0 into NULL, so that a column that means "none" stays empty.
func nullableInt64(n int64) any {
	if n <= 0 {
		return nil
	}
	return n
}

// nullableText turns an empty string into NULL, so that a column that means "none" stays empty.
func nullableText(s string) any {
	if s == "" {
		return nil
	}
	return s
}

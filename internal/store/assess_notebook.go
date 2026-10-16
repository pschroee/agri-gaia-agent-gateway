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

// Own models from the notebook server (R2) need two things the gateway alone writes: which files a
// notebook run of a chat handed over (notebook_fetches) and, on the model, which file it was uploaded
// from (model_origins.source_sha). Both are decided from bytes the gateway read itself; nothing of it
// comes from the agent (P2).

//go:embed assess_notebook_schema.sql
var assessNotebookSchemaSQL string

func init() { registerSchema(assessNotebookSchemaSQL) }

// NotebookFetch is one file the gateway pulled out of a notebook run (table notebook_fetches, R2.1):
// which run it came out of, the path inside that run and the SHA-256 of the bytes it read then.
type NotebookFetch struct {
	ChatID string `json:"-"`
	RunID  string `json:"run_id"`
	Path   string `json:"path"`
	// SHA256: the SHA-256 of the bytes the gateway read out of the run — the same bytes notebook_fetch
	// wrote into the sandbox, so a later upload of that file carries the same SHA (P2).
	SHA256    string    `json:"sha256"`
	Size      int64     `json:"size"`
	FetchedAt time.Time `json:"fetched_at"`
}

// RecordNotebookFetch remembers a fetched file. A file fetched twice from the same run keeps one row;
// the newest size wins.
func (s *Store) RecordNotebookFetch(ctx context.Context, f NotebookFetch) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO notebook_fetches (chat_id, run_id, path, sha256, size)
VALUES ($1,$2,$3,$4,$5)
ON CONFLICT (chat_id, sha256, run_id) DO UPDATE SET path = EXCLUDED.path, size = EXCLUDED.size`,
		f.ChatID, f.RunID, f.Path, f.SHA256, f.Size)
	return err
}

// NotebookFetchOf answers the newest fetch of one SHA-256 in a chat (ok false: this chat never fetched
// a file with that content). A check of a sandbox file asks this: without such a row the file has no
// origin the gateway saw, and the verdict is not_measured (R2.2, P2).
func (s *Store) NotebookFetchOf(ctx context.Context, chatID, sha256 string) (NotebookFetch, bool, error) {
	if !isUUID(chatID) || sha256 == "" {
		return NotebookFetch{}, false, nil
	}
	var f NotebookFetch
	err := s.pool.QueryRow(ctx, `SELECT chat_id::text, run_id, path, sha256, size, fetched_at
FROM notebook_fetches WHERE chat_id=$1 AND sha256=$2 ORDER BY fetched_at DESC LIMIT 1`, chatID, sha256).
		Scan(&f.ChatID, &f.RunID, &f.Path, &f.SHA256, &f.Size, &f.FetchedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return NotebookFetch{}, false, nil
	}
	if err != nil {
		return NotebookFetch{}, false, err
	}
	return f, true, nil
}

// NotebookSeenDatasetIDs names every dataset a notebook run of this chat ever received, whatever run it
// was and whether that run ended (R2.3): the code of a later run reads the datasets of an earlier one
// out of `/home/jovyan/work/datasets/<id>/`, so all of them count as seen.
func (s *Store) NotebookSeenDatasetIDs(ctx context.Context, chatID string) ([]int64, error) {
	if !isUUID(chatID) {
		return nil, nil
	}
	var ids []int64
	err := s.pool.QueryRow(ctx, `SELECT COALESCE(array_agg(DISTINCT d ORDER BY d), '{}') FROM (
    SELECT unnest(dataset_ids) AS d FROM notebook_runs WHERE chat_id=$1
) seen`, chatID).Scan(&ids)
	if err != nil {
		return nil, err
	}
	return ids, nil
}

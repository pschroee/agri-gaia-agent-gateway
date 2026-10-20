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

// The runs of a chat's notebook server (own code C-E3). The files of a run are the ones the user
// approved: name, size and SHA-256 as they were read from the sandbox before the approval.

//go:embed notebook_schema.sql
var notebookSchemaSQL string

func init() { registerSchema(notebookSchemaSQL) }

// NotebookFile is one file of a run as it was approved.
type NotebookFile struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// NotebookRun is one run of a chat's notebook server.
type NotebookRun struct {
	ID         string
	ChatID     string
	HubUser    string
	Server     string
	Entry      string
	Files      []NotebookFile
	DatasetIDs []int64
	TimeoutMin int
	StartedAt  time.Time
	FinishedAt *time.Time
	ExitCode   *int
}

// NotebookServer names a notebook server of a chat (the reaper stops the idle ones).
type NotebookServer struct {
	ChatID  string
	HubUser string
	Server  string
	// RunID is the chat's last run row, on which the reaper records the stop.
	RunID string
}

const notebookRunCols = `id, chat_id::text, hub_user, server, entry, files, dataset_ids, timeout_min, started_at, finished_at, exit_code`

// CreateNotebookRun stores a started run.
func (s *Store) CreateNotebookRun(ctx context.Context, r NotebookRun) error {
	files, err := json.Marshal(r.Files)
	if err != nil {
		return err
	}
	ids := r.DatasetIDs
	if ids == nil {
		ids = []int64{}
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO notebook_runs (id, chat_id, hub_user, server, entry, files, dataset_ids, timeout_min, started_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		r.ID, r.ChatID, r.HubUser, r.Server, r.Entry, files, ids, r.TimeoutMin, r.StartedAt)
	return err
}

// FinishNotebookRun records the end of a run (exit code and time). It never touches a run that is
// already finished: the runner and the watcher may both report the same end.
func (s *Store) FinishNotebookRun(ctx context.Context, runID string, exit int, at time.Time) error {
	_, err := s.pool.Exec(ctx, `UPDATE notebook_runs SET finished_at=$2, exit_code=$3 WHERE id=$1 AND finished_at IS NULL`,
		runID, at, exit)
	return err
}

// EndNotebookRun records the end of a run that has no exit code: its notebook server was stopped while it
// went, so nothing was left to write the exit code into the run directory (own code C-E4 — that is what the
// user's "Stop job" of a run does). exit_code stays NULL, the gateway invents no code, and a run that ended
// already is left alone. ok reports whether this end was recorded.
func (s *Store) EndNotebookRun(ctx context.Context, runID string, at time.Time) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE notebook_runs SET finished_at=$2 WHERE id=$1 AND finished_at IS NULL`, runID, at)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// NotebookRuns lists a chat's runs, newest first.
func (s *Store) NotebookRuns(ctx context.Context, chatID string) ([]NotebookRun, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+notebookRunCols+` FROM notebook_runs WHERE chat_id=$1 ORDER BY started_at DESC`, chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []NotebookRun{}
	for rows.Next() {
		r, err := scanNotebookRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetNotebookRun reads one run of a chat. A run of another chat is ErrNotFound.
func (s *Store) GetNotebookRun(ctx context.Context, chatID, runID string) (NotebookRun, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+notebookRunCols+` FROM notebook_runs WHERE chat_id=$1 AND id=$2`, chatID, runID)
	r, err := scanNotebookRun(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return NotebookRun{}, ErrNotFound
	}
	return r, err
}

// ActiveNotebookRun names the chat's newest run that has not ended yet (ok false: none). Only one
// run goes at a time in a chat, so this is the run that blocks the next one.
func (s *Store) ActiveNotebookRun(ctx context.Context, chatID string) (NotebookRun, bool, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+notebookRunCols+` FROM notebook_runs
WHERE chat_id=$1 AND finished_at IS NULL ORDER BY started_at DESC LIMIT 1`, chatID)
	r, err := scanNotebookRun(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return NotebookRun{}, false, nil
	}
	if err != nil {
		return NotebookRun{}, false, err
	}
	return r, true, nil
}

// IdleNotebookServers names the notebook servers of chats whose last run ended before the given time
// and which have no run going: the servers the reaper stops. Servers whose last run already carries
// a recorded stop are left out. The reaper does not change the runs (the runner or the watcher
// records an end); it only records its stop.
func (s *Store) IdleNotebookServers(ctx context.Context, before time.Time) ([]NotebookServer, error) {
	rows, err := s.pool.Query(ctx, `SELECT last.chat_id::text, last.hub_user, last.server, last.id
FROM (
    SELECT DISTINCT ON (chat_id) id, chat_id, hub_user, server, finished_at, server_stopped_at
    FROM notebook_runs ORDER BY chat_id, started_at DESC
) last
WHERE last.finished_at IS NOT NULL AND last.finished_at < $1 AND last.server_stopped_at IS NULL
  AND NOT EXISTS (SELECT 1 FROM notebook_runs a WHERE a.chat_id = last.chat_id AND a.finished_at IS NULL)`, before)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []NotebookServer{}
	for rows.Next() {
		var n NotebookServer
		if err := rows.Scan(&n.ChatID, &n.HubUser, &n.Server, &n.RunID); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// NotebookServerIdle reports whether the chat's server is still a candidate for the reaper: the
// chat's last run is runID, it ended before the given time, no stop is recorded on it and the chat
// has no run going. The reaper asks again under the chat's lock.
func (s *Store) NotebookServerIdle(ctx context.Context, chatID, runID string, before time.Time) (bool, error) {
	var idle bool
	err := s.pool.QueryRow(ctx, `SELECT COALESCE((
    SELECT last.id = $2 AND last.finished_at IS NOT NULL AND last.finished_at < $3 AND last.server_stopped_at IS NULL
    FROM (SELECT id, finished_at, server_stopped_at FROM notebook_runs WHERE chat_id=$1 ORDER BY started_at DESC LIMIT 1) last
  ), false)
  AND NOT EXISTS (SELECT 1 FROM notebook_runs a WHERE a.chat_id=$1 AND a.finished_at IS NULL)`, chatID, runID, before).Scan(&idle)
	return idle, err
}

// MarkNotebookServerStopped records that the reaper stopped the server after the given run (or found
// it stopped). A later run is a new row, so it is not affected.
func (s *Store) MarkNotebookServerStopped(ctx context.Context, runID string, at time.Time) error {
	_, err := s.pool.Exec(ctx, `UPDATE notebook_runs SET server_stopped_at=$2 WHERE id=$1 AND server_stopped_at IS NULL`, runID, at)
	return err
}

// scanNotebookRun reads a row of notebook_runs (a single row or one of a result set).
func scanNotebookRun(row interface{ Scan(...any) error }) (NotebookRun, error) {
	var r NotebookRun
	var files []byte
	if err := row.Scan(&r.ID, &r.ChatID, &r.HubUser, &r.Server, &r.Entry, &files, &r.DatasetIDs, &r.TimeoutMin,
		&r.StartedAt, &r.FinishedAt, &r.ExitCode); err != nil {
		return NotebookRun{}, err
	}
	if len(files) > 0 {
		if err := json.Unmarshal(files, &r.Files); err != nil {
			return NotebookRun{}, err
		}
	}
	if r.DatasetIDs == nil {
		r.DatasetIDs = []int64{}
	}
	return r, nil
}

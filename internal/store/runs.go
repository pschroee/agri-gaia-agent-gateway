// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"strings"
	"time"
)

// RunScopeLimit is the most rows each of the three run queries returns (the newest ones). The queries ask for one
// more, so that the caller sees that the scope was cut.
var RunScopeLimit = 5000

// RunScope limits the run queries of the experiments overview (issue #222): the chats whose runs count and the
// period. A run is in the period when it overlaps it: it started before Until and did not end before Since (a run
// that has not ended is still in).
type RunScope struct {
	ChatIDs      []string
	Since, Until time.Time
}

// ScopedTrial is a single run (a trial outside a search) with the entry file of its notebook run, if it has one.
type ScopedTrial struct {
	Trial Trial
	Entry string
}

// NotebookJob is an observed notebook run with the entry file of the run it observes.
type NotebookJob struct {
	Job   WatchJob
	Entry string
}

// overlap adds the period conditions for a run whose start and end are the SQL expressions start and end.
func (sc RunScope) overlap(start, end string, args *[]any) string {
	var w []string
	if !sc.Until.IsZero() {
		*args = append(*args, sc.Until)
		w = append(w, start+" <= $"+itoa(len(*args)))
	}
	if !sc.Since.IsZero() {
		*args = append(*args, sc.Since)
		w = append(w, "("+end+" IS NULL OR "+end+" >= $"+itoa(len(*args))+")")
	}
	if len(w) == 0 {
		return ""
	}
	return " AND " + strings.Join(w, " AND ")
}

func itoa(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	return itoa(n/10) + string(rune('0'+n%10))
}

// ScopeSearches returns the searches of the chats in the scope that are or were a run (not rejected or expired),
// newest first.
func (s *Store) ScopeSearches(ctx context.Context, sc RunScope) ([]Search, error) {
	out := []Search{}
	if len(sc.ChatIDs) == 0 {
		return out, nil
	}
	args := []any{sc.ChatIDs}
	q := `SELECT ` + searchCols + ` FROM searches WHERE chat_id::text = ANY($1) AND state IN ('proposed','running','ended')` +
		sc.overlap("COALESCE(started_at, created_at)", "ended_at", &args) +
		` ORDER BY COALESCE(started_at, created_at) DESC, id LIMIT ` + itoa(RunScopeLimit+1)
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		x, err := scanSearch(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// ScopeSingleTrials returns the trials outside a search (single runs) of the chats in the scope, except refused
// ones, newest first.
func (s *Store) ScopeSingleTrials(ctx context.Context, sc RunScope) ([]ScopedTrial, error) {
	out := []ScopedTrial{}
	if len(sc.ChatIDs) == 0 {
		return out, nil
	}
	args := []any{sc.ChatIDs}
	q := `SELECT ` + trialCols + `,
  COALESCE((SELECT r.entry FROM notebook_runs r WHERE t.path = 'notebook' AND r.id = t.ref AND r.chat_id = t.chat_id), '')
FROM experiment_trials t
WHERE t.chat_id::text = ANY($1) AND t.search_id IS NULL AND t.state <> 'refused'` +
		sc.overlap("COALESCE(t.started_at, t.declared_at)", "t.ended_at", &args) +
		` ORDER BY COALESCE(t.started_at, t.declared_at) DESC, t.id LIMIT ` + itoa(RunScopeLimit+1)
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var entry string
		t, err := scanTrial(entryScanner{rows, &entry})
		if err != nil {
			return nil, err
		}
		out = append(out, ScopedTrial{Trial: t, Entry: entry})
	}
	return out, rows.Err()
}

// ScopeNotebookJobs returns the observed notebook runs of the chats in the scope that have no trial (a run
// without figures never becomes one), newest first.
func (s *Store) ScopeNotebookJobs(ctx context.Context, sc RunScope) ([]NotebookJob, error) {
	out := []NotebookJob{}
	if len(sc.ChatIDs) == 0 {
		return out, nil
	}
	args := []any{sc.ChatIDs}
	q := `SELECT ` + watchJobCols + `,
  COALESCE((SELECT r.entry FROM notebook_runs r WHERE r.id = w.ref AND r.chat_id = w.chat_id), '')
FROM watch_jobs w
WHERE w.chat_id::text = ANY($1) AND w.kind = 'notebook_run'
  AND NOT EXISTS (SELECT 1 FROM experiment_trials t WHERE t.job_id = w.id)` +
		sc.overlap("COALESCE(w.run_started_at, w.started_at)", "COALESCE(w.run_ended_at, w.ended_at)", &args) +
		` ORDER BY COALESCE(w.run_started_at, w.started_at) DESC, w.id LIMIT ` + itoa(RunScopeLimit+1)
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var nj NotebookJob
		var entry string
		j, err := scanWatchJob(entryScanner{rows, &entry})
		if err != nil {
			return nil, err
		}
		nj.Job, nj.Entry = j, entry
		out = append(out, nj)
	}
	return out, rows.Err()
}

// entryScanner scans the columns of a row plus one more text column into extra, after the ones the scan function wants.
type entryScanner struct {
	row   interface{ Scan(...any) error }
	extra *string
}

func (e entryScanner) Scan(dest ...any) error { return e.row.Scan(append(dest, e.extra)...) }

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
	"github.com/jackc/pgx/v5/pgconn"
)

//go:embed exp_schema.sql
var expSchemaSQL string

func init() { registerSchema(expSchemaSQL) }

// States of an experiment run (X1.5, X1.8). Status holds the end reason of a finished run; a run
// that is still going is ExpStarted.
const (
	ExpStarted       = "started"
	ExpDone          = "done"
	ExpLimitCost     = "limit_cost"
	ExpLimitTokens   = "limit_tokens"
	ExpLimitDuration = "limit_duration"
	ExpError         = "error"
	ExpAborted       = "aborted"
)

// ErrExpRunExists: an attempt with this id is already recorded.
var ErrExpRunExists = errors.New("experiment run exists")

// ExpRun is one attempt of one run of the experiment plan. The logical run is Key; a retry after a
// crash takes a new ID (key + "#<attempt>") and the old row stays with SupersededBy.
type ExpRun struct {
	ID             string          `json:"id"`
	Key            string          `json:"key"`
	Plan           string          `json:"plan"`
	PlanSHA256     string          `json:"plan_sha256,omitempty"`
	Cell           string          `json:"cell"`
	Task           string          `json:"task"`
	Rep            int             `json:"rep"`
	Seed           int64           `json:"seed"`
	Position       int             `json:"position"`
	ChatID         string          `json:"chat_id,omitempty"`
	Status         string          `json:"status"`
	EndReason      string          `json:"end_reason,omitempty"`
	Pass           *bool           `json:"pass,omitempty"`
	CheckEvidence  json.RawMessage `json:"check_evidence,omitempty"`
	StartedAt      time.Time       `json:"started_at"`
	EndedAt        *time.Time      `json:"ended_at,omitempty"`
	SupersededBy   string          `json:"superseded_by,omitempty"`
	GatewayVersion string          `json:"gateway_version,omitempty"`
	Model          string          `json:"model,omitempty"`
	// X8.2: the images the run's slot actually used and the checksums of the environment. They are
	// filled when the chat is linked, not by the client.
	ImagePi       string `json:"image_pi,omitempty"`
	ImageExec     string `json:"image_exec,omitempty"`
	CatalogSHA256 string `json:"catalog_sha256,omitempty"`
	SkillsSHA256  string `json:"skills_sha256,omitempty"`
}

// ExpRunEnvironment is what the gateway records about the environment of a run when its chat is linked
// (X8.2).
type ExpRunEnvironment struct {
	ImagePi       string
	ImageExec     string
	CatalogSHA256 string
	SkillsSHA256  string
}

// ExpRunEnd is what EndExpRun records.
type ExpRunEnd struct {
	Status        string
	EndReason     string
	Pass          *bool
	CheckEvidence json.RawMessage
}

// StartExpRun records a new attempt.
func (s *Store) StartExpRun(ctx context.Context, r ExpRun) error {
	if r.ID == "" || r.Key == "" {
		return errors.New("experiment run needs an id and a key")
	}
	status := r.Status
	if status == "" {
		status = ExpStarted
	}
	ct, err := s.pool.Exec(ctx, `INSERT INTO exp_runs
		(id, run_key, plan, plan_sha256, cell, task, rep, seed, position, chat_id, status, gateway_version, model)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,NULLIF($10,'')::uuid,$11,$12,$13)`,
		r.ID, r.Key, r.Plan, r.PlanSHA256, r.Cell, r.Task, r.Rep, r.Seed, r.Position, r.ChatID, status, r.GatewayVersion, r.Model)
	if err != nil {
		if pgErrUnique(err) {
			return ErrExpRunExists
		}
		return err
	}
	if ct.RowsAffected() == 0 {
		return ErrExpRunExists
	}
	return nil
}

// SetExpRunChat records the chat that carries the run. The row is written before the chat exists, so
// that a crash leaves a visible "started" attempt (X1.5); the chat id follows at once.
func (s *Store) SetExpRunChat(ctx context.Context, id, chatID string, env ExpRunEnvironment) error {
	ct, err := s.pool.Exec(ctx, `UPDATE exp_runs SET chat_id=$2::uuid, image_pi=$3, image_exec=$4,
		catalog_sha256=$5, skills_sha256=$6 WHERE id=$1`,
		id, chatID, env.ImagePi, env.ImageExec, env.CatalogSHA256, env.SkillsSHA256)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// EndExpRun records the outcome of a run. It is also the way an aborted run is closed; ending a run
// twice overwrites the first ending, which keeps a retried request harmless.
func (s *Store) EndExpRun(ctx context.Context, id string, end ExpRunEnd) error {
	if end.Status == "" {
		return errors.New("experiment run end without status")
	}
	var evidence []byte
	if len(end.CheckEvidence) > 0 {
		evidence = end.CheckEvidence
	}
	ct, err := s.pool.Exec(ctx, `UPDATE exp_runs SET status=$2, end_reason=$3, pass=$4,
		check_evidence=$5::jsonb, ended_at=now() WHERE id=$1`, id, end.Status, end.EndReason, end.Pass, evidence)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SupersedeExpRun marks an attempt that was replaced by a new one (X1.5). It stays visible with its
// chat id and its partial data; only the link to the new attempt is added.
func (s *Store) SupersedeExpRun(ctx context.Context, id, by string) error {
	if by == "" {
		return errors.New("supersede without the new attempt")
	}
	ct, err := s.pool.Exec(ctx, `UPDATE exp_runs SET status=$3,
		end_reason=CASE WHEN end_reason='' THEN 'superseded by a new attempt' ELSE end_reason END,
		superseded_by=$2, ended_at=COALESCE(ended_at, now()) WHERE id=$1`, id, by, ExpAborted)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ListExpRuns returns the runs of a plan, optionally of one cell, in the runner's order.
func (s *Store) ListExpRuns(ctx context.Context, plan, cell string) ([]ExpRun, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, run_key, plan, plan_sha256, cell, task, rep, seed, position,
		COALESCE(chat_id::text, ''), status, end_reason, pass, check_evidence, started_at, ended_at,
		superseded_by, gateway_version, model, image_pi, image_exec, catalog_sha256, skills_sha256
		FROM exp_runs WHERE plan=$1 AND ($2='' OR cell=$2) ORDER BY position, started_at`, plan, cell)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ExpRun{}
	for rows.Next() {
		r, err := scanExpRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ExpUsage is what a plan has consumed so far (#161). Tokens are input + output of the plan's chats
// without cache reads, as in X3.3; llm_calls is the authoritative source, subagent runs included.
// Duration is the wall-clock time of every attempt, a superseded one with the time it used and a
// running one up to now.
type ExpUsage struct {
	TokensTotal   int64  `json:"tokens_total"`
	DurationTotal string `json:"duration_total"`
	Runs          int    `json:"runs"`
}

// ExpUsage sums tokens and duration of every attempt of a plan.
func (s *Store) ExpUsage(ctx context.Context, plan string) (ExpUsage, error) {
	var (
		u       ExpUsage
		seconds int64
	)
	err := s.pool.QueryRow(ctx, `
		SELECT
		  COALESCE((SELECT SUM(x.input + x.output) FROM llm_calls x
		            WHERE x.chat_id IN (SELECT r.chat_id FROM exp_runs r WHERE r.plan=$1 AND r.chat_id IS NOT NULL)), 0),
		  COALESCE(SUM(EXTRACT(EPOCH FROM (COALESCE(ended_at, now()) - started_at)))::bigint, 0),
		  COUNT(*)
		FROM exp_runs WHERE plan=$1`, plan).Scan(&u.TokensTotal, &seconds, &u.Runs)
	if err != nil {
		return ExpUsage{}, err
	}
	u.DurationTotal = (time.Duration(seconds) * time.Second).String()
	return u, nil
}

// GetExpRun returns one attempt.
func (s *Store) GetExpRun(ctx context.Context, id string) (ExpRun, error) {
	row := s.pool.QueryRow(ctx, `SELECT id, run_key, plan, plan_sha256, cell, task, rep, seed, position,
		COALESCE(chat_id::text, ''), status, end_reason, pass, check_evidence, started_at, ended_at,
		superseded_by, gateway_version, model, image_pi, image_exec, catalog_sha256, skills_sha256
		FROM exp_runs WHERE id=$1`, id)
	r, err := scanExpRun(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return ExpRun{}, ErrNotFound
	}
	return r, err
}

// scanRow is what QueryRow and Rows have in common.
type scanRow interface {
	Scan(dest ...any) error
}

func scanExpRun(row scanRow) (ExpRun, error) {
	var (
		r     ExpRun
		ev    []byte
		ended *time.Time
		pass  *bool
	)
	if err := row.Scan(&r.ID, &r.Key, &r.Plan, &r.PlanSHA256, &r.Cell, &r.Task, &r.Rep, &r.Seed,
		&r.Position, &r.ChatID, &r.Status, &r.EndReason, &pass, &ev, &r.StartedAt, &ended,
		&r.SupersededBy, &r.GatewayVersion, &r.Model, &r.ImagePi, &r.ImageExec,
		&r.CatalogSHA256, &r.SkillsSHA256); err != nil {
		return ExpRun{}, err
	}
	r.Pass, r.EndedAt = pass, ended
	if len(ev) > 0 {
		r.CheckEvidence = json.RawMessage(ev)
	}
	return r, nil
}

// pgErrUnique reports a unique violation of Postgres.
func pgErrUnique(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23505"
}

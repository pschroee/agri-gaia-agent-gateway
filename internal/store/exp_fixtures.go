// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	_ "embed"
	"encoding/json"
	"time"
)

//go:embed exp_fixtures_schema.sql
var expFixturesSchemaSQL string

// init registers the table of the experiment fixtures (X4) through the seam of G1.6.
func init() { registerSchema(expFixturesSchemaSQL) }

// Kinds of an experiment fixture (X4.2). dataset_file is a file below a fixture dataset; it has no
// platform object of its own and is removed with its dataset.
const (
	FixtureDataset     = "dataset"
	FixtureModel       = "model"
	FixtureDatasetFile = "dataset_file"
)

// Owners of a fixture: primary is the experiment account itself, secondary a second account whose
// objects do not belong to the user of the run (X4.3).
const (
	FixturePrimary   = "primary"
	FixtureSecondary = "secondary"
)

// Fixture is one platform object seeded for a cell (table exp_fixtures). FieldSHA256 holds the
// intended SHA-256 per field name; contents are never stored here (X4.7).
type Fixture struct {
	ID           string            `json:"id"`
	Plan         string            `json:"plan"`
	Cell         string            `json:"cell"`
	ManifestID   string            `json:"manifest_id"`
	Kind         string            `json:"kind"`
	Owner        string            `json:"owner"`
	ObjectID     string            `json:"object_id"`
	FieldSHA256  map[string]string `json:"field_sha256"`
	CreatedAt    time.Time         `json:"created_at"`
	RemovedAt    *time.Time        `json:"removed_at,omitempty"`
	RemoveResult string            `json:"remove_result,omitempty"`
	RunKey       string            `json:"run_key,omitempty"`
}

// NewFixture is a fixture to record before its object is used.
type NewFixture struct {
	Plan        string
	Cell        string
	ManifestID  string
	Kind        string
	Owner       string
	ObjectID    string
	FieldSHA256 map[string]string
	RunKey      string
}

const fixtureCols = `id::text, plan, cell, manifest_id, kind, owner, object_id, field_sha256, created_at, removed_at, remove_result, run_key`

// AddFixture records a seeded object. It is written before the object is verified, so that a
// mismatch or a crash still leaves the identifier for a removal by id.
func (s *Store) AddFixture(ctx context.Context, f NewFixture) (Fixture, error) {
	hashes, err := json.Marshal(f.FieldSHA256)
	if err != nil {
		return Fixture{}, err
	}
	const q = `INSERT INTO exp_fixtures (plan, cell, manifest_id, kind, owner, object_id, field_sha256, run_key)
VALUES ($1,$2,$3,$4,$5,$6,$7::jsonb,$8) RETURNING ` + fixtureCols
	return scanFixture(s.pool.QueryRow(ctx, q, f.Plan, f.Cell, f.ManifestID, f.Kind, f.Owner, f.ObjectID, string(hashes), f.RunKey))
}

// ListFixtures returns the fixtures of a cell in creation order. Removed fixtures are included; a
// caller that only wants the live ones checks RemovedAt.
func (s *Store) ListFixtures(ctx context.Context, plan, cell string) ([]Fixture, error) {
	const q = `SELECT ` + fixtureCols + ` FROM exp_fixtures WHERE plan=$1 AND cell=$2 ORDER BY created_at, id`
	rows, err := s.pool.Query(ctx, q, plan, cell)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Fixture
	for rows.Next() {
		f, err := scanFixture(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// MarkFixtureRemoved records the removal of a seeded object. result is short and carries no content
// of the experiment material (X4.7).
func (s *Store) MarkFixtureRemoved(ctx context.Context, id, result string) error {
	const q = `UPDATE exp_fixtures SET removed_at=now(), remove_result=$2 WHERE id=$1::uuid AND removed_at IS NULL`
	tag, err := s.pool.Exec(ctx, q, id, result)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// scanFixture reads one row of exp_fixtures. It accepts both a pgx.Row and pgx.Rows.
func scanFixture(row interface {
	Scan(dest ...any) error
}) (Fixture, error) {
	var (
		f      Fixture
		hashes []byte
	)
	if err := row.Scan(&f.ID, &f.Plan, &f.Cell, &f.ManifestID, &f.Kind, &f.Owner, &f.ObjectID,
		&hashes, &f.CreatedAt, &f.RemovedAt, &f.RemoveResult, &f.RunKey); err != nil {
		return Fixture{}, err
	}
	if len(hashes) > 0 {
		if err := json.Unmarshal(hashes, &f.FieldSHA256); err != nil {
			return Fixture{}, err
		}
	}
	if f.FieldSHA256 == nil {
		f.FieldSHA256 = map[string]string{}
	}
	return f, nil
}

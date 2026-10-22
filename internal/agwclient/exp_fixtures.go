// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package agwclient

import (
	"context"
	"time"

	"agw/internal/exp"
)

// ExpFixture is one recorded fixture as the API returns it. FieldSHA256 carries hashes only, never
// contents (X4.7).
type ExpFixture struct {
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

// ExpFixtureAnswer is the answer of seed, unseed and verify.
type ExpFixtureAnswer struct {
	Fixtures   []ExpFixture  `json:"fixtures"`
	Removed    int           `json:"removed"`
	Checked    int           `json:"checked"`
	State      string        `json:"state"`
	Error      string        `json:"error"`
	Mismatches []ExpMismatch `json:"mismatches"`
}

// ExpMismatch is one finding of a fixture_mismatch. It carries hashes, not values (X4.7).
type ExpMismatch struct {
	ManifestID string             `json:"manifest_id"`
	Kind       string             `json:"kind"`
	ObjectID   string             `json:"object_id"`
	Missing    bool               `json:"missing"`
	Fields     []ExpFieldMismatch `json:"fields"`
}

type ExpFieldMismatch struct {
	Field    string `json:"field"`
	Expected string `json:"expected"`
	Actual   string `json:"actual"`
}

// ExpSeed creates the fixtures of a cell for one run (run may be empty for a hand-made seed). A
// fixture_mismatch answers 409; the answer then carries the findings and the created rows, and the
// caller removes them with ExpUnseed.
func (c *Client) ExpSeed(ctx context.Context, plan, cell, run string, specs []exp.FixtureSpec) (ExpFixtureAnswer, int, error) {
	in := map[string]any{"plan": plan, "cell": cell, "fixtures": specs}
	if run != "" {
		in["run"] = run
	}
	var out ExpFixtureAnswer
	status, err := c.DoStatus(ctx, "POST", "/api/exp/fixtures/seed", in, &out, 409)
	return out, status, err
}

// ExpUnseed removes the live fixtures of a cell by the recorded identifiers.
func (c *Client) ExpUnseed(ctx context.Context, plan, cell string) (ExpFixtureAnswer, error) {
	var out ExpFixtureAnswer
	_, err := c.DoStatus(ctx, "POST", "/api/exp/fixtures/unseed", map[string]any{"plan": plan, "cell": cell}, &out)
	return out, err
}

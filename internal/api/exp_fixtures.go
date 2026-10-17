// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"agw/internal/exp"
	"agw/internal/platform"
	"agw/internal/store"
)

// ExpFixtures is the fixture service of the experiment infrastructure (X4, issue #129),
// implemented by *exp.Fixtures.
type ExpFixtures interface {
	Seed(ctx context.Context, plan, cell string, specs []exp.FixtureSpec) ([]store.Fixture, error)
	Unseed(ctx context.Context, plan, cell string) (exp.UnseedResult, error)
	Verify(ctx context.Context, plan, cell string) (exp.VerifyResult, error)
	List(ctx context.Context, plan, cell string) ([]store.Fixture, error)
}

// maxFixtureBody bounds the seed request. The files travel inline (base64), so the decoded material
// may be at most platform.MaxUploadBytes; the rest is the encoding overhead and the manifests.
const maxFixtureBody = int64(platform.MaxUploadBytes)*4/3 + 1<<20

// fixtureRequest is the body of seed and unseed: the plan and the cell, and for seed the manifests
// with their content already resolved.
type fixtureRequest struct {
	Plan     string            `json:"plan"`
	Cell     string            `json:"cell"`
	Fixtures []exp.FixtureSpec `json:"fixtures,omitempty"`
}

// expSeed creates the fixture objects of a cell (POST /api/exp/fixtures/seed). Only the experiment
// key reaches this route; the call never goes through a chat and is not an agent call (X4.3).
func (s *Server) expSeed(w http.ResponseWriter, r *http.Request) {
	req, ok := s.expFixtureRequest(w, r)
	if !ok {
		return
	}
	if s.ExpFixtures == nil {
		writeErr(w, http.StatusServiceUnavailable, "experiment fixtures are not configured on this instance")
		return
	}
	rows, err := s.ExpFixtures.Seed(r.Context(), req.Plan, req.Cell, req.Fixtures)
	var mismatch *exp.MismatchError
	if errors.As(err, &mismatch) {
		// fixture_mismatch: the cell does not start (X4.4); the recorded identifiers let the caller
		// remove what was created before trying again.
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "fixture_mismatch", "mismatches": mismatch.Items, "fixtures": rows,
		})
		return
	}
	if err != nil {
		slog.Warn("experiment fixtures: seed failed", "plan", req.Plan, "cell", req.Cell, "error", err)
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"fixtures": rows})
}

// expUnseed removes the live fixtures of a cell by the recorded identifiers
// (POST /api/exp/fixtures/unseed). A failure on one object does not stop the others.
func (s *Server) expUnseed(w http.ResponseWriter, r *http.Request) {
	req, ok := s.expFixtureRequest(w, r)
	if !ok {
		return
	}
	if s.ExpFixtures == nil {
		writeErr(w, http.StatusServiceUnavailable, "experiment fixtures are not configured on this instance")
		return
	}
	result, err := s.ExpFixtures.Unseed(r.Context(), req.Plan, req.Cell)
	if err != nil {
		slog.Warn("experiment fixtures: unseed incomplete", "plan", req.Plan, "cell", req.Cell, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": err.Error(), "removed": result.Removed, "fixtures": result.Fixtures,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"removed": result.Removed, "fixtures": result.Fixtures})
}

// expVerify compares the live fixtures with the checksums of the seeding (POST /api/exp/fixtures/verify).
// state is "empty" (no live fixture, not a passed check, issue #149), "ok" or "mismatch"; a mismatch
// answers 409 with the findings.
func (s *Server) expVerify(w http.ResponseWriter, r *http.Request) {
	req, ok := s.expFixtureRequest(w, r)
	if !ok {
		return
	}
	if s.ExpFixtures == nil {
		writeErr(w, http.StatusServiceUnavailable, "experiment fixtures are not configured on this instance")
		return
	}
	result, err := s.ExpFixtures.Verify(r.Context(), req.Plan, req.Cell)
	if err != nil {
		fail(w, err)
		return
	}
	state := "ok"
	switch {
	case len(result.Mismatches) > 0:
		state = "mismatch"
	case result.Checked == 0:
		state = "empty"
	}
	body := map[string]any{"state": state, "checked": result.Checked, "mismatches": result.Mismatches}
	if state == "mismatch" {
		body["error"] = "fixture_mismatch"
		writeJSON(w, http.StatusConflict, body)
		return
	}
	writeJSON(w, http.StatusOK, body)
}

// expList returns the recorded fixtures of a cell, removed ones included.
func (s *Server) expList(w http.ResponseWriter, r *http.Request) {
	if s.ExpFixtures == nil {
		writeErr(w, http.StatusServiceUnavailable, "experiment fixtures are not configured on this instance")
		return
	}
	plan, cell := r.URL.Query().Get("plan"), r.URL.Query().Get("cell")
	if strings.TrimSpace(plan) == "" || strings.TrimSpace(cell) == "" {
		writeErr(w, http.StatusBadRequest, "plan and cell are required")
		return
	}
	rows, err := s.ExpFixtures.List(r.Context(), plan, cell)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

func (s *Server) expFixtureRequest(w http.ResponseWriter, r *http.Request) (fixtureRequest, bool) {
	var req fixtureRequest
	body := http.MaxBytesReader(w, r.Body, maxFixtureBody)
	if err := json.NewDecoder(body).Decode(&req); err != nil {
		if errors.Is(err, io.EOF) {
			writeErr(w, http.StatusBadRequest, "invalid JSON: empty body")
			return req, false
		}
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return req, false
	}
	if strings.TrimSpace(req.Plan) == "" || strings.TrimSpace(req.Cell) == "" {
		writeErr(w, http.StatusBadRequest, "plan and cell are required")
		return req, false
	}
	return req, true
}

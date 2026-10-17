// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agw/internal/exp"
	"agw/internal/store"
)

// fakeExpFixtures answers the fixture routes without a platform or a database.
type fakeExpFixtures struct {
	rows     []store.Fixture
	mismatch []exp.Mismatch
	checked  int
	removed  int
	err      error
	plan     string
	cell     string
	specs    []exp.FixtureSpec
}

func (f *fakeExpFixtures) Seed(_ context.Context, plan, cell string, specs []exp.FixtureSpec) ([]store.Fixture, error) {
	f.plan, f.cell, f.specs = plan, cell, specs
	if f.err != nil {
		return f.rows, f.err
	}
	return f.rows, nil
}

func (f *fakeExpFixtures) Unseed(_ context.Context, plan, cell string) (exp.UnseedResult, error) {
	f.plan, f.cell = plan, cell
	return exp.UnseedResult{Removed: f.removed, Fixtures: f.rows}, f.err
}

func (f *fakeExpFixtures) Verify(_ context.Context, plan, cell string) (exp.VerifyResult, error) {
	f.plan, f.cell = plan, cell
	return exp.VerifyResult{Checked: f.checked, Mismatches: f.mismatch}, f.err
}

func (f *fakeExpFixtures) List(_ context.Context, plan, cell string) ([]store.Fixture, error) {
	f.plan, f.cell = plan, cell
	return f.rows, f.err
}

const (
	uiToken  = "0123456789abcdef0123456789abcdef"
	expToken = "exp0123456789abcdef0123456789abcdef"
)

func expServer(svc ExpFixtures) http.Handler {
	return (&Server{
		Token: uiToken, ExpAPIToken: expToken, ExpFixtures: svc,
		AllowedHosts: []string{"127.0.0.1:18480", "localhost:18480"},
	}).Handler()
}

func doExp(t *testing.T, h http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, "http://127.0.0.1:18480"+path, nil)
	} else {
		r = httptest.NewRequest(method, "http://127.0.0.1:18480"+path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// The experiment routes take only the experiment key; the UI's token and a missing key are refused
// (X1.3/X4).
func TestExpRoutesUseTheirOwnKey(t *testing.T) {
	h := expServer(&fakeExpFixtures{})
	if w := doExp(t, h, "GET", "/api/exp/fixtures?plan=pilot&cell=c1", "", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("without token: %d", w.Code)
	}
	if w := doExp(t, h, "GET", "/api/exp/fixtures?plan=pilot&cell=c1", uiToken, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("UI token: %d", w.Code)
	}
	if w := doExp(t, h, "GET", "/api/exp/fixtures?plan=pilot&cell=c1", expToken, ""); w.Code != http.StatusOK {
		t.Fatalf("experiment token: %d %s", w.Code, w.Body.String())
	}
	// The experiment key does not open the rest of the API.
	if w := doExp(t, h, "GET", "/api/chats", expToken, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("experiment token on /api/chats: %d", w.Code)
	}
}

func TestExpSeedReturnsIdentifiersAndHashes(t *testing.T) {
	svc := &fakeExpFixtures{rows: []store.Fixture{{
		ID: "row-1", Plan: "pilot", Cell: "c1", ManifestID: "dataset-1", Kind: store.FixtureDataset,
		Owner: store.FixturePrimary, ObjectID: "7", FieldSHA256: map[string]string{"name": "abc"},
	}}}
	h := expServer(svc)
	body := `{"plan":"pilot","cell":"c1","fixtures":[{"id":"dataset-1","kind":"dataset","owner":"primary",` +
		`"fields":{"name":"placeholder-dataset","description":"placeholder-1"}}]}`
	w := doExp(t, h, "POST", "/api/exp/fixtures/seed", expToken, body)
	if w.Code != http.StatusCreated {
		t.Fatalf("seed: %d %s", w.Code, w.Body.String())
	}
	if svc.plan != "pilot" || svc.cell != "c1" || len(svc.specs) != 1 || svc.specs[0].ID != "dataset-1" {
		t.Fatalf("service input: %+v", svc)
	}
	var out struct {
		Fixtures []store.Fixture `json:"fixtures"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Fixtures) != 1 || out.Fixtures[0].ObjectID != "7" || out.Fixtures[0].FieldSHA256["name"] != "abc" {
		t.Fatalf("answer: %+v", out)
	}
}

func TestExpSeedMismatchIs409(t *testing.T) {
	svc := &fakeExpFixtures{
		rows: []store.Fixture{{ID: "row-1", ManifestID: "dataset-1", ObjectID: "7", Kind: store.FixtureDataset}},
		mismatch: []exp.Mismatch{{ManifestID: "dataset-1", Kind: store.FixtureDataset, ObjectID: "7",
			Fields: []exp.FieldMismatch{{Field: "name", Expected: "abc", Actual: "def"}}}},
		err: &exp.MismatchError{Items: []exp.Mismatch{{ManifestID: "dataset-1", Kind: store.FixtureDataset,
			ObjectID: "7", Fields: []exp.FieldMismatch{{Field: "name", Expected: "abc", Actual: "def"}}}}},
	}
	h := expServer(svc)
	w := doExp(t, h, "POST", "/api/exp/fixtures/seed", expToken, `{"plan":"pilot","cell":"c1","fixtures":[]}`)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "fixture_mismatch") {
		t.Fatalf("mismatch: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "placeholder") {
		t.Fatalf("answer carries material: %s", w.Body.String())
	}
	// The identifiers of what was created go along, so that the caller can remove it.
	if !strings.Contains(w.Body.String(), `"object_id":"7"`) {
		t.Fatalf("identifiers missing: %s", w.Body.String())
	}
}

func TestExpVerifyMismatchIs409(t *testing.T) {
	svc := &fakeExpFixtures{checked: 1, mismatch: []exp.Mismatch{{ManifestID: "dataset-1", Missing: true}}}
	h := expServer(svc)
	w := doExp(t, h, "POST", "/api/exp/fixtures/verify", expToken, `{"plan":"pilot","cell":"c1"}`)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), `"state":"mismatch"`) ||
		!strings.Contains(w.Body.String(), `"missing":true`) {
		t.Fatalf("verify: %d %s", w.Code, w.Body.String())
	}
}

// Issue #149: a cell without live fixtures is named as empty, not as a passed check; the status
// stays 200 because it is no finding.
func TestExpVerifyEmptyCell(t *testing.T) {
	h := expServer(&fakeExpFixtures{})
	w := doExp(t, h, "POST", "/api/exp/fixtures/verify", expToken, `{"plan":"pilot","cell":"c1"}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"state":"empty"`) ||
		!strings.Contains(w.Body.String(), `"checked":0`) {
		t.Fatalf("empty cell: %d %s", w.Code, w.Body.String())
	}
}

func TestExpVerifyMatchingCell(t *testing.T) {
	h := expServer(&fakeExpFixtures{checked: 2})
	w := doExp(t, h, "POST", "/api/exp/fixtures/verify", expToken, `{"plan":"pilot","cell":"c1"}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"state":"ok"`) ||
		!strings.Contains(w.Body.String(), `"checked":2`) {
		t.Fatalf("matching cell: %d %s", w.Code, w.Body.String())
	}
}

// Issue #149: unseed names the empty case as well.
func TestExpUnseedEmptyCell(t *testing.T) {
	h := expServer(&fakeExpFixtures{})
	w := doExp(t, h, "POST", "/api/exp/fixtures/unseed", expToken, `{"plan":"pilot","cell":"c1"}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"removed":0`) {
		t.Fatalf("empty unseed: %d %s", w.Code, w.Body.String())
	}
}

func TestExpRoutesNeedConfiguration(t *testing.T) {
	h := expServer(nil) // typed nil must not be identical to "no service"
	w := doExp(t, h, "POST", "/api/exp/fixtures/seed", expToken, `{"plan":"pilot","cell":"c1","fixtures":[]}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured: %d %s", w.Code, w.Body.String())
	}
}

func TestExpSeedRequiresPlanAndCell(t *testing.T) {
	h := expServer(&fakeExpFixtures{})
	w := doExp(t, h, "POST", "/api/exp/fixtures/seed", expToken, `{"plan":"pilot"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing cell: %d %s", w.Code, w.Body.String())
	}
	w = doExp(t, h, "GET", "/api/exp/fixtures?plan=pilot", expToken, "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("list without cell: %d", w.Code)
	}
}

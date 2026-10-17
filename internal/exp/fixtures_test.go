// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package exp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agw/internal/platform"
	"agw/internal/store"
)

// fakeCaller is a platform of two accounts. It keeps the objects so that the read-back and the
// removal work like on the real service; truncate makes a field shorter than it was sent. meta
// answers GET /<kind>s/<id>/metadata (X4, #146); noMetadataRoute makes it answer 404 like a backend
// before that route.
type fakeCaller struct {
	mu              sync.Mutex
	objects         map[string]map[string]any // "datasets/1" -> object
	meta            map[string]map[string]any // "datasets/1" -> {"description", "metadata"}
	nextID          int
	reqs            []platform.Request
	truncate        string // field name to cut in half on creation
	failWith        string // returned as a transport error, to test that no content reaches a log
	noMetadataRoute bool
}

func newFakeCaller() *fakeCaller {
	return &fakeCaller{objects: map[string]map[string]any{}, meta: map[string]map[string]any{}}
}

func (f *fakeCaller) Do(_ context.Context, _ string, r platform.Request) (platform.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, r)
	if f.failWith != "" {
		return platform.Result{}, errors.New(f.failWith)
	}
	field := func(name string) string {
		if v := r.Form[name]; len(v) > 0 {
			return v[0]
		}
		return ""
	}
	switch {
	case r.Method == "POST" && r.Path == "/datasets":
		name := field("name")
		if f.truncate == "name" {
			name = name[:len(name)/2]
		}
		size := 0
		for _, u := range r.Uploads {
			size += len(u.Data)
		}
		f.nextID++
		key := fmt.Sprintf("datasets/%d", f.nextID)
		obj := map[string]any{
			"id": float64(f.nextID), "name": name, "dataset_type": field("dataset_type"),
			"filecount": float64(len(r.Uploads)), "total_filesize": float64(size),
		}
		f.objects[key] = obj
		md := map[string]any{}
		if raw := field("metadata"); raw != "" {
			_ = json.Unmarshal([]byte(raw), &md)
		}
		f.meta[key] = map[string]any{"description": field("description"), "metadata": md}
		return jsonResult(201, obj)
	case r.Method == "POST" && r.Path == "/models":
		name := field("name")
		if f.truncate == "name" {
			name = name[:len(name)/2]
		}
		f.nextID++
		key := fmt.Sprintf("models/%d", f.nextID)
		obj := map[string]any{"id": float64(f.nextID), "name": name, "format": field("format")}
		if len(r.Uploads) == 1 {
			obj["file_name"] = r.Uploads[0].Name
			obj["file_size"] = float64(len(r.Uploads[0].Data))
		}
		f.objects[key] = obj
		f.meta[key] = map[string]any{"description": field("description"), "metadata": map[string]any{}}
		return jsonResult(201, obj)
	case r.Method == "GET" && strings.HasSuffix(r.Path, "/metadata"):
		if f.noMetadataRoute {
			return platform.Result{Status: "error", HTTPStatus: 404, Body: `{"detail":"not found"}`}, nil
		}
		key := strings.TrimSuffix(strings.TrimPrefix(r.Path, "/"), "/metadata")
		md, ok := f.meta[key]
		if !ok {
			return platform.Result{Status: "error", HTTPStatus: 404, Body: `{"detail":"not found"}`}, nil
		}
		return jsonResult(200, md)
	case r.Method == "GET":
		key := strings.TrimPrefix(r.Path, "/")
		obj, ok := f.objects[key]
		if !ok {
			return platform.Result{Status: "error", HTTPStatus: 404, Body: `{"detail":"not found"}`}, nil
		}
		return jsonResult(200, obj)
	case r.Method == "DELETE":
		key := strings.TrimPrefix(r.Path, "/")
		if _, ok := f.objects[key]; !ok {
			return platform.Result{Status: "error", HTTPStatus: 404}, nil
		}
		delete(f.objects, key)
		delete(f.meta, key)
		return platform.Result{Status: "ok", HTTPStatus: 200}, nil
	}
	return platform.Result{Status: "error", HTTPStatus: 400}, nil
}

func (f *fakeCaller) calls(method, path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.reqs {
		if r.Method == method && r.Path == path {
			n++
		}
	}
	return n
}

func jsonResult(status int, obj map[string]any) (platform.Result, error) {
	b, _ := json.Marshal(obj)
	return platform.Result{Status: "ok", HTTPStatus: status, Body: string(b)}, nil
}

// fakeStore records the fixtures in memory. AddFixture assigns its own row id, so that removal is
// by the row and never by a name.
type fakeStore struct {
	mu   sync.Mutex
	rows []store.Fixture
	n    int
}

func (s *fakeStore) AddFixture(_ context.Context, f store.NewFixture) (store.Fixture, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n++
	row := store.Fixture{
		ID: fmt.Sprintf("row-%d", s.n), Plan: f.Plan, Cell: f.Cell, ManifestID: f.ManifestID,
		Kind: f.Kind, Owner: f.Owner, ObjectID: f.ObjectID, FieldSHA256: f.FieldSHA256, CreatedAt: time.Now(),
	}
	if row.FieldSHA256 == nil {
		row.FieldSHA256 = map[string]string{}
	}
	s.rows = append(s.rows, row)
	return row, nil
}

func (s *fakeStore) ListFixtures(_ context.Context, plan, cell string) ([]store.Fixture, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []store.Fixture
	for _, r := range s.rows {
		if r.Plan == plan && r.Cell == cell {
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *fakeStore) MarkFixtureRemoved(_ context.Context, id, result string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.rows {
		if s.rows[i].ID == id {
			now := time.Now()
			s.rows[i].RemovedAt = &now
			s.rows[i].RemoveResult = result
			return nil
		}
	}
	return store.ErrNotFound
}

func fixtureSpecs() []FixtureSpec {
	return []FixtureSpec{
		{
			ID: "dataset-1", Kind: KindDataset, Owner: OwnerPrimary,
			Fields: map[string]any{
				"name": "placeholder-dataset", "description": "placeholder-1",
				"metadata": map[string]any{"placeholder": "1"},
			},
			Files: []FixtureFile{{Field: "files", Name: "data.csv", Data: []byte("a,b\n1,2\n")}},
		},
		{
			ID: "model-1", Kind: KindModel, Owner: OwnerSecondary,
			Fields: map[string]any{
				"name": "placeholder-model", "description": "placeholder-2", "format": "onnx",
			},
			Files: []FixtureFile{{Field: "modelfile", Name: "model.onnx", Data: []byte("not really onnx")}},
		},
	}
}

func newFixtures(log *slog.Logger) (*Fixtures, *fakeCaller, *fakeCaller, *fakeStore) {
	primary, secondary, st := newFakeCaller(), newFakeCaller(), &fakeStore{}
	return &Fixtures{Store: st, Primary: primary, Secondary: secondary, Log: log}, primary, secondary, st
}

func TestSeedCreatesAndVerifies(t *testing.T) {
	f, primary, secondary, st := newFixtures(nil)
	rows, err := f.Seed(context.Background(), "pilot", "cli-none-no-main", fixtureSpecs())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows: %d", len(rows))
	}
	if rows[0].ManifestID != "dataset-1" || rows[0].ObjectID != "1" || rows[0].Kind != KindDataset {
		t.Fatalf("dataset row: %+v", rows[0])
	}
	if rows[1].ManifestID != "model-1" || rows[1].ObjectID != "1" || rows[1].Owner != OwnerSecondary {
		t.Fatalf("model row: %+v", rows[1])
	}
	// Creation and read-back per object, on the account of its owner.
	if n := primary.calls("POST", "/datasets"); n != 1 {
		t.Fatalf("dataset calls: %d", n)
	}
	if n := primary.calls("GET", "/datasets/1"); n != 1 {
		t.Fatalf("dataset read-backs: %d", n)
	}
	if n := secondary.calls("POST", "/models"); n != 1 {
		t.Fatalf("model calls: %d", n)
	}
	for _, name := range []string{"name", "description", "metadata", "files", "filecount", "total_filesize", "dataset_type"} {
		if rows[0].FieldSHA256[name] == "" {
			t.Errorf("dataset field %s has no checksum: %+v", name, rows[0].FieldSHA256)
		}
	}
	for _, name := range []string{"name", "format", "file.name", "file.size"} {
		if rows[1].FieldSHA256[name] == "" {
			t.Errorf("model field %s has no checksum: %+v", name, rows[1].FieldSHA256)
		}
	}
	// The files went along as uploads: one dataset file, one model file.
	primary.mu.Lock()
	datasetUploads := len(primary.reqs[0].Uploads)
	primary.mu.Unlock()
	if datasetUploads != 1 {
		t.Fatalf("dataset uploads: %d", datasetUploads)
	}
	secondary.mu.Lock()
	modelUploads := len(secondary.reqs[0].Uploads)
	secondary.mu.Unlock()
	if modelUploads != 1 || secondary.reqs[0].Uploads[0].Field != "modelfile" {
		t.Fatalf("model uploads: %+v", secondary.reqs[0].Uploads)
	}
	if len(st.rows) != 2 {
		t.Fatalf("stored rows: %d", len(st.rows))
	}
}

func TestSeedRefusesUnknownKind(t *testing.T) {
	f, primary, _, st := newFixtures(nil)
	spec := FixtureSpec{ID: "x", Kind: "bucket", Owner: OwnerPrimary}
	if _, err := f.Seed(context.Background(), "pilot", "c1", []FixtureSpec{spec}); err == nil || !strings.Contains(err.Error(), "kind") {
		t.Fatalf("unknown kind: %v", err)
	}
	if len(primary.reqs) != 0 || len(st.rows) != 0 {
		t.Fatalf("nothing may be created: %d calls, %d rows", len(primary.reqs), len(st.rows))
	}
}

func TestSeedRefusesUnknownField(t *testing.T) {
	f, primary, _, st := newFixtures(nil)
	spec := fixtureSpecs()[0]
	spec.Fields["shell"] = "rm -rf /"
	if _, err := f.Seed(context.Background(), "pilot", "c1", []FixtureSpec{spec}); err == nil || !strings.Contains(err.Error(), "field \"shell\"") {
		t.Fatalf("unknown field: %v", err)
	}
	if len(primary.reqs) != 0 || len(st.rows) != 0 {
		t.Fatalf("nothing may be created")
	}
}

// The backend may shorten a field; the read-back must catch that before the cell starts (X4.4).
func TestSeedDetectsTruncatedField(t *testing.T) {
	f, primary, _, _ := newFixtures(nil)
	primary.truncate = "name"
	rows, err := f.Seed(context.Background(), "pilot", "c1", fixtureSpecs()[:1])
	var mismatch *MismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("expected a MismatchError, got %v", err)
	}
	if len(mismatch.Items) != 1 || len(mismatch.Items[0].Fields) == 0 {
		t.Fatalf("mismatch: %+v", mismatch.Items)
	}
	if mismatch.Items[0].Fields[0].Field != "name" {
		t.Fatalf("field: %+v", mismatch.Items[0].Fields)
	}
	// The identifier was recorded before the check, so the object can be removed.
	if len(rows) != 1 || rows[0].ObjectID != "1" {
		t.Fatalf("row for removal: %+v", rows)
	}
}

// Removal goes by the recorded identifier: an object with the same name that the cell did not
// create stays (CLAUDE.md).
func TestUnseedOnlyRecordedIDs(t *testing.T) {
	f, primary, _, st := newFixtures(nil)
	ctx := context.Background()
	if _, err := st.AddFixture(ctx, store.NewFixture{
		Plan: "pilot", Cell: "c1", ManifestID: "dataset-1", Kind: KindDataset, Owner: OwnerPrimary, ObjectID: "1",
	}); err != nil {
		t.Fatal(err)
	}
	primary.objects["datasets/1"] = map[string]any{"id": float64(1), "name": "placeholder-dataset"}
	primary.objects["datasets/2"] = map[string]any{"id": float64(2), "name": "placeholder-dataset"}
	rows, err := f.Unseed(ctx, "pilot", "c1")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := primary.objects["datasets/1"]; ok {
		t.Fatal("recorded object was not removed")
	}
	if _, ok := primary.objects["datasets/2"]; !ok {
		t.Fatal("an object with the same name but a foreign id was removed")
	}
	if len(rows) != 1 || rows[0].RemovedAt == nil {
		t.Fatalf("row after removal: %+v", rows)
	}
}

func TestVerifyFindsMissingObject(t *testing.T) {
	f, primary, _, _ := newFixtures(nil)
	ctx := context.Background()
	if _, err := f.Seed(ctx, "pilot", "c1", fixtureSpecs()[:1]); err != nil {
		t.Fatal(err)
	}
	delete(primary.objects, "datasets/1") // deleted by a run inside the cell
	findings, err := f.Verify(ctx, "pilot", "c1")
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || !findings[0].Missing || findings[0].ManifestID != "dataset-1" {
		t.Fatalf("findings: %+v", findings)
	}
}

func TestSeedRefusesLiveFixtures(t *testing.T) {
	f, _, _, _ := newFixtures(nil)
	ctx := context.Background()
	if _, err := f.Seed(ctx, "pilot", "c1", fixtureSpecs()[:1]); err != nil {
		t.Fatal(err)
	}
	// A second seed without an unseed would create a second set with a suffixed name.
	if _, err := f.Seed(ctx, "pilot", "c1", fixtureSpecs()[:1]); err == nil || !strings.Contains(err.Error(), "unseed") {
		t.Fatalf("second seed: %v", err)
	}
	// After the removal a new seed is allowed.
	if _, err := f.Unseed(ctx, "pilot", "c1"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seed(ctx, "pilot", "c1", fixtureSpecs()[:1]); err != nil {
		t.Fatalf("seed after unseed: %v", err)
	}
}

// A run inside the cell may change a fixture; the next Verify compares with the manifest, not with
// the read-back of the seeding (X4.6).
func TestVerifyDetectsChangedField(t *testing.T) {
	f, primary, _, _ := newFixtures(nil)
	ctx := context.Background()
	if _, err := f.Seed(ctx, "pilot", "c1", fixtureSpecs()[:1]); err != nil {
		t.Fatal(err)
	}
	primary.mu.Lock()
	primary.objects["datasets/1"]["name"] = "something-else"
	primary.mu.Unlock()
	findings, err := f.Verify(ctx, "pilot", "c1")
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || len(findings[0].Fields) != 1 || findings[0].Fields[0].Field != "name" {
		t.Fatalf("findings: %+v", findings)
	}
}

// #146: description and metadata come from the metadata route of the backend and are compared.
// An unchanged fixture stays without a finding; a changed description or metadata is one.
func TestVerifyComparesDescriptionAndMetadata(t *testing.T) {
	f, primary, _, _ := newFixtures(nil)
	ctx := context.Background()
	if _, err := f.Seed(ctx, "pilot", "c1", fixtureSpecs()[:1]); err != nil {
		t.Fatal(err)
	}
	if findings, err := f.Verify(ctx, "pilot", "c1"); err != nil {
		t.Fatal(err)
	} else if len(findings) != 0 {
		t.Fatalf("unchanged fixture: %+v", findings)
	}
	primary.mu.Lock()
	primary.meta["datasets/1"]["description"] = "placeholder-CHANGED"
	primary.mu.Unlock()
	findings, err := f.Verify(ctx, "pilot", "c1")
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || len(findings[0].Fields) != 1 || findings[0].Fields[0].Field != "description" {
		t.Fatalf("changed description: %+v", findings)
	}
	primary.mu.Lock()
	primary.meta["datasets/1"]["description"] = "placeholder-1"
	primary.meta["datasets/1"]["metadata"] = map[string]any{"placeholder": "2"}
	primary.mu.Unlock()
	findings, err = f.Verify(ctx, "pilot", "c1")
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || len(findings[0].Fields) != 1 || findings[0].Fields[0].Field != "metadata" {
		t.Fatalf("changed metadata: %+v", findings)
	}
	primary.mu.Lock()
	primary.meta["datasets/1"]["metadata"] = map[string]any{"placeholder": "1"}
	primary.mu.Unlock()
	if findings, err := f.Verify(ctx, "pilot", "c1"); err != nil {
		t.Fatal(err)
	} else if len(findings) != 0 {
		t.Fatalf("restored fixture: %+v", findings)
	}
}

// A backend before #146 answers the metadata route with 404. The cell must still work: the two
// fields stay recorded but unverified instead of reporting a mismatch.
func TestVerifySkipsMetadataOnOlderBackend(t *testing.T) {
	f, primary, _, _ := newFixtures(nil)
	primary.noMetadataRoute = true
	ctx := context.Background()
	if _, err := f.Seed(ctx, "pilot", "c1", fixtureSpecs()[:1]); err != nil {
		t.Fatalf("seed without the metadata route: %v", err)
	}
	if findings, err := f.Verify(ctx, "pilot", "c1"); err != nil {
		t.Fatal(err)
	} else if len(findings) != 0 {
		t.Fatalf("older backend: %+v", findings)
	}
	// The description is still recorded, so a later backend can compare it.
	if row, _ := f.List(ctx, "pilot", "c1"); len(row) == 0 || row[0].FieldSHA256["description"] == "" {
		t.Fatalf("description not recorded: %+v", row)
	}
}

// A dataset_file manifest is a file below a dataset: the backend has no route that adds a file to
// an existing dataset, so it goes along with the dataset's upload.
func TestDatasetFileFoldedIntoDataset(t *testing.T) {
	f, primary, _, st := newFixtures(nil)
	ctx := context.Background()
	specs := fixtureSpecs()[:1]
	specs = append(specs, FixtureSpec{
		ID: "file-1", Kind: KindDatasetFile, Owner: OwnerPrimary,
		Fields: map[string]any{"dataset": "dataset-1", "name": "notes.txt"},
		Files:  []FixtureFile{{Field: "files", Name: "notes.txt", Data: []byte("placeholder-3")}},
	})
	rows, err := f.Seed(ctx, "pilot", "c1", specs)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows: %d", len(rows))
	}
	if rows[1].Kind != KindDatasetFile || rows[1].ObjectID != rows[0].ObjectID {
		t.Fatalf("dataset_file row: %+v", rows[1])
	}
	primary.mu.Lock()
	uploads := primary.reqs[0].Uploads
	primary.mu.Unlock()
	if len(uploads) != 2 {
		t.Fatalf("uploads: %d", len(uploads))
	}
	names := []string{uploads[0].Name, uploads[1].Name}
	if !(names[0] == "data.csv" && names[1] == "notes.txt") && !(names[1] == "data.csv" && names[0] == "notes.txt") {
		t.Fatalf("upload names: %v", names)
	}
	if _, err := f.Unseed(ctx, "pilot", "c1"); err != nil {
		t.Fatal(err)
	}
	if n := primary.calls("DELETE", "/datasets/1"); n != 1 {
		t.Fatalf("dataset deletions: %d", n)
	}
	for _, row := range st.rows {
		if row.RemovedAt == nil {
			t.Fatalf("row not removed: %+v", row)
		}
	}
}

// The log must not carry the material: only identifiers and checksums (X4.7).
func TestContentNeverLogged(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	f, _, _, _ := newFixtures(log)
	if _, err := f.Seed(context.Background(), "pilot", "c1", fixtureSpecs()); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if out == "" {
		t.Fatal("nothing logged")
	}
	for _, secret := range []string{"placeholder-1", "placeholder-2", "placeholder-dataset", "not really onnx"} {
		if strings.Contains(out, secret) {
			t.Fatalf("log carries the material %q: %s", secret, out)
		}
	}
}

func TestSpecsResolvesFileReferences(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "fixtures", "inhalte"), 0o755); err != nil {
		t.Fatal(err)
	}
	text := "placeholder-1\n"
	data := []byte("a,b\n")
	if err := os.WriteFile(filepath.Join(dir, "fixtures", "inhalte", "desc.txt"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fixtures", "inhalte", "data.csv"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	ms := []Manifest{{
		ID: "dataset-1", Kind: KindDataset, Owner: OwnerPrimary,
		Fields: map[string]FieldValue{
			"name":        {Literal: "placeholder-dataset"},
			"description": {File: "fixtures/inhalte/desc.txt"},
			"metadata":    {Literal: map[string]any{"placeholder": "1"}},
		},
		Files: []string{"fixtures/inhalte/data.csv"},
	}}
	specs, err := Specs(dir, ms)
	if err != nil {
		t.Fatal(err)
	}
	if got := specs[0].Fields["description"]; got != text {
		t.Fatalf("description: %q", got)
	}
	if len(specs[0].Files) != 1 || specs[0].Files[0].Field != "files" || string(specs[0].Files[0].Data) != string(data) {
		t.Fatalf("files: %+v", specs[0].Files)
	}
	if specs[0].Files[0].SHA256 != hashBytes(data) {
		t.Fatalf("sha256: %s", specs[0].Files[0].SHA256)
	}
	// A path that leaves the material is refused.
	if _, err := readMaterial(dir, "../outside.txt"); err == nil {
		t.Fatal("path outside the material accepted")
	}
}

func TestForCellEmptyCellsMeansAll(t *testing.T) {
	ms := []Manifest{{ID: "a", Cells: nil}, {ID: "b", Cells: []string{"c1"}}, {ID: "c", Cells: []string{"c2"}}}
	got := ForCell(ms, "c1")
	if len(got) != 2 || got[0].ID != "a" || got[1].ID != "b" {
		t.Fatalf("for cell c1: %+v", got)
	}
}

// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

// Fixtures of an experiment cell (X4, issue #129): the platform objects a cell needs are created
// from the material before the cell, read back field by field, recorded with their identifiers and
// removed by identifier after the cell. The gateway knows fields and checksums only; the content
// files stay with the material of the author (X4.7). Nothing here runs through a chat, so a fixture
// never appears as a call of the agent.
package exp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"agw/internal/platform"
	"agw/internal/store"
)

// Kinds and owners a fixture manifest may use (X4.2). They match the constants of the store.
const (
	KindDataset     = store.FixtureDataset
	KindModel       = store.FixtureModel
	KindDatasetFile = store.FixtureDatasetFile

	OwnerPrimary   = store.FixturePrimary
	OwnerSecondary = store.FixtureSecondary
)

// FixtureSpec is a manifest with its content resolved for the service: the CLI reads the files and
// sends them along as bytes, the service stores only their SHA-256 (X4.7).
type FixtureSpec struct {
	ID     string         `json:"id"`
	Kind   string         `json:"kind"`
	Owner  string         `json:"owner"`
	Fields map[string]any `json:"fields,omitempty"`
	Files  []FixtureFile  `json:"files,omitempty"`
	Attack bool           `json:"attack,omitempty"`
}

// FixtureFile is one file of a fixture. Field is the form field of the upload ("files" for a
// dataset, "modelfile" for a model). SHA256 is checked against the content before the upload.
type FixtureFile struct {
	Field  string `json:"field"`
	Name   string `json:"name"`
	SHA256 string `json:"sha256,omitempty"`
	Data   []byte `json:"data"`
}

// FixtureStore is the part of the store the fixture service uses (implemented by *store.Store).
type FixtureStore interface {
	AddFixture(ctx context.Context, f store.NewFixture) (store.Fixture, error)
	ListFixtures(ctx context.Context, plan, cell string) ([]store.Fixture, error)
	MarkFixtureRemoved(ctx context.Context, id, result string) error
}

// PlatformCaller is the platform client of one experiment account (implemented by
// *platform.Client). The service keeps one per owner; no call goes through a chat.
type PlatformCaller interface {
	Do(ctx context.Context, chatID string, r platform.Request) (platform.Result, error)
}

// Fixtures seeds, verifies and removes the platform objects of a cell. Primary and Secondary may be
// nil when the account is not configured; only a fixture with that owner then fails.
type Fixtures struct {
	Store     FixtureStore
	Primary   PlatformCaller
	Secondary PlatformCaller
	Log       *slog.Logger
}

// Mismatch is a fixture that no longer matches the manifest. It names the manifest, the object and
// the differing fields with their SHA-256 — never a value (X4.7).
type Mismatch struct {
	ManifestID string          `json:"manifest_id"`
	Kind       string          `json:"kind,omitempty"`
	ObjectID   string          `json:"object_id,omitempty"`
	Missing    bool            `json:"missing,omitempty"`
	Fields     []FieldMismatch `json:"fields,omitempty"`
}

// FieldMismatch is one field whose read-back does not match the intended SHA-256.
type FieldMismatch struct {
	Field    string `json:"field"`
	Expected string `json:"expected"`
	Actual   string `json:"actual"`
}

func (m Mismatch) String() string {
	if m.Missing {
		return fmt.Sprintf("fixture %s (%s %s): object is missing", m.ManifestID, m.Kind, m.ObjectID)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "fixture %s (%s %s):", m.ManifestID, m.Kind, m.ObjectID)
	for _, f := range m.Fields {
		fmt.Fprintf(&b, " field %s differs (expected sha256 %s, read back %s);", f.Field, f.Expected, f.Actual)
	}
	return strings.TrimSuffix(b.String(), ";")
}

// MismatchError collects the fixtures a verify found. It carries no content (X4.7).
type MismatchError struct{ Items []Mismatch }

func (e *MismatchError) Error() string {
	parts := make([]string, 0, len(e.Items))
	for _, m := range e.Items {
		parts = append(parts, m.String())
	}
	return strings.Join(parts, "; ")
}

// Allowed fields per kind (X4.2). A field that is not listed here is refused: the fixture mechanism
// must not become a second, unchecked way into the platform.
var fixtureFields = map[string]map[string]bool{
	KindDataset:     {"name": true, "description": true, "metadata": true, "dataset_type": true, "keywords": true},
	KindModel:       {"name": true, "description": true, "format": true, "keywords": true},
	KindDatasetFile: {"dataset": true, "name": true},
}

// readableFields are the fields the read-back of the backend exposes; only they can be compared in
// Verify. Everything else (description and metadata, which live in the triple store, not in
// Postgres) is recorded with its SHA-256 but cannot be read back through the platform API.
var readableFields = map[string]map[string]bool{
	KindDataset: {"name": true, "dataset_type": true, "filecount": true, "total_filesize": true},
	KindModel:   {"name": true, "format": true, "file.name": true, "file.size": true},
}

var modelFormats = map[string]bool{"onnx": true, "pytorch": true, "tensorflow": true, "tensorrt": true}

// Seed creates the objects of the given manifests for one cell, records them by identifier and
// reads every readable field back. If a read-back deviates, the created rows stay and the returned
// *MismatchError names the fields; the caller removes them with Unseed before trying again (X4.4,
// X4.6). No error text contains a field value or a file's content (X4.7).
func (f *Fixtures) Seed(ctx context.Context, plan, cell string, specs []FixtureSpec) ([]store.Fixture, error) {
	if strings.TrimSpace(plan) == "" || strings.TrimSpace(cell) == "" {
		return nil, errors.New("plan and cell are required")
	}
	if err := validateSpecs(specs); err != nil {
		return nil, err
	}
	byKind := map[string][]FixtureSpec{}
	for _, s := range specs {
		byKind[s.Kind] = append(byKind[s.Kind], s)
	}
	datasetFiles := map[string][]FixtureSpec{}
	for _, s := range byKind[KindDatasetFile] {
		parent, _ := s.Fields["dataset"].(string)
		datasetFiles[parent] = append(datasetFiles[parent], s)
	}
	// Datasets first: their files include the dataset_file manifests below them.
	order := map[string]int{KindDataset: 0, KindModel: 1, KindDatasetFile: 2}
	sorted := append([]FixtureSpec(nil), specs...)
	sort.SliceStable(sorted, func(i, j int) bool { return order[sorted[i].Kind] < order[sorted[j].Kind] })

	var (
		rows     []store.Fixture
		mismatch []Mismatch
		parents  = map[string]store.Fixture{}
	)
	for _, spec := range sorted {
		if spec.Kind == KindDatasetFile {
			parent, ok := parents[spec.Fields["dataset"].(string)]
			if !ok {
				mismatch = append(mismatch, Mismatch{ManifestID: spec.ID, Kind: spec.Kind, Missing: true})
				continue
			}
			hashes, err := specHashes(spec)
			if err != nil {
				return rows, err
			}
			row, err := f.Store.AddFixture(ctx, store.NewFixture{
				Plan: plan, Cell: cell, ManifestID: spec.ID, Kind: spec.Kind, Owner: spec.Owner,
				ObjectID: parent.ObjectID, FieldSHA256: hashes,
			})
			if err != nil {
				return rows, err
			}
			rows = append(rows, row)
			continue
		}
		row, obj, err := f.seedOne(ctx, plan, cell, spec, datasetFiles[spec.ID])
		if row.ID != "" {
			rows = append(rows, row)
			parents[spec.ID] = row
		}
		if err != nil {
			return rows, err
		}
		if bad := verifyObject(row, obj); len(bad.Fields) > 0 || bad.Missing {
			mismatch = append(mismatch, bad)
		}
	}
	if len(mismatch) > 0 {
		return rows, &MismatchError{Items: mismatch}
	}
	return rows, nil
}

// seedOne creates one object and records it before it is read back.
func (f *Fixtures) seedOne(ctx context.Context, plan, cell string, spec FixtureSpec, files []FixtureSpec) (store.Fixture, map[string]any, error) {
	c, err := f.caller(spec.Owner)
	if err != nil {
		return store.Fixture{}, nil, fmt.Errorf("manifest %s: %w", spec.ID, err)
	}
	hashes, err := specHashes(spec)
	if err != nil {
		return store.Fixture{}, nil, err
	}
	uploads, err := uploadsOf(spec, files)
	if err != nil {
		return store.Fixture{}, nil, err
	}
	var (
		req platform.Request
	)
	switch spec.Kind {
	case KindDataset:
		req, err = datasetRequest(spec, uploads)
	case KindModel:
		req, err = modelRequest(spec, uploads)
	default:
		err = fmt.Errorf("manifest %s: kind %q cannot be created", spec.ID, spec.Kind)
	}
	if err != nil {
		return store.Fixture{}, nil, fmt.Errorf("manifest %s: %w", spec.ID, err)
	}
	res, err := c.Do(ctx, "", req)
	if err != nil {
		return store.Fixture{}, nil, fmt.Errorf("manifest %s: %s: %w", spec.ID, req.String(), sanitize(err))
	}
	if res.Status != "ok" || res.HTTPStatus < 200 || res.HTTPStatus >= 300 {
		return store.Fixture{}, nil, fmt.Errorf("manifest %s: %s: platform responds %d", spec.ID, req.String(), res.HTTPStatus)
	}
	obj, err := decodeObject(res.Body)
	if err != nil {
		return store.Fixture{}, nil, fmt.Errorf("manifest %s: response of %s not readable: %w", spec.ID, req.String(), err)
	}
	objectID := jsonID(obj)
	if objectID == "" {
		return store.Fixture{}, nil, fmt.Errorf("manifest %s: response of %s has no id", spec.ID, req.String())
	}
	for name, up := range uploadHashes(uploads) {
		hashes[name] = up
	}
	// The intended values of the fields the backend derives from the files, so that the read-back is
	// checked against the manifest and not against itself.
	for name, h := range expectedUploadHashes(spec.Kind, uploads) {
		hashes[name] = h
	}
	// Read the object back before the row is written, so that the row carries the hashes of what the
	// backend actually exposes. Fields already hashed from the manifest keep their intended value;
	// only fields the manifest does not name are taken from the read-back, so that a later change to
	// them shows up in Verify. If the read-back fails, the row is still written: the identifier is
	// the only way to remove the object again.
	read, readErr := f.readBack(ctx, c, spec.Kind, objectID)
	if readErr == nil {
		for name, h := range readableHashes(spec.Kind, read) {
			if _, declared := hashes[name]; !declared {
				hashes[name] = h
			}
		}
	}
	row, err := f.Store.AddFixture(ctx, store.NewFixture{
		Plan: plan, Cell: cell, ManifestID: spec.ID, Kind: spec.Kind, Owner: spec.Owner,
		ObjectID: objectID, FieldSHA256: hashes,
	})
	if err != nil {
		return store.Fixture{}, nil, err
	}
	f.log().Info("fixture seeded", "plan", plan, "cell", cell, "manifest", spec.ID, "kind", spec.Kind,
		"owner", spec.Owner, "object_id", objectID, "field_sha256", hashes)
	if readErr != nil {
		return row, nil, fmt.Errorf("manifest %s: %w", spec.ID, readErr)
	}
	return row, read, nil
}

// Unseed removes the live objects of a cell by the identifiers of exp_fixtures, models before
// datasets. It never looks at a name (CLAUDE.md); an object of the same name that this cell did not
// create stays. A dataset_file row is removed together with its dataset and has no call of its own.
func (f *Fixtures) Unseed(ctx context.Context, plan, cell string) ([]store.Fixture, error) {
	if strings.TrimSpace(plan) == "" || strings.TrimSpace(cell) == "" {
		return nil, errors.New("plan and cell are required")
	}
	rows, err := f.Store.ListFixtures(ctx, plan, cell)
	if err != nil {
		return nil, err
	}
	live := make([]store.Fixture, 0, len(rows))
	for _, r := range rows {
		if r.RemovedAt == nil {
			live = append(live, r)
		}
	}
	// Models before datasets (CLAUDE.md, order of the cleanup). dataset_file goes with its dataset.
	sort.SliceStable(live, func(i, j int) bool {
		return rank(live[i].Kind) < rank(live[j].Kind)
	})
	var errs []error
	removedDataset := map[string]bool{}
	for _, r := range live {
		if r.Kind == KindDatasetFile {
			continue
		}
		if err := f.removeOne(ctx, r); err != nil {
			errs = append(errs, err)
			continue
		}
		if r.Kind == KindDataset {
			removedDataset[r.ObjectID] = true
		}
	}
	for _, r := range live {
		if r.Kind != KindDatasetFile {
			continue
		}
		if !removedDataset[r.ObjectID] {
			errs = append(errs, fmt.Errorf("fixture %s: dataset %s was not removed", r.ManifestID, r.ObjectID))
			continue
		}
		if err := f.Store.MarkFixtureRemoved(ctx, r.ID, "removed with dataset "+r.ObjectID); err != nil {
			errs = append(errs, err)
		}
	}
	// Read again so that the caller sees removed_at and remove_result, also after a partial failure.
	after, listErr := f.Store.ListFixtures(ctx, plan, cell)
	errs = append(errs, listErr)
	return after, errors.Join(errs...)
}

func (f *Fixtures) removeOne(ctx context.Context, r store.Fixture) error {
	if r.ObjectID == "" {
		return fmt.Errorf("fixture %s (%s): no object id recorded", r.ManifestID, r.Kind)
	}
	c, err := f.caller(r.Owner)
	if err != nil {
		return fmt.Errorf("fixture %s: %w", r.ManifestID, err)
	}
	req := platform.Request{Method: http.MethodDelete, Path: "/" + r.Kind + "s/" + r.ObjectID}
	if r.Kind == KindDataset {
		// DELETE /datasets/{id} requires the CVAT login fields; without an annotation task the
		// placeholders suffice (CLAUDE.md, limits of the backend).
		req.Body = json.RawMessage(`{"key":"-","csrftoken":"-","sessionid":"-"}`)
	}
	res, err := c.Do(ctx, "", req)
	if err != nil {
		return fmt.Errorf("fixture %s (%s %s): %w", r.ManifestID, r.Kind, r.ObjectID, sanitize(err))
	}
	if res.Status != "ok" || res.HTTPStatus < 200 || res.HTTPStatus >= 300 {
		return fmt.Errorf("fixture %s (%s %s): platform responds %d", r.ManifestID, r.Kind, r.ObjectID, res.HTTPStatus)
	}
	if err := f.Store.MarkFixtureRemoved(ctx, r.ID, "deleted"); err != nil {
		return fmt.Errorf("fixture %s: removed on the platform, but not recorded: %w", r.ManifestID, err)
	}
	f.log().Info("fixture removed", "plan", r.Plan, "cell", r.Cell, "manifest", r.ManifestID,
		"kind", r.Kind, "owner", r.Owner, "object_id", r.ObjectID)
	return nil
}

// Verify compares the live fixtures of a cell with the checksums recorded at seeding time. It is
// called before every run (X4.6); a change made inside a cell shows up here before the next run.
func (f *Fixtures) Verify(ctx context.Context, plan, cell string) ([]Mismatch, error) {
	rows, err := f.Store.ListFixtures(ctx, plan, cell)
	if err != nil {
		return nil, err
	}
	var out []Mismatch
	for _, r := range rows {
		if r.RemovedAt != nil || r.Kind == KindDatasetFile {
			continue
		}
		c, err := f.caller(r.Owner)
		if err != nil {
			out = append(out, Mismatch{ManifestID: r.ManifestID, Kind: r.Kind, ObjectID: r.ObjectID, Missing: true})
			continue
		}
		obj, err := f.readBack(ctx, c, r.Kind, r.ObjectID)
		if err != nil {
			out = append(out, Mismatch{ManifestID: r.ManifestID, Kind: r.Kind, ObjectID: r.ObjectID, Missing: errors.Is(err, errNotFound)})
			continue
		}
		if bad := verifyObject(r, obj); len(bad.Fields) > 0 || bad.Missing {
			out = append(out, bad)
		}
	}
	return out, nil
}

// List returns the recorded fixtures of a cell, removed ones included.
func (f *Fixtures) List(ctx context.Context, plan, cell string) ([]store.Fixture, error) {
	if strings.TrimSpace(plan) == "" || strings.TrimSpace(cell) == "" {
		return nil, errors.New("plan and cell are required")
	}
	return f.Store.ListFixtures(ctx, plan, cell)
}

var errNotFound = errors.New("object not found")

// readBack reads one object through the platform API. The result carries no description; see
// readableFields for what can be checked.
func (f *Fixtures) readBack(ctx context.Context, c PlatformCaller, kind, objectID string) (map[string]any, error) {
	path := "/" + kind + "s/" + objectID
	res, err := c.Do(ctx, "", platform.Request{Method: http.MethodGet, Path: path})
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, sanitize(err))
	}
	if res.Status != "ok" || res.HTTPStatus < 200 || res.HTTPStatus >= 300 {
		if res.HTTPStatus == http.StatusNotFound {
			return nil, errNotFound
		}
		return nil, fmt.Errorf("reading %s: platform responds %d", path, res.HTTPStatus)
	}
	return decodeObject(res.Body)
}

func (f *Fixtures) caller(owner string) (PlatformCaller, error) {
	switch owner {
	case OwnerPrimary:
		if f.Primary == nil {
			return nil, errors.New("the primary experiment account is not configured (AGW_EXP_USER, AGW_EXP_PASSWORD)")
		}
		return f.Primary, nil
	case OwnerSecondary:
		if f.Secondary == nil {
			return nil, errors.New("the secondary experiment account is not configured (AGW_EXP2_USER, AGW_EXP2_PASSWORD)")
		}
		return f.Secondary, nil
	}
	return nil, fmt.Errorf("unknown owner %q", owner)
}

func (f *Fixtures) log() *slog.Logger {
	if f.Log != nil {
		return f.Log
	}
	return slog.Default()
}

// sanitize keeps messages of the platform client from carrying a response body into the log or an
// error (X4.7). The client already redacts secrets; the fixture content must not pass through here.
func sanitize(err error) error {
	if err == nil {
		return nil
	}
	return errors.New(strings.ReplaceAll(err.Error(), "\n", " "))
}

func rank(kind string) int {
	switch kind {
	case KindModel:
		return 0
	case KindDataset:
		return 1
	}
	return 2
}

// validateSpecs checks every manifest before anything is created, so that a bad manifest does not
// leave half a cell behind.
func validateSpecs(specs []FixtureSpec) error {
	seen := map[string]bool{}
	datasets := map[string]bool{}
	datasetOwner := map[string]string{}
	for _, s := range specs {
		if strings.TrimSpace(s.ID) == "" {
			return errors.New("a fixture has no id")
		}
		if seen[s.ID] {
			return fmt.Errorf("fixture %s is listed twice", s.ID)
		}
		seen[s.ID] = true
		if s.Kind == KindDataset {
			datasets[s.ID] = true
			datasetOwner[s.ID] = s.Owner
		}
	}
	for _, s := range specs {
		if err := validateSpec(s); err != nil {
			return fmt.Errorf("manifest %s: %w", s.ID, err)
		}
		if s.Kind == KindDatasetFile {
			parent, _ := s.Fields["dataset"].(string)
			if !datasets[parent] {
				return fmt.Errorf("manifest %s: dataset %q is not part of this seed call", s.ID, parent)
			}
			// The file lives in the dataset's bucket, so it belongs to the dataset's account.
			if owner := datasetOwner[parent]; owner != s.Owner {
				return fmt.Errorf("manifest %s: owner %q differs from its dataset %s (%q)", s.ID, s.Owner, parent, owner)
			}
		}
	}
	return nil
}

func validateSpec(s FixtureSpec) error {
	allowed, ok := fixtureFields[s.Kind]
	if !ok {
		return fmt.Errorf("kind %q is not allowed (dataset, model, dataset_file)", s.Kind)
	}
	if s.Owner != OwnerPrimary && s.Owner != OwnerSecondary {
		return fmt.Errorf("owner must be primary or secondary, not %q", s.Owner)
	}
	for name := range s.Fields {
		if !allowed[name] {
			return fmt.Errorf("field %q is not allowed for kind %q", name, s.Kind)
		}
	}
	for _, f := range s.Files {
		if f.Name == "" {
			return errors.New("a file has no name")
		}
		// The backend uses the name as the object name below the dataset's prefix; a path would
		// write outside it.
		if strings.ContainsAny(f.Name, "/\\\x00") || f.Name == "." || f.Name == ".." {
			return fmt.Errorf("file name %q must be a plain name without a path", f.Name)
		}
		sum := sha256.Sum256(f.Data)
		if f.SHA256 != "" && !strings.EqualFold(f.SHA256, hex.EncodeToString(sum[:])) {
			return fmt.Errorf("file %q: sha256 does not match the content", f.Name)
		}
	}
	switch s.Kind {
	case KindDataset:
		for _, name := range []string{"name", "description"} {
			if v, ok := s.Fields[name].(string); !ok || strings.TrimSpace(v) == "" {
				return fmt.Errorf("field %q is required and must be text", name)
			}
		}
		if v, ok := s.Fields["dataset_type"]; ok {
			if _, ok := v.(string); !ok {
				return errors.New("field \"dataset_type\" must be text")
			}
		}
		if v, ok := s.Fields["metadata"]; ok {
			if _, ok := v.(map[string]any); !ok {
				return errors.New("field \"metadata\" must be a mapping")
			}
		}
		if _, err := keywordsOf(s); err != nil {
			return err
		}
		for _, f := range s.Files {
			if f.Field != "files" {
				return fmt.Errorf("dataset file %q: form field must be \"files\"", f.Name)
			}
		}
	case KindModel:
		for _, name := range []string{"name", "description", "format"} {
			v, ok := s.Fields[name].(string)
			if !ok || strings.TrimSpace(v) == "" {
				return fmt.Errorf("field %q is required and must be text", name)
			}
		}
		format, _ := s.Fields["format"].(string)
		if !modelFormats[format] {
			return fmt.Errorf("format must be onnx, pytorch, tensorflow or tensorrt, not %q", format)
		}
		if _, err := keywordsOf(s); err != nil {
			return err
		}
		if len(s.Files) != 1 {
			return errors.New("a model needs exactly one file")
		}
		if s.Files[0].Field != "modelfile" {
			return fmt.Errorf("model file %q: form field must be \"modelfile\"", s.Files[0].Name)
		}
	case KindDatasetFile:
		if v, ok := s.Fields["dataset"].(string); !ok || strings.TrimSpace(v) == "" {
			return errors.New("field \"dataset\" is required and must name a dataset manifest")
		}
		if v, ok := s.Fields["name"]; ok {
			if _, ok := v.(string); !ok {
				return errors.New("field \"name\" must be text")
			}
		}
		if len(s.Files) != 1 {
			return errors.New("a dataset_file needs exactly one file")
		}
		if s.Files[0].Field != "files" {
			return fmt.Errorf("dataset_file %q: form field must be \"files\"", s.Files[0].Name)
		}
	}
	return nil
}

// datasetRequest builds POST /datasets. metadata is mandatory: without it the backend fails at the
// triple store (CLAUDE.md); is_classification_dataset=false keeps the labels (same limit).
func datasetRequest(spec FixtureSpec, uploads []platform.Upload) (platform.Request, error) {
	form := map[string][]string{
		"name":                      {spec.Fields["name"].(string)},
		"description":               {spec.Fields["description"].(string)},
		"dataset_type":              {"AgriImageDataResource"},
		"metadata":                  {"{}"},
		"is_classification_dataset": {"false"},
		"includes_annotation_file":  {"false"},
	}
	if v, ok := spec.Fields["dataset_type"].(string); ok && v != "" {
		form["dataset_type"] = []string{v}
	}
	if md, ok := spec.Fields["metadata"]; ok {
		b, err := json.Marshal(md)
		if err != nil {
			return platform.Request{}, err
		}
		form["metadata"] = []string{string(b)}
	}
	if kw, err := keywordsOf(spec); err != nil {
		return platform.Request{}, err
	} else if len(kw) > 0 {
		form["semantic_labels"] = kw
	}
	return platform.Request{Method: http.MethodPost, Path: "/datasets", Form: form, Uploads: uploads}, nil
}

// modelRequest builds POST /models (the same form fields as the upload_model tool).
func modelRequest(spec FixtureSpec, uploads []platform.Upload) (platform.Request, error) {
	form := map[string][]string{
		"name":        {spec.Fields["name"].(string)},
		"description": {spec.Fields["description"].(string)},
		"format":      {spec.Fields["format"].(string)},
	}
	// The form field is called labels, but it carries AGROVOC URIs (upload_model, 05.10.2026).
	if kw, err := keywordsOf(spec); err != nil {
		return platform.Request{}, err
	} else if len(kw) > 0 {
		form["labels"] = kw
	}
	return platform.Request{Method: http.MethodPost, Path: "/models", Form: form, Uploads: uploads}, nil
}

// uploadsOf turns the manifests' files into uploads. dataset_file manifests that name this dataset
// are folded in here: the backend has no route that adds a file to an existing dataset.
func uploadsOf(spec FixtureSpec, files []FixtureSpec) ([]platform.Upload, error) {
	var out []platform.Upload
	add := func(f FixtureFile, name string) {
		sum := sha256.Sum256(f.Data)
		out = append(out, platform.Upload{Field: f.Field, Name: name, Data: f.Data, SHA256: hex.EncodeToString(sum[:])})
	}
	for _, f := range spec.Files {
		add(f, f.Name)
	}
	// Dataset files below this dataset, sorted by manifest id for a reproducible upload order.
	sorted := append([]FixtureSpec(nil), files...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	for _, extra := range sorted {
		if len(extra.Files) != 1 {
			return nil, fmt.Errorf("dataset_file %s: exactly one file is required", extra.ID)
		}
		name, _ := extra.Fields["name"].(string)
		if name == "" {
			name = extra.Files[0].Name
		}
		add(extra.Files[0], name)
	}
	return out, nil
}

// specHashes hashes every declared field as canonical JSON. File contents are hashed as raw bytes
// under "files" (a canonical list of field, name and sha256); the read-back cannot reproduce that
// hash, it is the proof of the material that went in.
func specHashes(spec FixtureSpec) (map[string]string, error) {
	out := map[string]string{}
	for name, v := range spec.Fields {
		b, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("manifest %s: field %s cannot be encoded: %w", spec.ID, name, err)
		}
		out[name] = hashBytes(b)
	}
	if len(spec.Files) > 0 {
		type fileRef struct {
			Field  string `json:"field"`
			Name   string `json:"name"`
			SHA256 string `json:"sha256"`
		}
		refs := make([]fileRef, 0, len(spec.Files))
		for _, f := range spec.Files {
			refs = append(refs, fileRef{Field: f.Field, Name: f.Name, SHA256: hashBytes(f.Data)})
		}
		sort.Slice(refs, func(i, j int) bool {
			if refs[i].Field != refs[j].Field {
				return refs[i].Field < refs[j].Field
			}
			return refs[i].Name < refs[j].Name
		})
		b, err := json.Marshal(refs)
		if err != nil {
			return nil, err
		}
		out["files"] = hashBytes(b)
	}
	return out, nil
}

// uploadHashes are the additional checksums of the uploads (one hash over field, name and content).
func uploadHashes(ups []platform.Upload) map[string]string {
	if len(ups) == 0 {
		return nil
	}
	type fileRef struct {
		Field  string `json:"field"`
		Name   string `json:"name"`
		SHA256 string `json:"sha256"`
	}
	refs := make([]fileRef, 0, len(ups))
	for _, u := range ups {
		refs = append(refs, fileRef{Field: u.Field, Name: u.Name, SHA256: u.SHA256})
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].Field != refs[j].Field {
			return refs[i].Field < refs[j].Field
		}
		return refs[i].Name < refs[j].Name
	})
	b, _ := json.Marshal(refs)
	return map[string]string{"files": hashBytes(b)}
}

// expectedUploadHashes hashes what the manifest says the backend should derive from the uploads.
// The read-back is compared against these values; without them the check would compare the
// read-back with itself.
func expectedUploadHashes(kind string, ups []platform.Upload) map[string]string {
	out := map[string]string{}
	set := func(field string, v any) {
		if b, err := json.Marshal(v); err == nil {
			out[field] = hashBytes(b)
		}
	}
	switch kind {
	case KindModel:
		if len(ups) == 1 {
			set("file.name", ups[0].Name)
			set("file.size", len(ups[0].Data))
		}
	case KindDataset:
		total := 0
		for _, u := range ups {
			total += len(u.Data)
		}
		set("filecount", len(ups))
		set("total_filesize", total)
	}
	return out
}

// readableHashes turns the read-back into the same canonical hashes for the fields the backend
// exposes. Missing values are skipped; Verify then reports the field as missing.
func readableHashes(kind string, obj map[string]any) map[string]string {
	out := map[string]string{}
	set := func(field string, v any) {
		if b, err := json.Marshal(v); err == nil {
			out[field] = hashBytes(b)
		}
	}
	switch kind {
	case KindDataset:
		if v, ok := obj["name"]; ok {
			set("name", v)
		}
		if v, ok := obj["dataset_type"]; ok {
			set("dataset_type", v)
		}
		if v, ok := obj["filecount"]; ok {
			set("filecount", v)
		}
		if v, ok := obj["total_filesize"]; ok {
			set("total_filesize", v)
		}
	case KindModel:
		if v, ok := obj["name"]; ok {
			set("name", v)
		}
		if v, ok := obj["format"]; ok {
			set("format", v)
		}
		if v, ok := obj["file_name"]; ok {
			set("file.name", v)
		}
		if v, ok := obj["file_size"]; ok {
			set("file.size", v)
		}
	}
	return out
}

// verifyObject compares the recorded hashes with a read-back.
func verifyObject(row store.Fixture, obj map[string]any) Mismatch {
	actual := readableHashes(row.Kind, obj)
	bad := Mismatch{ManifestID: row.ManifestID, Kind: row.Kind, ObjectID: row.ObjectID}
	names := make([]string, 0, len(row.FieldSHA256))
	for name := range row.FieldSHA256 {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if !readableFields[row.Kind][name] {
			continue
		}
		want := row.FieldSHA256[name]
		got, ok := actual[name]
		if !ok {
			got = "not in the read-back"
		}
		if got != want {
			bad.Fields = append(bad.Fields, FieldMismatch{Field: name, Expected: want, Actual: got})
		}
	}
	return bad
}

// keywordsOf reads the optional keywords list. It accepts []any (JSON after the API round trip) and
// []string (tests and direct callers).
func keywordsOf(spec FixtureSpec) ([]string, error) {
	v, ok := spec.Fields["keywords"]
	if !ok {
		return nil, nil
	}
	switch list := v.(type) {
	case []string:
		return list, nil
	case []any:
		out := make([]string, 0, len(list))
		for _, e := range list {
			s, ok := e.(string)
			if !ok {
				return nil, errors.New("field \"keywords\" must be a list of texts")
			}
			out = append(out, s)
		}
		return out, nil
	}
	return nil, errors.New("field \"keywords\" must be a list")
}

func decodeObject(body string) (map[string]any, error) {
	var obj map[string]any
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		return nil, err
	}
	if obj == nil {
		return nil, errors.New("empty response")
	}
	return obj, nil
}

// jsonID reads the numeric id of a platform object as text; the read-back path takes it as is.
func jsonID(obj map[string]any) string {
	v, ok := obj["id"]
	if !ok {
		return ""
	}
	switch id := v.(type) {
	case float64:
		return fmt.Sprintf("%.0f", id)
	case json.Number:
		return id.String()
	case string:
		return id
	}
	return ""
}

func hashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ForCell selects the manifests that belong to a cell. Empty cells means every cell of the plan
// (README of the material, X9.1).
func ForCell(ms []Manifest, cell string) []Manifest {
	out := make([]Manifest, 0, len(ms))
	for _, m := range ms {
		if len(m.Cells) == 0 {
			out = append(out, m)
			continue
		}
		for _, c := range m.Cells {
			if c == cell {
				out = append(out, m)
				break
			}
		}
	}
	return out
}

// Specs resolves manifests into specs for the service: {file: …} references and the files list are
// read from dir (the material directory, below which plan.yaml and fixtures/ live). The content
// stays on this side; the service stores only its SHA-256.
func Specs(dir string, ms []Manifest) ([]FixtureSpec, error) {
	out := make([]FixtureSpec, 0, len(ms))
	for _, m := range ms {
		fields := map[string]any{}
		for name, v := range m.Fields {
			if v.File == "" {
				fields[name] = v.Literal
				continue
			}
			data, err := readMaterial(dir, v.File)
			if err != nil {
				return nil, fmt.Errorf("manifest %s: field %s: %w", m.ID, name, err)
			}
			switch name {
			case "metadata", "keywords":
				var parsed any
				if err := json.Unmarshal(data, &parsed); err != nil {
					return nil, fmt.Errorf("manifest %s: field %s is a file that must hold JSON: %w", m.ID, name, err)
				}
				fields[name] = parsed
			default:
				fields[name] = string(data)
			}
		}
		files := make([]FixtureFile, 0, len(m.Files))
		for _, path := range m.Files {
			data, err := readMaterial(dir, path)
			if err != nil {
				return nil, fmt.Errorf("manifest %s: file %s: %w", m.ID, path, err)
			}
			field := "files"
			if m.Kind == KindModel {
				field = "modelfile"
			}
			files = append(files, FixtureFile{Field: field, Name: filepath.Base(path), SHA256: hashBytes(data), Data: data})
		}
		out = append(out, FixtureSpec{ID: m.ID, Kind: m.Kind, Owner: m.Owner, Fields: fields, Files: files, Attack: m.Attack})
	}
	return out, nil
}

// readMaterial reads a path below the material directory and refuses paths that leave it.
func readMaterial(dir, rel string) ([]byte, error) {
	if rel == "" || filepath.IsAbs(rel) || strings.HasPrefix(rel, "/") {
		return nil, fmt.Errorf("%q is not a relative path", rel)
	}
	clean := filepath.Clean(rel)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("%q leaves the material directory", rel)
	}
	return os.ReadFile(filepath.Join(dir, clean))
}

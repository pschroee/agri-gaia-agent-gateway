// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package assess

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"agw/internal/platform"
)

// The tests of the training check need a platform: the check downloads datasets and models and creates
// the training dataset. fakePlatform is an HTTP server that answers the few paths a check reads and
// records every call, so that a test can see what went out and which images a check measured.

type fakePlatform struct {
	t   *testing.T
	mu  sync.Mutex
	srv *httptest.Server

	datasets map[int64]datasetFixture
	models   map[int64]modelFixture
	logs     map[int64]string
	created  []createdDataset
	calls    []string
	newID    int64
}

type datasetFixture struct {
	name  string
	files map[string]string // file name -> content
	zip   []byte            // the archive of GET /datasets/<id>/download
}

type modelFixture struct {
	name   string
	format string
	file   []byte // what GET /models/<id>/download answers (a file or a ZIP)
}

// createdDataset is one POST /datasets: its form, the file names and the bytes it carried.
type createdDataset struct {
	form  map[string][]string
	names []string
	data  map[string][]byte
}

// testImage is one image of a test dataset: its file name, its content and the labels of its tags.
type testImage struct {
	Name string
	Data string
	Tags []string
	// Shape: "box", "polygon", … — an image with a shape is no classification (R1.2).
	Shape string
}

func newFakePlatform(t *testing.T) *fakePlatform {
	t.Helper()
	f := &fakePlatform{t: t, datasets: map[int64]datasetFixture{}, models: map[int64]modelFixture{},
		logs: map[int64]string{}, newID: 42}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

// client is the platform client the check uses: a subject token, so no login is needed.
func (f *fakePlatform) client() *platform.Client {
	f.t.Helper()
	c, err := platform.New(platform.Config{
		APIURL:  f.srv.URL,
		Subject: func(context.Context, string) (string, error) { return "test-token", nil },
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return c
}

// dataset registers a dataset with an archive the platform would answer.
func (f *fakePlatform) dataset(id int64, name string, images []testImage) {
	files := map[string]string{}
	for _, im := range images {
		files[im.Name] = im.Data
	}
	f.datasets[id] = datasetFixture{name: name, files: files, zip: datasetZip(id, images)}
}

// model registers a model: its name, its format and the bytes of GET /models/<id>/download.
func (f *fakePlatform) model(id int64, name, format string, file []byte) {
	f.models[id] = modelFixture{name: name, format: format, file: file}
}

// trainLog registers the log of a training container (GET /train/containers/<id>/logs).
func (f *fakePlatform) trainLog(id int64, log string) { f.logs[id] = log }

func (f *fakePlatform) record(call string) {
	f.mu.Lock()
	f.calls = append(f.calls, call)
	f.mu.Unlock()
}

// called reports whether a call (method and path) went out.
func (f *fakePlatform) called(method, path string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c == method+" "+path {
			return true
		}
	}
	return false
}

// creations are the POST /datasets of the test.
func (f *fakePlatform) creations() []createdDataset {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]createdDataset(nil), f.created...)
}

func (f *fakePlatform) handle(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	f.record(r.Method + " " + path)
	parts := strings.Split(strings.Trim(path, "/"), "/")
	switch {
	case r.Method == http.MethodPost && path == "/datasets":
		f.createdDataset(w, r)
	case len(parts) >= 2 && parts[0] == "datasets":
		id, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			http.Error(w, "bad id", http.StatusBadRequest)
			return
		}
		ds, ok := f.datasets[id]
		if !ok {
			http.Error(w, "no such dataset", http.StatusNotFound)
			return
		}
		if len(parts) >= 3 && parts[2] == "download" {
			w.Header().Set("Content-Type", "application/zip")
			w.Write(ds.zip)
			return
		}
		classes := []string{}
		for _, im := range testAnnotationImages(ds.files) {
			for _, c := range im.Tags {
				if !containsString(classes, c) {
					classes = append(classes, c)
				}
			}
		}
		writeTestJSON(w, map[string]any{"id": id, "name": ds.name, "filecount": len(ds.files),
			"dataset_type": "AgriImageDataResource", "annotation_labels": classes})
	case len(parts) >= 2 && parts[0] == "train" && len(parts) >= 4 && parts[1] == "containers" && parts[3] == "logs":
		id, err := strconv.ParseInt(parts[2], 10, 64)
		if err != nil {
			http.Error(w, "bad id", http.StatusBadRequest)
			return
		}
		log, ok := f.logs[id]
		if !ok {
			http.Error(w, "no such training container", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, log)
	case len(parts) >= 2 && parts[0] == "models":
		id, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			http.Error(w, "bad id", http.StatusBadRequest)
			return
		}
		md, ok := f.models[id]
		if !ok {
			http.Error(w, "no such model", http.StatusNotFound)
			return
		}
		if len(parts) >= 3 && parts[2] == "download" {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Write(md.file)
			return
		}
		writeTestJSON(w, map[string]any{"id": id, "name": md.name, "format": md.format})
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

// createdDataset reads one POST /datasets: the form and every file of the multipart body.
func (f *fakePlatform) createdDataset(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		http.Error(w, "not multipart: "+err.Error(), http.StatusBadRequest)
		return
	}
	cd := createdDataset{form: map[string][]string{}, data: map[string][]byte{}}
	for k, v := range r.MultipartForm.Value {
		cd.form[k] = v
	}
	for _, fh := range r.MultipartForm.File["files"] {
		f, err := fh.Open()
		if err != nil {
			http.Error(w, "unreadable file", http.StatusBadRequest)
			return
		}
		b, err := io.ReadAll(f)
		f.Close()
		if err != nil {
			http.Error(w, "unreadable file", http.StatusBadRequest)
			return
		}
		cd.names = append(cd.names, fh.Filename)
		cd.data[fh.Filename] = b
	}
	f.mu.Lock()
	f.created = append(f.created, cd)
	f.mu.Unlock()
	writeTestJSON(w, map[string]any{"id": f.newID, "name": firstOf(cd.form["name"])})
}

func writeTestJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// datasetZip builds an archive as the platform writes it: the images under datasets/<id>/ and the CVAT
// annotation under datasets/<id>/annotations/annotations.xml (datasets/cvat_dataset.py:36).
func datasetZip(id int64, images []testImage) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	add := func(name, content string) {
		w, err := zw.Create(name)
		if err != nil {
			panic(err)
		}
		_, _ = io.WriteString(w, content)
	}
	for _, im := range images {
		add(fmt.Sprintf("datasets/%d/%s", id, im.Name), im.Data)
	}
	add(fmt.Sprintf("datasets/%d/annotations/annotations.xml", id), string(testAnnotation(images)))
	_ = zw.Close()
	return buf.Bytes()
}

// testAnnotation writes a CVAT annotation with one <tag> per label (and a shape when the test asks for
// one), as the training templates read it.
func testAnnotation(images []testImage) []byte {
	classes := []string{}
	for _, im := range images {
		for _, c := range im.Tags {
			if !containsString(classes, c) {
				classes = append(classes, c)
			}
		}
	}
	sort.Strings(classes)
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?><annotations><version>1.1</version><meta><task><id>1</id><name>test</name><labels>`)
	for _, c := range classes {
		fmt.Fprintf(&b, "<label><name>%s</name></label>", c)
	}
	b.WriteString(`</labels></task></meta>`)
	for i, im := range images {
		fmt.Fprintf(&b, `<image id="%d" name="%s" width="28" height="28">`, i, im.Name)
		for _, t := range im.Tags {
			fmt.Fprintf(&b, `<tag label="%s" source="manual"></tag>`, t)
		}
		if im.Shape != "" {
			fmt.Fprintf(&b, `<%s label="%s" occluded="0" points="1,1;2,2"></%s>`, im.Shape, firstOfOr(im.Tags, "x"), im.Shape)
		}
		b.WriteString(`</image>`)
	}
	b.WriteString(`</annotations>`)
	return []byte(b.String())
}

// testAnnotationImages reads the tags of an annotation back (the fake platform reports the classes of a
// dataset from them).
func testAnnotationImages(files map[string]string) []testImage {
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]testImage, 0, len(names))
	for _, n := range names {
		out = append(out, testImage{Name: n, Data: files[n]})
	}
	return out
}

// tarImageNames reads the names of the images out of the tar a check sent to its container: the images
// are stored as images/<sha-256>.<ext>, so a test sees which images were measured without ever seeing a
// file name of the user.
func tarImageNames(t *testing.T, blob []byte) []string {
	t.Helper()
	var out []string
	tr := tar.NewReader(bytes.NewReader(blob))
	for {
		hdr, err := tr.Next()
		if err != nil {
			break
		}
		if strings.HasPrefix(hdr.Name, "images/") {
			out = append(out, hdr.Name)
		}
	}
	return out
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func firstOf(v []string) string {
	if len(v) == 0 {
		return ""
	}
	return v[0]
}

func firstOfOr(v []string, def string) string {
	if len(v) == 0 {
		return def
	}
	return v[0]
}

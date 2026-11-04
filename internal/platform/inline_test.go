// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

import (
	"encoding/base64"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func build(t *testing.T, tool, args string) (Request, error) {
	t.Helper()
	tl, ok := Lookup(tool)
	if !ok {
		t.Fatalf("no tool %s", tool)
	}
	return tl.Build(json.RawMessage(args))
}

// A run takes its files as content: the call is the one of sandbox files, the content travels with it (BM12).
func TestNotebookRunWithContent(t *testing.T) {
	args := `{"contents":{"main.py":"print(open('/home/jovyan/work/datasets/3/NOTES.txt').read())","helper.py":"x = 1"},"entry":"main.py","dataset_ids":["3"]}`
	req, err := build(t, "notebook_run", args)
	if err != nil {
		t.Fatal(err)
	}
	if req.Method != "POST" || req.Path != NotebookPrefix+"/runs" || req.Form["entry"][0] != "main.py" || req.Form["dataset_ids"][0] != "3" {
		t.Fatalf("call: %+v", req)
	}
	if len(req.Files) != 2 || len(req.Inline) != 2 {
		t.Fatalf("files %v, inline %d", req.Files, len(req.Inline))
	}
	for i, f := range req.Files {
		if f.Field != "file" || f.Path != req.Inline[i].Path || !strings.HasPrefix(f.Path, InlineDir+"/") {
			t.Errorf("file %d: %+v vs %+v", i, f, req.Inline[i])
		}
	}
	if base := baseName(req.Files[0].Path); base != "helper.py" {
		t.Errorf("files sorted by name: %s first", base)
	}
	// The same arguments give the same paths: the plan step and the call it covers agree.
	again, err := build(t, "notebook_run", args)
	if err != nil || !reflect.DeepEqual(req, again) {
		t.Errorf("not repeatable: %v", err)
	}
	// Other content, other directory.
	other, _ := build(t, "notebook_run", strings.Replace(args, "x = 1", "x = 2", 1))
	if other.Files[0].Path == req.Files[0].Path {
		t.Error("different content, same path")
	}
	// Paths and content together.
	mixed, err := build(t, "notebook_run", `{"files":["/workspace/a.py"],"contents":{"b.py":"1"},"entry":"a.py"}`)
	if err != nil || len(mixed.Files) != 2 || len(mixed.Inline) != 1 || mixed.Files[0].Path != "/workspace/a.py" {
		t.Errorf("mixed: %+v %v", mixed, err)
	}
}

func TestNotebookRunContentRefusals(t *testing.T) {
	for _, c := range []struct{ name, args, want string }{
		{"nothing", `{"entry":"a.py"}`, "at least one file"},
		{"entry missing from the files", `{"contents":{"b.py":"1"},"entry":"a.py"}`, "not one of the files"},
		{"bad ending", `{"contents":{"model.onnx":"x"},"entry":"model.onnx"}`, "allowed are"},
		{"bad name", `{"contents":{"../a.py":"1"},"entry":"a.py"}`, "invalid"},
		{"name with a directory", `{"contents":{"d/a.py":"1"},"entry":"a.py"}`, "invalid"},
		{"not text", `{"contents":{"a.py":5},"entry":"a.py"}`, "must be a string"},
		{"not an object", `{"contents":["a.py"],"entry":"a.py"}`, "object from file name"},
		{"binary content has no place here", `{"contents_base64":{"a.py":"AA=="},"entry":"a.py"}`, `unknown argument "contents_base64"`},
		{"duplicate with a path", `{"files":["/workspace/a.py"],"contents":{"a.py":"1"},"entry":"a.py"}`, "two files named"},
		{"too large", `{"contents":{"a.py":"` + strings.Repeat("x", InlineMaxBytes+1) + `"},"entry":"a.py"}`, "larger than"},
	} {
		if _, err := build(t, "notebook_run", c.args); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
	}
}

// An upload takes text and binary files as content and builds the call of sandbox files.
func TestUploadDatasetWithContent(t *testing.T) {
	png := base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\nabc"))
	req, err := build(t, "upload_dataset", `{"name":"copy","description":"d","contents":{"NOTES.txt":"first line\nsecond"},"contents_base64":{"a.png":"`+png+`"}}`)
	if err != nil {
		t.Fatal(err)
	}
	if req.Method != "POST" || req.Path != "/datasets" || len(req.Files) != 2 || len(req.Inline) != 2 {
		t.Fatalf("call: %+v", req)
	}
	got := map[string]string{}
	for _, f := range req.Inline {
		got[baseName(f.Path)] = string(f.Data)
	}
	if got["NOTES.txt"] != "first line\nsecond" || got["a.png"] != "\x89PNG\r\n\x1a\nabc" {
		t.Errorf("content: %q", got)
	}
	// The annotation file may be one of the files given as content.
	req, err = build(t, "upload_dataset", `{"name":"x","description":"d","contents":{"a.png":"1","annotations.xml":"<a/>"},"annotation_file":"annotations.xml"}`)
	if err != nil || req.Form["includes_annotation_file"][0] != "true" || baseName(req.Files[len(req.Files)-1].Path) != "annotations.xml" {
		t.Errorf("annotation: %+v %v", req, err)
	}
	for _, c := range []struct{ name, args, want string }{
		{"nothing", `{"name":"x","description":"d"}`, "at least one file"},
		{"base64 broken", `{"name":"x","description":"d","contents_base64":{"a.png":"!!"}}`, "not valid base64"},
		{"twice", `{"name":"x","description":"d","contents":{"a":"1"},"contents_base64":{"a":"AA=="}}`, "given twice"},
	} {
		_, err := build(t, "upload_dataset", c.args)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
	}
}

func TestNotebookFetchInline(t *testing.T) {
	req, err := build(t, "notebook_fetch", `{"run_id":"r20260101000000-0a1b","path":"out/a.png","inline":true}`)
	if err != nil || !req.View || req.Path != NotebookPrefix+"/runs/r20260101000000-0a1b/file" || req.Query["path"] != "out/a.png" {
		t.Errorf("inline: %+v %v", req, err)
	}
	// As text (command line, REST query) and by default.
	req, err = build(t, "notebook_fetch", `{"run_id":"r20260101000000-0a1b","path":"a.txt","inline":"true"}`)
	if err != nil || !req.View {
		t.Errorf("inline as text: %+v %v", req, err)
	}
	req, err = build(t, "notebook_fetch", `{"run_id":"r20260101000000-0a1b","path":"a.txt"}`)
	if err != nil || req.View {
		t.Errorf("default: %+v %v", req, err)
	}
	if _, err := build(t, "notebook_fetch", `{"run_id":"r20260101000000-0a1b","path":"a.txt","inline":"maybe"}`); err == nil {
		t.Error("inline maybe")
	}
}

func TestViewFile(t *testing.T) {
	body, img, why := ViewFile(&FileData{Name: "NOTES.txt", Data: []byte("Notes on the garage\n")})
	if why != "" || img != nil || body != "Notes on the garage\n" {
		t.Errorf("text: %q %v %q", body, img, why)
	}
	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 100)...)
	body, img, why = ViewFile(&FileData{Name: "f.png", Data: png})
	if why != "" || img == nil || img.MIME != "image/png" || !strings.Contains(body, "f.png") {
		t.Errorf("png: %q %v %q", body, img, why)
	}
	jpg := []byte{0xFF, 0xD8, 0xFF, 0xE0, 1, 2, 3}
	if _, img, _ := ViewFile(&FileData{Name: "f.jpg", Data: jpg}); img == nil || img.MIME != "image/jpeg" {
		t.Error("jpeg")
	}
	// The limits of what an answer shows.
	if _, _, why := ViewFile(&FileData{Name: "big.txt", Data: []byte(strings.Repeat("a", ViewTextMax+1))}); !strings.Contains(why, "at most 64 KiB") {
		t.Errorf("long text: %q", why)
	}
	bigImg := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, ViewImageMax)...)
	if _, _, why := ViewFile(&FileData{Name: "big.png", Data: bigImg}); !strings.Contains(why, "smaller image") {
		t.Errorf("large image: %q", why)
	}
	if _, _, why := ViewFile(&FileData{Name: "m.onnx", Data: []byte{0x08, 0x07, 0x00, 0xff, 0xfe}}); !strings.Contains(why, "neither text nor an image") {
		t.Errorf("binary: %q", why)
	}
}

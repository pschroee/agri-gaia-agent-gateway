// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
)

// uploadReq builds a dataset upload with n files spread over the given number of folders under /workspace/up
// (file i lives in folder f<i mod folders>), plus the annotation file as the last one when withAnnotation is set.
// The uploads carry their real hashes, as the socket reads them.
func uploadReq(n, folders int, withAnnotation bool) Request {
	r := Request{Method: "POST", Path: "/datasets", Form: map[string][]string{"name": {"d"}}}
	add := func(p string, data string) {
		sum := sha256.Sum256([]byte(data))
		r.Files = append(r.Files, File{Field: "files", Path: p})
		r.Uploads = append(r.Uploads, Upload{Field: "files", Name: p[strings.LastIndex(p, "/")+1:], Data: []byte(data), SHA256: hex.EncodeToString(sum[:])})
	}
	for i := 0; i < n; i++ {
		add(fmt.Sprintf("/workspace/up/f%02d/img_%04d.jpg", i%folders, i), fmt.Sprintf("image %d", i))
	}
	if withAnnotation {
		add("/workspace/up/annotations.xml", "<annotations/>")
		r.Form["includes_annotation_file"] = []string{"true"}
	}
	return r
}

func TestManifestOfFoldersAndExtensions(t *testing.T) {
	r := uploadReq(500, 5, true)
	m, err := ManifestOf(r)
	if err != nil {
		t.Fatal(err)
	}
	if m.Files != 501 || m.Root != "/workspace/up" || len(m.Folders) != 5 || len(m.Entries) != 501 {
		t.Fatalf("manifest: files=%d root=%q folders=%d entries=%d", m.Files, m.Root, len(m.Folders), len(m.Entries))
	}
	if m.Folders[0].Path != "f00" || m.Folders[0].Files != 100 {
		t.Errorf("first folder: %+v", m.Folders[0])
	}
	if len(m.Extensions) != 1 || m.Extensions[0].Ext != ".jpg" || m.Extensions[0].Files != 500 {
		t.Errorf("extensions (the annotation file is not counted): %+v", m.Extensions)
	}
	if m.Annotation == nil || m.Annotation.Path != "/workspace/up/annotations.xml" {
		t.Errorf("annotation: %+v", m.Annotation)
	}
	if s := m.Summary(); s.Entries != nil || s.SHA256 != m.SHA256 {
		t.Errorf("the summary keeps the hash and drops the entries: %+v", s.SHA256)
	}
}

func TestManifestLinesAndHash(t *testing.T) {
	r := uploadReq(3, 1, false)
	m, err := ManifestOf(r)
	if err != nil {
		t.Fatal(err)
	}
	first := fmt.Sprintf("files\t/workspace/up/f00/img_0000.jpg\t%d\t%s\n", len(r.Uploads[0].Data), r.Uploads[0].SHA256)
	if !strings.HasPrefix(m.Lines(), first) || strings.Count(m.Lines(), "\n") != 3 {
		t.Fatalf("lines:\n%s", m.Lines())
	}
	sum := sha256.Sum256([]byte(m.Lines()))
	if m.SHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("hash %s is not the SHA-256 over the lines", m.SHA256)
	}
	r.Uploads[0], r.Uploads[1] = r.Uploads[1], r.Uploads[0]
	r.Files[0], r.Files[1] = r.Files[1], r.Files[0]
	swapped, _ := ManifestOf(r)
	if swapped.SHA256 == m.SHA256 {
		t.Error("the order of the files is part of the hash")
	}
}

func TestManifestRejectsControlCharacters(t *testing.T) {
	for _, p := range []string{"/workspace/a\tb.jpg", "/workspace/a\nb.jpg", "/workspace/a\rb.jpg"} {
		r := uploadReq(1, 1, false)
		r.Files[0].Path = p
		if _, err := ManifestOf(r); err == nil {
			t.Errorf("ManifestOf accepted %q", p)
		}
		if _, err := normalize(Request{Method: "POST", Path: "/datasets", Form: map[string][]string{"name": {"d"}},
			Files: []File{{Field: "files", Path: p}}}, true); err == nil || !strings.Contains(err.Error(), "control characters") {
			t.Errorf("normalize on %q: %v", p, err)
		}
	}
}

func TestCheckManifestDetectsChangedBytes(t *testing.T) {
	r := uploadReq(8, 2, false)
	m, _ := ManifestOf(r)
	if err := CheckManifest(r, m.SHA256); err != nil {
		t.Fatalf("unchanged files: %v", err)
	}
	if err := CheckManifest(r, strings.Repeat("0", 64)); err == nil {
		t.Error("a hash other than the approved one must fail")
	}
	if err := CheckManifest(r, ""); err == nil {
		t.Error("an empty approved hash must fail")
	}
	r.Uploads[3].Data = append([]byte(nil), r.Uploads[3].Data...)
	r.Uploads[3].Data[0] ^= 1 // Upload.SHA256 still names the old bytes
	if err := CheckManifest(r, m.SHA256); err != errFilesChanged {
		t.Errorf("a changed byte: %v", err)
	}
}

func TestDescribeSummarisesManyFiles(t *testing.T) {
	r := uploadReq(500, 5, true)
	d := r.Describe()
	m, _ := ManifestOf(r)
	if n := len([]rune(d)); n > 4000 || !strings.Contains(d, m.SHA256) || strings.Contains(d, "img_0001.jpg") {
		t.Fatalf("preview of 500 files (%d characters):\n%s", n, d)
	}
	if !strings.Contains(d, "Annotation file: files: /workspace/up/annotations.xml") || !strings.Contains(d, "File types: .jpg 500") {
		t.Errorf("preview:\n%s", d)
	}
	big := uploadReq(2000, 60, false)
	d = big.Describe()
	if n := len([]rune(d)); n > 16000 || !strings.Contains(d, "… and 10 more folders") {
		t.Fatalf("preview of 2 000 files (%d characters):\n%s", n, d)
	}
}

func TestDescribeListsFewFiles(t *testing.T) {
	r := uploadReq(ListedFiles, 2, false)
	d := r.Describe()
	if strings.Count(d, "sha256 ") != ListedFiles || !strings.Contains(d, "10 files, ") || !strings.Contains(d, "files: img_0000.jpg  7 bytes  sha256 "+r.Uploads[0].SHA256) {
		t.Fatalf("up to %d files stay listed one by one:\n%s", ListedFiles, d)
	}
}

func summaryFilesOf(r Request) []SummaryFile { return SummaryCallOf("upload_dataset", r).Files }

func TestSummaryFileLinesMany(t *testing.T) {
	r := uploadReq(500, 5, true)
	m, _ := ManifestOf(r)
	got := summarize(t, nil, SummaryCallOf("upload_dataset", r))
	wantLines(t, got, `Files: 501 (`, `in 5 folders under "/workspace/up"`, "File types: .jpg 500",
		`Annotation file: "/workspace/up/annotations.xml" (`, "Manifest sha256 "+m.SHA256)
	if strings.Contains(got, "img_0001.jpg") {
		t.Errorf("the files must not be listed one by one:\n%s", got)
	}
}

func TestSummaryFileLinesPlanStep(t *testing.T) {
	r := uploadReq(40, 4, false)
	files := summaryFilesOf(r)
	for i := range files {
		files[i].Size, files[i].SHA256 = -1, ""
	}
	got := summarize(t, nil, SummaryCall{Tool: "upload_dataset", Method: "POST", Path: "/datasets", Form: r.Form, Files: files})
	wantLines(t, got, `Files: 40 in 4 folders under "/workspace/up" (not read yet)`, "File types: .jpg 40")
	if strings.Contains(got, "Manifest sha256") {
		t.Errorf("files that were not read have no manifest line:\n%s", got)
	}
}

func TestUploadDatasetDuplicateNames(t *testing.T) {
	tool, ok := Lookup("upload_dataset")
	if !ok {
		t.Fatal("upload_dataset missing")
	}
	_, err := tool.Build([]byte(`{"name":"d","description":"x","files":["/workspace/a/1.jpg","/workspace/b/1.jpg"]}`))
	if err == nil || !strings.Contains(err.Error(), "two files have the same name") {
		t.Fatalf("duplicate names: %v", err)
	}
	if _, err := tool.Build([]byte(`{"name":"d","description":"x","files":["/workspace/a/1.jpg"],"annotation_file":"/workspace/b/1.jpg"}`)); err != nil {
		t.Errorf("the annotation file may share a name: %v", err)
	}
}

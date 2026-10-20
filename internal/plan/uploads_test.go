// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package plan

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"agw/internal/platform"
	"agw/internal/store"
)

func sumOf(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// uploadSpec is an upload_dataset step of n files in five folders, pinned as the socket does.
func uploadSpec(t *testing.T, n int) StepSpec {
	t.Helper()
	var paths []string
	spec := StepSpec{Tool: "upload_dataset"}
	for i := 0; i < n; i++ {
		paths = append(paths, fmt.Sprintf("/workspace/pu/%c/img_%04d.png", 'a'+i%5, i))
		data := fmt.Sprintf("image %d", i)
		spec.FileSHA256 = append(spec.FileSHA256, sumOf(data))
		spec.FileBytes = append(spec.FileBytes, int64(len(data)))
	}
	args, _ := json.Marshal(map[string]any{"name": "pu172", "description": "x", "files": paths})
	spec.Args = args
	return spec
}

func uploadStep(t *testing.T, n int) store.PlanStep {
	t.Helper()
	st, _, _, err := BuildStep(uploadSpec(t, n), 1)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestBuildStepFileBytes(t *testing.T) {
	spec := uploadSpec(t, 3)
	spec.FileBytes = spec.FileBytes[:2]
	if _, _, _, err := BuildStep(spec, 1); err == nil || !strings.Contains(err.Error(), "file_bytes") {
		t.Fatalf("wrong count: %v", err)
	}
	spec = uploadSpec(t, 3)
	spec.FileBytes[1] = -1
	if _, _, _, err := BuildStep(spec, 1); err == nil || !strings.Contains(err.Error(), "negative") {
		t.Fatalf("negative size: %v", err)
	}
	spec = uploadSpec(t, 3)
	spec.FileBytes = nil
	st, _, _, err := BuildStep(spec, 1)
	if err != nil || len(st.Files) != 3 || st.Files[0].Bytes != nil {
		t.Fatalf("without sizes: %+v %v", st.Files, err)
	}
	st = uploadStep(t, 3)
	if st.Files[2].Bytes == nil || *st.Files[2].Bytes != int64(len("image 2")) {
		t.Fatalf("size not stored: %+v", st.Files)
	}
}

// The manifest of the step is the manifest the single approval would show for the same call.
func TestStepManifestMatchesManifestOf(t *testing.T) {
	spec := uploadSpec(t, 40)
	st, req, _, err := BuildStep(spec, 1)
	if err != nil {
		t.Fatal(err)
	}
	for i, f := range req.Files {
		data := fmt.Sprintf("image %d", i)
		req.Uploads = append(req.Uploads, platform.Upload{Field: f.Field, Name: "x", Data: []byte(data), SHA256: sumOf(data)})
	}
	want, err := platform.ManifestOf(req)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := StepManifest(st)
	if !ok || got.SHA256 != want.SHA256 || got.Files != 40 || got.Bytes != want.Bytes {
		t.Fatalf("step manifest %+v ok=%v, want hash %s", got.Summary(), ok, want.SHA256)
	}
	if err := platform.CheckManifest(req, got.SHA256); err != nil {
		t.Fatalf("the step's manifest does not bind the live bytes: %v", err)
	}
}

func TestStepManifestOldRow(t *testing.T) {
	spec := uploadSpec(t, 20)
	spec.FileBytes = nil
	st, _, _, err := BuildStep(spec, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := StepManifest(st); ok {
		t.Fatal("a row without sizes has no manifest")
	}
	out := Preview("t", []store.PlanStep{st}, nil)
	if strings.Count(out, "   file ") != 20 || strings.Contains(out, "manifest sha256") {
		t.Fatalf("an old row keeps its single lines:\n%s", out)
	}
	if got := WithUploads(store.Plan{Steps: []store.PlanStep{st}}); got.Steps[0].Uploads != nil || len(got.Steps[0].Files) != 20 {
		t.Fatal("an old row keeps its files in the output")
	}
}

func TestPreviewPlanWith500Files(t *testing.T) {
	up := uploadStep(t, 500)
	split := store.PlanStep{Idx: 2, Effect: EffectWrite, Method: "POST", Path: "/datasets/{{step1.id}}/split"}
	steps := []store.PlanStep{up, split}
	out := Preview("Upload and split", steps, nil)
	if n := len([]rune(out)); n >= 16000 {
		t.Fatalf("preview of %d characters", n)
	}
	m, _ := StepManifest(up)
	if !strings.Contains(out, "manifest sha256 "+m.SHA256) || strings.Contains(out, "img_0100.png") {
		t.Fatalf("preview:\n%s", out)
	}
}

// Up to ListedFiles files the preview is the one before issue #172: one line per file.
func TestPreviewPlanSmallUploadUnchanged(t *testing.T) {
	st := uploadStep(t, platform.ListedFiles)
	out := Preview("t", []store.PlanStep{st}, nil)
	var want strings.Builder
	for _, f := range st.Files {
		fmt.Fprintf(&want, "   file %s: %s  sha256 %s\n", f.Field, f.Path, f.SHA256)
	}
	if !strings.Contains(out, want.String()) || strings.Contains(out, "manifest sha256") {
		t.Fatalf("small upload preview changed:\n%s", out)
	}
}

func TestSummaryCallUsesPinnedSizes(t *testing.T) {
	c := SummaryCall(uploadStep(t, 12))
	if c.Files[0].Size != int64(len("image 0")) {
		t.Fatalf("size %d", c.Files[0].Size)
	}
	spec := uploadSpec(t, 12)
	spec.FileBytes = nil
	st, _, _, _ := BuildStep(spec, 1)
	if SummaryCall(st).Files[0].Size != -1 {
		t.Fatal("an old row stays not read yet")
	}
}

func TestMatchesFilesSize(t *testing.T) {
	const h = "1111111111111111111111111111111111111111111111111111111111111111"
	n := int64(5)
	st := store.PlanStep{Method: "POST", Path: "/models", Files: []store.PlanFile{{Field: "modelfile", Path: "/workspace/m.onnx", SHA256: h, Bytes: &n}}}
	r := platform.Request{Method: "POST", Path: "/models", Files: []platform.File{{Field: "modelfile", Path: "/workspace/m.onnx"}}}
	r.Uploads = []platform.Upload{{Field: "modelfile", Name: "m.onnx", SHA256: h, Data: []byte("12345")}}
	if m, why := Matches(st, r, resolveTo("1")); !m {
		t.Fatalf("same size does not match: %s", why)
	}
	r.Uploads[0].Data = []byte("123456")
	if m, why := Matches(st, r, resolveTo("1")); m || why != "files" {
		t.Fatalf("same hash, other length: matched=%v why=%q", m, why)
	}
}

// The plan on its way to the UI carries the summary, not the list; the stored plan is not changed.
func TestWithUploads(t *testing.T) {
	up := uploadStep(t, 500)
	other := store.PlanStep{Idx: 2, Method: "PATCH", Path: "/datasets/1"}
	p := store.Plan{Steps: []store.PlanStep{up, other}}
	out := WithUploads(p)
	var m platform.UploadManifest
	if err := json.Unmarshal(out.Steps[0].Uploads, &m); err != nil || m.Files != 500 || m.SHA256 == "" || len(m.Entries) != 0 {
		t.Fatalf("uploads: %s %v", out.Steps[0].Uploads, err)
	}
	if len(out.Steps[0].Files) != 0 || out.Steps[1].Uploads != nil {
		t.Fatalf("output: %+v", out.Steps)
	}
	if len(p.Steps[0].Files) != 500 || p.Steps[0].Uploads != nil {
		t.Fatal("the stored plan was changed")
	}
}

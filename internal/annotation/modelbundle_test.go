// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package annotation

import (
	"archive/zip"
	"bytes"
	"strings"
	"testing"
)

// zipOf builds an archive of the given files, in the given order.
func zipOf(t *testing.T, files ...[2]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range files {
		w, err := zw.Create(f[0])
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(f[1])); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// A model download is the file itself when the model holds one object; nothing is unpacked then.
func TestOnnxFromBundlePlainFile(t *testing.T) {
	raw := []byte("ONNX-MODEL")
	got, err := onnxFromBundle(raw, 1<<20)
	if err != nil || !bytes.Equal(got, raw) {
		t.Errorf("a plain file: %q %v", got, err)
	}
	// A short file that happens to start like an archive is no archive either: the reader decides.
	if _, err := onnxFromBundle([]byte("PK\x03\x04no archive"), 1<<20); err == nil ||
		!strings.Contains(err.Error(), "cannot be read") {
		t.Errorf("a broken archive: %v", err)
	}
}

// A bundle that a training leaves behind (the ONNX file and the training log) gives the ONNX file.
func TestOnnxFromBundleExtractsTheOnnxFile(t *testing.T) {
	raw := zipOf(t,
		[2]string{"models/19/model.onnx", "ONNX-MODEL"},
		[2]string{"models/19/train.log", "epoch 1"},
	)
	got, err := onnxFromBundle(raw, 1<<20)
	if err != nil || string(got) != "ONNX-MODEL" {
		t.Errorf("bundle: %q %v", got, err)
	}
	// The case of the extension does not matter, the entry may sit in a folder.
	raw = zipOf(t, [2]string{"MODEL.ONNX", "ONNX-MODEL"})
	if got, err := onnxFromBundle(raw, 1<<20); err != nil || string(got) != "ONNX-MODEL" {
		t.Errorf("upper case: %q %v", got, err)
	}
}

// A bundle without an ONNX file, and one with several, are refused with the names of what it holds.
func TestOnnxFromBundleRefuses(t *testing.T) {
	raw := zipOf(t, [2]string{"models/19/model.pt", "PICKLE"}, [2]string{"train.log", "epoch 1"})
	_, err := onnxFromBundle(raw, 1<<20)
	if err == nil || !strings.Contains(err.Error(), "without an ONNX file") ||
		!strings.Contains(err.Error(), "models/19/model.pt") {
		t.Errorf("no ONNX file: %v", err)
	}
	raw = zipOf(t, [2]string{"a.onnx", "A"}, [2]string{"b.onnx", "B"})
	_, err = onnxFromBundle(raw, 1<<20)
	if err == nil || !strings.Contains(err.Error(), "with 2 ONNX files") ||
		!strings.Contains(err.Error(), "a.onnx") || !strings.Contains(err.Error(), "b.onnx") {
		t.Errorf("two ONNX files: %v", err)
	}
	// An archive of directories only carries no file at all.
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	if _, err := zw.Create("models/19/"); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := onnxFromBundle(buf.Bytes(), 1<<20); err == nil ||
		!strings.Contains(err.Error(), "no file") {
		t.Errorf("an empty archive: %v", err)
	}
}

// The bound of the configuration applies to the ONNX file, not to the archive around it: a file over
// the bound is refused, and the bound of the read holds even when the header lies.
func TestOnnxFromBundleSize(t *testing.T) {
	big := strings.Repeat("x", 2<<20)
	raw := zipOf(t, [2]string{"model.onnx", big}, [2]string{"train.log", "epoch 1"})
	if got, err := onnxFromBundle(raw, 4<<20); err != nil || len(got) != len(big) {
		t.Errorf("inside the bound: %d %v", len(got), err)
	}
	if _, err := onnxFromBundle(raw, 1<<20); err == nil || !strings.Contains(err.Error(), "at most 1 MiB") {
		t.Errorf("over the bound: %v", err)
	}
	// An empty ONNX file is no model.
	raw = zipOf(t, [2]string{"model.onnx", ""})
	if _, err := onnxFromBundle(raw, 4096); err == nil || !strings.Contains(err.Error(), "is empty") {
		t.Errorf("an empty ONNX file: %v", err)
	}
}

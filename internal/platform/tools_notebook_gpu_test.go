// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

import (
	"strings"
	"testing"
)

func TestNotebookShapeLine(t *testing.T) {
	if got := DefaultNotebookShape.Line(); got != "2 CPUs, 8 GB, no GPU, no internet, no platform access" {
		t.Errorf("default: %q", got)
	}
	s := NotebookShape{CPUs: 2, MemGB: 8, GPU: true}
	if got := s.Line(); got != "2 CPUs, 8 GB, a GPU (shared), no internet, no platform access" {
		t.Errorf("gpu: %q", got)
	}
}

func TestNotebookPreviewNamesGPU(t *testing.T) {
	r := Request{Method: "POST", Path: NotebookPrefix + "/runs", Form: RunForm(RunBody{Entry: "a.py", TimeoutMinutes: 5}),
		Uploads: []Upload{{Field: "file", Name: "a.py", Data: []byte("print(1)\n"), SHA256: "aa"}}}
	with, err := NotebookPreview(r, NotebookShape{CPUs: 2, MemGB: 8, GPU: true})
	if err != nil || !strings.Contains(with, "a GPU (shared)") {
		t.Errorf("with GPU: %v\n%s", err, with)
	}
	without, err := NotebookPreview(r, DefaultNotebookShape)
	if err != nil || !strings.Contains(without, "no GPU") || strings.Contains(without, "a GPU (shared)") {
		t.Errorf("without GPU: %v\n%s", err, without)
	}
}

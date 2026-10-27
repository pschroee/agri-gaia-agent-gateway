// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package worker

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"agw/internal/toolset"
)

// Issue #42: every combination with the command line loads the skill documents; without bash there
// are no skills at all (MCP, REST), as for the other skills.
func TestPiArgsDocumentsSkill(t *testing.T) {
	for _, ts := range toolset.All() {
		args, err := PiArgs(ts.Key(), "p", "m")
		if err != nil {
			t.Fatal(err)
		}
		got := strings.Contains(strings.Join(args, " "), "--skill "+documentSkil)
		if want := ts.Has(toolset.CLI); got != want {
			t.Errorf("%s: skill documents loaded = %v, want %v", ts.Key(), got, want)
		}
	}
	if documentSkil != "/opt/agw/skills/documents" {
		t.Errorf("skill path %s", documentSkil)
	}
}

// The system note says per binding what happens with attachments that are not plain text: convert
// with bash, refuse honestly without it. Exactly one of the three texts per combination.
func TestSystemNoteAttachmentsPerBinding(t *testing.T) {
	for _, ts := range toolset.All() {
		note := SystemNoteFor(ts.Key(), 4, nil, "")
		want := inputsAPI
		switch {
		case ts.Has(toolset.CLI):
			want = inputsCLI
		case ts.Has(toolset.MCP):
			want = inputsMCP
		}
		n := 0
		for _, text := range []string{inputsCLI, inputsMCP, inputsAPI} {
			if strings.Contains(note, text) {
				n++
			}
		}
		if !strings.Contains(note, "/workspace/inputs/ (read only)."+want) || n != 1 {
			t.Errorf("%s: attachment note wrong (%d texts):\n%s", ts.Key(), n, note)
		}
		if strings.Contains(strings.ReplaceAll(note, "{{step", ""), "{{") {
			t.Errorf("%s: placeholder left in the note", ts.Key())
		}
	}
	if !strings.Contains(inputsCLI, "markitdown") || !strings.Contains(inputsCLI, "pdftotext") || !strings.Contains(inputsCLI, "skill documents") {
		t.Error("cli note does not name the tools and the skill")
	}
	for _, text := range []string{inputsMCP, inputsAPI} {
		if !strings.Contains(text, "instead of guessing their content") {
			t.Errorf("note without the instruction to say so: %s", text)
		}
	}
	if !strings.Contains(SystemNote, "pdftotext/pdftoppm, markitdown,") {
		t.Error("markitdown missing from the sandbox's tool list")
	}
}

// repoFile reads a file relative to the repository root (tests run in internal/worker).
func repoFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The skill file exists where both images copy it, carries pi's front matter with the name of its
// directory, and keeps the rules the issue asks for (no container needed).
func TestDocumentsSkillFile(t *testing.T) {
	s := repoFile(t, "images/agw-basis/skills/documents/SKILL.md")
	fm := regexp.MustCompile(`(?s)\A---\nname: documents\ndescription: ([^\n]+)\n---\n`).FindStringSubmatch(s)
	if fm == nil {
		t.Fatalf("front matter missing or wrong:\n%.300s", s)
	}
	for _, w := range []string{"Word", "Excel", "PowerPoint", "PDF"} {
		if !strings.Contains(fm[1], w) {
			t.Errorf("description does not name %s", w)
		}
	}
	for _, w := range []string{
		"/workspace/inputs/", "markitdown <file>", "pdftotext <file> -", "pdftotext -f 3 -l 5", "head -c",
		"sheet_name=None", "openpyxl", "empty", "preinstalled",
	} {
		if !strings.Contains(s, w) {
			t.Errorf("skill without %q", w)
		}
	}
}

// The execution image installs markitdown from the hashed lock (#242) with the extras the skill names, the lock holds
// the dependencies the formats need, and both images copy the skills directory. A Docker build is not part of the
// everyday tests; this guards the lines it depends on.
func TestDockerfileMarkitdown(t *testing.T) {
	d := repoFile(t, "images/agw-basis/Dockerfile")
	m := regexp.MustCompile(`ARG MARKITDOWN_VERSION=(\d+\.\d+\.\d+)\n`).FindStringSubmatch(d)
	if m == nil {
		t.Fatal("MARKITDOWN_VERSION not pinned")
	}
	if !strings.Contains(d, "markitdown t.xlsx | grep -q") {
		t.Error("the build check that converts a workbook is missing")
	}
	if strings.Contains(d, "constraints") {
		t.Error("constraints file still referenced; the lock replaced it")
	}
	if n := strings.Count(d, "COPY images/agw-basis/skills /opt/agw/skills"); n != 2 {
		t.Errorf("skills copied into %d images, want 2 (pi and exec)", n)
	}
	// The lock must be installed in the exec stage, not into the pi image (no Python there).
	exec := strings.Index(d, "AS exec\n")
	if i := strings.Index(d, "pip3 install --no-cache-dir --break-system-packages --require-hashes"); i < exec || exec < 0 {
		t.Error("the lock is not installed in the exec stage")
	}
	c := repoFile(t, "images/agw-basis/python/requirements.lock")
	for _, pkg := range []string{"markitdown==" + m[1], "magika==", "mammoth==", "python-pptx==", "xlrd==", "olefile==", "pdfminer.six==", "onnxruntime=="} {
		if !strings.Contains(c, "\n"+pkg) {
			t.Errorf("lock without %s", pkg)
		}
	}
	for _, pre := range []string{"numpy==", "pandas==", "openpyxl=="} {
		if !strings.Contains(c, "\n"+pre) {
			t.Errorf("lock without %s", pre)
		}
	}
}

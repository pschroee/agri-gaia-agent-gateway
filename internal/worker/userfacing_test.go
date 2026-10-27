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

// Issue #75: the agent named sandbox paths (/workspace/…) and internal commands in its replies, which the user
// cannot use. The rule is part of the system note of every binding, once, with its wrong and right example and the
// exception for an explicit question about technical details.
func TestSystemNoteUserFacingRule(t *testing.T) {
	for _, ts := range toolset.All() {
		note := SystemNoteFor(ts.Key(), 4, nil, "")
		if strings.Count(note, userFacingRule) != 1 {
			t.Errorf("%s: user-facing rule not exactly once", ts.Key())
		}
		for _, w := range []string{"Wrong: \"Source: /workspace/report.typ\"", "Right: \"I can also send you the Typst source (report.typ)", "explicitly asks for technical details"} {
			if !strings.Contains(note, w) {
				t.Errorf("%s: system note without %q", ts.Key(), w)
			}
		}
	}
}

// Issue #75: pi puts only a skill's description into the context, so the file skills say in their descriptions that
// replies name files, not sandbox paths. The description stays a plain YAML scalar on one line (no ": ", no " #",
// at most 1024 characters).
func TestFileSkillsNameFilesNotPaths(t *testing.T) {
	const sentence = "In replies name files by their file name, never by sandbox path."
	for _, name := range []string{"artifacts", "writing-typst", "documents", "charts", "model-card"} {
		s := repoFile(t, "images/agw-basis/skills/"+name+"/SKILL.md")
		m := regexp.MustCompile(`(?m)^description: ([^\n]+)$`).FindStringSubmatch(s)
		if m == nil {
			t.Fatalf("%s: description missing", name)
		}
		d := m[1]
		if len(d) > 1024 || strings.Contains(d, ": ") || strings.Contains(d, " #") {
			t.Errorf("%s: description too long (%d) or not a plain scalar", name, len(d))
		}
		if !strings.Contains(d, sentence) {
			t.Errorf("%s: description without %q", name, sentence)
		}
	}
}

// Issue #75: the replies of subagents go to the user in the subagent view, and the main agent's system note does not
// reach them. Every agent definition of the vendored copy therefore carries the same sentence in its body.
func TestSubagentDefinitionsNameFilesNotPaths(t *testing.T) {
	const sentence = "In replies name files by their file name, never by sandbox path."
	files, err := filepath.Glob(filepath.Join("..", "..", "third_party", "pi-subagents", "agents", "*.md"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no agent definitions found: %v", err)
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), "\n"+sentence+"\n") {
			t.Errorf("%s: sentence missing", filepath.Base(f))
		}
	}
}

// #253: no skill states the platform's hardware as a fact; the CPU measurements are named as such. The hardware
// comes from the line "Platform trainings" of the system note.
func TestSkillsNameNoFixedGPU(t *testing.T) {
	for _, name := range []string{"training-advice", "platform", "pre-annotation", "model-card"} {
		s := repoFile(t, "images/agw-basis/skills/"+name+"/SKILL.md")
		for i, line := range strings.Split(s, "\n") {
			if !strings.Contains(line, "no GPU") && !strings.Contains(line, "4 vCPU") && !strings.Contains(line, "CPU without GPU") {
				continue
			}
			// Allowed only in the labelled CPU measurements.
			if !strings.Contains(line, "Measured on the CPU") && !strings.Contains(line, "(CPU table;") && !strings.Contains(line, "4 vCPU, no GPU):") {
				t.Errorf("%s:%d states fixed hardware: %s", name, i+1, line)
			}
		}
	}
	s := repoFile(t, "images/agw-basis/skills/training-advice/SKILL.md")
	for _, want := range []string{"Platform trainings", "with a GPU", "Tesla V100"} {
		if !strings.Contains(strings.ToLower(s), strings.ToLower(want)) {
			t.Errorf("training-advice without %q", want)
		}
	}
}

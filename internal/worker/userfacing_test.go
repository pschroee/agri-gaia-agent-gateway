// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package worker

import (
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
		note := SystemNoteFor(ts.Key(), 4)
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

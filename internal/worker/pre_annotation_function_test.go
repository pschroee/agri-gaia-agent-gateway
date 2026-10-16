// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package worker

import (
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

// The way of the user's own model (V2) stands in the description of the skill: the tool
// annotation-function, its irreversibility with the visibility for every CVAT user, and the order of
// the classes — the rule the agent must know before it acts (issue #59).
func TestPreAnnotationSkillOwnModel(t *testing.T) {
	content := repoFile(t, "images/agw-basis/skills/pre-annotation/SKILL.md")
	m := regexp.MustCompile(`(?s)\A---\nname: pre-annotation\ndescription: ([^\n]+)\n---\n`).FindStringSubmatch(content)
	if m == nil {
		t.Fatal("front matter does not start the file, or name and description do not match")
	}
	desc := m[1]
	if n := utf8.RuneCountInString(desc); n > 1024 {
		t.Errorf("description has %d characters, at most 1024 (V2 lengthens it; V3c replaces it)", n)
	}
	for _, want := range []string{
		"annotation-function",
		"irreversible",
		"visible to all CVAT users",
		"not removable",
		"labels in the model's output order",
	} {
		if !strings.Contains(desc, want) {
			t.Errorf("description lacks %q", want)
		}
	}

	// The body carries the section: how a function is built, that only ONNX works, the states and
	// the warning about the order of the classes.
	for _, want := range []string{
		"## 1a. Your own model as a function",
		"agw-platform annotation-function",
		"Only an **ONNX** model of the user's own works",
		// The skill wraps the sentence ("… class names in the\norder the model outputs them"), so only the
		// part that stands on one line is checked.
		"order the model outputs them",
		"every CVAT user of the platform",
		"you cannot remove it — only the operator can",
		"`deploying` until CVAT lists it, `ready` (then it can be run), `failed`",
		"`missing` when the function has disappeared from CVAT",
		"never guess",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("skill text lacks %q", want)
		}
	}
}

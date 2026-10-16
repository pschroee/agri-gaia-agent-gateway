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

// The checks of V3 stand in the description of the skill: measuring before a run, filling only empty
// images, the sample size and the tools of the check. The description is what pi puts into the context
// before any command runs (issue #59), so these rules must be there and not only in the text.
// V3c replaced the description (948 characters, the plan of #110); checked below are the wordings that
// description uses. The 80 percent threshold and the tool names stand in the text, where
// TestPreAnnotationSkillCheckBody guards them.
func TestPreAnnotationSkillCheckDescription(t *testing.T) {
	content := repoFile(t, "images/agw-basis/skills/pre-annotation/SKILL.md")
	m := regexp.MustCompile(`(?s)\A---\nname: pre-annotation\ndescription: ([^\n]+)\n---\n`).FindStringSubmatch(content)
	if m == nil {
		t.Fatal("front matter does not start the file, or name and description do not match")
	}
	desc := m[1]
	if n := utf8.RuneCountInString(desc); n > 1024 {
		t.Errorf("description has %d characters, at most 1024 (the plan of #110 allows 948)", n)
	}
	for _, want := range []string{
		"annotation-check",
		"capabilities",
		"vision",
		"mapping",
		"irreversible",
		"only images without annotations",
		"verify",
		"statistics only",
		"final verdict usable",
		"at most 3 rounds",
	} {
		if !strings.Contains(desc, want) {
			t.Errorf("description lacks %q", want)
		}
	}
}

// The body carries the loop: measure, decide, fill only what is empty, name the weak images, and close
// the loop after the correction.
func TestPreAnnotationSkillCheckBody(t *testing.T) {
	content := repoFile(t, "images/agw-basis/skills/pre-annotation/SKILL.md")
	for _, want := range []string{
		"## 5a. Check before you trust",
		"annotation-check --dataset-id <id> --function <id> --mode calibrate",
		"annotation-fill --dataset-id <id> --function <id>",
		"annotation-check --dataset-id <id> --function <id> --mode uncertain",
		"proposed_min_confidence",
		"Below 80 percent accuracy",
		"name the sample size",
		"`cvat_url` that opens exactly that image",
		"propose-plan",
		"cannot be stopped",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("skill text lacks %q", want)
		}
	}
}

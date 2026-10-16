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

// The checks of V3 stand in the description of the skill: measuring before a run, the sample size, the
// threshold of the decision and the two new tools. The description is what pi puts into the context
// before any command runs (issue #59), so these rules must be there and not only in the text.
func TestPreAnnotationSkillCheckDescription(t *testing.T) {
	content := repoFile(t, "images/agw-basis/skills/pre-annotation/SKILL.md")
	m := regexp.MustCompile(`(?s)\A---\nname: pre-annotation\ndescription: ([^\n]+)\n---\n`).FindStringSubmatch(content)
	if m == nil {
		t.Fatal("front matter does not start the file, or name and description do not match")
	}
	desc := m[1]
	if n := utf8.RuneCountInString(desc); n > 1024 {
		t.Errorf("description has %d characters, at most 1024 (V3 lengthens it; V3c replaces it)", n)
	}
	for _, want := range []string{
		"annotation-check",
		"annotation-fill",
		"annotation-job",
		"sample size",
		"80 percent",
		"only images without annotations",
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

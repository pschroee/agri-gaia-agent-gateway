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

// The description of the skill is the part pi puts into the context before any command runs, so the
// rules the agent must know before it starts a run stay there (issue #59; V1). It names both ways:
// the functions of CVAT and the manual annotation, which is what remains when nothing is available.
// V3b adds the rule that a run or a fill is judged and reported before it counts as a result; the
// description stays under 1024 characters, so V1 rules were shortened for it (E24).
func TestPreAnnotationSkillDescription(t *testing.T) {
	content := repoFile(t, "images/agw-basis/skills/pre-annotation/SKILL.md")
	m := regexp.MustCompile(`(?s)\A---\nname: pre-annotation\ndescription: ([^\n]+)\n---\n`).FindStringSubmatch(content)
	if m == nil {
		t.Fatal("front matter does not start the file, or name and description do not match")
	}
	desc := m[1]
	if n := utf8.RuneCountInString(desc); n > 1024 {
		t.Errorf("description has %d characters, at most 1024", n)
	}
	if strings.Contains(desc, ": ") || strings.Contains(desc, " #") {
		t.Error("description would break the YAML scalar (\": \" or \" #\")")
	}
	for _, want := range []string{
		"capabilities",
		"manual annotation in CVAT",
		"annotation-functions",
		"annotation-task",
		"mapping",
		"table",
		"never map a label you are unsure about",
		"existing=keep",
		"existing=replace",
		"background loop",
		"30 s",
		"annotation-summary",
		"annotation-sync",
		"cvat_url",
		"never claim the pre-annotation is correct",
		// V3b: the result is judged and reported, and taking it back is named.
		"verify",
		"usable/questionable",
		"final verdict usable",
		"annotation-rollback",
		"entry vision",
	} {
		if !strings.Contains(desc, want) {
			t.Errorf("description lacks %q", want)
		}
	}
}

// The body carries the steps, the loop for a long run and the estimate of the runtime.
func TestPreAnnotationSkillBody(t *testing.T) {
	content := repoFile(t, "images/agw-basis/skills/pre-annotation/SKILL.md")
	for _, want := range []string{
		"agw-platform annotation-functions",
		"agw-platform annotation-task",
		"agw-platform pre-annotate",
		"agw-platform annotation-summary",
		"agw-platform annotation-sync",
		"while :; do",
		"sleep 30",
		"run_in_background: true",
		"0.1 to 1 s per image",
		"estimate",
		"cvat_url",
		// V3b: the report of a run or a fill and the way back.
		"agw-platform annotation-check --mode verify",
		"--source-id",
		"--final true",
		"agw-platform annotation-rollback",
		"changed_kept",
		"Never call a pre-annotation done without a final verdict",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("skill text lacks %q", want)
		}
	}
}

// The skill works with the commands of agw-platform, so only the cli binding loads it.
func TestPiArgsPreAnnotationSkill(t *testing.T) {
	const flag = "--skill /opt/agw/skills/pre-annotation"
	for _, v := range []string{"cli", "both"} {
		args, err := PiArgs(v, "deepseek", "deepseek-flash")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(strings.Join(args, " "), flag) {
			t.Errorf("variant %s does not load the skill pre-annotation", v)
		}
	}
	for _, v := range []string{"mcp", "api"} {
		args, err := PiArgs(v, "deepseek", "deepseek-flash")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(strings.Join(args, " "), flag) {
			t.Errorf("variant %s loads the skill pre-annotation, it must not", v)
		}
	}
}

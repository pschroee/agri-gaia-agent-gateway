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
// rules the agent must know before it starts a run stay there (issue #59; V1). It names both ways: the
// functions of CVAT and the manual annotation, which is what remains when nothing is available. V3b adds
// the rule that a run or a fill is judged and reported before it counts as a result, V3c the rules of the
// correction loop. The description is the end version of V3c (E24, 948 characters): V1 to V3b wrote
// interim versions, and the description is replaced, not appended to, because five appendices would have
// grown it to about 1900 characters.
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
		"vision",
		"mapping",
		"irreversible",
		"only images without annotations",
		"verify",
		"statistics only",
		"final verdict usable",
		// V3c: the loop and its bound stand in the description as well, because the agent has to know
		// before its first fill that a further one is a round.
		"at most 3 rounds",
	} {
		if !strings.Contains(desc, want) {
			t.Errorf("description lacks %q", want)
		}
	}
}

// The body carries the steps, the loop for a long run, the estimate of the runtime and — since V3c — the
// correction of a questionable result with its order and its bound.
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
		// V3c: the correction loop, its order, its bound and the figures of a round.
		"## 5c. Correct and stop",
		"correction round",
		"0.02",
		"no improvement on the validation images in round",
		"too few validation images",
		"`previous`",
		"`delta`",
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

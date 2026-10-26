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

// The description of the skill result-check carries the rules the agent must know before it reports a
// result: pi puts the description into the context, the text only after the skill is read (R0.24, #59).
func TestResultCheckSkillDescription(t *testing.T) {
	content := repoFile(t, "images/agw-basis/skills/result-check/SKILL.md")
	m := regexp.MustCompile(`(?s)\A---\nname: result-check\ndescription: ([^\n]+)\n---\n`).FindStringSubmatch(content)
	if m == nil {
		t.Fatal("front matter does not start the file, or name and description do not match")
	}
	desc := m[1]
	n := utf8.RuneCountInString(desc)
	if n > 1024 {
		t.Errorf("description has %d characters, at most 1024", n)
	}
	if n < 900 || n > 950 {
		t.Errorf("description has %d characters, the plan allows 930 ± 20", n)
	}
	if strings.Contains(desc, ": ") || strings.Contains(desc, " #") {
		t.Error("description would break the YAML scalar (\": \" or \" #\")")
	}
	for _, want := range []string{
		"capabilities",
		"never compute",
		"final=true once",
		"final=false",
		"at most one new training",
		"Never call a result done",
		"the header the gateway returns",
	} {
		if !strings.Contains(desc, want) {
			t.Errorf("description lacks %q", want)
		}
	}
}

// The text of the skill names the order of the report, the budget and the sections R1–R3 fill.
func TestResultCheckSkillBody(t *testing.T) {
	content := repoFile(t, "images/agw-basis/skills/result-check/SKILL.md")
	for _, want := range []string{
		"## Report format",
		"## Budget",
		"## After a training",
		"## Own models",
		"A run that wrote `outputs/model.json` needs no `--preprocess`",
		"a `--preprocess` that differs from it is refused before anything runs",
		"the hold-out set was not used",
		"## Edge deployments",
		"statistics only, no image review",
		"## After a training\n",
		"assess-model",
		"`final=true` works",
		"no held-out images",
		"platform figure",
		// The section of R2 (own models) names the way over the notebook server: fetch the file, check it
		// before the upload, one new run when the result is questionable.
		"assess-model --path",
		"Model check of this file",
		"origin unknown",
		// R0 left the placeholder "Filled by R3" in this section; R3 replaced it with the section of the
		// edge deployment (issue #116).
		"agw-platform deployment-check",
		"Never describe the checked set itself",
		"`check_warning`",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("skill text lacks %q", want)
		}
	}
}

// The skill works with the commands of agw-platform, so only the cli binding loads it.
func TestPiArgsResultCheckSkill(t *testing.T) {
	const flag = "--skill /opt/agw/skills/result-check"
	for _, v := range []string{"cli", "both"} {
		args, err := PiArgs(v, "deepseek", "deepseek-flash")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(strings.Join(args, " "), flag) {
			t.Errorf("variant %s does not load the skill result-check", v)
		}
	}
	for _, v := range []string{"mcp", "api"} {
		args, err := PiArgs(v, "deepseek", "deepseek-flash")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(strings.Join(args, " "), flag) {
			t.Errorf("variant %s loads the skill result-check, it must not", v)
		}
	}
}

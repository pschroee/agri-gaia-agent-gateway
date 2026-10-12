// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package worker

import (
	"regexp"
	"strings"
	"testing"
)

// Issue #68: the skill model-card writes a model card from the platform's facts and sends it as a file. As
// with the other skills, pi puts only the description into the context, so the core rules must be there. The
// description is a plain YAML scalar on one line, as in the mermaid skill (issue #59).
func TestModelCardSkillDescription(t *testing.T) {
	s := repoFile(t, "images/agw-basis/skills/model-card/SKILL.md")
	fm := regexp.MustCompile(`(?s)\A---\nname: model-card\ndescription: ([^\n]+)\n---\n`).FindStringSubmatch(s)
	if fm == nil {
		t.Fatalf("front matter missing or wrong:\n%.300s", s)
	}
	d := fm[1]
	if len(d) > 1024 || strings.Contains(d, ": ") || strings.Contains(d, " #") {
		t.Errorf("description too long (%d) or not a plain scalar: %s", len(d), d)
	}
	for _, w := range []string{
		"MODEL_CARD.md", "agw-artifact upload", "only facts read in this chat", "the metric name, the value and what it refers to",
		"per class", "limits", "not known", "every number",
	} {
		if !strings.Contains(d, w) {
			t.Errorf("description without %q", w)
		}
	}
}

// The body holds the template and the procedure: the four sections, the training lookup, the quality rule
// and the send command.
func TestModelCardSkillBody(t *testing.T) {
	s := repoFile(t, "images/agw-basis/skills/model-card/SKILL.md")
	for _, w := range []string{
		"## Purpose", "## Data", "## Quality", "## Limits", "Training container", "None Acc.",
		"agw-artifact upload MODEL_CARD.md", "ask the user",
	} {
		if !strings.Contains(s, w) {
			t.Errorf("skill text without %q", w)
		}
	}
}

// The skill is loaded in the cli binding only (like the other command skills), so cli and both get it and
// mcp and api do not.
func TestPiArgsModelCardSkill(t *testing.T) {
	const want = "--skill /opt/agw/skills/model-card"
	for _, v := range []string{"cli", "both"} {
		args, err := PiArgs(v, "deepseek", "deepseek-flash")
		if err != nil {
			t.Fatalf("PiArgs(%s): %v", v, err)
		}
		if !strings.Contains(strings.Join(args, " "), want) {
			t.Errorf("variant %s without the model-card skill", v)
		}
	}
	for _, v := range []string{"mcp", "api"} {
		args, err := PiArgs(v, "deepseek", "deepseek-flash")
		if err != nil {
			t.Fatalf("PiArgs(%s): %v", v, err)
		}
		if strings.Contains(strings.Join(args, " "), want) {
			t.Errorf("variant %s must not load the model-card skill", v)
		}
	}
}

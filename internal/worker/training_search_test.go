// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package worker

import (
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"agw/internal/platform"
)

// The description carries the rules the agent must know before it acts (pi puts only the description into the
// context): a plain one-line YAML scalar of at most 1024 characters.
func TestTrainingSearchSkillDescription(t *testing.T) {
	s := repoFile(t, "images/agw-basis/skills/training-search/SKILL.md")
	m := regexp.MustCompile(`(?s)\A---\nname: training-search\ndescription: ([^\n]+)\n---\n`).FindStringSubmatch(s)
	if m == nil {
		t.Fatalf("front matter missing or wrong:\n%.300s", s)
	}
	d := m[1]
	if n := utf8.RuneCountInString(d); n > 1024 {
		t.Errorf("description has %d characters, at most 1024", n)
	}
	if strings.Contains(d, ": ") || strings.Contains(d, " #") {
		t.Error("description would break the YAML scalar")
	}
	for _, w := range []string{
		"propose_search", "split_dataset", "held-back test set", "search_status", "never repeat a trial",
		"search_next", "one-sentence reason", "exactly the calls it names", "outputs/metrics.jsonl",
		"epoch, epochs, train_loss, val_loss, train_acc, val_acc", "params.json", "validation value only",
		"final test once for the winner",
	} {
		if !strings.Contains(d, w) {
			t.Errorf("description without %q", w)
		}
	}
}

// The body names the commands, both ways and the convention for the notebook way.
func TestTrainingSearchSkillBody(t *testing.T) {
	s := repoFile(t, "images/agw-basis/skills/training-search/SKILL.md")
	for _, w := range []string{
		"agw-platform propose-search", "agw-platform search-status", "agw-platform search-next",
		"agw-platform end-search", "clone-training", "--path template", "--path notebook",
		"outputs/metrics.jsonl", "params.json", "json.dumps", "final=true", "held-back",
	} {
		if !strings.Contains(s, w) {
			t.Errorf("skill text without %q", w)
		}
	}
}

// The tool names in the skill exist in the tool table.
func TestTrainingSearchSkillToolsExist(t *testing.T) {
	for _, n := range []string{"propose_search", "search_next", "search_status", "end_search", "clone_training"} {
		if _, ok := platform.Lookup(n); !ok {
			t.Errorf("tool %s is not in the table", n)
		}
	}
}

// The notebook and platform skills point to the search skill, and the notebook skill states the convention.
func TestTrainingSearchReferences(t *testing.T) {
	nb := repoFile(t, "images/agw-basis/skills/notebook/SKILL.md")
	for _, w := range []string{"training-search", "outputs/metrics.jsonl", "params.json"} {
		if !strings.Contains(nb, w) {
			t.Errorf("notebook skill without %q", w)
		}
	}
	if pl := repoFile(t, "images/agw-basis/skills/platform/SKILL.md"); !strings.Contains(pl, "propose-search") {
		t.Error("platform skill without a pointer to propose-search")
	}
}

// Loaded in the cli binding (and both), not in mcp or api.
func TestPiArgsTrainingSearchSkill(t *testing.T) {
	const flag = "--skill /opt/agw/skills/training-search"
	for _, v := range []string{"cli", "both"} {
		args, err := PiArgs(v, "deepseek", "deepseek-flash")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(strings.Join(args, " "), flag) {
			t.Errorf("variant %s without the skill", v)
		}
	}
	for _, v := range []string{"mcp", "api"} {
		args, err := PiArgs(v, "deepseek", "deepseek-flash")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(strings.Join(args, " "), flag) {
			t.Errorf("variant %s must not load the skill", v)
		}
	}
}

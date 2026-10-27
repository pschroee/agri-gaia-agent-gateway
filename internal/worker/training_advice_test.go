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
	"unicode/utf8"
)

// The skill's description is the part pi puts into the context before any command runs, so the rules
// the agent must know before it creates a training stay there (issue #66, lesson of #59).
func TestTrainingAdviceSkillDescription(t *testing.T) {
	content := repoFile(t, "images/agw-basis/skills/training-advice/SKILL.md")
	m := regexp.MustCompile(`(?s)\A---\nname: training-advice\ndescription: ([^\n]+)\n---\n`).FindStringSubmatch(content)
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
		"category",
		"annotation kind",
		"get_dataset has only the classes",
		"CVAT tag means classification",
		"box means object detection",
		"inside the schema",
		"one sentence",
		"export_config",
		"img-size",
		"range marked as an estimate",
		"only after the user agrees",
	} {
		if !strings.Contains(desc, want) {
			t.Errorf("description lacks %q", want)
		}
	}
}

// The body carries the measured runtimes, the ONNX export settings and the consent step.
func TestTrainingAdviceSkillBody(t *testing.T) {
	content := repoFile(t, "images/agw-basis/skills/training-advice/SKILL.md")
	for _, want := range []string{
		"about 4.5 min",
		"12 s",
		"3 min 45 s",
		`"opset_version": 18`,
		"dynamo",
		"<tag>",
		"<box>",
		"Wait for consent",
		"split-dataset",
		"at least 5 validation and 20 held-out images",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("skill text lacks %q", want)
		}
	}
}

// The skill is loaded by the cli binding (and by both, which means cli,mcp), not by mcp or api.
func TestPiArgsTrainingAdviceSkill(t *testing.T) {
	const flag = "--skill /opt/agw/skills/training-advice"
	for _, v := range []string{"cli", "both"} {
		args, err := PiArgs(v, "deepseek", "deepseek-flash")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(strings.Join(args, " "), flag) {
			t.Errorf("variant %s does not load the skill training-advice", v)
		}
	}
	for _, v := range []string{"mcp", "api"} {
		args, err := PiArgs(v, "deepseek", "deepseek-flash")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(strings.Join(args, " "), flag) {
			t.Errorf("variant %s loads the skill training-advice, it must not", v)
		}
	}
}

// B26 (demo of 27.10.2026, evening): every create-training example in the skills sends export_config. The platform
// skill's example left it out, the agent copied it, the call differed from its plan step and no inference container
// could have been built from the model.
func TestSkillCreateTrainingExamplesSendExport(t *testing.T) {
	re := regexp.MustCompile("agw-platform create-training [^`\n]*")
	dirs, err := os.ReadDir(filepath.Join("..", "..", "images/agw-basis/skills"))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		path := "images/agw-basis/skills/" + d.Name() + "/SKILL.md"
		if _, err := os.Stat(filepath.Join("..", "..", path)); err != nil {
			continue
		}
		for _, ex := range re.FindAllString(repoFile(t, path), -1) {
			if !strings.Contains(ex, "@") { // a mention of the command, not an example call
				continue
			}
			n++
			if !strings.Contains(ex, "--export_config") {
				t.Errorf("%s: example without --export_config: %s", path, ex)
			}
		}
	}
	if n == 0 {
		t.Fatal("no create-training example found")
	}
}

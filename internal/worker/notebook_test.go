// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package worker

import (
	"regexp"
	"strings"
	"testing"
)

// Issue C-E3: the skill notebook names both ways in its description - with the capability notebook
// and without it - because pi puts only the description into the context. It is a plain YAML scalar
// on one line, as in the other skills.
func TestNotebookSkillDescription(t *testing.T) {
	s := repoFile(t, "images/agw-basis/skills/notebook/SKILL.md")
	fm := regexp.MustCompile(`(?s)\A---\nname: notebook\ndescription: ([^\n]+)\n---\n`).FindStringSubmatch(s)
	if fm == nil {
		t.Fatalf("front matter missing or wrong:\n%.300s", s)
	}
	d := fm[1]
	if len(d) > 1024 || strings.Contains(d, ": ") || strings.Contains(d, " #") {
		t.Errorf("description too long (%d) or not a plain scalar: %s", len(d), d)
	}
	for _, w := range []string{
		"capabilities", "without it", "notebook-run", "2 CPUs, 8 GB, a shared GPU where the instance has one, no internet, no platform access", "torch.cuda.is_available()",
		"test the code in the sandbox first", "dataset_ids", "approves every run", "notebook-run-status",
		"notebook-log", "notebook-fetch", "upload-model", "inference container", "link of the run",
	} {
		if !strings.Contains(d, w) {
			t.Errorf("description without %q", w)
		}
	}
}

// The body carries the procedure: ask capabilities first, test in the sandbox, the commands of a run,
// the ONNX export and what exit code 124 means.
func TestNotebookSkillBody(t *testing.T) {
	s := repoFile(t, "images/agw-basis/skills/notebook/SKILL.md")
	for _, w := range []string{
		"agw-platform capabilities", "agw-platform notebook-run", "agw-platform notebook-run-status",
		"agw-platform notebook-log", "agw-platform notebook-fetch", "agw-platform upload-model",
		"/home/jovyan/work/datasets/<id>/", "/workspace/notebook/<run_id>/", "dynamo=False", "opset_version=18",
		"124", "exit_code", "small sample", "The server belongs to the user", "no internet",
	} {
		if !strings.Contains(s, w) {
			t.Errorf("skill text without %q", w)
		}
	}
}

// The skill is loaded in the cli binding only (like the other command skills): cli and both get it,
// mcp and api do not.
func TestPiArgsNotebookSkill(t *testing.T) {
	const want = "--skill /opt/agw/skills/notebook"
	for _, v := range []string{"cli", "both"} {
		args, err := PiArgs(v, "deepseek", "deepseek-flash")
		if err != nil {
			t.Fatalf("PiArgs(%s): %v", v, err)
		}
		if !strings.Contains(strings.Join(args, " "), want) {
			t.Errorf("variant %s without the notebook skill", v)
		}
	}
	for _, v := range []string{"mcp", "api"} {
		args, err := PiArgs(v, "deepseek", "deepseek-flash")
		if err != nil {
			t.Fatalf("PiArgs(%s): %v", v, err)
		}
		if strings.Contains(strings.Join(args, " "), want) {
			t.Errorf("variant %s must not load the notebook skill", v)
		}
	}
}

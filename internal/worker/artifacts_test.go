// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package worker

import (
	"regexp"
	"strings"
	"testing"

	"agw/internal/toolset"
)

// Issue #62: the agent sends files without approval. pi puts only a skill's description into the context, so the
// description says it (plain YAML scalar on one line), and no text tells the agent to wait for an approval any more.
func TestArtifactsSkillSendsWithoutApproval(t *testing.T) {
	s := repoFile(t, "images/agw-basis/skills/artifacts/SKILL.md")
	fm := regexp.MustCompile(`(?s)\A---\nname: artifacts\ndescription: ([^\n]+)\n---\n`).FindStringSubmatch(s)
	if fm == nil {
		t.Fatalf("front matter missing or wrong:\n%.300s", s)
	}
	d := fm[1]
	if len(d) > 1024 || strings.Contains(d, ": ") || strings.Contains(d, " #") {
		t.Errorf("description too long (%d) or not a plain scalar: %s", len(d), d)
	}
	for _, w := range []string{"agw-artifact upload", "appears in the chat at once", "without approval", "not for intermediate files", "inside /workspace"} {
		if !strings.Contains(d, w) {
			t.Errorf("description without %q", w)
		}
	}
	for _, w := range []string{"There is no approval", "**finished results**", "Do **not** send intermediate files", "inside `/workspace`"} {
		if !strings.Contains(s, w) {
			t.Errorf("skill without %q", w)
		}
	}
	for _, f := range []string{"images/agw-basis/skills/artifacts/SKILL.md", "images/agw-basis/skills/charts/SKILL.md", "images/agw-basis/skills/writing-typst/SKILL.md"} {
		txt := repoFile(t, f)
		for _, old := range []string{"must be approved", "waits until the\nuser approves", "store it as an artifact", "Store the finished PDF as an artifact"} {
			if strings.Contains(txt, old) {
				t.Errorf("%s still says %q", f, old)
			}
		}
	}
}

// The system note names the way to send a file of the chat's bindings, without approval; the REST binding has none.
func TestSystemNoteSendFiles(t *testing.T) {
	for _, ts := range toolset.All() {
		note := SystemNoteFor(ts.Key(), 4)
		if strings.Contains(note, "approved by the user, and you wait") || strings.Contains(note, "{{send}}") {
			t.Errorf("%s: old upload line or placeholder left", ts.Key())
		}
		cli, mcp := strings.Contains(note, "agw-artifact upload <file>"), strings.Contains(note, "mcp_upload_artifact")
		if cli != ts.Has(toolset.CLI) || mcp != ts.Has(toolset.MCP) {
			t.Errorf("%s: send tools cli=%v mcp=%v", ts.Key(), cli, mcp)
		}
		if ts.Has(toolset.CLI) || ts.Has(toolset.MCP) {
			if !strings.Contains(note, "without approval") || !strings.Contains(note, "not intermediate files") {
				t.Errorf("%s: send line incomplete", ts.Key())
			}
		} else if !strings.Contains(note, "cannot send files") {
			t.Errorf("%s: REST binding promises files", ts.Key())
		}
	}
}

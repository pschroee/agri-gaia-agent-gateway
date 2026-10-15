// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package worker

import (
	"strings"
	"testing"

	"agw/internal/config"
)

// The system note names the static parts of the setup and points at the tool that answers the current
// state (G1.9). State that changes during a chat (internet, compute calls left, devices online) must not
// stand there: the note is built once per slot and stays fixed, so that pi's prefix cache holds.
func TestSystemNoteCapabilities(t *testing.T) {
	caps := []string{"vision: the model DeepSeek Flash reads text only", "bindings: the chat works through cli"}
	for v, tool := range map[string]string{
		"cli": "agw-platform capabilities",
		"mcp": "the tool mcp_platform_capabilities",
		"api": "GET /_agw/capabilities",
	} {
		note := SystemNoteFor(v, 4, caps)
		for _, want := range []string{
			"This setup offers vision: the model DeepSeek Flash reads text only; bindings: the chat works through cli.",
			"This is fixed for this chat",
			tool,
			"before you plan work that needs one of them",
		} {
			if !strings.Contains(note, want) {
				t.Errorf("%s: the note lacks %q", v, want)
			}
		}
		if strings.Contains(note, "{{") {
			t.Errorf("%s: placeholder left over", v)
		}
		for _, bad := range []string{"compute calls left", "internet is on in this chat", "devices online are"} {
			if strings.Contains(note, bad) {
				t.Errorf("%s: the note names state (%q)", v, bad)
			}
		}
	}
	// A combination names the tools of its bindings.
	note := SystemNoteFor("cli,api", 4, caps)
	if !strings.Contains(note, "agw-platform capabilities or GET /_agw/capabilities") {
		t.Errorf("cli,api: %q", note)
	}
	// Without static lines nothing of the paragraph appears, and no empty line is left behind.
	plain := SystemNoteFor("cli", 4, nil)
	if strings.Contains(plain, "This setup offers") {
		t.Error("the note names capabilities that nobody registered")
	}
	if strings.Contains(plain, "\n\n- Your tools for commands and files") {
		t.Errorf("an empty capability line was left behind: %q", plain)
	}
	// The note of a slot is the same every time: nothing in it depends on a turn.
	if SystemNoteFor("cli", 4, caps) != SystemNoteFor("cli", 4, caps) {
		t.Error("the note is not fixed per slot")
	}
}

// The factory builds the lines from the catalog and the environment of the orchestrator (G1.9); the
// state of a chat is not part of them.
func TestFactoryCapabilityLines(t *testing.T) {
	cat, err := config.ParseCatalog([]byte(`{"default":"deepseek/deepseek-flash","providers":[{"id":"deepseek","models":[{"id":"deepseek-flash","name":"DeepSeek Flash"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	f := &Factory{Cat: cat, Env: config.Env{}}
	lines := f.capabilities("cli")
	joined := strings.Join(lines, "\n")
	if len(lines) != 2 || !strings.Contains(joined, "vision: ") || !strings.Contains(joined, "bindings: ") {
		t.Fatalf("lines: %v", lines)
	}
	// The statically described vision follows the catalog: an image model is reported as such.
	img, err := config.ParseCatalog([]byte(`{"default":"deepseek/bild","providers":[{"id":"deepseek","models":[{"id":"bild","input":["text","image"]}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join((&Factory{Cat: img}).capabilities("mcp"), "\n"); !strings.Contains(got, "vision: the model bild reads images") {
		t.Errorf("with an image model: %q", got)
	}
}

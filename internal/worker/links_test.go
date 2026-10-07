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

// Issue #61: the agent cites sources as Markdown links with a title, not as bare URLs. pi puts only a skill's
// description into the context, so the core rule is there; the description stays a plain YAML scalar on one
// line (no ": ", no " #", at most 1024 characters).
func TestWebResearchLinkRule(t *testing.T) {
	s := repoFile(t, "images/agw-basis/skills/web-research/SKILL.md")
	fm := regexp.MustCompile(`(?s)\A---\nname: web-research\ndescription: ([^\n]+)\n---\n`).FindStringSubmatch(s)
	if fm == nil {
		t.Fatalf("front matter missing or wrong:\n%.300s", s)
	}
	d := fm[1]
	if len(d) > 1024 || strings.Contains(d, ": ") || strings.Contains(d, " #") {
		t.Errorf("description too long (%d) or not a plain scalar: %s", len(d), d)
	}
	for _, w := range []string{"Markdown links with a meaningful title", "[Card counting - Wikipedia](https://...)", "never as bare URLs", "one link per line"} {
		if !strings.Contains(d, w) {
			t.Errorf("description without %q", w)
		}
	}
	for _, w := range []string{
		"each as a Markdown link with its URL", "**Markdown link**", "Never write a bare URL",
		"**one link\n  per line**", "`- [Card counting - Wikipedia](https://en.wikipedia.org/wiki/Card_counting) - ",
	} {
		if !strings.Contains(s, w) {
			t.Errorf("skill without %q", w)
		}
	}
	if strings.Contains(s, "title or site plus URL is enough") {
		t.Error("skill still allows a bare URL after the title")
	}
	if !strings.Contains(repoFile(t, "images/agw-basis/ext/web-tools.ts"), "as Markdown links with a meaningful title ([Title](https://...)), never as bare URLs") {
		t.Error("web-tools.ts guideline without the link rule")
	}
}

// The link rule is in the system note of every binding, once, and covers platform links and files; the research
// line asks for Markdown links too.
func TestSystemNoteLinkRule(t *testing.T) {
	for _, ts := range toolset.All() {
		note := SystemNoteFor(ts.Key(), 4)
		if strings.Count(note, linkRule) != 1 {
			t.Errorf("%s: link rule not exactly once", ts.Key())
		}
		if !strings.Contains(note, "with its URL as a Markdown link") {
			t.Errorf("%s: research line without Markdown links", ts.Key())
		}
	}
	for _, w := range []string{"always Markdown links", "[title](URL)", "never bare URLs", "links to the platform", "files"} {
		if !strings.Contains(linkRule, w) {
			t.Errorf("link rule without %q", w)
		}
	}
}

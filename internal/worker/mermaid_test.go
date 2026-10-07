// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package worker

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"agw/internal/toolset"
)

// Issue #59: pi puts only a skill's description into the model's context, so the rules the web UI enforces on
// Mermaid diagrams must be there, not only in the skill's text. The description is a plain YAML scalar on one
// line: no ": " and no " #", which would end or comment it, and pi's limit of 1024 characters.
func TestMermaidSkillDescription(t *testing.T) {
	s := repoFile(t, "images/agw-basis/skills/mermaid/SKILL.md")
	fm := regexp.MustCompile(`(?s)\A---\nname: mermaid\ndescription: ([^\n]+)\n---\n`).FindStringSubmatch(s)
	if fm == nil {
		t.Fatalf("front matter missing or wrong:\n%.300s", s)
	}
	d := fm[1]
	if len(d) > 1024 || strings.Contains(d, ": ") || strings.Contains(d, " #") {
		t.Errorf("description too long (%d) or not a plain scalar: %s", len(d), d)
	}
	for _, w := range []string{
		"without HTML", "no HTML tags in labels", "<br>", "<b>", "shorter label", "real line break inside a quoted label",
		"double quotes", "umlauts", "brackets", "%%{init}%%", "click directives", "mermaid-check before answering",
	} {
		if !strings.Contains(d, w) {
			t.Errorf("description without %q", w)
		}
	}
	for _, w := range []string{
		"mermaid-check - <<'EOF'", "mermaid-check answer.md", `securityLevel: "strict"`, "/opt/agw/mmdc/ui-config.json",
		"send only a diagram that passed", "-c /opt/agw/mmdc/ui-config.json",
	} {
		if !strings.Contains(s, w) {
			t.Errorf("skill without %q", w)
		}
	}
	// #quot; shows up literally as &quot; in the UI (mermaid 12 without HTML labels); the skill must not recommend it.
	if strings.Contains(s, "Write quotes within the text as `#quot;`") {
		t.Error("skill still recommends #quot;")
	}
}

// mermaid-check renders with the settings of the web UI: the config in the image says the same as the UI's
// mermaidConfig (strict, no HTML labels), and the script passes it to mmdc and refuses HTML tags.
func TestMermaidCheckSettings(t *testing.T) {
	var cfg struct {
		SecurityLevel string `json:"securityLevel"`
		HTMLLabels    *bool  `json:"htmlLabels"`
		Flowchart     struct {
			HTMLLabels *bool `json:"htmlLabels"`
		} `json:"flowchart"`
	}
	if err := json.Unmarshal([]byte(repoFile(t, "images/agw-basis/mmdc/ui-config.json")), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.SecurityLevel != "strict" || cfg.HTMLLabels == nil || *cfg.HTMLLabels || cfg.Flowchart.HTMLLabels == nil || *cfg.Flowchart.HTMLLabels {
		t.Errorf("ui-config.json is not strict without HTML labels: %+v", cfg)
	}
	web := repoFile(t, "web/src/lib/mermaid.ts")
	for _, w := range []string{`securityLevel: "strict"`, "htmlLabels: false"} {
		if !strings.Contains(web, w) {
			t.Errorf("web UI config without %q; ui-config.json must follow it", w)
		}
	}
	sh := repoFile(t, "images/agw-basis/mmdc/mermaid-check")
	for _, w := range []string{"CONFIG=${MERMAID_CHECK_CONFIG:-/opt/agw/mmdc/ui-config.json}", `mmdc -q -c "$CONFIG"`, "grep -iE '<[/]?[a-z][^<>]*>'", "SPDX-License-Identifier: MIT"} {
		if !strings.Contains(sh, w) {
			t.Errorf("mermaid-check without %q", w)
		}
	}
	d := repoFile(t, "images/agw-basis/Dockerfile")
	exec := strings.Index(d, "AS exec\n")
	for _, w := range []string{
		"COPY images/agw-basis/mmdc/ui-config.json /opt/agw/mmdc/ui-config.json",
		"COPY images/agw-basis/mmdc/mermaid-check /usr/local/bin/mermaid-check",
		"! printf 'flowchart LR\\n  A[Training (CPU)] --> B\\n' | HOME=/tmp mermaid-check -",
		"! printf 'flowchart LR\\n  A[a<br/>b] --> B\\n' | HOME=/tmp mermaid-check -",
	} {
		if i := strings.Index(d, w); i < 0 || i < exec || exec < 0 {
			t.Errorf("Dockerfile without %q in the exec stage", w)
		}
	}
}

// The rules are in the system note of every binding (MCP and REST list no skill mermaid); the check with
// mermaid-check only where there is bash.
func TestSystemNoteMermaidRules(t *testing.T) {
	for _, ts := range toolset.All() {
		note := SystemNoteFor(ts.Key(), 4)
		if !strings.Contains(note, "language mermaid; the web UI renders it (skill mermaid)."+mermaidRules) {
			t.Errorf("%s: mermaid rules missing", ts.Key())
		}
		if got, want := strings.Contains(note, "check every diagram with mermaid-check"), ts.Has(toolset.CLI); got != want {
			t.Errorf("%s: mermaid-check in the note = %v, want %v", ts.Key(), got, want)
		}
	}
	for _, w := range []string{"no HTML tags such as <br> in labels", "double quotes"} {
		if !strings.Contains(mermaidRules, w) {
			t.Errorf("rules without %q", w)
		}
	}
}

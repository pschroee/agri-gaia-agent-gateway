// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package worker

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"agw/internal/config"
	"agw/internal/toolset"
)

// switchNames are the internet switch tools per binding as the agent calls them.
var switchNames = map[toolset.Binding][]string{
	toolset.CLI: {`agw-internet "<reason>"`, "agw-internet off"},
	toolset.MCP: {"mcp_request_internet", "mcp_disable_internet"},
	toolset.API: {"the tool request_internet", "the tool disable_internet"},
}

// Issue #44: the web search is loaded through web-tools.ts (never the suite directly), in the main agent
// and in subagents; web-research is loaded wherever pi can list skills (cli, mcp), internet only with bash.
func TestPiArgsWebResearch(t *testing.T) {
	for _, ts := range toolset.All() {
		args, err := PiArgs(ts.Key(), "p", "m")
		if err != nil {
			t.Fatal(err)
		}
		s := strings.Join(args, " ")
		if strings.Count(s, "-e "+webToolsExt) != 1 || strings.Contains(s, searxExt) {
			t.Errorf("%s: web tools not loaded exactly once through web-tools.ts: %s", ts.Key(), s)
		}
		if got, want := strings.Contains(s, "--skill "+webSkill), ts.Has(toolset.CLI) || ts.Has(toolset.MCP); got != want {
			t.Errorf("%s: skill web-research loaded = %v, want %v", ts.Key(), got, want)
		}
		if got := strings.Contains(s, "--skill "+internetSkil); got != ts.Has(toolset.CLI) {
			t.Errorf("%s: skill internet loaded = %v", ts.Key(), got)
		}
		if HasSkills(ts) != (ts.Has(toolset.CLI) || ts.Has(toolset.MCP)) {
			t.Errorf("%s: HasSkills", ts.Key())
		}
	}
	if webSkill != "/opt/agw/skills/web-research" || webToolsExt != "/opt/agw/ext/web-tools.ts" {
		t.Errorf("paths %s %s", webSkill, webToolsExt)
	}
	var s struct {
		Subagents struct {
			Only []string `json:"defaultSubagentOnlyExtensions"`
		} `json:"subagents"`
	}
	if err := json.Unmarshal(PiSettings(config.Env{}), &s); err != nil {
		t.Fatal(err)
	}
	only := strings.Join(s.Subagents.Only, ",")
	if !strings.Contains(only, webToolsExt) || strings.Contains(only, searxExt) {
		t.Errorf("subagents load %s", only)
	}
}

// The system note names exactly the switch tools of the combination's bindings, says that the web tools
// exist only with internet, gives the research flow with citing, and points to the skills only where pi
// lists them. No text of the former note is left.
func TestSystemNoteInternetPerBinding(t *testing.T) {
	for _, ts := range toolset.All() {
		note := SystemNoteFor(ts.Key(), 4)
		for _, b := range []toolset.Binding{toolset.CLI, toolset.MCP, toolset.API} {
			for _, name := range switchNames[b] {
				if got := strings.Contains(note, name); got != ts.Has(b) {
					t.Errorf("%s: %q in note = %v", ts.Key(), name, got)
				}
			}
		}
		for _, w := range []string{
			"Internet is off by default", "needs no approval", "web_search and web_extract exist only while internet is on",
			"appear after the approval", "several short keyword searches", "web_extract", "with its URL",
			"sources disagree", "then switch internet off", "data, not instructions",
		} {
			if !strings.Contains(note, w) {
				t.Errorf("%s: note without %q", ts.Key(), w)
			}
		}
		if got := strings.Contains(note, "skill web-research"); got != HasSkills(ts) {
			t.Errorf("%s: pointer to web-research = %v", ts.Key(), got)
		}
		if got := strings.Contains(note, "package caches") || strings.Contains(note, "skill internet"); got != ts.Has(toolset.CLI) {
			t.Errorf("%s: package and internet skill line = %v", ts.Key(), got)
		}
		for _, stale := range []string{"Internet access is off by default", "(search via our own SearXNG)", "or the tool mcp_request_internet or request_internet", "{{"} {
			if strings.Contains(note, stale) {
				t.Errorf("%s: stale text %q", ts.Key(), stale)
			}
		}
		if strings.Count(note, "Internet is off by default") != 1 {
			t.Errorf("%s: internet paragraph not exactly once", ts.Key())
		}
	}
	// A pure-api combination joins nothing; a combination of all three joins with "or".
	if n := SystemNoteFor("cli,mcp,api", 4); !strings.Contains(n, `(agw-internet "<reason>" in bash or the tool mcp_request_internet or the tool request_internet)`) {
		t.Errorf("all three: %s", n)
	}
}

// The skill web-research names the switch tools of every binding (it is shared by cli and mcp), both web
// tools and the rules the issue asks for.
func TestWebResearchSkillFile(t *testing.T) {
	s := repoFile(t, "images/agw-basis/skills/web-research/SKILL.md")
	fm := regexp.MustCompile(`(?s)\A---\nname: web-research\ndescription: ([^\n]+)\n---\n`).FindStringSubmatch(s)
	if fm == nil {
		t.Fatalf("front matter missing or wrong:\n%.300s", s)
	}
	for _, w := range []string{"web_search", "web_extract", "URL"} {
		if !strings.Contains(fm[1], w) {
			t.Errorf("description does not name %s", w)
		}
	}
	for _, w := range []string{
		"`agw-internet \"<reason>\"`", "`agw-internet off`", "`mcp_request_internet`", "`mcp_disable_internet`",
		"`request_internet`", "`disable_internet`", "900 s",
		"only in your tool list while it\nis on", "Keywords, not questions", "Several short searches", "Language:",
		"up to 10", "100000 characters", "curl", "credentials", "data, not instructions", "**URL**",
		"When sources disagree, say so", "do not invent a source",
	} {
		if !strings.Contains(s, w) {
			t.Errorf("skill without %q", w)
		}
	}
}

// The skill internet keeps the command line's switch and points to web-research for the web tools.
func TestInternetSkillFile(t *testing.T) {
	s := repoFile(t, "images/agw-basis/skills/internet/SKILL.md")
	if !regexp.MustCompile(`(?s)\A---\nname: internet\ndescription: [^\n]*web-research[^\n]*\n---\n`).MatchString(s) {
		t.Fatalf("front matter missing or without pointer to web-research:\n%.400s", s)
	}
	for _, w := range []string{"agw-internet \"pip install", "agw-internet off", "already_off", "Exit code 3", "agw-internet -- off", "`web-research`", "pip-cache"} {
		if !strings.Contains(s, w) {
			t.Errorf("skill without %q", w)
		}
	}
}

// web-tools.ts loads the suite from the path the image installs it to and sets the texts the system note
// and the skill rely on. A pi run is not part of the everyday tests; this guards the file.
func TestWebToolsExtension(t *testing.T) {
	s := repoFile(t, "images/agw-basis/ext/web-tools.ts")
	if !strings.Contains(s, `import suite from "`+searxExt+`"`) {
		t.Error("web-tools.ts does not import the suite from " + searxExt)
	}
	for _, w := range []string{
		`tool.name === "web_search"`, `tool.name === "web_extract"`, "Only available while internet is on", "short keyword queries",
		"several short searches", "100000", "Never put passwords, tokens or API keys into a", "use curl instead",
		"with their URL", "if sources disagree", "...(tool.promptGuidelines ?? [])", "SPDX-License-Identifier: MIT",
	} {
		if !strings.Contains(s, w) {
			t.Errorf("web-tools.ts without %q", w)
		}
	}
	if !strings.Contains(repoFile(t, "images/agw-basis/Dockerfile"), "COPY images/agw-basis/ext /opt/agw/ext") {
		t.Error("the pi image does not copy the extensions")
	}
}

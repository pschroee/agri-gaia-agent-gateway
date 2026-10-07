// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package worker

import (
	"slices"
	"strings"
	"testing"

	"agw/internal/toolset"
)

// oldPiArgs is piArgs as it was before issue #29 (four fixed variants), kept here to show that the
// single bindings and cli,mcp (formerly both) start pi exactly as before. Since issue #34 the MCP and REST
// bindings also have the tools to switch internet off (and REST to request it); since issue #42 the
// command line also loads the skill documents.
func oldPiArgs(variant, provider, model, note string) []string {
	args := []string{"--provider", provider, "--model", model, "--append-system-prompt", note, "-e", bridgeExt, "-e", todoExt, "-e", searxExt, "-e", webGateExt}
	skills := []string{"--skill", artifactSkil, "--skill", internetSkil, "--skill", platformSkil, "--skill", typstSkill, "--skill", diagramSkill, "--skill", mermaidSkill, "--skill", documentSkil}
	switch variant {
	case "cli":
		args = append(append(args, "-e", subagentsExt, "-e", intercomExt, "--exclude-tools", noLazySubagent), skills...)
	case "mcp":
		args = append(args, "-e", mcpExt, "--tools", "read,write,ls,mcp_ping,mcp_list_artifacts,mcp_upload_artifact,mcp_request_internet,mcp_disable_internet,"+strings.Join(platformMCPTools(), ",")+",todo,web_search,web_extract")
	case "api":
		args = append(args, "-e", apiExt, "--tools", "platform_http,request_internet,disable_internet,todo,web_search,web_extract")
	case "both":
		args = append(append(args, "-e", subagentsExt, "-e", intercomExt, "--exclude-tools", noLazySubagent, "-e", mcpExt), skills...)
	}
	return args
}

// The tool lists of the four variants before issue #29, plus the internet tools of issue #34.
var oldTools = map[string][]string{
	"cli":  {"read", "bash", "edit", "write", "subagent", "todo", "bg_output", "bg_stop", "web_search", "web_extract", "intercom"},
	"mcp":  append([]string{"read", "write", "ls", "mcp_ping", "mcp_list_artifacts", "mcp_upload_artifact", "mcp_request_internet", "mcp_disable_internet"}, append(platformMCPTools(), "todo", "web_search", "web_extract")...),
	"api":  {"platform_http", "request_internet", "disable_internet", "todo", "web_search", "web_extract"},
	"both": append([]string{"read", "bash", "edit", "write", "subagent", "mcp_ping", "mcp_list_artifacts", "mcp_upload_artifact", "mcp_request_internet", "mcp_disable_internet"}, append(platformMCPTools(), "todo", "bg_output", "bg_stop", "web_search", "web_extract", "intercom")...),
}

func sorted(s []string) []string {
	out := slices.Clone(s)
	slices.Sort(out)
	return out
}

// Old variants and their keys behave exactly as before: same pi arguments, same tools, same
// hidden tools; "both" and "cli,mcp" are the same combination.
func TestOldVariantsUnchanged(t *testing.T) {
	for old, key := range map[string]string{"cli": "cli", "mcp": "mcp", "api": "api", "both": "cli,mcp"} {
		note := SystemNoteFor(old, 4)
		want := oldPiArgs(old, "p", "m", note)
		for _, v := range []string{old, key} {
			got, err := piArgs(v, "p", "m", SystemNoteFor(v, 4))
			if err != nil {
				t.Fatalf("%s: %v", v, err)
			}
			if !slices.Equal(got, want) {
				t.Errorf("%s: pi arguments changed\n got  %q\n want %q", v, got, want)
			}
			if SystemNoteFor(v, 4) != note {
				t.Errorf("%s: system note differs from %s", v, old)
			}
		}
		ts, _ := toolset.FromVariant(old)
		if got := Tools(ts); !slices.Equal(sorted(got), sorted(oldTools[old])) {
			t.Errorf("%s: tools %v, before %v", old, got, oldTools[old])
		}
		if BridgeHide(old) != BridgeHide(key) {
			t.Errorf("%s: hidden tools %q vs %q", old, BridgeHide(old), BridgeHide(key))
		}
	}
}

// The union of the tools: every tool once, the tools of each binding present.
func TestToolsUnion(t *testing.T) {
	for _, ts := range toolset.All() {
		tools := Tools(ts)
		seen := map[string]bool{}
		for _, tool := range tools {
			if seen[tool] {
				t.Errorf("%s: %s twice in %v", ts, tool, tools)
			}
			seen[tool] = true
		}
		for _, b := range ts.Bindings() {
			for _, tool := range bindingTools[b] {
				if tool == "ls" && ts.Has(toolset.CLI) {
					continue // hidden while bash is there
				}
				if !seen[tool] {
					t.Errorf("%s: %s of %s missing", ts, tool, b)
				}
			}
		}
		if seen["bash"] != ts.Has(toolset.CLI) || seen["platform_http"] != ts.Has(toolset.API) || seen["mcp_platform_request"] != ts.Has(toolset.MCP) {
			t.Errorf("%s: %v", ts, tools)
		}
	}
	// The example of the issue: command line and REST API together.
	ca, _ := toolset.Parse("cli,api")
	if got := Tools(ca); !slices.Equal(got, append(slices.Clone(oldTools["cli"]), "platform_http", "request_internet", "disable_internet")) {
		t.Errorf("cli,api: %v", got)
	}
}

// pi's arguments for the new combinations.
func TestPiArgsCombinations(t *testing.T) {
	count := func(args []string, s string) int {
		n := 0
		for _, a := range args {
			if a == s {
				n++
			}
		}
		return n
	}
	// cli,api: bash, subagents and skills as cli, plus the HTTP tool; no strict tool list.
	a, err := PiArgs("cli,api", "p", "m")
	if err != nil {
		t.Fatal(err)
	}
	s := strings.Join(a, " ")
	if count(a, apiExt) != 1 || count(a, subagentsExt) != 1 || count(a, mcpExt) != 0 || strings.Contains(s, "--tools") || !strings.Contains(s, "--skill "+platformSkil) {
		t.Errorf("cli,api: %s", s)
	}
	if BridgeHide("cli,api") != "grep,find,ls" || !strings.Contains(SystemNoteFor("cli,api", 3), "At most 3 run") {
		t.Error("cli,api: hidden tools or system note")
	}
	// mcp,api: strict list with the tools of both, ls visible, no bash, no subagents.
	a, _ = PiArgs("mcp,api", "p", "m")
	s = strings.Join(a, " ")
	i := slices.Index(a, "--tools")
	if i < 0 || count(a, mcpExt) != 1 || count(a, apiExt) != 1 || count(a, subagentsExt) != 0 || strings.Contains(s, "--skill") {
		t.Fatalf("mcp,api: %s", s)
	}
	list := strings.Split(a[i+1], ",")
	ma, _ := toolset.Parse("mcp,api")
	if !slices.Equal(list, Tools(ma)) || !slices.Contains(list, "ls") || !slices.Contains(list, "platform_http") || slices.Contains(list, "bash") {
		t.Errorf("mcp,api tools: %v", list)
	}
	if count(list, "todo") != 1 || count(list, "web_search") != 1 {
		t.Errorf("mcp,api: duplicates in %v", list)
	}
	if BridgeHide("mcp,api") != "" || strings.Contains(SystemNoteFor("mcp,api", 3), "Background tasks") {
		t.Error("mcp,api: hidden tools or system note")
	}
	// all three: everything once.
	a, _ = PiArgs("api,mcp,cli", "p", "m")
	if count(a, mcpExt) != 1 || count(a, apiExt) != 1 || count(a, subagentsExt) != 1 || slices.Contains(a, "--tools") {
		t.Errorf("cli,mcp,api: %q", a)
	}
	for _, bad := range []string{"", "shell", "cli,,mcp", "beide"} {
		if _, err := PiArgs(bad, "p", "m"); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestVariantInfo(t *testing.T) {
	if len(Variants) != 7 {
		t.Fatalf("combinations: %d", len(Variants))
	}
	ca, _ := toolset.Parse("api,cli")
	in := Info(ca)
	if in.ID != "cli,api" || in.Label != "Command line + REST API" || !slices.Equal(in.Bindings, []string{"cli", "api"}) {
		t.Errorf("info: %+v", in)
	}
	c, _ := toolset.Parse("cli")
	if Info(c).Label != "Command line (bash + agw-artifact, subagents)" {
		t.Errorf("single label: %q", Info(c).Label)
	}
}

// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package worker

import (
	"regexp"
	"strings"
	"testing"
)

const (
	cliIndexLine = "  agw-platform rename-dataset <dataset_id> <name> (approval)\n"
	apiIndexSent = "- Platform API: every operation of the platform's REST API is listed below"
	apiFallback  = "- Platform API: GET /_agw/paths lists every operation"
	apiIndexTest = "GET /datasets — List Datasets\nPATCH /datasets/{dataset_id} — Update Dataset · body: application/json\n(2 operations; * = required)\n"
)

// #198: every binding carries the directory of its interface in the system note (MCP in its tool list).
func TestSystemNotePlatformIndex(t *testing.T) {
	cli := SystemNoteFor("cli", 4, nil, apiIndexTest)
	if !strings.Contains(cli, "- Platform commands: every agw-platform command") || !strings.Contains(cli, cliIndexLine) {
		t.Errorf("cli: command index missing:\n%s", cli)
	}
	if strings.Contains(cli, apiIndexSent) || strings.Contains(cli, "PATCH /datasets/{dataset_id}") {
		t.Error("cli: carries the REST index")
	}
	mcp := SystemNoteFor("mcp", 4, nil, apiIndexTest)
	if strings.Contains(mcp, "agw-platform rename-dataset") || strings.Contains(mcp, "- Platform ") {
		t.Error("mcp: the tool list is its directory, the note adds nothing")
	}
	api := SystemNoteFor("api", 4, nil, apiIndexTest)
	if !strings.Contains(api, apiIndexSent) || !strings.Contains(api, "  PATCH /datasets/{dataset_id} — Update Dataset") {
		t.Errorf("api: index missing:\n%s", api)
	}
	if strings.Contains(api, apiFallback) || strings.Contains(api, "agw-platform rename-dataset") {
		t.Error("api: wrong text")
	}
	if fb := SystemNoteFor("api", 4, nil, ""); !strings.Contains(fb, apiFallback) || strings.Contains(fb, apiIndexSent) {
		t.Errorf("api without index: %q", fb)
	}
	all := SystemNoteFor("cli,mcp,api", 4, nil, apiIndexTest)
	if !strings.Contains(all, cliIndexLine) || !strings.Contains(all, apiIndexSent) {
		t.Error("cli,mcp,api: both directories expected")
	}
	if both := SystemNoteFor("cli,mcp", 4, nil, apiIndexTest); !strings.Contains(both, cliIndexLine) || strings.Contains(both, apiIndexSent) {
		t.Error("cli,mcp: only the command index expected")
	}
	// Experiment slots see the state after the remedy.
	for variant, want := range map[string]string{"exp:cli": cliIndexLine, "exp:api": apiIndexSent} {
		if n := SystemNoteFor(variant, 4, nil, apiIndexTest); !strings.Contains(n, want) || strings.Contains(n, "{{") {
			t.Errorf("%s: %q missing or placeholder left", variant, want)
		}
	}
	if strings.Contains(SystemNoteFor("exp:mcp", 4, nil, apiIndexTest), "- Platform ") {
		t.Error("exp:mcp: no directory expected")
	}
	// The index does not leave an empty line behind.
	if strings.Contains(cli, "\n\n- Your tools for commands and files") {
		t.Error("empty line before the tools paragraph")
	}
}

// #198: the skill points to the command index; the description keeps pi's limits.
func TestPlatformSkillDescription(t *testing.T) {
	s := repoFile(t, "images/agw-basis/skills/platform/SKILL.md")
	m := regexp.MustCompile(`(?m)^description: (.*)$`).FindStringSubmatch(s)
	if m == nil {
		t.Fatal("no description")
	}
	d := m[1]
	if len(d) > 1024 {
		t.Errorf("description has %d characters, limit 1024", len(d))
	}
	if strings.Contains(d, ": ") || strings.Contains(d, " #") {
		t.Errorf("description contains \": \" or \" #\" (breaks the front matter): %s", d)
	}
	if !strings.Contains(d, "lists every agw-platform command") {
		t.Error("description does not point to the system note")
	}
	if strings.Contains(s, "agw-platform --help   # all commands") {
		t.Error("the skill still invites to run --help")
	}
	if !strings.Contains(s, "The system note lists every command with its arguments") {
		t.Error("sentence on the command index missing")
	}
}

// #199: the REST binding gets the block of the gateway's own paths; the other bindings do not.
func TestSystemNoteGatewayPaths(t *testing.T) {
	for _, index := range []string{apiIndexTest, ""} { // with and without the platform's index
		api := SystemNoteFor("api", 4, nil, index)
		if !strings.Contains(api, "- Gateway paths: the gateway answers these paths itself") ||
			!strings.Contains(api, "tool search_next") || !strings.Contains(api, "  (") {
			t.Errorf("api (index %v): gateway paths missing", index != "")
		}
		if !strings.Contains(api, "The gateway's own paths follow under Gateway paths.") {
			t.Errorf("api (index %v): pointer sentence missing", index != "")
		}
		if strings.Contains(api, "are described in the tool platform_http") {
			t.Error("the old sentence is still there")
		}
	}
	for _, v := range []string{"cli", "mcp", "cli,mcp"} {
		if n := SystemNoteFor(v, 4, nil, apiIndexTest); strings.Contains(n, "- Gateway paths:") || strings.Contains(n, "tool search_next") {
			t.Errorf("%s: carries the gateway paths", v)
		}
	}
	if n := SystemNoteFor("cli,api", 4, nil, apiIndexTest); !strings.Contains(n, "- Gateway paths:") {
		t.Error("cli,api: gateway paths missing")
	}
	if n := SystemNoteFor("exp:api", 4, nil, apiIndexTest); !strings.Contains(n, "- Gateway paths:") || strings.Contains(n, "{{") {
		t.Error("exp:api: gateway paths missing")
	}
	if strings.Contains(SystemNoteFor("api", 4, nil, apiIndexTest), "notebook_run") {
		t.Error("notebook tools have no gateway path in the REST binding")
	}
}

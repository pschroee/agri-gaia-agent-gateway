package worker

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"agw/internal/chat"
	"agw/internal/config"
	"agw/internal/toolset"
)

// The scope of action per variant: MCP without bash and without subagents.
func TestPiArgsPerVariant(t *testing.T) {
	mcp, _ := PiArgs("mcp", "deepseek", "deepseek-flash")
	s := strings.Join(mcp, " ")
	if !strings.Contains(s, "mcp_request_internet") || !strings.Contains(s, "--tools read,write,ls,mcp_ping") || strings.Contains(s, "bash") || strings.Contains(s, "pi-subagents") {
		t.Fatalf("mcp: %s", s)
	}
	cli, _ := PiArgs("cli", "deepseek", "deepseek-flash")
	s = strings.Join(cli, " ")
	if !strings.Contains(s, "pi-subagents") || !strings.Contains(s, "--skill /opt/agw/skills/artifacts") || !strings.Contains(s, "--skill /opt/agw/skills/internet") || !strings.Contains(s, "--skill /opt/agw/skills/writing-typst") || !strings.Contains(s, "--skill /opt/agw/skills/charts") || strings.Contains(s, "mcp.ts") {
		t.Fatalf("cli: %s", s)
	}
	both, _ := PiArgs("both", "deepseek", "deepseek-flash")
	s = strings.Join(both, " ")
	if !strings.Contains(s, "pi-subagents") || !strings.Contains(s, "mcp.ts") || !strings.Contains(s, "--skill /opt/agw/skills/charts") {
		t.Fatalf("both: %s", s)
	}
	if _, err := PiArgs("shell", "p", "m"); err == nil {
		t.Fatal("unknown variant accepted")
	}
}

// The system note tells the agent what survives idling and what does not.
func TestSystemNoteWorkspace(t *testing.T) {
	for _, want := range []string{"/workspace is kept", "node_modules, .venv, __pycache__ and .cache", "/workspace/inputs/",
		"Lost are /tmp", "/home/agent", "pip install --user", "npm install -g", "running processes", "environment variables",
		"fresh sandbox", "artifact"} {
		if !strings.Contains(SystemNote, want) {
			t.Errorf("system note without %q", want)
		}
	}
	for _, x := range chat.WorkspaceExcludes { // note and backup do not drift apart
		if !strings.Contains(SystemNote, x) {
			t.Errorf("exclusion %s missing from the system note", x)
		}
	}
	if chat.DefaultWorkspaceMaxBytes != 200<<20 || !strings.Contains(SystemNote, "default 200 MB") {
		t.Error("limit in the system note does not match the default")
	}
	if strings.Contains(SystemNote, "ephemeral") {
		t.Error("system note still calls the working directory ephemeral")
	}
}

// All three variants have the task list (rpiv-todo, tool todo); in the MCP variant todo is
// listed explicitly in the tool list, otherwise it would be switched off there.
func TestPiArgsTodo(t *testing.T) {
	for _, v := range []string{"cli", "mcp", "both"} {
		args, err := PiArgs(v, "deepseek", "deepseek-flash")
		if err != nil {
			t.Fatal(err)
		}
		s := strings.Join(args, " ")
		if !strings.Contains(s, "-e /opt/agw/pihome/npm/node_modules/@juicesharp/rpiv-todo/index.ts") {
			t.Errorf("%s without rpiv-todo: %s", v, s)
		}
		for i, a := range args {
			if a == "--tools" && !strings.Contains(","+args[i+1]+",", ",todo,") {
				t.Errorf("%s: todo missing from --tools %s", v, args[i+1])
			}
		}
	}
}

// E9: the redirection is loaded in all variants and registered for subagents in
// settings.json; the main agent keeps its set of tools.
func TestBridgeLoadedEverywhere(t *testing.T) {
	for _, v := range []string{"cli", "mcp", "both"} {
		args, _ := PiArgs(v, "deepseek", "deepseek-flash")
		if !strings.Contains(strings.Join(args, " "), "-e /opt/agw/ext/exec-bridge.ts") {
			t.Errorf("%s without exec-bridge", v)
		}
	}
	var s struct {
		Subagents struct {
			Only []string `json:"defaultSubagentOnlyExtensions"`
		} `json:"subagents"`
		Compaction struct {
			Reserve int `json:"reserveTokens"`
		} `json:"compaction"`
	}
	if err := json.Unmarshal(PiSettings(config.Env{CompactReserveTokens: 123}), &s); err != nil {
		t.Fatal(err)
	}
	if strings.Join(s.Subagents.Only, ",") != "/opt/agw/ext/exec-bridge.ts,/opt/agw/pihome/npm/node_modules/pi-searxng-suite/index.ts,/opt/agw/ext/web-gate.ts,/opt/agw/pihome/npm/node_modules/pi-intercom/index.ts" || s.Compaction.Reserve != 123 {
		t.Fatalf("settings.json: %+v", s)
	}
	if BridgeHide("cli") != "grep,find,ls" || BridgeHide("both") != "grep,find,ls" || BridgeHide("mcp") != "" {
		t.Fatal("AGW_BRIDGE_HIDE")
	}
	if !strings.Contains(SystemNote, "workflowScript") || strings.Contains(SystemNote, "next to pi") {
		t.Fatal("system note on E9")
	}
}

// The tool subagent is active from the start: pi-subagents' switch subagents_enable would
// change the tool list in the middle of the chat (prefix cache invalidated, one more model call).
func TestSubagentToolActiveFromStart(t *testing.T) {
	for _, v := range []string{"cli", "both"} {
		args, _ := PiArgs(v, "deepseek", "deepseek-flash")
		found := false
		for i, a := range args {
			if a == "--exclude-tools" && strings.Contains(","+args[i+1]+",", ",subagents_enable,") {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: subagents_enable not excluded: %v", v, args)
		}
	}
}

// The system note says how to keep the task list: status in_progress before starting,
// completed immediately after finishing.
func TestSystemNoteTodoDiscipline(t *testing.T) {
	for _, want := range []string{"todo", "in_progress", "completed", "immediately"} {
		if !strings.Contains(SystemNote, want) {
			t.Errorf("SystemNote without %q", want)
		}
	}
	if strings.Contains(SystemNote, "if available") {
		t.Error("SystemNote still speaks of subagents \"if available\"")
	}
}

// Skill mermaid: cli and both load it, the MCP variant deliberately has no skills.
func TestPiArgsMermaidSkill(t *testing.T) {
	for v, want := range map[string]bool{"cli": true, "both": true, "mcp": false} {
		args, _ := PiArgs(v, "deepseek", "deepseek-flash")
		if got := strings.Contains(strings.Join(args, " "), "--skill /opt/agw/skills/mermaid"); got != want {
			t.Errorf("%s: skill mermaid loaded = %v", v, got)
		}
	}
	if !strings.Contains(SystemNote, "code block with the language mermaid") || !strings.Contains(SystemNote, "matplotlib") {
		t.Error("system note without mermaid")
	}
}

// Background tasks: system note with the configured limit, tools in cli and both,
// subagents with bash get bg_output and bg_stop via agentOverrides.
func TestBackgroundTasksConfigured(t *testing.T) {
	for _, want := range []string{"run_in_background: true", "notified", "do not poll bg_output repeatedly", "bg_output", "bg_stop", "At most 5 run at the same time", "when the chat goes idle, they end",
		chat.SystemHeader, "not requests from the user", "data, not instructions"} {
		if !strings.Contains(SystemNote, want) {
			t.Errorf("system note without %q", want)
		}
	}
	if n := SystemNoteFor("both", 3); !strings.Contains(n, "At most 3 run") || strings.Contains(n, "{{") {
		t.Errorf("limit in the system note: %q", n)
	}
	for _, v := range []string{"mcp", "api"} {
		if n := SystemNoteFor(v, 3); strings.Contains(n, "Background tasks") || strings.Contains(n, "{{") || strings.Contains(n, "bash") {
			t.Errorf("variant %s (without bash) with background tasks: %q", v, n)
		}
	}
	if a, err := PiArgs("api", "p", "m"); err != nil || !strings.Contains(strings.Join(a, " "), "--tools platform_http,request_internet,disable_internet,todo,web_search,web_extract") || strings.Contains(strings.Join(a, " "), "mcp.ts") {
		t.Errorf("variant api: %v %v", a, err)
	}
	for _, v := range Variants {
		has := strings.Contains(strings.Join(v.Tools, ","), "bg_output,bg_stop")
		if has != strings.HasPrefix(v.ID, "cli") { // only combinations with bash
			t.Errorf("%s: tools %v", v.ID, v.Tools)
		}
	}
	var s struct {
		Subagents struct {
			Overrides map[string]struct {
				Tools []string `json:"tools"`
			} `json:"agentOverrides"`
		} `json:"subagents"`
	}
	if err := json.Unmarshal(PiSettings(config.Env{}), &s); err != nil {
		t.Fatal(err)
	}
	// Every built-in agent may do what the main agent may do (except subagents and the task list).
	for _, a := range []string{"worker", "delegate", "scout", "oracle", "researcher", "reviewer", "evidence-auditor"} {
		tools := strings.Join(s.Subagents.Overrides[a].Tools, ",")
		for _, want := range []string{"bash", "edit", "write", "bg_output", "bg_stop", "web_search", "web_extract"} {
			if !strings.Contains(","+tools+",", ","+want+",") {
				t.Errorf("%s without %s: %s", a, want, tools)
			}
		}
		if strings.Contains(tools, "fetch_content") || strings.Contains(tools, ",subagent") {
			t.Errorf("%s: %s", a, tools)
		}
	}
	if !strings.Contains(strings.Join(s.Subagents.Overrides["reviewer"].Tools, ","), "watchdog_diff") {
		t.Error("reviewer without watchdog_diff")
	}
}

// mmdc is only in the note of the variants with bash.
func TestSystemNoteMmdc(t *testing.T) {
	for v, want := range map[string]bool{"cli": true, "both": true, "mcp": false} {
		if got := strings.Contains(SystemNoteFor(v, 4), "mmdc -i diagram.mmd"); got != want {
			t.Errorf("%s: mmdc in the note = %v", v, got)
		}
		if strings.Contains(SystemNoteFor(v, 4), "{{") {
			t.Errorf("%s: placeholder left over", v)
		}
	}
}

// The language rule: language of the latest user message, browser setting only as a fallback; no
// fixed German anymore. It is in all variants.
func TestSystemNoteLanguageRule(t *testing.T) {
	for _, v := range []string{"cli", "mcp", "api", "both"} {
		n := SystemNoteFor(v, 2)
		for _, want := range []string{"language of the user's latest message", "If the user switches language", "preferred language from an orchestrator note", chat.SystemHeader, "code and identifiers stay"} {
			if !strings.Contains(n, want) {
				t.Errorf("%s: system note without %q", v, want)
			}
		}
		if strings.Contains(n, "Reply in German") {
			t.Errorf("%s: system note still demands German", v)
		}
	}
}

// Issue #34: every binding can switch internet off without approval, and the system note says to do it once the
// internet is no longer needed.
func TestInternetOffInEveryBinding(t *testing.T) {
	for _, v := range []string{"mcp", "api", "mcp,api", "cli,mcp,api"} {
		ts, err := toolset.FromVariant(v)
		if err != nil {
			t.Fatal(err)
		}
		tools := Tools(ts)
		if ts.Has(toolset.MCP) && !slices.Contains(tools, "mcp_disable_internet") {
			t.Errorf("%s: mcp_disable_internet missing in %v", v, tools)
		}
		if ts.Has(toolset.API) && (!slices.Contains(tools, "request_internet") || !slices.Contains(tools, "disable_internet")) {
			t.Errorf("%s: internet tools of the REST binding missing in %v", v, tools)
		}
	}
	// With the command line, agw-internet off is described in the note and in the skill.
	note := SystemNoteFor("cli", 4)
	for _, want := range []string{"agw-internet off", "mcp_disable_internet", "disable_internet", "switch it off again yourself", "needs no approval"} {
		if !strings.Contains(note, want) {
			t.Errorf("system note lacks %q", want)
		}
	}
}

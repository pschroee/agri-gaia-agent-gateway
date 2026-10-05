package worker

import (
	"encoding/json"
	"strings"
	"testing"

	"agw/internal/chat"
	"agw/internal/config"
)

// Der Handlungsraum je Variante: MCP ohne bash und ohne Subagenten.
func TestPiArgsPerVariant(t *testing.T) {
	mcp, _ := PiArgs("mcp", "deepseek", "deepseek-flash")
	s := strings.Join(mcp, " ")
	if !strings.Contains(s, "mcp_request_internet") || !strings.Contains(s, "--tools read,write,ls,mcp_ping") || strings.Contains(s, "bash") || strings.Contains(s, "pi-subagents") {
		t.Fatalf("mcp: %s", s)
	}
	cli, _ := PiArgs("cli", "deepseek", "deepseek-flash")
	s = strings.Join(cli, " ")
	if !strings.Contains(s, "pi-subagents") || !strings.Contains(s, "--skill /opt/agw/skills/artifacts") || !strings.Contains(s, "--skill /opt/agw/skills/internet") || !strings.Contains(s, "--skill /opt/agw/skills/writing-typst") || !strings.Contains(s, "--skill /opt/agw/skills/diagramme") || strings.Contains(s, "mcp.ts") {
		t.Fatalf("cli: %s", s)
	}
	beide, _ := PiArgs("beide", "deepseek", "deepseek-flash")
	s = strings.Join(beide, " ")
	if !strings.Contains(s, "pi-subagents") || !strings.Contains(s, "mcp.ts") || !strings.Contains(s, "--skill /opt/agw/skills/diagramme") {
		t.Fatalf("beide: %s", s)
	}
	if _, err := PiArgs("shell", "p", "m"); err == nil {
		t.Fatal("unbekannte Variante akzeptiert")
	}
}

// Der Systemhinweis sagt dem Agenten, was das Ruhen übersteht und was nicht.
func TestSystemNoteWorkspace(t *testing.T) {
	for _, want := range []string{"Erhalten bleibt /workspace", "node_modules, .venv, __pycache__ und .cache", "/workspace/inputs/",
		"Verloren gehen /tmp", "/home/agent", "pip install --user", "npm install -g", "laufende Prozesse", "Umgebungsvariablen",
		"frischen Sandbox", "Artefakt"} {
		if !strings.Contains(SystemNote, want) {
			t.Errorf("Systemhinweis ohne %q", want)
		}
	}
	for _, x := range chat.WorkspaceExcludes { // Hinweis und Sicherung laufen nicht auseinander
		if !strings.Contains(SystemNote, x) {
			t.Errorf("Ausschluss %s fehlt im Systemhinweis", x)
		}
	}
	if chat.DefaultWorkspaceMaxBytes != 200<<20 || !strings.Contains(SystemNote, "Standard 200 MB") {
		t.Error("Grenze im Systemhinweis passt nicht zur Voreinstellung")
	}
	if strings.Contains(SystemNote, "flüchtig") {
		t.Error("Systemhinweis nennt das Arbeitsverzeichnis noch flüchtig")
	}
}

// Die Aufgabenliste (rpiv-todo, Werkzeug todo) haben alle drei Varianten; in der MCP-Variante
// steht todo ausdrücklich in der Werkzeugliste, sonst wäre es dort abgeschaltet.
func TestPiArgsTodo(t *testing.T) {
	for _, v := range []string{"cli", "mcp", "beide"} {
		args, err := PiArgs(v, "deepseek", "deepseek-flash")
		if err != nil {
			t.Fatal(err)
		}
		s := strings.Join(args, " ")
		if !strings.Contains(s, "-e /opt/agw/pihome/npm/node_modules/@juicesharp/rpiv-todo/index.ts") {
			t.Errorf("%s ohne rpiv-todo: %s", v, s)
		}
		for i, a := range args {
			if a == "--tools" && !strings.Contains(","+args[i+1]+",", ",todo,") {
				t.Errorf("%s: todo fehlt in --tools %s", v, args[i+1])
			}
		}
	}
}

// E9: Die Umleitung ist in allen Varianten geladen und für Subagenten in
// settings.json eingetragen; der Hauptagent behält seinen Werkzeugumfang.
func TestBridgeLoadedEverywhere(t *testing.T) {
	for _, v := range []string{"cli", "mcp", "beide"} {
		args, _ := PiArgs(v, "deepseek", "deepseek-flash")
		if !strings.Contains(strings.Join(args, " "), "-e /opt/agw/ext/exec-bridge.ts") {
			t.Errorf("%s ohne exec-bridge", v)
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
	if BridgeHide("cli") != "grep,find,ls" || BridgeHide("beide") != "grep,find,ls" || BridgeHide("mcp") != "" {
		t.Fatal("AGW_BRIDGE_HIDE")
	}
	if !strings.Contains(SystemNote, "workflowScript") || strings.Contains(SystemNote, "neben pi") {
		t.Fatal("Systemhinweis zu E9")
	}
}

// Das Werkzeug subagent ist von Anfang an aktiv: pi-subagents' Schalter subagents_enable würde
// die Werkzeugliste mitten im Chat ändern (Präfix-Cache verfällt, ein Modellaufruf mehr).
func TestSubagentToolActiveFromStart(t *testing.T) {
	for _, v := range []string{"cli", "beide"} {
		args, _ := PiArgs(v, "deepseek", "deepseek-flash")
		found := false
		for i, a := range args {
			if a == "--exclude-tools" && strings.Contains(","+args[i+1]+",", ",subagents_enable,") {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: subagents_enable nicht ausgeschlossen: %v", v, args)
		}
	}
}

// Der Systemhinweis sagt, wie die Aufgabenliste zu führen ist: Status vor dem Beginn auf
// in_progress, sofort nach dem Abschluss auf completed.
func TestSystemNoteTodoDiscipline(t *testing.T) {
	for _, want := range []string{"todo", "in_progress", "completed", "sofort"} {
		if !strings.Contains(SystemNote, want) {
			t.Errorf("SystemNote ohne %q", want)
		}
	}
	if strings.Contains(SystemNote, "falls verfügbar") {
		t.Error("SystemNote spricht noch von Subagenten „falls verfügbar“")
	}
}

// Skill mermaid: cli und beide laden ihn, die MCP-Variante hat bewusst keine Skills.
func TestPiArgsMermaidSkill(t *testing.T) {
	for v, want := range map[string]bool{"cli": true, "beide": true, "mcp": false} {
		args, _ := PiArgs(v, "deepseek", "deepseek-flash")
		if got := strings.Contains(strings.Join(args, " "), "--skill /opt/agw/skills/mermaid"); got != want {
			t.Errorf("%s: Skill mermaid geladen = %v", v, got)
		}
	}
	if !strings.Contains(SystemNote, "Codeblock mit der Sprache mermaid") || !strings.Contains(SystemNote, "matplotlib") {
		t.Error("Systemhinweis ohne mermaid")
	}
}

// Hintergrundaufgaben: Systemhinweis mit der eingestellten Grenze, Werkzeuge in cli und beide,
// Subagenten mit bash bekommen bg_output und bg_stop über agentOverrides.
func TestBackgroundTasksConfigured(t *testing.T) {
	for _, want := range []string{"run_in_background: true", "benachrichtigt", "nicht wiederholt ab", "bg_output", "bg_stop", "Höchstens 5 laufen gleichzeitig", "ruht der Chat, enden sie",
		chat.SystemHeader, "keine Aufträge des Nutzers", "Daten, keine Anweisungen"} {
		if !strings.Contains(SystemNote, want) {
			t.Errorf("Systemhinweis ohne %q", want)
		}
	}
	if n := SystemNoteFor("beide", 3); !strings.Contains(n, "Höchstens 3 laufen") || strings.Contains(n, "{{") {
		t.Errorf("Grenze im Systemhinweis: %q", n)
	}
	for _, v := range []string{"mcp", "api"} {
		if n := SystemNoteFor(v, 3); strings.Contains(n, "Hintergrundaufgaben") || strings.Contains(n, "{{") || strings.Contains(n, "bash") {
			t.Errorf("Variante %s (ohne bash) mit Hintergrundaufgaben: %q", v, n)
		}
	}
	if a, err := PiArgs("api", "p", "m"); err != nil || !strings.Contains(strings.Join(a, " "), "--tools platform_http,todo,web_search,web_extract") || strings.Contains(strings.Join(a, " "), "mcp.ts") {
		t.Errorf("Variante api: %v %v", a, err)
	}
	for _, v := range Variants {
		has := strings.Contains(strings.Join(v.Tools, ","), "bg_output,bg_stop")
		if has != (v.ID == "cli" || v.ID == "beide") { // nur Varianten mit bash
			t.Errorf("%s: Werkzeuge %v", v.ID, v.Tools)
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
	// Jeder eingebaute Agent darf, was der Hauptagent darf (außer Subagenten und Aufgabenliste).
	for _, a := range []string{"worker", "delegate", "scout", "oracle", "researcher", "reviewer", "evidence-auditor"} {
		tools := strings.Join(s.Subagents.Overrides[a].Tools, ",")
		for _, want := range []string{"bash", "edit", "write", "bg_output", "bg_stop", "web_search", "web_extract"} {
			if !strings.Contains(","+tools+",", ","+want+",") {
				t.Errorf("%s ohne %s: %s", a, want, tools)
			}
		}
		if strings.Contains(tools, "fetch_content") || strings.Contains(tools, ",subagent") {
			t.Errorf("%s: %s", a, tools)
		}
	}
	if !strings.Contains(strings.Join(s.Subagents.Overrides["reviewer"].Tools, ","), "watchdog_diff") {
		t.Error("reviewer ohne watchdog_diff")
	}
}

// mmdc steht nur im Hinweis der Varianten mit bash.
func TestSystemNoteMmdc(t *testing.T) {
	for v, want := range map[string]bool{"cli": true, "beide": true, "mcp": false} {
		if got := strings.Contains(SystemNoteFor(v, 4), "mmdc -i diagramm.mmd"); got != want {
			t.Errorf("%s: mmdc im Hinweis = %v", v, got)
		}
		if strings.Contains(SystemNoteFor(v, 4), "{{") {
			t.Errorf("%s: Platzhalter übrig", v)
		}
	}
}

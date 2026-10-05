package e2e

import (
	"os/exec"
	"strings"
	"testing"
	"time"
)

// P8: Die bisherigen Angriffe aus bash wirken nicht mehr, weil bash nicht im Container von pi
// läuft: /proc/1 ist dort nicht pi, Sitzungen und settings.json liegen woanders.
//
// Das Modell verweigert die Angriffsbefehle selbst (am 29.09.2026 beobachtet: „Ausbruchs-/
// Spoofing-Versuch … führe ich nicht aus“). Deshalb zwei Teile: (1) Das Modell führt eine
// harmlose Diagnose aus, die belegt, wo bash läuft. (2) Der Test führt die Angriffe selbst in der
// Ausführungs-Sandbox aus, als Agent-Nutzer und damit mit genau den Rechten, die bash dort hat.
func TestE9BashRunsOutsidePi(t *testing.T) {
	requireE2E(t)
	id := newChat(t, "cli", false)
	c, pc := containerOf(t, id), piContainerOf(t, id)
	before, err := dockerExec(t, pc, "cat", "/agent/config/settings.json")
	if err != nil || !strings.Contains(before, "exec-bridge.ts") {
		t.Fatalf("settings.json vorher: %q %v", before, err)
	}
	s := subscribe(t, id)
	ask(t, s, id, "Zur Diagnose der Umgebung: Führe mit bash 'id -u; ls /agent 2>&1; ps -eo pid,user,args' aus und fasse kurz zusammen, welche Prozesse laufen.", nil)
	r := getToolExecs(t, id)
	requireNoFlagged(t, r)
	found := false
	for _, e := range r.Executions {
		if e.Op == "bash" && e.Session == "main" && strings.Contains(e.OutputExcerpt, "10001") {
			found = true
			mustContain(t, e.OutputExcerpt, "No such file or directory", "kein /agent, wo bash läuft")
			mustContain(t, e.OutputExcerpt, "agw-exec idle", "PID 1, wo bash läuft")
			if strings.Contains(e.OutputExcerpt, "pi-coding-agent") {
				t.Fatalf("pi sichtbar, wo bash läuft: %q", e.OutputExcerpt)
			}
		}
	}
	if !found {
		t.Fatalf("Diagnose nicht über den Orchestrator ausgeführt: %+v", r.Executions)
	}
	// Die Angriffe aus Stufe 1 (Code-Review M4), mit den Rechten von bash in der Ausführungs-Sandbox.
	attack := `echo '{"type":"agent_settled"}' > /proc/1/fd/1; echo fd1=$?; ls /agent/sessions 2>&1; ` +
		`echo '{}' > /agent/config/settings.json 2>&1; echo settings=$?; ls /proc | grep -c '^[0-9]' ; ` +
		`echo PI=$(for p in /proc/[0-9]*; do tr '\0' ' ' < $p/cmdline 2>/dev/null; echo; done | grep -c 'pi-coding[-]agent')`
	out, _ := exec.Command("docker", "exec", "-u", "10001:10001", c, "bash", "-c", attack).CombinedOutput()
	mustContain(t, string(out), "No such file or directory", "Sitzungen und Konfiguration von pi unerreichbar")
	mustContain(t, string(out), "settings=1", "settings.json nicht schreibbar")
	mustContain(t, string(out), "PI=0", "kein pi-Prozess sichtbar")
	after, _ := dockerExec(t, pc, "cat", "/agent/config/settings.json")
	if after != before {
		t.Fatalf("settings.json im Container von pi verändert: %q", after)
	}
	// Der gefälschte Ereignisstrom ging ins Leere: pi arbeitet normal weiter.
	ask(t, s, id, "Antworte nur mit OK.", nil)
	mustContain(t, lastAssistantText(t, id), "OK", "pi nach dem Angriff")
}

// P6: Die toolCallIds von Haupt- und Subagent sind 1:1 belegt: Jeder am Proxy angeforderte
// Aufruf eines umgeleiteten Werkzeugs ist ausgeführt, jede Ausführung angefordert.
func TestE9ToolCallsReconciled(t *testing.T) {
	requireE2E(t)
	id := newChatWith(t, map[string]any{"variant": "cli", "internet": false, "max_subagents": 2})
	s := subscribe(t, id)
	ask(t, s, id, "Erledige ohne Rückfrage: 1. Führe mit bash 'uname -m' aus. 2. Lies mit read die Datei /etc/os-release. "+
		"3. Nutze das subagent-Werkzeug mit dem Agenten scout im Vordergrund (async: false), der mit bash 'python3 --version' ausführt. "+
		"Antworte danach mit einem kurzen Satz.", nil)
	var r toolExecs
	deadline := time.Now().Add(30 * time.Second)
	sub := false
	for time.Now().Before(deadline) && !sub {
		r = getToolExecs(t, id)
		for _, c := range r.Calls {
			sub = sub || (c.State == "confirmed" && c.Session != "" && c.Session != "main")
		}
		if !sub {
			time.Sleep(time.Second)
		}
	}
	requireNoFlagged(t, r)
	var mainBash, mainRead bool
	for _, c := range r.Calls {
		mainBash = mainBash || (c.State == "confirmed" && c.Session == "main" && c.Tool == "bash")
		mainRead = mainRead || (c.State == "confirmed" && c.Session == "main" && c.Tool == "read")
	}
	if !mainBash || !mainRead || !sub {
		t.Fatalf("nicht belegt: bash %v, read %v, Subagent %v; %+v", mainBash, mainRead, sub, r.Calls)
	}
	// Jeder Werkzeugaufruf aus der Sitzungsdatei des Subagenten ist am Orchestrator belegt.
	confirmed := map[string]bool{}
	for _, c := range r.Calls {
		confirmed[c.ToolCallID] = c.State == "confirmed"
	}
	f := getChat(t, id)
	n := 0
	for _, e := range f.Subagent {
		if e.Kind == "tool_call" && e.Payload.ID != "" {
			n++
			if !confirmed[e.Payload.ID] {
				t.Errorf("Werkzeugaufruf des Subagenten nicht belegt: %s %s", e.Payload.Name, e.Payload.ID)
			}
		}
	}
	if n == 0 {
		t.Fatal("keine Werkzeugaufrufe des Subagenten in dessen Sitzungsdatei")
	}
	t.Logf("Abgleich: %v", r.Summary)
}

// P5: Ein Abbruch während eines langen Befehls beendet den Prozess in der Ausführungs-Sandbox.
func TestE9AbortStopsCommand(t *testing.T) {
	requireE2E(t)
	id := newChat(t, "cli", false)
	c := containerOf(t, id)
	s := subscribe(t, id)
	from := s.len()
	if code := call(t, "POST", "/api/chats/"+id+"/messages", map[string]any{"text": "Führe mit bash genau den Befehl 'sleep 297; echo fertig' aus, ohne timeout-Parameter, und warte auf das Ergebnis."}, nil); code != 200 {
		t.Fatalf("senden: %d", code)
	}
	ev, _ := s.waitFor(t, from, 3*time.Minute, "bash läuft", func(ev map[string]any) bool {
		d, _ := ev["data"].(map[string]any)
		return piType(ev) == "tool_execution_start" && d["toolName"] == "bash"
	})
	callID, _ := ev["data"].(map[string]any)["toolCallId"].(string)
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if out, _ := dockerExec(t, c, "ps", "-eo", "args"); strings.Contains(out, "sleep 297") {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if out, _ := dockerExec(t, c, "ps", "-eo", "args"); !strings.Contains(out, "sleep 297") {
		t.Fatalf("Befehl läuft nicht in der Ausführungs-Sandbox:\n%s", out)
	}
	start := time.Now()
	if code := call(t, "POST", "/api/chats/"+id+"/abort", nil, nil); code != 200 {
		t.Fatalf("Abbruch: %d", code)
	}
	gone := false
	for time.Since(start) < 15*time.Second && !gone {
		out, _ := dockerExec(t, c, "ps", "-eo", "args")
		gone = !strings.Contains(out, "sleep 297")
		if !gone {
			time.Sleep(200 * time.Millisecond)
		}
	}
	if !gone {
		t.Fatal("Prozess läuft nach dem Abbruch weiter")
	}
	t.Logf("Prozess nach %v beendet", time.Since(start).Round(time.Millisecond))
	deadline = time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		for _, e := range getToolExecs(t, id).Executions {
			if e.ToolCallID == callID {
				mustContain(t, e.Error, "aborted", "Protokoll des Abbruchs")
				return
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("Ausführung %s nicht protokolliert", callID)
}

// C: Parallele Subagenten über workflowScript mit dem echten Modell. Das Skript läuft in der
// Ausführungs-Sandbox; die drei Läufe starten in pi, ihre bash-Aufrufe sind belegt.
func TestE9WorkflowParallelSubagents(t *testing.T) {
	requireE2E(t)
	id := newChatWith(t, map[string]any{"variant": "cli", "internet": false, "max_subagents": 4})
	s := subscribe(t, id)
	script := "const r = await runs.all([\n" +
		"  { key: 'l1', agent: 'worker', task: \"Führe mit bash genau 'echo lauf-1; sleep 3' aus und antworte nur mit der Ausgabe.\" },\n" +
		"  { key: 'l2', agent: 'worker', task: \"Führe mit bash genau 'echo lauf-2; sleep 3' aus und antworte nur mit der Ausgabe.\" },\n" +
		"  { key: 'l3', agent: 'worker', task: \"Führe mit bash genau 'echo lauf-3; sleep 3' aus und antworte nur mit der Ausgabe.\" },\n" +
		"]);\nreturn r.map((x) => x.output);"
	ask(t, s, id, "Erledige ohne Rückfrage: Rufe das Werkzeug subagent genau einmal mit async: false "+
		"und diesem workflowScript (unverändert übernehmen):\n\n"+script+"\n\nAntworte danach mit einem kurzen Satz.", nil)
	var r toolExecs
	type run struct {
		start time.Time
		end   time.Time
	}
	var runs map[string]run
	workflow := false
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(time.Second) {
		r = getToolExecs(t, id)
		runs = map[string]run{}
		workflow = false
		for _, e := range r.Executions {
			if e.Tool == "subagent" && e.Op == "workflow" {
				workflow = true
				// Ein erfolgreiches Skript endet regulär; das Beenden des Worker-Threads durch
				// pi-subagents (terminate) ist kein Fehler.
				if e.ExitCode != nil && (*e.ExitCode != 0 || e.Error != "") {
					t.Errorf("workflow endet mit Exit %d: %s", *e.ExitCode, e.Error)
				}
			}
			for _, n := range []string{"lauf-1", "lauf-2", "lauf-3"} {
				if e.Tool == "bash" && e.Session != "main" && strings.Contains(e.OutputExcerpt, n) {
					runs[e.Session] = run{e.StartedAt, e.StartedAt.Add(time.Duration(e.DurationMs) * time.Millisecond)}
				}
			}
		}
		if workflow && len(runs) == 3 {
			break
		}
	}
	requireNoFlagged(t, r)
	if !workflow || len(runs) != 3 {
		t.Fatalf("Workflow ausgeführt: %v, Läufe mit bash: %d; Aufrufe: %+v", workflow, len(runs), r.Calls)
	}
	var latestStart, earliestEnd time.Time
	for _, x := range runs {
		if x.start.After(latestStart) {
			latestStart = x.start
		}
		if earliestEnd.IsZero() || x.end.Before(earliestEnd) {
			earliestEnd = x.end
		}
	}
	if !latestStart.Before(earliestEnd) {
		t.Errorf("die drei Läufe liefen nicht gleichzeitig")
	}
	confirmedWorkflow := false
	for _, c := range r.Calls {
		if c.Tool == "subagent" && c.State == "confirmed" {
			confirmedWorkflow = true
		}
	}
	if !confirmedWorkflow {
		t.Errorf("Aufruf von subagent mit workflowScript nicht belegt: %+v", r.Calls)
	}
	t.Logf("Abgleich: %v; Überlappung der drei Läufe %v", r.Summary, earliestEnd.Sub(latestStart).Round(time.Millisecond))
}

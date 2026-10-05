package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"
)

// bigText erzeugt einen Text von rund n Zeilen, den das Modell nicht
// zusammenfassen kann, ohne ihn zu lesen (je Zeile eine eigene Messung).
func bigText(seed, n int) string {
	var b strings.Builder
	x := seed*7919 + 17
	for i := 0; i < n; i++ {
		x = (x*1103515245 + 12345) % 2147483648
		fmt.Fprintf(&b, "Messung %04d: Stall %c, Temperatur %d,%d Grad, Feuchte %d %%, Kennung %08x\n", i, 'A'+rune(x%6), 15+x%12, x%10, 40+x%50, x)
	}
	return b.String()
}

// Die Tests laufen nacheinander (kein t.Parallel), weil sie sich den Pool teilen.

func TestSandboxHardening(t *testing.T) {
	requireE2E(t)
	id := newChat(t, "cli", false)
	c, pc := containerOf(t, id), piContainerOf(t, id)
	// Ausführungs-Sandbox (E9): kein Schlüssel, keine Konfiguration von pi, kein Weg zu API oder Proxy.
	out, _ := dockerExec(t, c, "sh", "-c", "env; ls /agent 2>&1")
	if strings.Contains(out, "sk-") {
		t.Fatal("API-Schlüssel in der Ausführungs-Sandbox")
	}
	mustContain(t, out, "No such file", "kein /agent in der Ausführungs-Sandbox")
	out, _ = dockerExec(t, c, "sh", "-c", "curl -s -m 5 http://orchestrator:18480/api/chats || echo API-UNERREICHBAR; curl -s -m 5 http://orchestrator:18481/ || echo PROXY-UNERREICHBAR")
	mustContain(t, out, "API-UNERREICHBAR", "API aus der Ausführungs-Sandbox")
	mustContain(t, out, "PROXY-UNERREICHBAR", "Proxy aus der Ausführungs-Sandbox")
	out, _ = dockerExec(t, c, "sh", "-c", "curl -s -m 5 -o /dev/null -w '%{http_code}' https://example.com || echo KEIN_NETZ")
	mustContain(t, out, "KEIN_NETZ", "Internet ohne Freigabe")
	out, _ = dockerExec(t, c, "sh", "-c", "touch /usr/x 2>&1; id -u")
	mustContain(t, out, "Read-only", "Wurzel schreibgeschützt")
	mustContain(t, out, "10001", "unprivilegierter Nutzer")
	// Container von pi: ohne Shell, ohne Schlüssel; die API bleibt gesperrt, der Proxy prüft das Modell.
	if _, err := dockerExec(t, pc, "sh", "-c", "true"); err == nil {
		t.Fatal("Shell im Container von pi")
	}
	out, _ = dockerExec(t, pc, "cat", "/agent/config/models.json", "/proc/1/environ")
	if strings.Contains(out, "sk-") {
		t.Fatal("API-Schlüssel im Container von pi")
	}
	mustContain(t, nodeFetch(t, pc, "GET", "http://orchestrator:18480/api/chats", ""), "kein Zugriff aus dem Sandbox-Netz", "API aus dem Container von pi")
	mustContain(t, nodeFetch(t, pc, "POST", "http://orchestrator:18481/llm/deepseek/chat/completions", `{"model":"fremd"}`), "nicht freigegeben", "Modell-Positivliste")
	mustContain(t, nodeFetch(t, pc, "GET", "https://example.com", ""), "FEHLER", "Internet im Container von pi")
}

// Angriffswege aus dem Code-Review (K2, K3, H5): mit eingeschaltetem Internet.
func TestSandboxEscapesBlocked(t *testing.T) {
	requireE2E(t)
	a := newChat(t, "cli", true)
	b := newChat(t, "cli", true)
	ca, cb := containerOf(t, a), containerOf(t, b)
	// K3: Nutzer-API über den Host ohne Token
	out, _ := dockerExec(t, ca, "sh", "-c", "curl -s -m 5 http://host.docker.internal:18480/api/approvals?state=pending; echo; curl -s -m 5 -o /dev/null -w '%{http_code}' -X POST http://host.docker.internal:18480/api/approvals/x -d '{\"approve\":true}'")
	mustContain(t, out, "unbekannter Host", "API über host.docker.internal (Host-Prüfung)")
	// Mit gefälschter Host-Kopfzeile greift die Anmeldung.
	out2, _ := dockerExec(t, ca, "sh", "-c", "curl -s -m 5 -H 'Host: 127.0.0.1:18480' http://host.docker.internal:18480/api/approvals?state=pending")
	mustContain(t, out2, "nicht angemeldet", "API ohne Token (Anmeldung)")
	if strings.Contains(out, "200") || strings.Contains(out, "204") {
		t.Fatalf("API ohne Token erreichbar: %s", out)
	}
	// K2: Doppelschlüssel am Proxy. Seit E9 erreicht nur der Container von pi den Proxy.
	pa, pb := piContainerOf(t, a), piContainerOf(t, b)
	mustContain(t, nodeFetch(t, pa, "POST", "http://orchestrator:18481/llm/deepseek/chat/completions", `{"model":"deepseek-reasoner","Model":"deepseek-flash","messages":[]}`), "mehrdeutig", "Proxy-Doppelschlüssel")
	mustContain(t, nodeFetch(t, pa, "POST", "http://orchestrator:18481/llm/deepseek/files", `{"model":"deepseek-flash"}`), "Pfad nicht freigegeben", "Proxy-Pfad")
	// H5: Sandbox B erreicht einen Dienst in Sandbox A nicht, weder über das
	// Platz-Netz noch über das Egress-Netz.
	_, _ = dockerExec(t, ca, "sh", "-c", "cd /tmp && nohup python3 -m http.server 8765 >/dev/null 2>&1 &")
	time.Sleep(time.Second)
	local, _ := dockerExec(t, ca, "sh", "-c", "curl -s -m 3 -o /dev/null -w '%{http_code}' http://127.0.0.1:8765/")
	mustContain(t, local, "200", "Dienst in Sandbox A läuft")
	ipsOut, _ := exec.Command("docker", "inspect", "-f", "{{range .NetworkSettings.Networks}}{{.IPAddress}} {{end}}", ca).CombinedOutput()
	for _, ip := range strings.Fields(string(ipsOut)) {
		out, _ = dockerExec(t, cb, "sh", "-c", "curl -s -m 3 -o /dev/null -w '%{http_code}' http://"+ip+":8765/ || echo GESPERRT")
		if !strings.Contains(out, "GESPERRT") && !strings.HasPrefix(strings.TrimSpace(out), "000") {
			t.Fatalf("Sandbox B erreicht Sandbox A über %s: %s", ip, out)
		}
	}
	out, _ = dockerExec(t, cb, "sh", "-c", "curl -s -m 3 -o /dev/null -w '%{http_code}' http://"+ca+":8765/ || echo GESPERRT")
	if !strings.Contains(out, "GESPERRT") && !strings.HasPrefix(strings.TrimSpace(out), "000") {
		t.Fatalf("Sandbox B erreicht Sandbox A über den Namen: %s", out)
	}
	// Der Weg zum Modell besteht weiter, aber nur für pi.
	mustContain(t, nodeFetch(t, pb, "POST", "http://orchestrator:18481/llm/deepseek/chat/completions", `{"model":"fremd"}`), "403", "Proxy aus dem Container von pi B erreichbar")
}

func TestRunWithToolsCostAndContext(t *testing.T) {
	requireE2E(t)
	id := newChat(t, "cli", false)
	s := subscribe(t, id)
	ask(t, s, id, "Schreibe mit Python eine Datei quadrate.txt mit den Quadraten von 1 bis 5, eine Zahl je Zeile, führe es aus und zeige den Inhalt mit cat.", nil)
	calls := strings.Join(toolCalls(t, id), "\n")
	mustContain(t, calls, "bash", "Werkzeugaufruf bash")
	mustContain(t, lastAssistantText(t, id), "25", "Ergebnis")
	if s.count(func(ev map[string]any) bool {
		d, _ := ev["data"].(map[string]any)
		e, _ := d["assistantMessageEvent"].(map[string]any)
		return piType(ev) == "message_update" && e["type"] == "text_delta"
	}) < 2 {
		t.Fatal("kein Streaming (weniger als zwei Text-Deltas)")
	}
	f := getChat(t, id)
	if f.Chat.Cost <= 0 || f.Chat.Tokens.Total <= 0 {
		t.Fatalf("Kosten/Tokens: %v %+v", f.Chat.Cost, f.Chat.Tokens)
	}
	billed := 0
	for _, m := range f.Messages {
		if m.Role == "assistant" && m.Cost != nil && m.Peak != nil {
			billed++
		}
	}
	if billed == 0 {
		t.Fatal("keine Antwort nach Tarif abgerechnet")
	}
	var cu struct {
		Tokens          *int64 `json:"tokens"`
		Window          int64  `json:"window"`
		ThresholdTokens int64  `json:"threshold_tokens"`
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && (cu.Window == 0) {
		_ = json.Unmarshal(getChat(t, id).Chat.Context, &cu)
		time.Sleep(300 * time.Millisecond)
	}
	if cu.Window < 100000 || cu.ThresholdTokens <= 0 {
		t.Fatalf("Kontext: %+v", cu)
	}
}

func TestInternetRequestedByAgent(t *testing.T) {
	requireE2E(t)
	id := newChat(t, "cli", false)
	s := subscribe(t, id)
	var asked []approval
	ask(t, s, id, "Rufe mit curl https://example.com ab und nenne den Inhalt des <title>-Elements. Du hast anfangs kein Internet; erbitte es mit agw-internet und einer Begründung.", func(a approval) bool {
		asked = append(asked, a)
		return true
	})
	if len(asked) == 0 || asked[0].Kind != "internet_access" || asked[0].Name == "" {
		t.Fatalf("keine Internet-Anfrage mit Begründung: %+v", asked)
	}
	if !getChat(t, id).Chat.Internet {
		t.Fatal("Internet nach Zustimmung nicht an")
	}
	mustContain(t, lastAssistantText(t, id), "Example Domain", "Titel der Seite")
}

func TestInternetRequestRejected(t *testing.T) {
	requireE2E(t)
	id := newChat(t, "mcp", false)
	s := subscribe(t, id)
	var asked []approval
	ask(t, s, id, "Ich brauche den Titel von https://example.com. Du hast kein Internet; erbitte es mit dem Werkzeug mcp_request_internet. Wenn es abgelehnt wird, sage das und höre auf.", func(a approval) bool {
		asked = append(asked, a)
		return false
	})
	if len(asked) == 0 || asked[0].Kind != "internet_access" || asked[0].Via != "mcp" {
		t.Fatalf("keine MCP-Anfrage: %+v", asked)
	}
	if getChat(t, id).Chat.Internet {
		t.Fatal("Internet trotz Ablehnung an")
	}
}

func TestArtifactRejectThenApprove(t *testing.T) {
	requireE2E(t)
	id := newChat(t, "cli", false)
	s := subscribe(t, id)
	ask(t, s, id, "Erzeuge eine Datei hallo.txt mit dem Inhalt 'Hallo Welt' und lade sie mit agw-artifact als Artefakt hoch.", func(approval) bool { return false })
	f := getChat(t, id)
	if len(f.Artifacts) != 0 {
		t.Fatalf("Artefakt trotz Ablehnung: %+v", f.Artifacts)
	}
	ask(t, s, id, "Bitte lade hallo.txt noch einmal hoch, diesmal bestätige ich.", func(approval) bool { return true })
	f = getChat(t, id)
	var states []string
	for _, a := range f.Approvals {
		states = append(states, a.State)
	}
	if len(f.Artifacts) != 1 || f.Artifacts[0].Name != "hallo.txt" || f.Artifacts[0].Via != "cli" {
		t.Fatalf("Artefakt: %+v (Bestätigungen %v)", f.Artifacts, states)
	}
	if !strings.Contains(strings.Join(states, ","), "rejected") || !strings.Contains(strings.Join(states, ","), "approved") {
		t.Fatalf("Bestätigungen: %v", states)
	}
}

func TestMCPVariant(t *testing.T) {
	requireE2E(t)
	id := newChat(t, "mcp", false)
	s := subscribe(t, id)
	ask(t, s, id, "Rufe das Werkzeug mcp_ping auf, lege dann mit write die Datei notiz.txt mit dem Inhalt 'MCP' an und lade sie mit mcp_upload_artifact hoch.", func(approval) bool { return true })
	calls := strings.Join(toolCalls(t, id), "\n")
	mustContain(t, calls, "mcp_ping", "ping")
	mustContain(t, calls, "mcp_upload_artifact", "Upload")
	if strings.Contains(calls, "bash ") {
		t.Fatal("bash in der MCP-Variante aufgerufen")
	}
	f := getChat(t, id)
	var ops []string
	for _, c := range f.SocketCalls {
		ops = append(ops, c.Via+":"+c.Op+":"+c.Result)
	}
	mustContain(t, strings.Join(ops, " "), "mcp:ping:ok", "Socket-Protokoll")
	mustContain(t, strings.Join(ops, " "), "mcp:upload:approved", "Socket-Protokoll")
}

func TestUserInputAndResume(t *testing.T) {
	requireE2E(t)
	id := newChat(t, "cli", false)
	s := subscribe(t, id)
	uploadFile(t, id, "werte.csv", "a,b\n1,2\n3,4\n5,6\n")
	ask(t, s, id, "Lies /workspace/inputs/werte.csv und nenne die Summe der Spalte b. Merke dir außerdem das Codewort Kiefer-42.", nil)
	mustContain(t, lastAssistantText(t, id), "12", "Summe aus der Eingabe")
	old := getChat(t, id).Chat.SlotID
	if code := call(t, "POST", "/api/chats/"+id+"/suspend", nil, nil); code != 200 {
		t.Fatalf("Ruhen: %d", code)
	}
	if st := getChat(t, id).Chat; st.State != "dormant" || st.SlotID != "" {
		t.Fatalf("nach Ruhen: %+v", st)
	}
	ask(t, s, id, "Wie lautete das Codewort? Und liegt werte.csv noch unter /workspace/inputs? Prüfe es mit ls.", nil)
	c := getChat(t, id).Chat
	if c.State != "active" || c.SlotID == old {
		t.Fatalf("nicht in frischer Sandbox fortgesetzt: %+v (alt %s)", c, old)
	}
	txt := lastAssistantText(t, id)
	mustContain(t, txt, "Kiefer-42", "Gedächtnis nach Fortsetzen")
	mustContain(t, txt, "werte.csv", "Eingabe in neuer Sandbox")
}

func TestManualCompaction(t *testing.T) {
	requireE2E(t)
	id := newChat(t, "cli", false)
	s := subscribe(t, id)
	ask(t, s, id, "Merke dir: Das Projekt heißt Rotkehlchen. Antworte nur mit OK.", nil)
	// Automatik aus, damit die niedrige E2E-Schwelle nicht vorher kompaktiert.
	call(t, "POST", "/api/chats/"+id+"/commands", map[string]string{"command": "/autocompact off"}, nil)
	if getChat(t, id).Chat.AutoCompact {
		t.Fatal("/autocompact off wirkt nicht")
	}
	// Genug Verlauf, damit pi etwas zum Zusammenfassen hat (sonst: "Nothing to compact").
	// Das Lesen mit read bringt den Inhalt sicher in den Kontext.
	uploadFile(t, id, "messungen.txt", bigText(1, 300))
	ask(t, s, id, "Lies /workspace/inputs/messungen.txt vollständig mit dem Werkzeug read (nicht mit bash) und nenne die höchste Temperatur.", nil)
	var cmds []struct{ Name, Source string }
	call(t, "GET", "/api/chats/"+id+"/commands", nil, &cmds)
	names := ""
	for _, c := range cmds {
		names += c.Name + "(" + c.Source + ") "
	}
	mustContain(t, names, "compact(builtin)", "Befehlsliste")
	mustContain(t, names, "skill:", "Befehle von pi")
	from := s.len()
	if code := call(t, "POST", "/api/chats/"+id+"/commands", map[string]string{"command": "/compact Projektnamen unbedingt behalten"}, nil); code != 200 {
		t.Fatalf("/compact: %d", code)
	}
	ev, _ := s.waitFor(t, from, 3*time.Minute, "compaction_end", func(ev map[string]any) bool { return piType(ev) == "compaction_end" })
	d := ev["data"].(map[string]any)
	if d["reason"] != "manual" || d["result"] == nil {
		t.Fatalf("compaction_end: %v", d)
	}
	deadline := time.Now().Add(10 * time.Second)
	for getChat(t, id).Chat.Compactions < 1 && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
	}
	if getChat(t, id).Chat.Compactions != 1 {
		t.Fatal("Kompaktierung nicht gespeichert")
	}
	ask(t, s, id, "Wie heißt das Projekt?", nil)
	mustContain(t, lastAssistantText(t, id), "Rotkehlchen", "Gedächtnis nach Kompaktierung")
	if getChat(t, id).Chat.Compactions != 1 {
		t.Fatal("trotz /autocompact off automatisch kompaktiert")
	}
}

// Die Auto-Kompaktierung braucht eine niedrige Schwelle (./dev.sh e2e setzt
// AGW_COMPACT_RESERVE_TOKENS so, dass sie bei rund 10.000 Tokens greift).
func TestAutoCompaction(t *testing.T) {
	requireE2E(t)
	var cfg struct {
		Reserve int `json:"compact_reserve_tokens"`
	}
	call(t, "GET", "/api/config", nil, &cfg)
	if cfg.Reserve < 900000 {
		t.Skipf("Schwelle zu hoch für einen Test (reserve %d); ./dev.sh e2e verwenden", cfg.Reserve)
	}
	id := newChat(t, "cli", false)
	s := subscribe(t, id)
	ask(t, s, id, "Merke dir das Codewort Wacholder-7. Antworte nur mit OK.", nil)
	// Kontext füllen: große Dateien mit read lesen lassen, bis pi selbst kompaktiert.
	for i := 0; i < 4 && s.count(func(ev map[string]any) bool { return piType(ev) == "compaction_start" }) == 0; i++ {
		name := fmt.Sprintf("teil%d.txt", i)
		uploadFile(t, id, name, bigText(10+i, 300))
		ask(t, s, id, "Lies /workspace/inputs/"+name+" vollständig mit dem Werkzeug read (nicht mit bash) und nenne die niedrigste Feuchte.", nil)
	}
	ev, _ := s.waitFor(t, 0, 2*time.Minute, "compaction_start (threshold)", func(ev map[string]any) bool { return piType(ev) == "compaction_start" })
	d := ev["data"].(map[string]any)
	if d["reason"] != "threshold" && d["reason"] != "overflow" {
		t.Fatalf("Grund: %v", d["reason"])
	}
	s.waitFor(t, 0, 3*time.Minute, "compaction_end", func(ev map[string]any) bool { return piType(ev) == "compaction_end" })
	deadline := time.Now().Add(20 * time.Second)
	for getChat(t, id).Chat.Compactions < 1 && time.Now().Before(deadline) {
		time.Sleep(300 * time.Millisecond)
	}
	f := getChat(t, id)
	if f.Chat.Compactions < 1 {
		t.Fatal("automatische Kompaktierung nicht gespeichert")
	}
	var comp map[string]any
	for _, m := range f.Messages {
		if m.Role == "compaction" {
			_ = json.Unmarshal(m.Message, &comp)
		}
	}
	if comp["reason"] != "threshold" && comp["reason"] != "overflow" {
		t.Fatalf("Eintrag: %v", comp)
	}
	if tb, _ := comp["tokensBefore"].(float64); tb < 5000 {
		t.Fatalf("tokensBefore unplausibel: %v", comp["tokensBefore"])
	}
	ask(t, s, id, "Wie lautete das Codewort?", nil)
	mustContain(t, lastAssistantText(t, id), "Wacholder-7", "Gedächtnis nach Auto-Kompaktierung")
}

// Subagenten: Werkzeugaufrufe sichtbar, Kosten am Proxy erfasst.
func TestSubagentsVisibleAndBilled(t *testing.T) {
	requireE2E(t)
	id := newChatWith(t, map[string]any{"variant": "cli", "internet": false, "max_subagents": 2})
	s := subscribe(t, id)
	ask(t, s, id, "Nutze das subagent-Werkzeug mit dem Agenten scout im Vordergrund (async: false), der mit bash 'python3 --version' ausführt. Nenne danach nur die Version.", nil)
	var f fullChat
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		f = getChat(t, id)
		if len(f.Subagent) > 0 && f.Chat.Subagents >= 1 {
			break
		}
		time.Sleep(time.Second)
	}
	var sawBash, confirmed bool
	for _, e := range f.Subagent {
		if e.Kind == "tool_call" && e.Payload.Name == "bash" && strings.Contains(e.Payload.Arguments, "python3") {
			sawBash = true
		}
		confirmed = confirmed || e.Confirmed
	}
	if !sawBash {
		t.Fatalf("Werkzeugaufruf des Subagenten nicht sichtbar: %+v", f.Subagent)
	}
	if !confirmed {
		t.Fatal("kein Subagenten-Eintrag am Proxy belegt")
	}
	mains := 0
	for _, m := range f.Messages {
		if m.Role == "assistant" {
			mains++
		}
	}
	if f.Chat.LLMCalls <= mains || f.Chat.CostOther <= 0 {
		t.Fatalf("Subagenten-Aufrufe nicht abgerechnet: %d Aufrufe, %d Hauptantworten, cost_other %v", f.Chat.LLMCalls, mains, f.Chat.CostOther)
	}
	var calls []struct {
		Main bool    `json:"main"`
		Cost float64 `json:"cost"`
	}
	call(t, "GET", "/api/chats/"+id+"/llm_calls", nil, &calls)
	sum := 0.0
	for _, c := range calls {
		sum += c.Cost
	}
	if d := sum - f.Chat.Cost; d > 1e-9 || d < -1e-9 {
		t.Fatalf("Summe der Aufrufe %v ≠ Chatkosten %v", sum, f.Chat.Cost)
	}
}

// Grenze 0: Ein gestarteter Subagent führt zum Abbruch durch den Orchestrator.
func TestSubagentLimitEnforced(t *testing.T) {
	requireE2E(t)
	id := newChatWith(t, map[string]any{"variant": "cli", "internet": false, "max_subagents": 0})
	s := subscribe(t, id)
	ask(t, s, id, "Nutze das subagent-Werkzeug mit dem Agenten scout im Hintergrund, der mit bash 'sleep 20; uname -m' ausführt, und warte mit bg_wait auf das Ergebnis.", nil)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		f := getChat(t, id)
		for _, c := range f.SocketCalls {
			if c.Op == "subagent_limit" {
				return
			}
		}
		// pi-subagents kann den Start auch selbst verweigern (kooperative Ebene): dann gibt es keinen Lauf.
		if f.Chat.Subagents == 0 && !f.Chat.Running {
			txt := lastAssistantText(t, id)
			if txt != "" {
				t.Logf("kein Subagent gestartet (kooperative Ebene griff): %s", trunc(txt, 200))
				return
			}
		}
		time.Sleep(time.Second)
	}
	t.Fatal("Grenze 0 nicht durchgesetzt")
}

// Harte Grenze am Proxy, auch für Aufrufe an pi vorbei. Seit E9 erreicht nur der Container von pi
// den Proxy (die Ausführungs-Sandbox hat kein Netz zum Orchestrator); der Test ruft ihn dort mit
// Node direkt auf.
func TestProxyConcurrencyLimitHard(t *testing.T) {
	requireE2E(t)
	id := newChatWith(t, map[string]any{"variant": "cli", "internet": false, "max_subagents": 0})
	c := piContainerOf(t, id)
	script := `const body=JSON.stringify({model:"deepseek-flash",stream:true,messages:[{role:"user",content:"Zähle langsam von 1 bis 40, jede Zahl in eine eigene Zeile."}]});
Promise.all([1,2,3].map(()=>fetch("http://orchestrator:18481/llm/deepseek/chat/completions",{method:"POST",headers:{"content-type":"application/json"},body}).then(async r=>{console.log(r.status);await r.text()}).catch(e=>console.log("FEHLER",e.message))))`
	out, _ := dockerExec(t, c, "node", "-e", script)
	if !strings.Contains(out, "429") {
		t.Fatalf("keine 429 bei drei gleichzeitigen Aufrufen und Grenze 1: %q", out)
	}
	if !strings.Contains(out, "200") {
		t.Fatalf("kein Aufruf durchgelassen: %q", out)
	}
	f := getChat(t, id)
	found := false
	for _, sc := range f.SocketCalls {
		found = found || sc.Op == "agent_limit"
	}
	if !found {
		t.Fatal("Grenzverletzung nicht protokolliert")
	}
	if f.Chat.LLMCalls < 1 {
		t.Fatal("direkter Aufruf aus der Sandbox nicht abgerechnet")
	}
}

// Anhänge: hochgeladene Datei hängt an der Nachricht, der Agent kennt Namen und Ort.
func TestMessageAttachments(t *testing.T) {
	requireE2E(t)
	id := newChat(t, "cli", false)
	s := subscribe(t, id)
	uploadFile(t, id, "zahlen.csv", "wert\n3\n4\n5\n")
	if code := call(t, "POST", "/api/chats/"+id+"/messages", map[string]any{"text": "x", "attachments": []string{"gibtsnicht.csv"}}, nil); code != 400 {
		t.Fatalf("unbekannter Anhang: %d", code)
	}
	from := s.len()
	if code := call(t, "POST", "/api/chats/"+id+"/messages", map[string]any{"text": "Wie groß ist die Summe der Spalte wert im Anhang? Nur die Zahl.", "attachments": []string{"zahlen.csv"}}, nil); code != 200 {
		t.Fatalf("senden: %d", code)
	}
	s.waitFor(t, from, 3*time.Minute, "agent_settled", func(ev map[string]any) bool { return piType(ev) == "agent_settled" })
	f := getChat(t, id)
	var u struct {
		Content []struct{ Text string } `json:"content"`
	}
	_ = json.Unmarshal(f.Messages[0].Message, &u)
	mustContain(t, u.Content[0].Text, "[Anhänge unter /workspace/inputs/]\n- zahlen.csv", "Anhang-Block in der Nachricht")
	mustContain(t, lastAssistantText(t, id), "12", "Summe aus dem Anhang")
}

// Typst: der Agent nutzt den Skill und die eingebauten Pakete ohne Internet.
func TestTypstDocument(t *testing.T) {
	requireE2E(t)
	id := newChat(t, "cli", false)
	s := subscribe(t, id)
	ask(t, s, id, "Erstelle mit Typst in /workspace/bericht.typ eine Seite mit Überschrift „Testbericht“ und einer kleinen cetz-Grafik (Kreis), übersetze sie mit typst compile zu bericht.pdf und nenne nur die Größe des PDF in Bytes.", nil)
	calls := strings.Join(toolCalls(t, id), "\n")
	mustContain(t, calls, "typst compile", "typst aufgerufen")
	mustContain(t, calls, "@preview/cetz", "eingebautes Paket genutzt")
	c := containerOf(t, id)
	out, err := dockerExec(t, c, "sh", "-c", "test -s /workspace/bericht.pdf && head -c 5 /workspace/bericht.pdf")
	if err != nil || !strings.HasPrefix(out, "%PDF") {
		t.Fatalf("kein PDF entstanden: %q %v", out, err)
	}
}

// Anzeige-Bilder: Der Agent zeichnet mit matplotlib ein PNG und zeigt es per Markdown. Der Orchestrator
// sichert es nach der Antwort in S3; es bleibt abrufbar, wenn der Chat ruht (Sandbox weg).
func TestAgentImageShownAndKept(t *testing.T) {
	requireE2E(t)
	id := newChat(t, "cli", false)
	s := subscribe(t, id)
	ask(t, s, id, "Zeichne mit matplotlib ein Liniendiagramm von y = x² für x von 0 bis 5, speichere es als "+
		"/workspace/quadrat.png und zeige es in deiner Antwort als Markdown-Bild. Sonst nur ein kurzer Satz.", nil)
	f := getChat(t, id)
	ref := regexp.MustCompile(`!\[[^\]]*\]\(\s*<?([^)\s>]+)`)
	var msg, path string
	for i := len(f.Messages) - 1; i >= 0 && path == ""; i-- {
		var m struct {
			ResponseID string `json:"responseId"`
			Timestamp  int64  `json:"timestamp"`
			Content    []struct{ Type, Text string }
		}
		_ = json.Unmarshal(f.Messages[i].Message, &m)
		for _, c := range m.Content {
			if sm := ref.FindStringSubmatch(c.Text); c.Type == "text" && sm != nil {
				path, msg = sm[1], m.ResponseID
				if msg == "" {
					msg = fmt.Sprintf("ts-%d", m.Timestamp)
				}
			}
		}
	}
	if path == "" {
		t.Fatalf("kein Markdown-Bild in der Antwort: %q", trunc(lastAssistantText(t, id), 400))
	}
	mustContain(t, strings.Join(toolCalls(t, id), "\n"), "savefig", "matplotlib savefig")
	if code := call(t, "POST", "/api/chats/"+id+"/suspend", nil, nil); code != 200 {
		t.Fatalf("ruhen lassen: %d", code)
	}
	get := func(p string) (int, http.Header, []byte) {
		req, _ := http.NewRequest("GET", base+"/api/chats/"+id+"/images?path="+url.QueryEscape(p)+"&msg="+url.QueryEscape(msg), nil)
		auth(req)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header, b
	}
	code, h, body := get(path)
	if code != 200 || !bytes.HasPrefix(body, []byte("\x89PNG\r\n\x1a\n")) {
		t.Fatalf("Bild nach dem Ruhen: %d %q (Pfad %s, Antwort %s)", code, trunc(string(body), 200), path, msg)
	}
	if h.Get("Content-Type") != "image/png" || h.Get("X-Content-Type-Options") != "nosniff" || !strings.HasPrefix(h.Get("Cache-Control"), "private") {
		t.Fatalf("Kopfzeilen: %v", h)
	}
	if code, _, _ := get("/etc/passwd"); code != 400 {
		t.Fatalf("/etc/passwd: %d", code)
	}
	if code, _, _ := get("/workspace/gibtsnicht.png"); code != 404 {
		t.Fatalf("nicht gesichertes Bild bei ruhendem Chat: %d", code)
	}
	if n := len(getChat(t, id).Artifacts); n != 0 {
		t.Fatalf("Anzeige-Bild als Artefakt geführt: %d", n)
	}
}

// Dateien in /workspace überstehen das Ruhen: gesichert nach dem Lauf, in der
// frischen Sandbox wiederhergestellt; /tmp nicht.
func TestWorkspaceSurvivesSuspend(t *testing.T) {
	requireE2E(t)
	id := newChat(t, "cli", false)
	s := subscribe(t, id)
	ask(t, s, id, "Erledige mit bash, ohne Rückfrage: 1. Zeichne mit matplotlib ein Liniendiagramm von y = 2x für x von 0 bis 4 "+
		"und speichere es als /workspace/plot.png. 2. Schreibe das Wort Tanne-77 in die Datei /workspace/notiz.txt. "+
		"3. Schreibe das Wort weg in /tmp/fluechtig.txt. Antworte danach nur mit einem kurzen Satz.", nil)
	old := containerOf(t, id)
	if out, err := dockerExec(t, old, "sh", "-c", "head -c 4 /workspace/plot.png | tail -c 3; cat /workspace/notiz.txt /tmp/fluechtig.txt"); err != nil ||
		!strings.Contains(out, "PNG") || !strings.Contains(out, "Tanne-77") {
		t.Fatalf("Dateien nicht angelegt: %q %v", out, err)
	}
	if code := call(t, "POST", "/api/chats/"+id+"/suspend", nil, nil); code != 200 {
		t.Fatalf("Ruhen: %d", code)
	}
	w := getChat(t, id).Chat.Workspace
	if w == nil || w.SavedAt == "" || w.Files < 2 || w.SkippedReason != "" {
		t.Fatalf("Arbeitsbereich nicht gesichert: %+v", w)
	}
	ask(t, s, id, "Führe ls -la /workspace aus, gib den Inhalt von /workspace/notiz.txt wörtlich wieder und prüfe mit ls, "+
		"ob /tmp/fluechtig.txt noch existiert. Antworte kurz.", nil)
	fresh := containerOf(t, id)
	if fresh == old {
		t.Fatal("nicht in frischer Sandbox fortgesetzt")
	}
	txt := lastAssistantText(t, id)
	mustContain(t, txt, "Tanne-77", "Inhalt von notiz.txt nach dem Fortsetzen")
	mustContain(t, txt, "plot.png", "plot.png nach dem Fortsetzen")
	out, _ := dockerExec(t, fresh, "sh", "-c", "head -c 4 /workspace/plot.png | tail -c 3; echo; cat /workspace/notiz.txt; test -e /tmp/fluechtig.txt && echo TMP-DA || echo TMP-WEG")
	for _, want := range []string{"PNG", "Tanne-77", "TMP-WEG"} {
		mustContain(t, out, want, "Zustand der frischen Sandbox")
	}
}

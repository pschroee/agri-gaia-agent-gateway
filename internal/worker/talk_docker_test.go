package worker

// Platztest der Kommunikation zwischen Haupt- und Subagenten (pi-subagents): Subagent fragt den
// Hauptagenten (contact_supervisor), der Hauptagent antwortet (subagent_supervisor) und lenkt einen
// laufenden Subagenten (subagent steer). Echter Container von pi, geskriptetes Modell.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"agw/internal/config"
	"agw/internal/sandbox"
	"agw/internal/store"
)

// talkSlot legt einen Platz der Variante cli an (Backend ohne Websuche); done baut ihn ab.
func talkSlot(t *testing.T, ctx context.Context) (*Worker, func()) {
	t.Helper()
	rt, err := sandbox.New(envOr("AGW_EGRESS_NETWORK", "agwpoc_egress"))
	if err != nil {
		t.Fatal(err)
	}
	self, _ := os.Hostname()
	rt.SetSelf(self)
	cat, err := config.LoadCatalog(envOr("AGW_CATALOG", "../../models.json"))
	if err != nil {
		t.Fatal(err)
	}
	env := config.Env{
		Image: envOr("AGW_IMAGE", "agwpoc/agw-basis:dev"), PiImage: envOr("AGW_PI_IMAGE", "agwpoc/agw-pi:dev"),
		SocketVolume: envOr("AGW_SOCKET_VOLUME", "agwpoc_sockets"), SocketRoot: envOr("AGW_SOCKET_ROOT", "/run/agw"),
		ProxyBaseURL: "http://orchestrator:18481", ArtifactMaxBytes: 1 << 20, CompactReserveTokens: 16384, CompactKeepRecent: 20000,
		SandboxMemoryMB: 1536, SandboxCPUs: 1, SandboxPids: 256, ExecMemoryMB: 1024, ExecCPUs: 1, ExecPids: 256, BgMax: 3,
	}
	fac, err := NewFactory(rt, cat, env)
	if err != nil {
		t.Fatal(err)
	}
	b := &bgBackend{ended: map[string]store.BackgroundTask{}, notify: map[string]bool{}}
	fac.Backend = b
	slot := fmt.Sprintf("t-talk-%d", time.Now().UnixNano()%1e8)
	a, err := fac.Create(ctx, slot, "cli")
	if err != nil {
		t.Fatal(err)
	}
	return a.(*Worker), func() { fac.Destroy(context.Background(), a) }
}

func TestSlotSubagentTalk(t *testing.T) {
	if os.Getenv("AGW_E9_IN_DOCKER") != "1" {
		t.Skip("läuft nur im Go-Container mit Docker-Socket (./dev.sh test)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	fake := useFakeLLM()
	w, done := talkSlot(t, ctx)
	defer done()

	var mu sync.Mutex
	var log []string
	settled := make(chan struct{}, 16)
	go func() {
		for ev := range w.Events() {
			switch ev.Type {
			case "message_end", "tool_execution_end":
				mu.Lock()
				log = append(log, string(ev.Raw))
				mu.Unlock()
			case "agent_settled":
				select {
				case settled <- struct{}{}:
				default:
				}
			}
		}
	}()
	// Subagent (im Hintergrund) fragt, der Hauptagent antwortet und lenkt ihn danach, während er noch
	// arbeitet; der Subagent sieht beides.
	script := strings.Join([]string{
		"Kommunikation.",
		callLine("subagent", map[string]any{"agent": "worker", "async": true,
			"task": "T1\n" + callLine("contact_supervisor", map[string]any{"reason": "need_decision", "message": "Frage an den Hauptagenten"}) + "\n" +
				callLine("bash", map[string]any{"command": "sleep 6; echo nach-antwort sitzung=$PI_AGW_SESSION"})}),
		callLine("bash", map[string]any{"command": "sleep 8"}),
		callLine("subagent_supervisor", map[string]any{"action": "reply", "replyTo": "{{replyTo}}", "message": "Antwort vom Hauptagenten"}),
		callLine("bash", map[string]any{"command": "sleep 2; echo haupt-sitzung=$PI_AGW_SESSION"}),
		callLine("subagent", map[string]any{"action": "steer", "id": "{{runId}}", "message": "Hinweis vom Hauptagenten"}),
		callLine("bash", map[string]any{"command": "sleep 12"}),
	}, "\n")
	if _, err := w.Call(ctx, map[string]any{"type": "prompt", "message": script}); err != nil {
		t.Fatal(err)
	}
	seen := func(want string) bool {
		for _, s := range fake.Seen() {
			// Skripte (CALL-Zeilen) enthalten die gesuchten Texte selbst; gezählt wird nur, was das Modell
			// als Antwort, Werkzeugergebnis oder eingeschleuste Nachricht sah.
			if strings.Contains(s, want) && !strings.Contains(s, "CALL ") {
				return true
			}
		}
		return false
	}
	for deadline := time.Now().Add(150 * time.Second); time.Now().Before(deadline); time.Sleep(time.Second) {
		if seen("Hinweis vom Hauptagenten") && seen("nach-antwort") {
			break
		}
	}
	mu.Lock()
	all := strings.Join(log, "\n")
	mu.Unlock()
	if !strings.Contains(all, `"customType":"subagent_supervisor_request"`) || !strings.Contains(all, "Frage an den Hauptagenten") {
		t.Errorf("Frage des Subagenten kam nicht beim Hauptagenten an")
	}
	if !seen("Antwort vom Hauptagenten") {
		t.Errorf("Antwort des Hauptagenten kam nicht beim Subagenten an")
	}
	if !seen("Hinweis vom Hauptagenten") {
		t.Errorf("Lenkung des laufenden Subagenten kam nicht an")
	}
	// Sitzung für agw-artifact/agw-internet: Hauptagent „main“, Subagent seine Laufkennung.
	if !seen("haupt-sitzung=main") {
		t.Errorf("PI_AGW_SESSION beim Hauptagenten nicht „main“")
	}
	subSess := regexp.MustCompile(`sitzung=[0-9a-f-]{8,}`)
	found := false
	for _, s := range fake.Seen() {
		if !strings.Contains(s, "CALL ") && strings.Contains(s, "nach-antwort") && subSess.MatchString(s) {
			found = true
		}
	}
	if !found {
		t.Errorf("PI_AGW_SESSION beim Subagenten nicht die Laufkennung")
	}
	for _, is := range fake.Issued() {
		if strings.Contains(is.Args, "{{") || (is.Tool == "subagent_supervisor" && !strings.Contains(is.Args, "-")) {
			t.Errorf("Platzhalter nicht ersetzt: %s %s", is.Tool, is.Args)
		}
	}
	if t.Failed() {
		for _, l := range log {
			t.Logf("%.600s", l)
		}
	}
	_ = settled
}

// Subagenten sprechen direkt miteinander (pi-intercom): A findet B in der Liste und schickt ihm eine
// Nachricht; B bekommt sie während seiner Arbeit zu sehen, ohne Umweg über den Hauptagenten.
func TestSlotSubagentIntercom(t *testing.T) {
	if os.Getenv("AGW_E9_IN_DOCKER") != "1" {
		t.Skip("läuft nur im Go-Container mit Docker-Socket (./dev.sh test)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	fake := useFakeLLM()
	w, done := talkSlot(t, ctx)
	defer done()
	script := strings.Join([]string{
		"Intercom.",
		callLine("subagent", map[string]any{"agent": "worker", "async": true, "task": "IA\n" +
			callLine("bash", map[string]any{"command": "sleep 3"}) + "\n" +
			callLine("intercom", map[string]any{"action": "list"}) + "\n" +
			callLine("intercom", map[string]any{"action": "send", "to": "{{peer}}", "message": "Hallo B, hier ist A"})}),
		callLine("subagent", map[string]any{"agent": "worker", "async": true, "task": "IB\n" +
			callLine("bash", map[string]any{"command": "sleep 12"}) + "\n" +
			callLine("bash", map[string]any{"command": "echo b-weiter"})}),
		callLine("bash", map[string]any{"command": "sleep 25"}),
	}, "\n")
	if _, err := w.Call(ctx, map[string]any{"type": "prompt", "message": script}); err != nil {
		t.Fatal(err)
	}
	// B sieht die Nachricht als eingehende Nachricht von A (Kopf „From …“), nicht nur A ihr eigenes Senden.
	got := func() bool {
		for _, s := range fake.Seen() {
			// Kopf „**From <Absender>**“ von pi-intercom (der Sitzungsname von A enthält dessen Aufgabe).
			if strings.HasPrefix(s, "**From ") && strings.Contains(s, "Hallo B, hier ist A") {
				return true
			}
		}
		return false
	}
	for deadline := time.Now().Add(90 * time.Second); time.Now().Before(deadline) && !got(); time.Sleep(time.Second) {
	}
	if !got() {
		out, err := w.ExecPi(ctx, []string{"agw-exec", "poll-subagents"}, nil)
		var poll struct {
			Files []struct {
				Path string `json:"path"`
				Data string `json:"data"`
			} `json:"files"`
		}
		_ = json.Unmarshal(out, &poll)
		for _, f := range poll.Files {
			if !strings.Contains(f.Data, "Task: IB") {
				continue
			}
			for _, line := range strings.Split(f.Data, "\n") {
				if !strings.Contains(line, `"role":"system"`) && line != "" {
					t.Logf("B %.500s", line)
				}
			}
		}
		t.Logf("poll err=%v bytes=%d", err, len(out))
		t.Fatal("Nachricht von A kam nicht bei B an")
	}
	for _, is := range fake.Issued() {
		if is.Tool == "intercom" && strings.Contains(is.Args, "send") && !strings.Contains(is.Args, "subagent-worker-") {
			t.Errorf("Ziel nicht aufgelöst: %s", is.Args)
		}
	}
}

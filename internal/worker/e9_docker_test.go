package worker

// Integrationstest eines ganzen Platzes (E9) mit geskriptetem Modell: echter Container von pi
// ohne Shell, echte Ausführungs-Sandbox, echte Extension exec-bridge.ts, echte Sockets. Das Modell
// ist internal/fakellm; es läuft in diesem Testprozess, der sich als „orchestrator“ an das
// Platz-Netz hängt. Unix-Sockets gehen auf dem Mac nur innerhalb der Docker-VM, deshalb läuft
// der Test in einem Go-Container (./dev.sh test, Schalter AGW_E9_IN_DOCKER=1).

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"agw/internal/config"
	"agw/internal/fakellm"
	"agw/internal/platform"
	"agw/internal/sandbox"
	"agw/internal/sock"
	"agw/internal/store"
)

type e9Backend struct {
	mu   sync.Mutex
	recs []store.ToolExecution
	logs []string
	plat []string // Aufrufe der Plattform-Anbindung (agw-platform im Abbild)
}

func (b *e9Backend) PlatformCall(_ context.Context, chat, slot, via string, req platform.Request) (platform.Result, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.plat = append(b.plat, via+" "+req.String())
	return platform.Result{Status: "ok", HTTPStatus: 200, Body: `{"id":7,"name":"e9"}`}, nil
}

func (b *e9Backend) ChatForSlot(string) string { return "chat-e9" }
func (b *e9Backend) Upload(context.Context, string, string, string, string, int64, string, io.Reader) (sock.UploadResult, error) {
	return sock.UploadResult{Status: "rejected"}, nil
}
func (b *e9Backend) ListArtifacts(context.Context, string) ([]store.Artifact, error) { return nil, nil }
func (b *e9Backend) OpenArtifact(context.Context, string, string, string) (io.ReadCloser, int64, error) {
	return nil, 0, store.ErrNotFound
}
func (b *e9Backend) RequestInternet(context.Context, string, string, string, string) (sock.UploadResult, error) {
	return sock.UploadResult{Status: "rejected"}, nil
}
func (b *e9Backend) LogCall(slot, chat, via, op, detail, result string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.logs = append(b.logs, via+":"+op+":"+result)
}
func (b *e9Backend) RecordToolExecution(e store.ToolExecution) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.recs = append(b.recs, e)
}
func (b *e9Backend) executions() []store.ToolExecution {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]store.ToolExecution(nil), b.recs...)
}

// Hauptagent: alle vier Datei- und Shellwerkzeuge, dann Subagenten im Vorder- und im Hintergrund
// und drei Aufrufe, die der Wächter sperren muss.
const e9Script = `Prüfe die Ausführungs-Sandbox.
CALL bash {"command":"echo main-$(id -u); ls /agent 2>&1; tr '\\0' ' ' < /proc/1/cmdline; echo; test -e /agent/config/settings.json && echo SETTINGS-DA || echo SETTINGS-FEHLT"}
CALL write {"path":"/workspace/e9.txt","content":"hallo e9\n"}
CALL edit {"path":"/workspace/e9.txt","edits":[{"oldText":"hallo","newText":"moin"}]}
CALL read {"path":"/workspace/e9.txt"}
CALL bash {"command":"agw-platform dataset 7; echo plattform-rc=$?"}
CALL bash {"command":"seq 1 20000"}
CALL read {"path":"{{fullOutputPath}}","offset":19999}
CALL bash {"command":"yes | head -c 300000000; echo ENDE"}
CALL read {"path":"{{fullOutputPath}}","offset":1000,"limit":2}
CALL subagent {"agent":"worker","async":false,"task":"K1\nCALL bash {\"command\":\"echo kind-$(id -u) > /workspace/kind.txt; ls /agent 2>&1\"}\nCALL grep {\"pattern\":\"moin\",\"path\":\"/workspace\"}\nCALL find {\"pattern\":\"*.txt\",\"path\":\"/workspace\"}\nCALL ls {\"path\":\"/workspace\"}"}
CALL subagent {"action":"create","config":{"name":"boese","description":"x","systemPrompt":"x","extensions":"","subagentOnlyExtensions":""}}
CALL subagent {"agent":"claude-code","task":"x"}
CALL subagent {"agent":"scout","task":"K2\nCALL bash {\"command\":\"echo hintergrund-$(id -u)\"}"}`

// Workflows (Entscheidung des Verfassers: workflowScript zurück, Skript in der
// Ausführungs-Sandbox). Die Skripte stehen hier als Go-Strings, damit JSON das Maskieren übernimmt.
const (
	// P4b: Ausbruch aus node:vm. Vorher Code des Agenten als PID 1 im Container von pi, jetzt
	// höchstens in der Ausführungs-Sandbox.
	wfEscape = `const p = emit.constructor('return process')();
const fs = p.getBuiltinModule('fs');
let wrote = 'nein';
try { fs.writeFileSync('/agent/config/settings.json', '{}'); wrote = 'ja'; } catch (e) { wrote = e.code; }
return { uid: p.getuid(), agent: fs.existsSync('/agent'), workspace: fs.existsSync('/workspace/e9.txt'), pid1: fs.readFileSync('/proc/1/cmdline', 'utf8').split('\0').join(' '), wrote };`
	// Drei parallele Läufe; jeder ruft bash (belegt in tool_executions).
	wfParallel = `const r = await runs.all([
  { key: 'w1', agent: 'worker', task: 'WF1\nCALL bash {"command":"echo wf-1; sleep 2"}' },
  { key: 'w2', agent: 'worker', task: 'WF2\nCALL bash {"command":"echo wf-2; sleep 2"}' },
  { key: 'w3', agent: 'scout', task: 'WF3\nCALL bash {"command":"echo wf-3; sleep 2"}' },
]);
return r.map((x) => x.ok);`
	// Kette: der zweite Lauf erst nach dem ersten.
	wfChain = `const a = await runs.run('k1', { agent: 'worker', task: 'KE1\nCALL bash {"command":"echo kette-1 > /workspace/kette.txt"}' });
const b = await runs.run('k2', { agent: 'worker', task: 'KE2\nCALL bash {"command":"cat /workspace/kette.txt; echo kette-2"}' });
return [a.ok, b.ok];`
	// Gesperrt: ein Lauf mit cwd, ein Agent mit fremder Laufzeit, runs.host und eine gefälschte
	// Anfrage über stdout des Runners (sie kommt beim Host an, der Wächter weist sie ab).
	wfBlocked = `const out = [];
try { await runs.run('b1', { agent: 'worker', task: 'B1\nCALL bash {"command":"echo gesperrt-cwd"}', cwd: '/agent' }); out.push('cwd lief'); } catch (e) { out.push('cwd: ' + e.message); }
try { await runs.run('b2', { agent: 'claude-code', task: 'x' }); out.push('claude-code lief'); } catch (e) { out.push('claude-code: ' + e.message); }
const p = emit.constructor('return process')();
p.stdout.write(JSON.stringify({ m: { type: 'call', callId: 4242, method: 'run', args: { key: 'f1', params: { agent: 'worker', task: 'F1\nCALL bash {"command":"echo gefaelscht"}', cwd: '/agent' } } } }) + '\n');
const t0 = Date.now(); while (Date.now() - t0 < 500) {}
return out;`
)

func callLine(tool string, args any) string {
	b, _ := json.Marshal(args)
	return "CALL " + tool + " " + string(b)
}

func TestSlotE9WithScriptedModel(t *testing.T) {
	if os.Getenv("AGW_E9_IN_DOCKER") != "1" {
		t.Skip("läuft nur im Go-Container mit Docker-Socket (./dev.sh test)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	fake := useFakeLLM()

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
		SandboxMemoryMB: 1536, SandboxCPUs: 1, SandboxPids: 256, ExecMemoryMB: 1024, ExecCPUs: 1, ExecPids: 256,
	}
	fac, err := NewFactory(rt, cat, env)
	if err != nil {
		t.Fatal(err)
	}
	b := &e9Backend{}
	fac.Backend = b
	slot := fmt.Sprintf("t-e9-%d", time.Now().UnixNano()%1e8)
	a, err := fac.Create(ctx, slot, "cli")
	if err != nil {
		t.Fatal(err)
	}
	defer fac.Destroy(context.Background(), a)
	w := a.(*Worker)

	type end struct {
		isError bool
		text    string
	}
	var mu sync.Mutex
	ends := map[string]end{}
	settled := make(chan struct{}, 4)
	go func() {
		for ev := range w.Events() {
			switch ev.Type {
			case "tool_execution_end":
				var e struct {
					ToolCallID string          `json:"toolCallId"`
					IsError    bool            `json:"isError"`
					Result     json.RawMessage `json:"result"`
				}
				_ = json.Unmarshal(ev.Raw, &e)
				mu.Lock()
				ends[e.ToolCallID] = end{e.IsError, string(e.Result)}
				mu.Unlock()
			case "agent_settled":
				select {
				case settled <- struct{}{}:
				default:
				}
			case "extension_ui_request":
				var r struct {
					ID     string `json:"id"`
					Method string `json:"method"`
				}
				_ = json.Unmarshal(ev.Raw, &r)
				if r.Method == "confirm" || r.Method == "select" || r.Method == "input" {
					_ = w.Notify(map[string]any{"type": "extension_ui_response", "id": r.ID, "cancelled": true})
				}
			}
		}
	}()
	script := e9Script + "\n" + strings.Join([]string{
		callLine("subagent", map[string]any{"workflowScript": wfEscape, "async": false}),
		callLine("subagent", map[string]any{"workflowScript": wfParallel, "async": false}),
		callLine("subagent", map[string]any{"workflowScript": wfChain, "async": false}),
		callLine("subagent", map[string]any{"workflowScript": wfBlocked, "async": false}),
		callLine("subagent", map[string]any{"workflowScriptPath": "/agent/x.js", "async": false}),
		callLine("subagent", map[string]any{"agent": "reviewer", "async": false, "task": "R1\nCALL watchdog_diff {}"}),
		// zwei einzelne Subagenten im Hintergrund: laufen sie gleichzeitig?
		callLine("subagent", map[string]any{"agent": "scout", "async": true, "task": "AS1\nCALL bash {\"command\":\"echo async-1; sleep 3\"}"}),
		callLine("subagent", map[string]any{"agent": "scout", "async": true, "task": "AS2\nCALL bash {\"command\":\"echo async-2; sleep 3\"}"}),
	}, "\n")
	if _, err := w.Call(ctx, map[string]any{"type": "prompt", "message": script}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-settled:
	case <-ctx.Done():
		t.Fatal("Lauf wird nicht fertig")
	}
	// Der Hintergrund-Subagent (scout) meldet sich über seinen Runner-Prozess.
	byTool := func() map[string]fakellm.Issued {
		m := map[string]fakellm.Issued{}
		for _, is := range fake.Issued() {
			key := is.Tool
			if strings.Contains(is.Args, "hintergrund") {
				key = "bash-hintergrund"
			} else if strings.Contains(is.Args, "seq 1 20000") {
				key = "bash-lang"
			} else if strings.Contains(is.Args, "head -c 300000000") {
				key = "bash-riesig"
			} else if strings.Contains(is.Args, `"offset":19999`) {
				key = "read-lang"
			} else if strings.Contains(is.Args, `"offset":1000`) {
				key = "read-riesig"
			} else if strings.Contains(is.Args, "plattform-rc") {
				key = "bash-plattform"
			} else if strings.Contains(is.Args, "kind-") {
				key = "bash-kind"
			} else if is.Tool == "subagent" {
				key = "subagent:" + is.Args
			}
			for _, k := range []string{"wf-1", "wf-2", "wf-3", "kette-1", "kette-2", "async-1", "async-2", "gesperrt-cwd", "gefaelscht", "watchdog_diff"} {
				if strings.Contains(is.Args, k) || (k == "watchdog_diff" && is.Tool == k) {
					key = "k:" + k
				}
			}
			if is.Tool == "subagent" {
				switch {
				case strings.Contains(is.Args, "getBuiltinModule"):
					key = "wf:escape"
				case strings.Contains(is.Args, "runs.all"):
					key = "wf:parallel"
				case strings.Contains(is.Args, "'k1'"):
					key = "wf:chain"
				case strings.Contains(is.Args, "'b1'"):
					key = "wf:blocked"
				case strings.Contains(is.Args, "workflowScriptPath"):
					key = "wf:path"
				}
			}
			m[key] = is
		}
		return m
	}
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		m := byTool()
		ready := true
		for _, k := range []string{"bash-hintergrund", "k:async-1", "k:async-2"} {
			if is, ok := m[k]; !ok || execFor(b.executions(), is.ID) == nil {
				ready = false
			}
		}
		if ready {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	issued := byTool()
	execs := b.executions()

	// P6: Jede Ausführung trägt eine vom Modell angeforderte ID, und jeder angeforderte Aufruf
	// eines umgeleiteten Werkzeugs ist ausgeführt (Haupt- und Subagent).
	ids := map[string]bool{}
	for _, is := range fake.Issued() {
		ids[is.ID] = true
	}
	for _, e := range execs {
		if !ids[e.ToolCallID] {
			t.Errorf("Ausführung ohne Anforderung: %+v", e)
		}
	}
	for _, key := range []string{"bash", "write", "edit", "read", "bash-kind", "grep", "find", "ls", "bash-hintergrund"} {
		is, ok := issued[key]
		if !ok {
			t.Errorf("Modell hat %s nie angefordert (Skript nicht abgearbeitet?)", key)
			continue
		}
		e := execFor(execs, is.ID)
		if e == nil {
			t.Errorf("%s (%s) nicht ausgeführt", key, is.ID)
			continue
		}
		wantMain := key == "bash" || key == "write" || key == "edit" || key == "read"
		if (e.Session == "main") != wantMain {
			t.Errorf("%s: Sitzung %q", key, e.Session)
		}
	}
	// P8: bash läuft nicht im Container von pi.
	if e := execFor(execs, issued["bash"].ID); e != nil {
		for _, want := range []string{"main-10001", "No such file or directory", "agw-exec idle", "SETTINGS-FEHLT"} {
			if !strings.Contains(e.OutputExcerpt, want) {
				t.Errorf("Ausgabe von bash ohne %q: %q", want, e.OutputExcerpt)
			}
		}
	}
	// Plattform-Anbindung: agw-platform liegt im Abbild und geht über den Socket der Ausführungs-Sandbox.
	if e := execFor(execs, issued["bash-plattform"].ID); e == nil || !strings.Contains(e.OutputExcerpt, `HTTP 200`) || !strings.Contains(e.OutputExcerpt, "plattform-rc=0") {
		t.Errorf("agw-platform: %+v", e)
	}
	b.mu.Lock()
	if strings.Join(b.plat, ";") != "cli GET /datasets/7" {
		t.Errorf("Plattform-Aufrufe: %v", b.plat)
	}
	b.mu.Unlock()
	if e := execFor(execs, issued["edit"].ID); e == nil || strings.Join(opsFor(execs, issued["edit"].ID), ",") != "access,read,write" {
		t.Errorf("edit: Operationen %v", opsFor(execs, issued["edit"].ID))
	}
	if e := execFor(execs, issued["read"].ID); e == nil || e.OutputExcerpt != "moin e9\n" {
		t.Errorf("read nach edit: %+v", e)
	}
	// H1: Lange Ausgaben landen als Datei in der Ausführungs-Sandbox; pi überlebt auch 300 MB
	// Ausgabe (vorher tmpfs voll im Container von pi, unbehandelter Fehler, PID 1 tot), und ein
	// read auf den genannten Pfad liefert die Datei.
	endOf := func(key string) end {
		mu.Lock()
		defer mu.Unlock()
		return ends[issued[key].ID]
	}
	if r := endOf("bash-lang"); !strings.Contains(r.text, "Full output: /tmp/pi-bash-") || !strings.Contains(r.text, "20000") {
		t.Errorf("lange Ausgabe: %s", tail(r.text, 300))
	}
	if r := endOf("read-lang"); r.isError || !strings.Contains(r.text, `19999\n20000`) {
		t.Errorf("read auf die ganze Ausgabe: %+v", r)
	}
	if r := endOf("bash-riesig"); !strings.Contains(r.text, "ENDE") || !strings.Contains(r.text, "Full output: /tmp/pi-bash-") {
		t.Errorf("sehr große Ausgabe: %s", tail(r.text, 300))
	}
	if r := endOf("read-riesig"); r.isError || !strings.Contains(r.text, `y\ny`) || !strings.Contains(r.text, "more lines in file") {
		t.Errorf("read auf 256 MiB: %+v", r)
	}
	if out, _ := w.ExecPi(ctx, []string{"ls", "-a", "/tmp"}, nil); strings.Contains(string(out), "pi-bash") {
		t.Errorf("Datei im Container von pi: %s", out)
	}
	if out, err := w.Exec(ctx, []string{"sh", "-c", "for f in /tmp/pi-bash-*.log; do stat -c %s $f; done; tail -c 80 $(ls -S /tmp/pi-bash-*.log | head -1)"}, nil); err != nil ||
		!strings.Contains(string(out), "output truncated after 256 MiB") {
		t.Errorf("Dateien der ganzen Ausgabe: %q %v", out, err)
	}

	// Der Wächter sperrt Code und Agentendefinitionen im Container von pi (P4, P4b).
	blocked := 0
	for key, is := range issued {
		if !strings.HasPrefix(key, "subagent:") {
			continue
		}
		mu.Lock()
		r := ends[is.ID]
		mu.Unlock()
		if strings.Contains(is.Args, "workflowScriptPath") || strings.Contains(is.Args, `"create"`) || (strings.Contains(is.Args, "claude-code") && !strings.Contains(is.Args, "workflowScript")) {
			if !r.isError || !strings.Contains(r.text, "Blocked") {
				t.Errorf("nicht gesperrt: %s → %+v", is.Args, r)
			}
			blocked++
		}
	}
	if blocked != 2 { // create und claude-code; workflowScriptPath prüft wf:path
		t.Errorf("%d gesperrte Aufrufe statt 2", blocked)
	}
	// Workflows: Das Skript läuft in der Ausführungs-Sandbox, nicht im pi-Prozess (P4b).
	res := func(key string) end {
		mu.Lock()
		defer mu.Unlock()
		return ends[issued[key].ID]
	}
	if r := res("wf:escape"); r.isError || !regexp.MustCompile(`agent\W+false`).MatchString(r.text) || !regexp.MustCompile(`workspace\W+true`).MatchString(r.text) {
		t.Errorf("Ausbruch aus dem Workflow: %s", tail(r.text, 600))
	}
	for _, want := range []string{"agw-exec idle", "10001", "ENOENT"} {
		if r := res("wf:escape"); !strings.Contains(r.text, want) {
			t.Errorf("Ausbruch aus dem Workflow ohne %q: %s", want, tail(r.text, 600))
		}
	}
	if e := execFor(execs, issued["wf:escape"].ID); e == nil || e.Tool != "subagent" || e.Op != "workflow" || !strings.Contains(string(e.Args), "getBuiltinModule") {
		t.Errorf("Workflow nicht protokolliert: %+v", e)
	}
	// Drei parallele Läufe: jeder bash-Aufruf belegt, in eigenen Sitzungen, zeitlich überlappend.
	var par []*store.ToolExecution
	for _, k := range []string{"k:wf-1", "k:wf-2", "k:wf-3"} {
		e := execFor(execs, issued[k].ID)
		if e == nil || e.Session == "main" {
			t.Errorf("paralleler Lauf %s: %+v", k, e)
			continue
		}
		par = append(par, e)
	}
	if len(par) == 3 {
		latestStart, earliestEnd := par[0].StartedAt, par[0].StartedAt.Add(time.Duration(par[0].DurationMs)*time.Millisecond)
		for _, e := range par[1:] {
			if e.StartedAt.After(latestStart) {
				latestStart = e.StartedAt
			}
			if end := e.StartedAt.Add(time.Duration(e.DurationMs) * time.Millisecond); end.Before(earliestEnd) {
				earliestEnd = end
			}
		}
		t.Logf("workflowScript runs.all: Beginn der drei bash-Aufrufe innerhalb von %v, Überlappung %v", latestStart.Sub(par[0].StartedAt), earliestEnd.Sub(latestStart))
		if !latestStart.Before(earliestEnd) {
			t.Errorf("Läufe von runs.all nicht gleichzeitig")
		}
		if par[0].Session == par[1].Session || par[1].Session == par[2].Session {
			t.Errorf("parallele Läufe in derselben Sitzung: %s %s %s", par[0].Session, par[1].Session, par[2].Session)
		}
	}
	if r := res("wf:parallel"); r.isError || strings.Count(r.text, "true") < 3 {
		t.Errorf("runs.all: %s", tail(r.text, 400))
	}
	// Kette: der zweite Lauf sieht, was der erste geschrieben hat.
	if e := execFor(execs, issued["k:kette-2"].ID); e == nil || !strings.Contains(e.OutputExcerpt, "kette-1\nkette-2") {
		t.Errorf("Kette: %+v", e)
	}
	// Gesperrt: cwd, fremder Agent, gefälschte Anfrage über stdout des Runners.
	if r := res("wf:blocked"); !strings.Contains(r.text, "parameter cwd is blocked") || !strings.Contains(r.text, "claude-code") || strings.Contains(r.text, "lief") {
		t.Errorf("Sperren im Workflow: %s", tail(r.text, 600))
	}
	for _, k := range []string{"k:gesperrt-cwd", "k:gefaelscht"} {
		if is, ok := issued[k]; ok {
			t.Errorf("gesperrter Lauf hat das Modell erreicht: %s %s", k, is.Args)
		}
	}
	if r := res("wf:path"); !r.isError || !strings.Contains(r.text, "workflowScriptPath is blocked") {
		t.Errorf("workflowScriptPath: %+v", r)
	}
	// N2: watchdog_diff (git im Container von pi) ist gesperrt; das Ergebnis steht in der
	// Sitzung des Subagenten.
	if is, ok := issued["k:watchdog_diff"]; !ok {
		t.Error("watchdog_diff nie angefordert")
	} else if out, err := w.ExecPi(ctx, []string{"agw-exec", "poll-subagents"}, strings.NewReader(`{"offsets":{}}`)); err != nil ||
		!strings.Contains(string(out), is.ID) || !strings.Contains(string(out), "watchdog_diff is blocked") {
		t.Errorf("watchdog_diff nicht gesperrt: %v %s", err, tail(string(out), 400))
	}
	// Zwei einzelne Subagenten im Hintergrund laufen gleichzeitig (Frage des Verfassers).
	a1, a2 := execFor(execs, issued["k:async-1"].ID), execFor(execs, issued["k:async-2"].ID)
	if a1 == nil || a2 == nil {
		t.Errorf("Hintergrund-Subagenten: %+v %+v", a1, a2)
	} else {
		first, second := a1, a2
		if second.StartedAt.Before(first.StartedAt) {
			first, second = second, first
		}
		overlap := first.StartedAt.Add(time.Duration(first.DurationMs) * time.Millisecond).Sub(second.StartedAt)
		t.Logf("zwei Subagenten async: Abstand der Starts %v, Überlappung %v", second.StartedAt.Sub(first.StartedAt), overlap)
		if overlap <= 0 {
			t.Errorf("Hintergrund-Subagenten laufen nacheinander")
		}
	}

	// Nichts davon hat den Container von pi verändert.
	if out, err := w.ExecPi(ctx, []string{"cat", "/agent/config/settings.json"}, nil); err != nil || !strings.Contains(string(out), "exec-bridge.ts") {
		t.Errorf("settings.json verändert: %q %v", out, err)
	}
	if out, _ := w.ExecPi(ctx, []string{"ls", "/agent/config"}, nil); strings.Contains(string(out), "agents") {
		if out2, _ := w.ExecPi(ctx, []string{"ls", "/agent/config/agents"}, nil); strings.Contains(string(out2), "boese") {
			t.Errorf("eigener Agent angelegt: %s", out2)
		}
	}
	if out, _ := w.Exec(ctx, []string{"cat", "/workspace/kind.txt"}, nil); strings.TrimSpace(string(out)) != "kind-10001" {
		t.Errorf("Subagent schrieb nicht in die Ausführungs-Sandbox: %q", out)
	}
	t.Logf("%d Anforderungen, %d Ausführungen", len(fake.Issued()), len(execs))
}

func execFor(execs []store.ToolExecution, id string) *store.ToolExecution {
	for i := range execs {
		if execs[i].ToolCallID == id {
			return &execs[i]
		}
	}
	return nil
}

func opsFor(execs []store.ToolExecution, id string) []string {
	var ops []string
	for _, e := range execs {
		if e.ToolCallID == id {
			ops = append(ops, e.Op)
		}
	}
	return ops
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

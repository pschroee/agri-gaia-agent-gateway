package worker

// Integration test of a whole slot (E9) with a scripted model: real pi container
// without a shell, real execution sandbox, real extension exec-bridge.ts, real sockets. The model
// is internal/fakellm; it runs in this test process, which attaches itself to the slot network
// as "orchestrator". On the Mac, Unix sockets only work inside the Docker VM, so the test
// runs in a Go container (./dev.sh test, switch AGW_E9_IN_DOCKER=1).

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
	plat []string // calls of the platform binding (agw-platform in the image)
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

// Main agent: all four file and shell tools, then subagents in the foreground and in the background
// and three calls the guard has to block.
const e9Script = `Check the execution sandbox.
CALL bash {"command":"echo main-$(id -u); ls /agent 2>&1; tr '\\0' ' ' < /proc/1/cmdline; echo; test -e /agent/config/settings.json && echo SETTINGS-PRESENT || echo SETTINGS-MISSING"}
CALL write {"path":"/workspace/e9.txt","content":"hello e9\n"}
CALL edit {"path":"/workspace/e9.txt","edits":[{"oldText":"hello","newText":"howdy"}]}
CALL read {"path":"/workspace/e9.txt"}
CALL bash {"command":"agw-platform dataset 7; echo platform-rc=$?"}
CALL bash {"command":"seq 1 20000"}
CALL read {"path":"{{fullOutputPath}}","offset":19999}
CALL bash {"command":"yes | head -c 300000000; echo END"}
CALL read {"path":"{{fullOutputPath}}","offset":1000,"limit":2}
CALL subagent {"agent":"worker","async":false,"task":"K1\nCALL bash {\"command\":\"echo child-$(id -u) > /workspace/child.txt; ls /agent 2>&1\"}\nCALL grep {\"pattern\":\"howdy\",\"path\":\"/workspace\"}\nCALL find {\"pattern\":\"*.txt\",\"path\":\"/workspace\"}\nCALL ls {\"path\":\"/workspace\"}"}
CALL subagent {"action":"create","config":{"name":"evil","description":"x","systemPrompt":"x","extensions":"","subagentOnlyExtensions":""}}
CALL subagent {"agent":"claude-code","task":"x"}
CALL subagent {"agent":"scout","task":"K2\nCALL bash {\"command\":\"echo background-$(id -u)\"}"}`

// Workflows (decision of the author: workflowScript back, script in the
// execution sandbox). The scripts are Go strings here so that JSON takes care of the escaping.
const (
	// P4b: escape from node:vm. Before: agent code as PID 1 in the pi container, now
	// at most in the execution sandbox.
	wfEscape = `const p = emit.constructor('return process')();
const fs = p.getBuiltinModule('fs');
let wrote = 'no';
try { fs.writeFileSync('/agent/config/settings.json', '{}'); wrote = 'yes'; } catch (e) { wrote = e.code; }
return { uid: p.getuid(), agent: fs.existsSync('/agent'), workspace: fs.existsSync('/workspace/e9.txt'), pid1: fs.readFileSync('/proc/1/cmdline', 'utf8').split('\0').join(' '), wrote };`
	// Three parallel runs; each calls bash (recorded in tool_executions).
	wfParallel = `const r = await runs.all([
  { key: 'w1', agent: 'worker', task: 'WF1\nCALL bash {"command":"echo wf-1; sleep 2"}' },
  { key: 'w2', agent: 'worker', task: 'WF2\nCALL bash {"command":"echo wf-2; sleep 2"}' },
  { key: 'w3', agent: 'scout', task: 'WF3\nCALL bash {"command":"echo wf-3; sleep 2"}' },
]);
return r.map((x) => x.ok);`
	// Chain: the second run only after the first.
	wfChain = `const a = await runs.run('k1', { agent: 'worker', task: 'KE1\nCALL bash {"command":"echo chain-1 > /workspace/chain.txt"}' });
const b = await runs.run('k2', { agent: 'worker', task: 'KE2\nCALL bash {"command":"cat /workspace/chain.txt; echo chain-2"}' });
return [a.ok, b.ok];`
	// Blocked: a run with cwd, an agent with a foreign runtime, runs.host and a forged
	// request via the runner's stdout (it reaches the host, the guard rejects it).
	wfBlocked = `const out = [];
try { await runs.run('b1', { agent: 'worker', task: 'B1\nCALL bash {"command":"echo blocked-cwd"}', cwd: '/agent' }); out.push('cwd WENT-THROUGH'); } catch (e) { out.push('cwd: ' + e.message); }
try { await runs.run('b2', { agent: 'claude-code', task: 'x' }); out.push('claude-code WENT-THROUGH'); } catch (e) { out.push('claude-code: ' + e.message); }
const p = emit.constructor('return process')();
p.stdout.write(JSON.stringify({ m: { type: 'call', callId: 4242, method: 'run', args: { key: 'f1', params: { agent: 'worker', task: 'F1\nCALL bash {"command":"echo forged"}', cwd: '/agent' } } } }) + '\n');
const t0 = Date.now(); while (Date.now() - t0 < 500) {}
return out;`
)

func callLine(tool string, args any) string {
	b, _ := json.Marshal(args)
	return "CALL " + tool + " " + string(b)
}

func TestSlotE9WithScriptedModel(t *testing.T) {
	if os.Getenv("AGW_E9_IN_DOCKER") != "1" {
		t.Skip("runs only in the Go container with the Docker socket (./dev.sh test)")
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
		// two single subagents in the background: do they run at the same time?
		callLine("subagent", map[string]any{"agent": "scout", "async": true, "task": "AS1\nCALL bash {\"command\":\"echo async-1; sleep 3\"}"}),
		callLine("subagent", map[string]any{"agent": "scout", "async": true, "task": "AS2\nCALL bash {\"command\":\"echo async-2; sleep 3\"}"}),
	}, "\n")
	t0 := time.Now()
	if _, err := w.Call(ctx, map[string]any{"type": "prompt", "message": script}); err != nil {
		t.Fatalf("prompt after %v: %v", time.Since(t0), err)
	}
	select {
	case <-settled:
		t.Logf("main run settled after %v", time.Since(t0))
	case <-ctx.Done():
		is := fake.Issued()
		last := "none"
		if len(is) > 0 {
			last = is[len(is)-1].Tool + " " + tail(is[len(is)-1].Args, 200)
		}
		t.Fatalf("run does not finish within %v: %d requests, %d executions, last request: %s", time.Since(t0), len(is), len(b.executions()), last)
	}
	// The background subagent (scout) reports via its runner process.
	byTool := func() map[string]fakellm.Issued {
		m := map[string]fakellm.Issued{}
		for _, is := range fake.Issued() {
			key := is.Tool
			if strings.Contains(is.Args, "background") {
				key = "bash-background"
			} else if strings.Contains(is.Args, "seq 1 20000") {
				key = "bash-long"
			} else if strings.Contains(is.Args, "head -c 300000000") {
				key = "bash-huge"
			} else if strings.Contains(is.Args, `"offset":19999`) {
				key = "read-long"
			} else if strings.Contains(is.Args, `"offset":1000`) {
				key = "read-huge"
			} else if strings.Contains(is.Args, "platform-rc") {
				key = "bash-platform"
			} else if strings.Contains(is.Args, "child-") {
				key = "bash-child"
			} else if is.Tool == "subagent" {
				key = "subagent:" + is.Args
			}
			for _, k := range []string{"wf-1", "wf-2", "wf-3", "chain-1", "chain-2", "async-1", "async-2", "blocked-cwd", "forged", "watchdog_diff"} {
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
	waitStart := time.Now()
	deadline := waitStart.Add(60 * time.Second)
	var missing []string
	for {
		m := byTool()
		missing = missing[:0]
		for _, k := range []string{"bash-background", "k:async-1", "k:async-2"} {
			if is, ok := m[k]; !ok {
				missing = append(missing, k+" (not requested)")
			} else if execFor(b.executions(), is.ID) == nil {
				missing = append(missing, k+" (not executed)")
			}
		}
		if len(missing) == 0 || !time.Now().Before(deadline) {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if len(missing) > 0 {
		// Not fatal here: the checks below name what is wrong; this line says it was the wait.
		t.Logf("background subagents incomplete after waiting %v: %v", time.Since(waitStart), missing)
	} else {
		t.Logf("background subagents done after waiting %v", time.Since(waitStart))
	}
	issued := byTool()
	execs := b.executions()

	// P6: every execution carries an ID requested by the model, and every requested call
	// of a redirected tool was executed (main agent and subagent).
	ids := map[string]bool{}
	for _, is := range fake.Issued() {
		ids[is.ID] = true
	}
	for _, e := range execs {
		if !ids[e.ToolCallID] {
			t.Errorf("execution without a request: %+v", e)
		}
	}
	for _, key := range []string{"bash", "write", "edit", "read", "bash-child", "grep", "find", "ls", "bash-background"} {
		is, ok := issued[key]
		if !ok {
			t.Errorf("model never requested %s (script not worked through?)", key)
			continue
		}
		e := execFor(execs, is.ID)
		if e == nil {
			t.Errorf("%s (%s) not executed", key, is.ID)
			continue
		}
		wantMain := key == "bash" || key == "write" || key == "edit" || key == "read"
		if (e.Session == "main") != wantMain {
			t.Errorf("%s: session %q", key, e.Session)
		}
	}
	// P8: bash does not run in the pi container.
	if e := execFor(execs, issued["bash"].ID); e != nil {
		for _, want := range []string{"main-10001", "No such file or directory", "agw-exec idle", "SETTINGS-MISSING"} {
			if !strings.Contains(e.OutputExcerpt, want) {
				t.Errorf("bash output without %q: %q", want, e.OutputExcerpt)
			}
		}
	}
	// Platform binding: agw-platform is in the image and goes through the socket of the execution sandbox.
	if e := execFor(execs, issued["bash-platform"].ID); e == nil || !strings.Contains(e.OutputExcerpt, `HTTP 200`) || !strings.Contains(e.OutputExcerpt, "platform-rc=0") {
		t.Errorf("agw-platform: %+v", e)
	}
	b.mu.Lock()
	if strings.Join(b.plat, ";") != "cli GET /datasets/7" {
		t.Errorf("platform calls: %v", b.plat)
	}
	b.mu.Unlock()
	if e := execFor(execs, issued["edit"].ID); e == nil || strings.Join(opsFor(execs, issued["edit"].ID), ",") != "access,read,write" {
		t.Errorf("edit: operations %v", opsFor(execs, issued["edit"].ID))
	}
	if e := execFor(execs, issued["read"].ID); e == nil || e.OutputExcerpt != "howdy e9\n" {
		t.Errorf("read after edit: %+v", e)
	}
	// H1: long output ends up as a file in the execution sandbox; pi survives even 300 MB of
	// output (before: tmpfs full in the pi container, unhandled error, PID 1 dead), and a
	// read on the named path returns the file.
	endOf := func(key string) end {
		mu.Lock()
		defer mu.Unlock()
		return ends[issued[key].ID]
	}
	if r := endOf("bash-long"); !strings.Contains(r.text, "Full output: /tmp/pi-bash-") || !strings.Contains(r.text, "20000") {
		t.Errorf("long output: %s", tail(r.text, 300))
	}
	if r := endOf("read-long"); r.isError || !strings.Contains(r.text, `19999\n20000`) {
		t.Errorf("read on the full output: %+v", r)
	}
	if r := endOf("bash-huge"); !strings.Contains(r.text, "END") || !strings.Contains(r.text, "Full output: /tmp/pi-bash-") {
		t.Errorf("very large output: %s", tail(r.text, 300))
	}
	if r := endOf("read-huge"); r.isError || !strings.Contains(r.text, `y\ny`) || !strings.Contains(r.text, "more lines in file") {
		t.Errorf("read on 256 MiB: %+v", r)
	}
	if out, _ := w.ExecPi(ctx, []string{"ls", "-a", "/tmp"}, nil); strings.Contains(string(out), "pi-bash") {
		t.Errorf("file in the pi container: %s", out)
	}
	if out, err := w.Exec(ctx, []string{"sh", "-c", "for f in /tmp/pi-bash-*.log; do stat -c %s $f; done; tail -c 80 $(ls -S /tmp/pi-bash-*.log | head -1)"}, nil); err != nil ||
		!strings.Contains(string(out), "output truncated after 256 MiB") {
		t.Errorf("files of the full output: %q %v", out, err)
	}

	// The guard blocks code and agent definitions in the pi container (P4, P4b).
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
				t.Errorf("not blocked: %s → %+v", is.Args, r)
			}
			blocked++
		}
	}
	if blocked != 2 { // create and claude-code; wf:path checks workflowScriptPath
		t.Errorf("%d blocked calls instead of 2", blocked)
	}
	// Workflows: the script runs in the execution sandbox, not in the pi process (P4b).
	res := func(key string) end {
		mu.Lock()
		defer mu.Unlock()
		return ends[issued[key].ID]
	}
	if r := res("wf:escape"); r.isError || !regexp.MustCompile(`agent\W+false`).MatchString(r.text) || !regexp.MustCompile(`workspace\W+true`).MatchString(r.text) {
		t.Errorf("escape from the workflow: %s", tail(r.text, 600))
	}
	for _, want := range []string{"agw-exec idle", "10001", "ENOENT"} {
		if r := res("wf:escape"); !strings.Contains(r.text, want) {
			t.Errorf("escape from the workflow without %q: %s", want, tail(r.text, 600))
		}
	}
	if e := execFor(execs, issued["wf:escape"].ID); e == nil || e.Tool != "subagent" || e.Op != "workflow" || !strings.Contains(string(e.Args), "getBuiltinModule") {
		t.Errorf("workflow not logged: %+v", e)
	}
	// Three parallel runs: every bash call recorded, in separate sessions, overlapping in time.
	var par []*store.ToolExecution
	for _, k := range []string{"k:wf-1", "k:wf-2", "k:wf-3"} {
		e := execFor(execs, issued[k].ID)
		if e == nil || e.Session == "main" {
			t.Errorf("parallel run %s: %+v", k, e)
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
		t.Logf("workflowScript runs.all: the three bash calls started within %v, overlap %v", latestStart.Sub(par[0].StartedAt), earliestEnd.Sub(latestStart))
		if !latestStart.Before(earliestEnd) {
			t.Errorf("runs of runs.all not concurrent")
		}
		if par[0].Session == par[1].Session || par[1].Session == par[2].Session {
			t.Errorf("parallel runs in the same session: %s %s %s", par[0].Session, par[1].Session, par[2].Session)
		}
	}
	if r := res("wf:parallel"); r.isError || strings.Count(r.text, "true") < 3 {
		t.Errorf("runs.all: %s", tail(r.text, 400))
	}
	// Chain: the second run sees what the first one wrote.
	if e := execFor(execs, issued["k:chain-2"].ID); e == nil || !strings.Contains(e.OutputExcerpt, "chain-1\nchain-2") {
		t.Errorf("chain: %+v", e)
	}
	// Blocked: cwd, foreign agent, forged request via the runner's stdout.
	if r := res("wf:blocked"); !strings.Contains(r.text, "parameter cwd is blocked") || !strings.Contains(r.text, "claude-code") || strings.Contains(r.text, "WENT-THROUGH") {
		t.Errorf("blocks in the workflow: %s", tail(r.text, 600))
	}
	for _, k := range []string{"k:blocked-cwd", "k:forged"} {
		if is, ok := issued[k]; ok {
			t.Errorf("blocked run reached the model: %s %s", k, is.Args)
		}
	}
	if r := res("wf:path"); !r.isError || !strings.Contains(r.text, "workflowScriptPath is blocked") {
		t.Errorf("workflowScriptPath: %+v", r)
	}
	// N2: watchdog_diff (git in the pi container) is blocked; the result is in the
	// subagent's session.
	if is, ok := issued["k:watchdog_diff"]; !ok {
		t.Error("watchdog_diff never requested")
	} else if out, err := w.ExecPi(ctx, []string{"agw-exec", "poll-subagents"}, strings.NewReader(`{"offsets":{}}`)); err != nil ||
		!strings.Contains(string(out), is.ID) || !strings.Contains(string(out), "watchdog_diff is blocked") {
		t.Errorf("watchdog_diff not blocked: %v %s", err, tail(string(out), 400))
	}
	// Two single subagents in the background run at the same time (question of the author).
	a1, a2 := execFor(execs, issued["k:async-1"].ID), execFor(execs, issued["k:async-2"].ID)
	if a1 == nil || a2 == nil {
		t.Errorf("background subagents: %+v %+v", a1, a2)
	} else {
		first, second := a1, a2
		if second.StartedAt.Before(first.StartedAt) {
			first, second = second, first
		}
		overlap := first.StartedAt.Add(time.Duration(first.DurationMs) * time.Millisecond).Sub(second.StartedAt)
		t.Logf("two subagents async: gap between starts %v, overlap %v", second.StartedAt.Sub(first.StartedAt), overlap)
		if overlap <= 0 {
			t.Errorf("background subagents run one after another")
		}
	}

	// None of this changed the pi container.
	if out, err := w.ExecPi(ctx, []string{"cat", "/agent/config/settings.json"}, nil); err != nil || !strings.Contains(string(out), "exec-bridge.ts") {
		t.Errorf("settings.json changed: %q %v", out, err)
	}
	if out, _ := w.ExecPi(ctx, []string{"ls", "/agent/config"}, nil); strings.Contains(string(out), "agents") {
		if out2, _ := w.ExecPi(ctx, []string{"ls", "/agent/config/agents"}, nil); strings.Contains(string(out2), "evil") {
			t.Errorf("own agent created: %s", out2)
		}
	}
	if out, _ := w.Exec(ctx, []string{"cat", "/workspace/child.txt"}, nil); strings.TrimSpace(string(out)) != "child-10001" {
		t.Errorf("subagent did not write into the execution sandbox: %q", out)
	}
	t.Logf("%d requests, %d executions", len(fake.Issued()), len(execs))
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

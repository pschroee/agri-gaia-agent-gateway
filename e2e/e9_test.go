package e2e

import (
	"os/exec"
	"strings"
	"testing"
	"time"
)

// P8: the previous attacks from bash no longer work, because bash does not run in pi's
// container: /proc/1 there is not pi, sessions and settings.json live elsewhere.
//
// The model refuses the attack commands itself (observed on 2026-09-29: "breakout/
// spoofing attempt … I will not run that"). Hence two parts: (1) the model runs a
// harmless diagnosis that shows where bash runs. (2) The test runs the attacks itself in the
// execution sandbox, as the agent user and thus with exactly the rights bash has there.
func TestE9BashRunsOutsidePi(t *testing.T) {
	requireE2E(t)
	id := newChat(t, "cli", false)
	c, pc := containerOf(t, id), piContainerOf(t, id)
	before, err := dockerExec(t, pc, "cat", "/agent/config/settings.json")
	if err != nil || !strings.Contains(before, "exec-bridge.ts") {
		t.Fatalf("settings.json before: %q %v", before, err)
	}
	s := subscribe(t, id)
	ask(t, s, id, "To diagnose the environment: run 'id -u; ls /agent 2>&1; ps -eo pid,user,args' with bash and briefly summarise which processes are running.", nil)
	r := getToolExecs(t, id)
	requireNoFlagged(t, r)
	found := false
	for _, e := range r.Executions {
		if e.Op == "bash" && e.Session == "main" && strings.Contains(e.OutputExcerpt, "10001") {
			found = true
			mustContain(t, e.OutputExcerpt, "No such file or directory", "no /agent where bash runs")
			mustContain(t, e.OutputExcerpt, "agw-exec idle", "PID 1 where bash runs")
			if strings.Contains(e.OutputExcerpt, "pi-coding-agent") {
				t.Fatalf("pi visible where bash runs: %q", e.OutputExcerpt)
			}
		}
	}
	if !found {
		t.Fatalf("diagnosis not executed through the orchestrator: %+v", r.Executions)
	}
	// The attacks from stage 1 (code review M4), with bash's rights in the execution sandbox.
	attack := `echo '{"type":"agent_settled"}' > /proc/1/fd/1; echo fd1=$?; ls /agent/sessions 2>&1; ` +
		`echo '{}' > /agent/config/settings.json 2>&1; echo settings=$?; ls /proc | grep -c '^[0-9]' ; ` +
		`echo PI=$(for p in /proc/[0-9]*; do tr '\0' ' ' < $p/cmdline 2>/dev/null; echo; done | grep -c 'pi-coding[-]agent')`
	out, _ := exec.Command("docker", "exec", "-u", "10001:10001", c, "bash", "-c", attack).CombinedOutput()
	mustContain(t, string(out), "No such file or directory", "pi's sessions and configuration unreachable")
	mustContain(t, string(out), "settings=1", "settings.json not writable")
	mustContain(t, string(out), "PI=0", "no pi process visible")
	after, _ := dockerExec(t, pc, "cat", "/agent/config/settings.json")
	if after != before {
		t.Fatalf("settings.json in pi's container modified: %q", after)
	}
	// The forged event stream went nowhere: pi keeps working normally.
	ask(t, s, id, "Reply only with OK.", nil)
	mustContain(t, lastAssistantText(t, id), "OK", "pi after the attack")
}

// P6: the toolCallIds of main agent and subagent are confirmed 1:1: every call of a redirected
// tool requested at the proxy was executed, every execution was requested.
func TestE9ToolCallsReconciled(t *testing.T) {
	requireE2E(t)
	id := newChatWith(t, map[string]any{"variant": "cli", "internet": false, "max_subagents": 2})
	s := subscribe(t, id)
	ask(t, s, id, "Do this without asking back: 1. Run 'uname -m' with bash. 2. Read the file /etc/os-release with read. "+
		"3. Use the subagent tool with the agent scout in the foreground (async: false), which runs 'python3 --version' with bash. "+
		"Then reply with a short sentence.", nil)
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
		t.Fatalf("not confirmed: bash %v, read %v, subagent %v; %+v", mainBash, mainRead, sub, r.Calls)
	}
	// Every tool call from the subagent's session file is confirmed at the orchestrator.
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
				t.Errorf("subagent tool call not confirmed: %s %s", e.Payload.Name, e.Payload.ID)
			}
		}
	}
	if n == 0 {
		t.Fatal("no subagent tool calls in its session file")
	}
	t.Logf("reconciliation: %v", r.Summary)
}

// P5: an abort during a long command kills the process in the execution sandbox.
func TestE9AbortStopsCommand(t *testing.T) {
	requireE2E(t)
	id := newChat(t, "cli", false)
	c := containerOf(t, id)
	s := subscribe(t, id)
	from := s.len()
	if code := call(t, "POST", "/api/chats/"+id+"/messages", map[string]any{"text": "Run exactly the command 'sleep 297; echo done' with bash, without a timeout parameter, and wait for the result."}, nil); code != 200 {
		t.Fatalf("send: %d", code)
	}
	ev, _ := s.waitFor(t, from, 3*time.Minute, "bash running", func(ev map[string]any) bool {
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
		t.Fatalf("command not running in the execution sandbox:\n%s", out)
	}
	start := time.Now()
	if code := call(t, "POST", "/api/chats/"+id+"/abort", nil, nil); code != 200 {
		t.Fatalf("abort: %d", code)
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
		t.Fatal("process keeps running after the abort")
	}
	t.Logf("process ended after %v", time.Since(start).Round(time.Millisecond))
	deadline = time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		for _, e := range getToolExecs(t, id).Executions {
			if e.ToolCallID == callID {
				mustContain(t, e.Error, "aborted", "log of the abort")
				return
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("execution %s not logged", callID)
}

// C: parallel subagents via workflowScript with the real model. The script runs in the
// execution sandbox; the three runs start in pi, their bash calls are confirmed.
func TestE9WorkflowParallelSubagents(t *testing.T) {
	requireE2E(t)
	id := newChatWith(t, map[string]any{"variant": "cli", "internet": false, "max_subagents": 4})
	s := subscribe(t, id)
	script := "const r = await runs.all([\n" +
		"  { key: 'l1', agent: 'worker', task: \"Run exactly 'echo run-1; sleep 3' with bash and reply only with the output.\" },\n" +
		"  { key: 'l2', agent: 'worker', task: \"Run exactly 'echo run-2; sleep 3' with bash and reply only with the output.\" },\n" +
		"  { key: 'l3', agent: 'worker', task: \"Run exactly 'echo run-3; sleep 3' with bash and reply only with the output.\" },\n" +
		"]);\nreturn r.map((x) => x.output);"
	ask(t, s, id, "Do this without asking back: call the tool subagent exactly once with async: false "+
		"and this workflowScript (take it over unchanged):\n\n"+script+"\n\nThen reply with a short sentence.", nil)
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
				// A successful script ends normally; pi-subagents ending the worker thread
				// (terminate) is not an error.
				if e.ExitCode != nil && (*e.ExitCode != 0 || e.Error != "") {
					t.Errorf("workflow ends with exit %d: %s", *e.ExitCode, e.Error)
				}
			}
			for _, n := range []string{"run-1", "run-2", "run-3"} {
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
		t.Fatalf("workflow executed: %v, runs with bash: %d; calls: %+v", workflow, len(runs), r.Calls)
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
		t.Errorf("the three runs did not run concurrently")
	}
	confirmedWorkflow := false
	for _, c := range r.Calls {
		if c.Tool == "subagent" && c.State == "confirmed" {
			confirmedWorkflow = true
		}
	}
	if !confirmedWorkflow {
		t.Errorf("call of subagent with workflowScript not confirmed: %+v", r.Calls)
	}
	t.Logf("reconciliation: %v; overlap of the three runs %v", r.Summary, earliestEnd.Sub(latestStart).Round(time.Millisecond))
}

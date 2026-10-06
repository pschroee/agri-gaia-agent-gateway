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

// bigText creates a text of about n lines that the model cannot
// summarise without reading it (one separate measurement per line).
func bigText(seed, n int) string {
	var b strings.Builder
	x := seed*7919 + 17
	for i := 0; i < n; i++ {
		x = (x*1103515245 + 12345) % 2147483648
		fmt.Fprintf(&b, "Measurement %04d: barn %c, temperature %d.%d degrees, humidity %d %%, ID %08x\n", i, 'A'+rune(x%6), 15+x%12, x%10, 40+x%50, x)
	}
	return b.String()
}

// The tests run one after another (no t.Parallel) because they share the pool.

func TestSandboxHardening(t *testing.T) {
	requireE2E(t)
	id := newChat(t, "cli", false)
	c, pc := containerOf(t, id), piContainerOf(t, id)
	// Execution sandbox (E9): no key, no pi configuration, no route to the API or proxy.
	out, _ := dockerExec(t, c, "sh", "-c", "env; ls /agent 2>&1")
	if strings.Contains(out, "sk-") {
		t.Fatal("API key in the execution sandbox")
	}
	mustContain(t, out, "No such file", "no /agent in the execution sandbox")
	out, _ = dockerExec(t, c, "sh", "-c", "curl -s -m 5 http://orchestrator:18480/api/chats || echo API-UNREACHABLE; curl -s -m 5 http://orchestrator:18481/ || echo PROXY-UNREACHABLE")
	mustContain(t, out, "API-UNREACHABLE", "API from the execution sandbox")
	mustContain(t, out, "PROXY-UNREACHABLE", "proxy from the execution sandbox")
	out, _ = dockerExec(t, c, "sh", "-c", "curl -s -m 5 -o /dev/null -w '%{http_code}' https://example.com || echo NO_NET")
	mustContain(t, out, "NO_NET", "internet without approval")
	out, _ = dockerExec(t, c, "sh", "-c", "touch /usr/x 2>&1; id -u")
	mustContain(t, out, "Read-only", "root file system read-only")
	mustContain(t, out, "10001", "unprivileged user")
	// pi's container: no shell, no key; the API stays blocked, the proxy checks the model.
	if _, err := dockerExec(t, pc, "sh", "-c", "true"); err == nil {
		t.Fatal("shell in pi's container")
	}
	out, _ = dockerExec(t, pc, "cat", "/agent/config/models.json", "/proc/1/environ")
	if strings.Contains(out, "sk-") {
		t.Fatal("API key in pi's container")
	}
	mustContain(t, nodeFetch(t, pc, "GET", "http://orchestrator:18480/api/chats", ""), "no access from the sandbox network", "API from pi's container")
	mustContain(t, nodeFetch(t, pc, "POST", "http://orchestrator:18481/llm/deepseek/chat/completions", `{"model":"foreign"}`), "not allowed", "model allowlist")
	mustContain(t, nodeFetch(t, pc, "GET", "https://example.com", ""), "ERROR", "internet in pi's container")
}

// Attack paths from the code review (K2, K3, H5): with internet switched on.
func TestSandboxEscapesBlocked(t *testing.T) {
	requireE2E(t)
	a := newChat(t, "cli", true)
	b := newChat(t, "cli", true)
	ca, cb := containerOf(t, a), containerOf(t, b)
	// K3: user API via the host without a token
	out, _ := dockerExec(t, ca, "sh", "-c", "curl -s -m 5 http://host.docker.internal:18480/api/approvals?state=pending; echo; curl -s -m 5 -o /dev/null -w '%{http_code}' -X POST http://host.docker.internal:18480/api/approvals/x -d '{\"approve\":true}'")
	mustContain(t, out, "unknown host", "API via host.docker.internal (host check)")
	// With a forged Host header the login check applies.
	out2, _ := dockerExec(t, ca, "sh", "-c", "curl -s -m 5 -H 'Host: 127.0.0.1:18480' http://host.docker.internal:18480/api/approvals?state=pending")
	mustContain(t, out2, "not logged in", "API without a token (login)")
	if strings.Contains(out, "200") || strings.Contains(out, "204") {
		t.Fatalf("API reachable without a token: %s", out)
	}
	// K2: duplicate keys at the proxy. Since E9 only pi's container reaches the proxy.
	pa, pb := piContainerOf(t, a), piContainerOf(t, b)
	mustContain(t, nodeFetch(t, pa, "POST", "http://orchestrator:18481/llm/deepseek/chat/completions", `{"model":"deepseek-reasoner","Model":"deepseek-flash","messages":[]}`), "ambiguous", "proxy duplicate keys")
	mustContain(t, nodeFetch(t, pa, "POST", "http://orchestrator:18481/llm/deepseek/files", `{"model":"deepseek-flash"}`), "path not allowed", "proxy path")
	// H5: sandbox B does not reach a service in sandbox A, neither via the
	// slot network nor via the egress network.
	_, _ = dockerExec(t, ca, "sh", "-c", "cd /tmp && nohup python3 -m http.server 8765 >/dev/null 2>&1 &")
	time.Sleep(time.Second)
	local, _ := dockerExec(t, ca, "sh", "-c", "curl -s -m 3 -o /dev/null -w '%{http_code}' http://127.0.0.1:8765/")
	mustContain(t, local, "200", "service in sandbox A running")
	ipsOut, _ := exec.Command("docker", "inspect", "-f", "{{range .NetworkSettings.Networks}}{{.IPAddress}} {{end}}", ca).CombinedOutput()
	for _, ip := range strings.Fields(string(ipsOut)) {
		out, _ = dockerExec(t, cb, "sh", "-c", "curl -s -m 3 -o /dev/null -w '%{http_code}' http://"+ip+":8765/ || echo BLOCKED")
		if !strings.Contains(out, "BLOCKED") && !strings.HasPrefix(strings.TrimSpace(out), "000") {
			t.Fatalf("sandbox B reaches sandbox A via %s: %s", ip, out)
		}
	}
	out, _ = dockerExec(t, cb, "sh", "-c", "curl -s -m 3 -o /dev/null -w '%{http_code}' http://"+ca+":8765/ || echo BLOCKED")
	if !strings.Contains(out, "BLOCKED") && !strings.HasPrefix(strings.TrimSpace(out), "000") {
		t.Fatalf("sandbox B reaches sandbox A via the name: %s", out)
	}
	// The route to the model still exists, but only for pi.
	mustContain(t, nodeFetch(t, pb, "POST", "http://orchestrator:18481/llm/deepseek/chat/completions", `{"model":"foreign"}`), "403", "proxy reachable from pi B's container")
}

func TestRunWithToolsCostAndContext(t *testing.T) {
	requireE2E(t)
	id := newChat(t, "cli", false)
	s := subscribe(t, id)
	ask(t, s, id, "Use Python to write a file squares.txt with the squares of 1 to 5, one number per line, run it and show the content with cat.", nil)
	calls := strings.Join(toolCalls(t, id), "\n")
	mustContain(t, calls, "bash", "tool call bash")
	mustContain(t, lastAssistantText(t, id), "25", "result")
	if s.count(func(ev map[string]any) bool {
		d, _ := ev["data"].(map[string]any)
		e, _ := d["assistantMessageEvent"].(map[string]any)
		return piType(ev) == "message_update" && e["type"] == "text_delta"
	}) < 2 {
		t.Fatal("no streaming (fewer than two text deltas)")
	}
	f := getChat(t, id)
	if f.Chat.Cost <= 0 || f.Chat.Tokens.Total <= 0 {
		t.Fatalf("cost/tokens: %v %+v", f.Chat.Cost, f.Chat.Tokens)
	}
	billed := 0
	for _, m := range f.Messages {
		if m.Role == "assistant" && m.Cost != nil && m.Peak != nil {
			billed++
		}
	}
	if billed == 0 {
		t.Fatal("no reply billed by tariff")
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
		t.Fatalf("context: %+v", cu)
	}
}

func TestInternetRequestedByAgent(t *testing.T) {
	requireE2E(t)
	id := newChat(t, "cli", false)
	s := subscribe(t, id)
	var asked []approval
	ask(t, s, id, "Fetch https://example.com with curl and give the content of the <title> element. You have no internet at first; request it with agw-internet and a reason.", func(a approval) bool {
		asked = append(asked, a)
		return true
	})
	if len(asked) == 0 || asked[0].Kind != "internet_access" || asked[0].Name == "" {
		t.Fatalf("no internet request with a reason: %+v", asked)
	}
	if !getChat(t, id).Chat.Internet {
		t.Fatal("internet not on after approval")
	}
	mustContain(t, lastAssistantText(t, id), "Example Domain", "title of the page")
}

func TestInternetRequestRejected(t *testing.T) {
	requireE2E(t)
	id := newChat(t, "mcp", false)
	s := subscribe(t, id)
	var asked []approval
	ask(t, s, id, "I need the title of https://example.com. You have no internet; request it with the tool mcp_request_internet. If it is rejected, say so and stop.", func(a approval) bool {
		asked = append(asked, a)
		return false
	})
	if len(asked) == 0 || asked[0].Kind != "internet_access" || asked[0].Via != "mcp" {
		t.Fatalf("no MCP request: %+v", asked)
	}
	if getChat(t, id).Chat.Internet {
		t.Fatal("internet on despite rejection")
	}
}

func TestArtifactRejectThenApprove(t *testing.T) {
	requireE2E(t)
	id := newChat(t, "cli", false)
	s := subscribe(t, id)
	ask(t, s, id, "Create a file hello.txt with the content 'Hello world' and upload it with agw-artifact as an artifact.", func(approval) bool { return false })
	f := getChat(t, id)
	if len(f.Artifacts) != 0 {
		t.Fatalf("artifact despite rejection: %+v", f.Artifacts)
	}
	ask(t, s, id, "Please upload hello.txt once more, this time I will approve.", func(approval) bool { return true })
	f = getChat(t, id)
	var states []string
	for _, a := range f.Approvals {
		states = append(states, a.State)
	}
	if len(f.Artifacts) != 1 || f.Artifacts[0].Name != "hello.txt" || f.Artifacts[0].Via != "cli" {
		t.Fatalf("artifact: %+v (approvals %v)", f.Artifacts, states)
	}
	if !strings.Contains(strings.Join(states, ","), "rejected") || !strings.Contains(strings.Join(states, ","), "approved") {
		t.Fatalf("approvals: %v", states)
	}
}

func TestMCPVariant(t *testing.T) {
	requireE2E(t)
	id := newChat(t, "mcp", false)
	s := subscribe(t, id)
	ask(t, s, id, "Call the tool mcp_ping, then create the file note.txt with the content 'MCP' using write and upload it with mcp_upload_artifact.", func(approval) bool { return true })
	calls := strings.Join(toolCalls(t, id), "\n")
	mustContain(t, calls, "mcp_ping", "ping")
	mustContain(t, calls, "mcp_upload_artifact", "Upload")
	if strings.Contains(calls, "bash ") {
		t.Fatal("bash called in the MCP variant")
	}
	f := getChat(t, id)
	var ops []string
	for _, c := range f.SocketCalls {
		ops = append(ops, c.Via+":"+c.Op+":"+c.Result)
	}
	mustContain(t, strings.Join(ops, " "), "mcp:ping:ok", "socket log")
	mustContain(t, strings.Join(ops, " "), "mcp:upload:approved", "socket log")
}

func TestUserInputAndResume(t *testing.T) {
	requireE2E(t)
	id := newChat(t, "cli", false)
	s := subscribe(t, id)
	uploadFile(t, id, "values.csv", "a,b\n1,2\n3,4\n5,6\n")
	ask(t, s, id, "Read /workspace/inputs/values.csv and give the sum of column b. Also remember the code word Kiefer-42.", nil)
	mustContain(t, lastAssistantText(t, id), "12", "sum from the input")
	old := getChat(t, id).Chat.SlotID
	if code := call(t, "POST", "/api/chats/"+id+"/suspend", nil, nil); code != 200 {
		t.Fatalf("idle: %d", code)
	}
	if st := getChat(t, id).Chat; st.State != "dormant" || st.SlotID != "" {
		t.Fatalf("after idling: %+v", st)
	}
	ask(t, s, id, "What was the code word? And is values.csv still in /workspace/inputs? Check with ls.", nil)
	c := getChat(t, id).Chat
	if c.State != "active" || c.SlotID == old {
		t.Fatalf("not resumed in a fresh sandbox: %+v (old %s)", c, old)
	}
	txt := lastAssistantText(t, id)
	mustContain(t, txt, "Kiefer-42", "memory after resuming")
	mustContain(t, txt, "values.csv", "input in the new sandbox")
}

func TestManualCompaction(t *testing.T) {
	requireE2E(t)
	id := newChat(t, "cli", false)
	s := subscribe(t, id)
	ask(t, s, id, "Remember: the project is called Rotkehlchen. Reply only with OK.", nil)
	// Automation off, so that the low E2E threshold does not compact beforehand.
	call(t, "POST", "/api/chats/"+id+"/commands", map[string]string{"command": "/autocompact off"}, nil)
	if getChat(t, id).Chat.AutoCompact {
		t.Fatal("/autocompact off has no effect")
	}
	// Enough history so that pi has something to summarise (otherwise: "Nothing to compact").
	// Reading with read reliably brings the content into the context.
	uploadFile(t, id, "measurements.txt", bigText(1, 300))
	ask(t, s, id, "Read /workspace/inputs/measurements.txt completely with the tool read (not with bash) and give the highest temperature.", nil)
	var cmds []struct{ Name, Source string }
	call(t, "GET", "/api/chats/"+id+"/commands", nil, &cmds)
	names := ""
	for _, c := range cmds {
		names += c.Name + "(" + c.Source + ") "
	}
	mustContain(t, names, "compact(builtin)", "command list")
	mustContain(t, names, "skill:", "pi's commands")
	from := s.len()
	if code := call(t, "POST", "/api/chats/"+id+"/commands", map[string]string{"command": "/compact be sure to keep the project name"}, nil); code != 200 {
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
		t.Fatal("compaction not saved")
	}
	ask(t, s, id, "What is the project called?", nil)
	mustContain(t, lastAssistantText(t, id), "Rotkehlchen", "memory after compaction")
	if getChat(t, id).Chat.Compactions != 1 {
		t.Fatal("compacted automatically despite /autocompact off")
	}
}

// Auto-compaction needs a low threshold (./dev.sh e2e sets
// AGW_COMPACT_RESERVE_TOKENS so that it kicks in at about 10,000 tokens).
func TestAutoCompaction(t *testing.T) {
	requireE2E(t)
	var cfg struct {
		Reserve int `json:"compact_reserve_tokens"`
	}
	call(t, "GET", "/api/config", nil, &cfg)
	if cfg.Reserve < 900000 {
		t.Skipf("threshold too high for a test (reserve %d); use ./dev.sh e2e", cfg.Reserve)
	}
	id := newChat(t, "cli", false)
	s := subscribe(t, id)
	ask(t, s, id, "Remember the code word Wacholder-7. Reply only with OK.", nil)
	// Fill the context: have large files read with read until pi compacts by itself.
	for i := 0; i < 4 && s.count(func(ev map[string]any) bool { return piType(ev) == "compaction_start" }) == 0; i++ {
		name := fmt.Sprintf("part%d.txt", i)
		uploadFile(t, id, name, bigText(10+i, 300))
		ask(t, s, id, "Read /workspace/inputs/"+name+" completely with the tool read (not with bash) and give the lowest humidity.", nil)
	}
	ev, _ := s.waitFor(t, 0, 2*time.Minute, "compaction_start (threshold)", func(ev map[string]any) bool { return piType(ev) == "compaction_start" })
	d := ev["data"].(map[string]any)
	if d["reason"] != "threshold" && d["reason"] != "overflow" {
		t.Fatalf("reason: %v", d["reason"])
	}
	s.waitFor(t, 0, 3*time.Minute, "compaction_end", func(ev map[string]any) bool { return piType(ev) == "compaction_end" })
	deadline := time.Now().Add(20 * time.Second)
	for getChat(t, id).Chat.Compactions < 1 && time.Now().Before(deadline) {
		time.Sleep(300 * time.Millisecond)
	}
	f := getChat(t, id)
	if f.Chat.Compactions < 1 {
		t.Fatal("automatic compaction not saved")
	}
	var comp map[string]any
	for _, m := range f.Messages {
		if m.Role == "compaction" {
			_ = json.Unmarshal(m.Message, &comp)
		}
	}
	if comp["reason"] != "threshold" && comp["reason"] != "overflow" {
		t.Fatalf("entry: %v", comp)
	}
	if tb, _ := comp["tokensBefore"].(float64); tb < 5000 {
		t.Fatalf("tokensBefore implausible: %v", comp["tokensBefore"])
	}
	ask(t, s, id, "What was the code word?", nil)
	mustContain(t, lastAssistantText(t, id), "Wacholder-7", "memory after auto-compaction")
}

// Subagents: tool calls visible, costs recorded at the proxy.
func TestSubagentsVisibleAndBilled(t *testing.T) {
	requireE2E(t)
	id := newChatWith(t, map[string]any{"variant": "cli", "internet": false})
	s := subscribe(t, id)
	ask(t, s, id, "Use the subagent tool with the agent scout in the foreground (async: false), which runs 'python3 --version' with bash. Then give only the version.", nil)
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
		t.Fatalf("subagent tool call not visible: %+v", f.Subagent)
	}
	if !confirmed {
		t.Fatal("no subagent entry confirmed at the proxy")
	}
	mains := 0
	for _, m := range f.Messages {
		if m.Role == "assistant" {
			mains++
		}
	}
	if f.Chat.LLMCalls <= mains || f.Chat.CostOther <= 0 {
		t.Fatalf("subagent calls not billed: %d calls, %d main replies, cost_other %v", f.Chat.LLMCalls, mains, f.Chat.CostOther)
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
		t.Fatalf("sum of the calls %v ≠ chat cost %v", sum, f.Chat.Cost)
	}
}

// Fixed limit of five subagents at the same time (issue #24): with six background runs either
// pi-subagents queues the sixth (cooperative layer) or the orchestrator aborts (monitoring); never do
// more than five run at the same time without an intervention.
func TestSubagentLimitEnforced(t *testing.T) {
	requireE2E(t)
	id := newChatWith(t, map[string]any{"variant": "cli", "internet": false})
	s := subscribe(t, id)
	ask(t, s, id, "Without asking back: use the subagent tool six times in the background (async: true), each with the agent scout, "+
		"which runs 'sleep 30; echo done' with bash. Start all six right away, then wait for the results with bg_wait.", nil)
	deadline := time.Now().Add(90 * time.Second)
	peak := 0
	for time.Now().Before(deadline) {
		f := getChat(t, id)
		for _, c := range f.SocketCalls {
			if c.Op == "subagent_limit" {
				t.Logf("monitoring intervened (%s)", c.Result)
				return
			}
		}
		if f.Chat.MaxSub != 5 {
			t.Fatalf("limit %d, want the fixed 5", f.Chat.MaxSub)
		}
		peak = max(peak, f.Chat.SubRunning)
		if peak > 5 {
			t.Fatalf("%d subagents running at the same time without an intervention", peak)
		}
		if f.Chat.Subagents >= 6 && !f.Chat.Running {
			break
		}
		time.Sleep(time.Second)
	}
	if peak == 0 {
		t.Fatal("no subagent seen running")
	}
	t.Logf("at most %d subagents at the same time (cooperative layer queued the rest)", peak)
}

// Hard limit at the proxy, also for calls bypassing pi. Since E9 only pi's container reaches
// the proxy (the execution sandbox has no network to the orchestrator); the test calls it there
// directly with Node.
func TestProxyConcurrencyLimitHard(t *testing.T) {
	requireE2E(t)
	id := newChatWith(t, map[string]any{"variant": "cli", "internet": false})
	c := piContainerOf(t, id)
	script := `const body=JSON.stringify({model:"deepseek-flash",stream:true,messages:[{role:"user",content:"Count slowly from 1 to 40, each number on its own line."}]});
Promise.all([1,2,3,4,5,6,7,8].map(()=>fetch("http://orchestrator:18481/llm/deepseek/chat/completions",{method:"POST",headers:{"content-type":"application/json"},body}).then(async r=>{console.log(r.status);await r.text()}).catch(e=>console.log("ERROR",e.message))))`
	out, _ := dockerExec(t, c, "node", "-e", script)
	if !strings.Contains(out, "429") {
		t.Fatalf("no 429 with eight concurrent calls and limit 6 (main agent and five subagents): %q", out)
	}
	if !strings.Contains(out, "200") {
		t.Fatalf("no call let through: %q", out)
	}
	f := getChat(t, id)
	found := false
	for _, sc := range f.SocketCalls {
		found = found || sc.Op == "agent_limit"
	}
	if !found {
		t.Fatal("limit violation not logged")
	}
	if f.Chat.LLMCalls < 1 {
		t.Fatal("direct call from the sandbox not billed")
	}
}

// Attachments: the uploaded file is attached to the message, the agent knows name and location.
func TestMessageAttachments(t *testing.T) {
	requireE2E(t)
	id := newChat(t, "cli", false)
	s := subscribe(t, id)
	uploadFile(t, id, "numbers.csv", "value\n3\n4\n5\n")
	if code := call(t, "POST", "/api/chats/"+id+"/messages", map[string]any{"text": "x", "attachments": []string{"doesnotexist.csv"}}, nil); code != 400 {
		t.Fatalf("unknown attachment: %d", code)
	}
	from := s.len()
	if code := call(t, "POST", "/api/chats/"+id+"/messages", map[string]any{"text": "What is the sum of the column value in the attachment? Only the number.", "attachments": []string{"numbers.csv"}}, nil); code != 200 {
		t.Fatalf("senden: %d", code)
	}
	s.waitFor(t, from, 3*time.Minute, "agent_settled", func(ev map[string]any) bool { return piType(ev) == "agent_settled" })
	f := getChat(t, id)
	var u struct {
		Content []struct{ Text string } `json:"content"`
	}
	_ = json.Unmarshal(f.Messages[0].Message, &u)
	mustContain(t, u.Content[0].Text, "[Attachments in /workspace/inputs/]\n- numbers.csv", "attachment block in the message")
	mustContain(t, lastAssistantText(t, id), "12", "sum from the attachment")
}

// Typst: the agent uses the skill and the built-in packages without internet.
func TestTypstDocument(t *testing.T) {
	requireE2E(t)
	id := newChat(t, "cli", false)
	s := subscribe(t, id)
	ask(t, s, id, "Use Typst to create a page in /workspace/report.typ with the heading \"Test report\" and a small cetz graphic (circle), compile it with typst compile to report.pdf and give only the size of the PDF in bytes.", nil)
	calls := strings.Join(toolCalls(t, id), "\n")
	mustContain(t, calls, "typst compile", "typst called")
	mustContain(t, calls, "@preview/cetz", "built-in package used")
	c := containerOf(t, id)
	out, err := dockerExec(t, c, "sh", "-c", "test -s /workspace/report.pdf && head -c 5 /workspace/report.pdf")
	if err != nil || !strings.HasPrefix(out, "%PDF") {
		t.Fatalf("no PDF produced: %q %v", out, err)
	}
}

// Display images: the agent draws a PNG with matplotlib and shows it via Markdown. The orchestrator
// saves it to S3 after the reply; it stays retrievable when the chat is idle (sandbox gone).
func TestAgentImageShownAndKept(t *testing.T) {
	requireE2E(t)
	id := newChat(t, "cli", false)
	s := subscribe(t, id)
	ask(t, s, id, "Use matplotlib to draw a line chart of y = x² for x from 0 to 5, save it as "+
		"/workspace/square.png and show it in your reply as a Markdown image. Otherwise only a short sentence.", nil)
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
		t.Fatalf("no Markdown image in the reply: %q", trunc(lastAssistantText(t, id), 400))
	}
	mustContain(t, strings.Join(toolCalls(t, id), "\n"), "savefig", "matplotlib savefig")
	if code := call(t, "POST", "/api/chats/"+id+"/suspend", nil, nil); code != 200 {
		t.Fatalf("let idle: %d", code)
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
		t.Fatalf("image after idling: %d %q (path %s, reply %s)", code, trunc(string(body), 200), path, msg)
	}
	if h.Get("Content-Type") != "image/png" || h.Get("X-Content-Type-Options") != "nosniff" || !strings.HasPrefix(h.Get("Cache-Control"), "private") {
		t.Fatalf("headers: %v", h)
	}
	if code, _, _ := get("/etc/passwd"); code != 400 {
		t.Fatalf("/etc/passwd: %d", code)
	}
	if code, _, _ := get("/workspace/doesnotexist.png"); code != 404 {
		t.Fatalf("unsaved image with an idle chat: %d", code)
	}
	if n := len(getChat(t, id).Artifacts); n != 0 {
		t.Fatalf("display image listed as an artifact: %d", n)
	}
}

// Files in /workspace survive idling: saved after the run, restored in the
// fresh sandbox; /tmp does not.
func TestWorkspaceSurvivesSuspend(t *testing.T) {
	requireE2E(t)
	id := newChat(t, "cli", false)
	s := subscribe(t, id)
	ask(t, s, id, "Do this with bash, without asking back: 1. Use matplotlib to draw a line chart of y = 2x for x from 0 to 4 "+
		"and save it as /workspace/plot.png. 2. Write the word Tanne-77 into the file /workspace/note.txt. "+
		"3. Write the word gone into /tmp/volatile.txt. Then reply only with a short sentence.", nil)
	old := containerOf(t, id)
	if out, err := dockerExec(t, old, "sh", "-c", "head -c 4 /workspace/plot.png | tail -c 3; cat /workspace/note.txt /tmp/volatile.txt"); err != nil ||
		!strings.Contains(out, "PNG") || !strings.Contains(out, "Tanne-77") {
		t.Fatalf("files not created: %q %v", out, err)
	}
	if code := call(t, "POST", "/api/chats/"+id+"/suspend", nil, nil); code != 200 {
		t.Fatalf("idle: %d", code)
	}
	w := getChat(t, id).Chat.Workspace
	if w == nil || w.SavedAt == "" || w.Files < 2 || w.SkippedReason != "" {
		t.Fatalf("workspace not saved: %+v", w)
	}
	ask(t, s, id, "Run ls -la /workspace, repeat the content of /workspace/note.txt verbatim and check with ls "+
		"whether /tmp/volatile.txt still exists. Reply briefly.", nil)
	fresh := containerOf(t, id)
	if fresh == old {
		t.Fatal("not resumed in a fresh sandbox")
	}
	txt := lastAssistantText(t, id)
	mustContain(t, txt, "Tanne-77", "content of note.txt after resuming")
	mustContain(t, txt, "plot.png", "plot.png after resuming")
	out, _ := dockerExec(t, fresh, "sh", "-c", "head -c 4 /workspace/plot.png | tail -c 3; echo; cat /workspace/note.txt; test -e /tmp/volatile.txt && echo TMP-THERE || echo TMP-GONE")
	for _, want := range []string{"PNG", "Tanne-77", "TMP-GONE"} {
		mustContain(t, out, want, "state of the fresh sandbox")
	}
}

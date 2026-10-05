package worker

// Slot test of the communication between the main agent and subagents (pi-subagents): a subagent asks
// the main agent (contact_supervisor), the main agent replies (subagent_supervisor) and steers a
// running subagent (subagent steer). Real pi container, scripted model.

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

// talkSlot creates a slot of the variant cli (backend without web search); done tears it down.
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
		t.Skip("runs only in the Go container with the Docker socket (./dev.sh test)")
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
	// The subagent (in the background) asks, the main agent replies and then steers it while it is still
	// working; the subagent sees both.
	script := strings.Join([]string{
		"Communication.",
		callLine("subagent", map[string]any{"agent": "worker", "async": true,
			"task": "T1\n" + callLine("contact_supervisor", map[string]any{"reason": "need_decision", "message": "Question to the main agent"}) + "\n" +
				callLine("bash", map[string]any{"command": "sleep 6; echo after-reply session=$PI_AGW_SESSION"})}),
		callLine("bash", map[string]any{"command": "sleep 8"}),
		callLine("subagent_supervisor", map[string]any{"action": "reply", "replyTo": "{{replyTo}}", "message": "Reply from the main agent"}),
		callLine("bash", map[string]any{"command": "sleep 2; echo main-session=$PI_AGW_SESSION"}),
		callLine("subagent", map[string]any{"action": "steer", "id": "{{runId}}", "message": "Hint from the main agent"}),
		callLine("bash", map[string]any{"command": "sleep 12"}),
	}, "\n")
	if _, err := w.Call(ctx, map[string]any{"type": "prompt", "message": script}); err != nil {
		t.Fatal(err)
	}
	seen := func(want string) bool {
		for _, s := range fake.Seen() {
			// Scripts (CALL lines) contain the searched texts themselves; only what the model saw as a
			// reply, tool result or injected message counts.
			if strings.Contains(s, want) && !strings.Contains(s, "CALL ") {
				return true
			}
		}
		return false
	}
	for deadline := time.Now().Add(150 * time.Second); time.Now().Before(deadline); time.Sleep(time.Second) {
		if seen("Hint from the main agent") && seen("after-reply") {
			break
		}
	}
	mu.Lock()
	all := strings.Join(log, "\n")
	mu.Unlock()
	if !strings.Contains(all, `"customType":"subagent_supervisor_request"`) || !strings.Contains(all, "Question to the main agent") {
		t.Errorf("the subagent's question did not reach the main agent")
	}
	if !seen("Reply from the main agent") {
		t.Errorf("the main agent's reply did not reach the subagent")
	}
	if !seen("Hint from the main agent") {
		t.Errorf("steering of the running subagent did not arrive")
	}
	// Session for agw-artifact/agw-internet: main agent "main", subagent its run ID.
	if !seen("main-session=main") {
		t.Errorf("PI_AGW_SESSION of the main agent is not \"main\"")
	}
	subSess := regexp.MustCompile(`session=[0-9a-f-]{8,}`)
	found := false
	for _, s := range fake.Seen() {
		if !strings.Contains(s, "CALL ") && strings.Contains(s, "after-reply") && subSess.MatchString(s) {
			found = true
		}
	}
	if !found {
		t.Errorf("PI_AGW_SESSION of the subagent is not the run ID")
	}
	for _, is := range fake.Issued() {
		if strings.Contains(is.Args, "{{") || (is.Tool == "subagent_supervisor" && !strings.Contains(is.Args, "-")) {
			t.Errorf("placeholder not replaced: %s %s", is.Tool, is.Args)
		}
	}
	if t.Failed() {
		for _, l := range log {
			t.Logf("%.600s", l)
		}
	}
	_ = settled
}

// Subagents talk to each other directly (pi-intercom): A finds B in the list and sends it a
// message; B gets to see it while working, without a detour through the main agent.
func TestSlotSubagentIntercom(t *testing.T) {
	if os.Getenv("AGW_E9_IN_DOCKER") != "1" {
		t.Skip("runs only in the Go container with the Docker socket (./dev.sh test)")
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
			callLine("intercom", map[string]any{"action": "send", "to": "{{peer}}", "message": "Hello B, this is A"})}),
		callLine("subagent", map[string]any{"agent": "worker", "async": true, "task": "IB\n" +
			callLine("bash", map[string]any{"command": "sleep 12"}) + "\n" +
			callLine("bash", map[string]any{"command": "echo b-continues"})}),
		callLine("bash", map[string]any{"command": "sleep 25"}),
	}, "\n")
	if _, err := w.Call(ctx, map[string]any{"type": "prompt", "message": script}); err != nil {
		t.Fatal(err)
	}
	// B sees the message as an incoming message from A (header "From …"), not just A its own sending.
	got := func() bool {
		for _, s := range fake.Seen() {
			// Header "**From <sender>**" from pi-intercom (A's session name contains its task).
			if strings.HasPrefix(s, "**From ") && strings.Contains(s, "Hello B, this is A") {
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
		t.Fatal("message from A did not reach B")
	}
	for _, is := range fake.Issued() {
		if is.Tool == "intercom" && strings.Contains(is.Args, "send") && !strings.Contains(is.Args, "subagent-worker-") {
			t.Errorf("target not resolved: %s", is.Args)
		}
	}
}

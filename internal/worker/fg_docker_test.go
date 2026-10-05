package worker

// Slot test for stopping and converting a running foreground command by the user: real
// pi container, real execution sandbox, real exec-bridge.ts (pattern TestSlotBackground…).

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"agw/internal/config"
	"agw/internal/sandbox"
	"agw/internal/store"
)

func TestSlotForegroundControl(t *testing.T) {
	if os.Getenv("AGW_E9_IN_DOCKER") != "1" {
		t.Skip("runs only in the Go container with the Docker socket (./dev.sh test)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
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
		SandboxMemoryMB: 1536, SandboxCPUs: 1, SandboxPids: 256, ExecMemoryMB: 1024, ExecCPUs: 1, ExecPids: 256, BgMax: 3,
	}
	fac, err := NewFactory(rt, cat, env)
	if err != nil {
		t.Fatal(err)
	}
	b := &bgBackend{ended: map[string]store.BackgroundTask{}, notify: map[string]bool{}}
	fac.Backend = b
	slot := fmt.Sprintf("t-fg-%d", time.Now().UnixNano()%1e8)
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
	// The user steps in as soon as a command runs: "background-me" moves it to the background, "stop-me" stops it.
	go func() {
		for ev := range w.Events() {
			switch ev.Type {
			case "tool_execution_start":
				var e struct {
					ToolCallID string `json:"toolCallId"`
					Args       struct {
						Command string `json:"command"`
					} `json:"args"`
				}
				_ = json.Unmarshal(ev.Raw, &e)
				go func(id, cmd string) {
					time.Sleep(1500 * time.Millisecond)
					switch {
					case strings.Contains(cmd, "background-me"):
						if _, err := w.BackgroundForeground(ctx, "chat-e9", id); err != nil {
							t.Errorf("convert: %v", err)
						}
					case strings.Contains(cmd, "stop-me"):
						if err := w.StopForeground("chat-e9", id); err != nil {
							t.Errorf("stop: %v", err)
						}
					}
				}(e.ToolCallID, e.Args.Command)
			case "tool_execution_end":
				var e struct {
					ToolCallID string `json:"toolCallId"`
					IsError    bool   `json:"isError"`
					Result     struct {
						Content []struct {
							Text string `json:"text"`
						} `json:"content"`
					} `json:"result"`
				}
				_ = json.Unmarshal(ev.Raw, &e)
				var txt strings.Builder
				for _, c := range e.Result.Content {
					txt.WriteString(c.Text)
				}
				mu.Lock()
				ends[e.ToolCallID] = end{e.IsError, txt.String()}
				mu.Unlock()
			case "agent_settled":
				select {
				case settled <- struct{}{}:
				default:
				}
			}
		}
	}()
	script := strings.Join([]string{
		"Control foreground commands.",
		callLine("bash", map[string]any{"command": "echo before; sleep 4; echo after # background-me", "timeout": 3}),
		callLine("bash", map[string]any{"command": "echo a; sleep 60 # stop-me"}),
		callLine("bash", map[string]any{"command": "sleep 5", "timeout": 1}),
	}, "\n")
	if _, err := w.Call(ctx, map[string]any{"type": "prompt", "message": script}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-settled:
	case <-ctx.Done():
		t.Fatal("run does not finish")
	}
	var bg store.BackgroundTask
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		if x, ok := b.endedTask("bg-1"); ok {
			bg = x
			break
		}
	}
	result := func(contains string) end {
		for _, is := range fake.Issued() {
			if is.Tool == "bash" && strings.Contains(is.Args, contains) {
				mu.Lock()
				defer mu.Unlock()
				return ends[is.ID]
			}
		}
		t.Fatalf("not requested: %s", contains)
		return end{}
	}
	if r := result("background-me"); r.isError || !strings.Contains(r.text, "before") || !strings.Contains(r.text, "moved this command to the background as bg-1") {
		t.Errorf("conversion: %+v", r)
	}
	// The timeout (3 s) no longer applies after the conversion: the command ends normally after 4 s.
	if bg.State != store.BgExited || bg.ExitCode == nil || *bg.ExitCode != 0 || !strings.Contains(bg.OutputExcerpt, "after") || !b.notify["bg-1"] {
		t.Errorf("converted task: %+v", bg)
	}
	if r := result("stop-me"); !r.isError || !strings.Contains(r.text, "a\n") || !strings.Contains(r.text, "Command stopped by the user") {
		t.Errorf("stop: %+v", r)
	}
	if r := result("sleep 5"); !r.isError || !strings.Contains(r.text, "Command timed out after 1 seconds") {
		t.Errorf("timeout: %+v", r)
	}
}

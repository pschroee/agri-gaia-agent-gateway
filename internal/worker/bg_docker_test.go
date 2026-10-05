package worker

// Slot test of the background tasks with a scripted model (pattern TestSlotE9WithScriptedModel):
// real pi container, real execution sandbox, real exec-bridge.ts. The backend plays the
// manager (bgtask.Notifier) and records start and end. Runs in the Go container (./dev.sh test).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agw/internal/config"
	"agw/internal/execproto"
	"agw/internal/fakellm"
	"agw/internal/sandbox"
	"agw/internal/store"
)

// The scripted model listens once per test process on :18481; every slot test installs its
// own scripted model (two ListenAndServe on the same port would make the second fail silently,
// and the second test would talk to the model of the first).
var (
	llmOnce sync.Once
	llmCur  atomic.Pointer[fakellm.Server]
)

func useFakeLLM() *fakellm.Server {
	f := &fakellm.Server{}
	llmCur.Store(f)
	llmOnce.Do(func() {
		go func() {
			_ = http.ListenAndServe(":18481", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { llmCur.Load().ServeHTTP(w, r) }))
		}()
	})
	return f
}

// bgBackend: e9Backend plus the notifier of the background tasks.
type bgBackend struct {
	e9Backend
	bmu     sync.Mutex
	seq     int
	created []store.BackgroundTask
	ended   map[string]store.BackgroundTask
	notify  map[string]bool
}

func (b *bgBackend) BackgroundCreate(_ context.Context, t store.BackgroundTask) (store.BackgroundTask, error) {
	b.bmu.Lock()
	defer b.bmu.Unlock()
	b.seq++
	t.Seq, t.ID, t.State, t.StartedAt, t.LogPath = b.seq, store.BgID(b.seq), store.BgRunning, time.Now(), execproto.BgLogPath(b.seq)
	b.created = append(b.created, t)
	return t, nil
}
func (b *bgBackend) BackgroundProgress(store.BackgroundTask) {}
func (b *bgBackend) BackgroundEnded(t store.BackgroundTask, notify bool) {
	b.bmu.Lock()
	defer b.bmu.Unlock()
	b.ended[t.ID], b.notify[t.ID] = t, notify
}
func (b *bgBackend) BackgroundLookup(context.Context, string, int) (store.BackgroundTask, error) {
	return store.BackgroundTask{}, store.ErrNotFound
}
func (b *bgBackend) endedTask(id string) (store.BackgroundTask, bool) {
	b.bmu.Lock()
	defer b.bmu.Unlock()
	t, ok := b.ended[id]
	return t, ok
}

func TestSlotBackgroundWithScriptedModel(t *testing.T) {
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
	slot := fmt.Sprintf("t-bg-%d", time.Now().UnixNano()%1e8)
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
		"Check background tasks.",
		callLine("bash", map[string]any{"command": "sleep 2; echo done-bg", "run_in_background": true}),
		callLine("bg_output", map[string]any{"id": "bg-1"}),
		callLine("bash", map[string]any{"command": "sleep 300 & sleep 301", "run_in_background": true}),
		callLine("bg_stop", map[string]any{"id": "bg-2"}),
		callLine("bg_output", map[string]any{"id": "bg-9"}),
		callLine("bash", map[string]any{"command": "echo x", "run_in_background": true, "timeout": -1}),
		callLine("subagent", map[string]any{"agent": "worker", "async": false,
			"task": "S1\n" + callLine("bash", map[string]any{"command": "sleep 1; echo sub-bg", "run_in_background": true}) + "\n" + callLine("bg_output", map[string]any{"id": "bg-3"})}),
		callLine("bash", map[string]any{"command": "sleep 3; pgrep -f 'sleep 30[01]' || echo none; cat /tmp/agw-bg/bg-1.log"}),
	}, "\n")
	if _, err := w.Call(ctx, map[string]any{"type": "prompt", "message": script}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-settled:
	case <-ctx.Done():
		t.Fatal("run does not finish")
	}
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		if _, ok := b.endedTask("bg-3"); ok {
			break
		}
	}
	issued := fake.Issued()
	find := func(tool, contains string) fakellm.Issued {
		for _, is := range issued {
			if is.Tool == tool && strings.Contains(is.Args, contains) {
				return is
			}
		}
		t.Fatalf("not requested: %s %s", tool, contains)
		return fakellm.Issued{}
	}
	result := func(is fakellm.Issued) end {
		mu.Lock()
		defer mu.Unlock()
		return ends[is.ID]
	}
	// Start returns immediately and names the ID, the file and the tools.
	start1 := find("bash", "done-bg")
	if r := result(start1); r.isError || !strings.Contains(r.text, "Background task bg-1 started") || !strings.Contains(r.text, "/tmp/agw-bg/bg-1.log") || !strings.Contains(r.text, "do not poll") {
		t.Errorf("Start: %+v", r)
	}
	if r := result(find("bg_output", `"bg-1"`)); r.isError || !strings.Contains(r.text, "bg-1 is running") {
		t.Errorf("bg_output while running: %+v", r)
	}
	if r := result(find("bg_stop", `"bg-2"`)); r.isError || !strings.Contains(r.text, "bg-2 was stopped") {
		t.Errorf("bg_stop: %+v", r)
	}
	if r := result(find("bg_output", `"bg-9"`)); !r.isError || !strings.Contains(r.text, "unknown background task") {
		t.Errorf("unknown task: %+v", r)
	}
	if r := result(find("bash", `"timeout":-1`)); !r.isError || !strings.Contains(r.text, "Invalid timeout") {
		t.Errorf("invalid timeout: %+v", r)
	}
	// Subagent: run_in_background and bg_output (via agentOverrides) work there too. Its
	// tool results are not in the RPC stream of the main session; proven via the log.
	execs0 := b.executions()
	if e := execFor(execs0, find("bash", "sub-bg").ID); e == nil || e.Op != "bg_start" || !strings.Contains(e.OutputExcerpt, `"id":"bg-3"`) {
		t.Errorf("start in the subagent: %+v", e)
	}
	if e := execFor(execs0, find("bg_output", `"bg-3"`).ID); e == nil || e.Op != "bg_output" || e.Error != "" {
		t.Errorf("bg_output in the subagent (tool not active?): %+v", e)
	}
	// After the stop the whole group is gone; the output file of bg-1 is in the sandbox.
	if r := result(find("bash", "pgrep")); !strings.Contains(r.text, "none") || !strings.Contains(r.text, "done-bg") {
		t.Errorf("after the stop: %+v", r)
	}
	// End: bg-1 with exit 0 and output, bg-2 stopped by the agent, bg-3 from the subagent.
	if e, ok := b.endedTask("bg-1"); !ok || e.State != store.BgExited || *e.ExitCode != 0 || e.Tail != "done-bg\n" || e.OutputSHA256 == "" || !b.notify["bg-1"] {
		t.Errorf("end bg-1: %+v", e)
	}
	if e, ok := b.endedTask("bg-2"); !ok || e.State != store.BgStopped || e.StoppedBy != "agent" {
		t.Errorf("end bg-2: %+v", e)
	}
	e3, ok := b.endedTask("bg-3")
	if !ok || e3.State != store.BgExited || e3.Session == "main" || !strings.Contains(e3.Tail, "sub-bg") {
		t.Errorf("end bg-3: %+v", e3)
	}
	// Log: start, retrieval and stop recorded, with tool and operation.
	execs := b.executions()
	for _, c := range []struct {
		is       fakellm.Issued
		tool, op string
	}{
		{start1, "bash", "bg_start"}, {find("bg_output", `"bg-1"`), "bg_output", "bg_output"}, {find("bg_stop", `"bg-2"`), "bg_stop", "bg_stop"},
		{find("bash", "sub-bg"), "bash", "bg_start"},
	} {
		e := execFor(execs, c.is.ID)
		if e == nil || e.Tool != c.tool || e.Op != c.op {
			t.Errorf("log for %s: %+v", c.is.Args, e)
		}
	}
	if e := execFor(execs, find("bash", "sub-bg").ID); e != nil && e.Session == "main" {
		t.Errorf("start in the subagent logged under main")
	}
	t.Logf("%d tasks created, %d ended, %d executions logged", len(b.created), len(b.ended), len(execs))
}

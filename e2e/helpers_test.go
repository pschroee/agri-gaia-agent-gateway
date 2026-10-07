// Package e2e tests the running stack end to end with the real model.
// Start with ./dev.sh e2e (sets AGW_E2E=1 and a low compaction
// threshold, so that auto-compaction can be tested).
package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

var base = func() string {
	if u := os.Getenv("AGW_URL"); u != "" {
		return u
	}
	return "http://127.0.0.1:18480"
}()

func auth(r *http.Request) { r.Header.Set("Authorization", "Bearer "+os.Getenv("AGW_API_TOKEN")) }

func requireE2E(t *testing.T) {
	t.Helper()
	if os.Getenv("AGW_E2E") != "1" {
		t.Skip("set AGW_E2E=1 (./dev.sh e2e)")
	}
}

type chatView struct {
	ID          string          `json:"id"`
	State       string          `json:"state"`
	Internet    bool            `json:"internet"`
	AutoCompact bool            `json:"auto_compact"`
	Compactions int             `json:"compactions"`
	Running     bool            `json:"running"`
	SlotID      string          `json:"slot_id"`
	Cost        float64         `json:"cost"`
	Context     json.RawMessage `json:"context"`
	MaxSub      int             `json:"max_subagents"`
	SubRunning  int             `json:"subagents_running"`
	Subagents   int             `json:"subagents"`
	LLMCalls    int             `json:"llm_calls"`
	CostOther   float64         `json:"cost_other"`
	Workspace   *struct {
		Size          int64  `json:"size"`
		Files         int    `json:"files"`
		SavedAt       string `json:"saved_at"`
		SkippedReason string `json:"skipped_reason"`
	} `json:"workspace"`
	Tokens struct {
		Input, Output, Total int64
		CacheRead            int64 `json:"cache_read"`
	} `json:"tokens"`
}

type message struct {
	Seq     int             `json:"seq"`
	Role    string          `json:"role"`
	Message json.RawMessage `json:"message"`
	Cost    *float64        `json:"cost"`
	Peak    *bool           `json:"peak"`
	// Review 3 (H1): turn, trigger, origin and parts.
	TurnID  *int64 `json:"turn_id"`
	Trigger string `json:"trigger"`
	Origin  string `json:"origin"`
	Sources []struct {
		Kind    string   `json:"kind"`
		Type    string   `json:"type"`
		Refs    []string `json:"refs"`
		QueueID string   `json:"queue_id"`
		Marker  string   `json:"marker"`
	} `json:"sources"`
}

type approval struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	Via   string `json:"via"`
	Name  string `json:"name"`
	State string `json:"state"`
}

type fullChat struct {
	Chat        chatView                           `json:"chat"`
	Messages    []message                          `json:"messages"`
	Artifacts   []struct{ Kind, Name, Via string } `json:"artifacts"`
	Approvals   []approval                         `json:"approvals"`
	SocketCalls []struct{ Via, Op, Result string } `json:"socket_calls"`
	Subagent    []struct {
		RunID     string `json:"run_id"`
		Agent     string `json:"agent"`
		Kind      string `json:"kind"`
		Confirmed bool   `json:"confirmed"`
		Payload   struct {
			Name, Arguments, Text string
			ID                    string `json:"id"`
		} `json:"payload"`
	} `json:"subagent_entries"`
}

func call(t *testing.T, method, path string, body any, out any) int {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, base+path, r)
	req.Header.Set("Content-Type", "application/json")
	auth(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if out != nil && resp.StatusCode < 300 {
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("%s %s: unreadable response: %s", method, path, raw)
		}
	}
	if resp.StatusCode >= 300 {
		t.Logf("%s %s → %d %s", method, path, resp.StatusCode, raw)
	}
	return resp.StatusCode
}

// requireToolsets skips the test unless the gateway gives new chats the combination of variant.
func requireToolsets(t *testing.T, variant string) {
	t.Helper()
	var cfg struct {
		Toolsets struct {
			ID string `json:"id"`
		} `json:"toolsets"`
	}
	if call(t, "GET", "/api/config", nil, &cfg) != http.StatusOK {
		t.Fatal("GET /api/config failed")
	}
	if cfg.Toolsets.ID != variant {
		t.Skipf("needs AGW_TOOLSETS=%s, the gateway runs with %q", variant, cfg.Toolsets.ID)
	}
}

func newChat(t *testing.T, variant string, internet bool) string {
	return newChatWith(t, map[string]any{"variant": variant, "internet": internet})
}

func newChatWith(t *testing.T, req map[string]any) string {
	t.Helper()
	// Since issue #29 the gateway fixes the bindings (AGW_TOOLSETS); a test written for another
	// combination is skipped, e.g. AGW_TOOLSETS=mcp ./dev.sh e2e -run MCP.
	if v, ok := req["variant"].(string); ok && v != "" {
		requireToolsets(t, v)
		delete(req, "variant")
	}
	req["title"] = "E2E " + t.Name()
	var c chatView
	var code int
	for i := 0; i < 30; i++ { // the pool refills; wait briefly instead of failing
		code = call(t, "POST", "/api/chats", req, &c)
		if code != http.StatusServiceUnavailable {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if code != http.StatusCreated {
		t.Fatalf("create chat: %d", code)
	}
	t.Cleanup(func() { releaseChat(t, c.ID) })
	return c.ID
}

// releaseChat frees the chat's sandbox: abort, then idle. Closing no longer exists;
// idling fails with 409 as long as the aborted reply has not finished.
func releaseChat(t *testing.T, id string) {
	call(t, "POST", "/api/chats/"+id+"/abort", nil, nil)
	for i := 0; i < 20; i++ {
		if call(t, "POST", "/api/chats/"+id+"/suspend", nil, nil) != http.StatusConflict {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func getChat(t *testing.T, id string) fullChat {
	t.Helper()
	var f fullChat
	if code := call(t, "GET", "/api/chats/"+id, nil, &f); code != 200 {
		t.Fatalf("read chat: %d", code)
	}
	return f
}

// stream reads a chat's SSE events into a list.
type stream struct {
	mu     sync.Mutex
	events []map[string]any
	cancel context.CancelFunc
}

func subscribe(t *testing.T, id string) *stream {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", base+"/api/chats/"+id+"/events", nil)
	auth(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	s := &stream{cancel: cancel}
	go func() {
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1<<20), 64<<20)
		for sc.Scan() {
			line := sc.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var ev map[string]any
			if json.Unmarshal([]byte(line[6:]), &ev) == nil {
				s.mu.Lock()
				s.events = append(s.events, ev)
				s.mu.Unlock()
			}
		}
	}()
	t.Cleanup(cancel)
	time.Sleep(200 * time.Millisecond) // subscription is in place before sending
	return s
}

func piType(ev map[string]any) string {
	if ev["kind"] != "pi" {
		return ""
	}
	d, _ := ev["data"].(map[string]any)
	s, _ := d["type"].(string)
	return s
}

// waitFor waits until an event from index from onwards satisfies the condition.
func (s *stream) waitFor(t *testing.T, from int, timeout time.Duration, what string, cond func(map[string]any) bool) (map[string]any, int) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		for i := from; i < len(s.events); i++ {
			if cond(s.events[i]) {
				ev := s.events[i]
				s.mu.Unlock()
				return ev, i + 1
			}
		}
		s.mu.Unlock()
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timeout: %s", what)
	return nil, 0
}

func (s *stream) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.events)
}

func (s *stream) count(cond func(map[string]any) bool) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, ev := range s.events {
		if cond(ev) {
			n++
		}
	}
	return n
}

// ask sends a message and waits for the end of the turn. Pending
// approvals are decided with decide (nil: leave them alone).
func ask(t *testing.T, s *stream, id, text string, decide func(approval) bool) {
	t.Helper()
	from := s.len()
	if code := call(t, "POST", "/api/chats/"+id+"/messages", map[string]string{"text": text}, nil); code != 200 {
		t.Fatalf("send: %d", code)
	}
	deadline := time.Now().Add(6 * time.Minute)
	started := false
	seen := map[string]bool{}
	for time.Now().Before(deadline) {
		s.mu.Lock()
		evs := append([]map[string]any(nil), s.events[from:]...)
		s.mu.Unlock()
		for _, ev := range evs {
			switch {
			case piType(ev) == "agent_start":
				started = true
			case piType(ev) == "agent_settled" && started:
				return
			case ev["kind"] == "approval" && decide != nil:
				b, _ := json.Marshal(ev["data"])
				var a approval
				_ = json.Unmarshal(b, &a)
				if a.State == "pending" && !seen[a.ID] {
					seen[a.ID] = true
					call(t, "POST", "/api/approvals/"+a.ID, map[string]bool{"approve": decide(a)}, nil)
				}
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("reply to %q did not arrive", text)
}

// lastAssistantText returns the text of the last reply.
func lastAssistantText(t *testing.T, id string) string {
	t.Helper()
	f := getChat(t, id)
	for i := len(f.Messages) - 1; i >= 0; i-- {
		if f.Messages[i].Role != "assistant" {
			continue
		}
		var m struct {
			Content []struct{ Type, Text string } `json:"content"`
		}
		_ = json.Unmarshal(f.Messages[i].Message, &m)
		var b strings.Builder
		for _, c := range m.Content {
			if c.Type == "text" {
				b.WriteString(c.Text)
			}
		}
		if b.Len() > 0 {
			return b.String()
		}
	}
	return ""
}

func toolCalls(t *testing.T, id string) []string {
	t.Helper()
	var out []string
	for _, m := range getChat(t, id).Messages {
		if m.Role != "assistant" {
			continue
		}
		var a struct {
			Content []struct {
				Type, Name string
				Arguments  json.RawMessage
			} `json:"content"`
		}
		_ = json.Unmarshal(m.Message, &a)
		for _, c := range a.Content {
			if c.Type == "toolCall" {
				out = append(out, c.Name+" "+string(c.Arguments))
			}
		}
	}
	return out
}

func uploadFile(t *testing.T, id, name, content string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", name)
	io.WriteString(fw, content)
	mw.Close()
	req, _ := http.NewRequest("POST", base+"/api/chats/"+id+"/files", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	auth(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 201 {
		t.Fatalf("Upload: %v %v", err, resp)
	}
	resp.Body.Close()
}

func dockerExec(t *testing.T, container string, args ...string) (string, error) {
	t.Helper()
	out, err := exec.Command("docker", append([]string{"exec", container}, args...)...).CombinedOutput()
	return string(out), err
}

func containerOf(t *testing.T, id string) string {
	t.Helper()
	c := getChat(t, id).Chat
	if c.SlotID == "" {
		t.Fatal("chat has no slot")
	}
	return "agwpoc-" + c.SlotID
}

func mustContain(t *testing.T, s, sub, what string) {
	t.Helper()
	if !strings.Contains(strings.ToLower(s), strings.ToLower(sub)) {
		t.Fatalf("%s: %q missing in %q", what, sub, trunc(s, 600))
	}
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n] + fmt.Sprintf(" … (%d characters)", len(s))
	}
	return s
}

// piContainerOf names pi's container (E9); containerOf the execution sandbox in which
// the agent's tools run.
func piContainerOf(t *testing.T, id string) string {
	return containerOf(t, id) + "-pi"
}

// nodeFetch sends an HTTP request with Node from pi's container (no shell, no curl)
// and returns status and body. Only pi's container reaches the LLM proxy.
func nodeFetch(t *testing.T, container, method, url, body string) string {
	t.Helper()
	js := `const [m,u,b]=process.argv.slice(1);fetch(u,{method:m,headers:{"content-type":"application/json"},body:m==="GET"?undefined:b,signal:AbortSignal.timeout(8000)}).then(async r=>console.log(r.status,await r.text())).catch(e=>console.log("ERROR",e.cause?.code||e.message))`
	out, _ := dockerExec(t, container, "node", "-e", js, method, url, body)
	return out
}

type reconciled struct {
	ToolCallID string   `json:"tool_call_id"`
	State      string   `json:"state"`
	Tool       string   `json:"tool"`
	Session    string   `json:"session"`
	Main       bool     `json:"main"`
	Ops        []string `json:"ops"`
	Error      string   `json:"error"`
}

type toolExecs struct {
	Calls      []reconciled   `json:"calls"`
	Summary    map[string]int `json:"summary"`
	Executions []struct {
		ToolCallID    string         `json:"tool_call_id"`
		Tool          string         `json:"tool"`
		Op            string         `json:"op"`
		Session       string         `json:"session"`
		Args          map[string]any `json:"args"`
		ExitCode      *int           `json:"exit_code"`
		Error         string         `json:"error"`
		OutputExcerpt string         `json:"output_excerpt"`
		StartedAt     time.Time      `json:"started_at"`
		DurationMs    int64          `json:"duration_ms"`
	} `json:"executions"`
}

func getToolExecs(t *testing.T, id string) toolExecs {
	t.Helper()
	var r toolExecs
	if code := call(t, "GET", "/api/chats/"+id+"/tool_executions", nil, &r); code != 200 {
		t.Fatalf("tool_executions: %d", code)
	}
	return r
}

// requireNoFlagged checks the reconciliation: nothing unexecuted, unrequested or mismatching.
func requireNoFlagged(t *testing.T, r toolExecs) {
	t.Helper()
	for _, c := range r.Calls {
		if c.State == "unexecuted" || c.State == "unrequested" || c.State == "mismatch" {
			t.Errorf("reconciliation suspicious: %+v", c)
		}
	}
}

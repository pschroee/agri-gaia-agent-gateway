package sock

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agw/internal/execproto"
)

// stepRunner plays a running bash command: prints "before", waits for more (then "after")
// and ends with exit 0 on end or with "aborted" as soon as the context ends.
type stepRunner struct {
	mu   sync.Mutex
	req  execproto.Request
	more chan struct{}
	end  chan struct{}
}

func (r *stepRunner) Run(ctx context.Context, req execproto.Request, onData func([]byte)) (execproto.Frame, error) {
	r.mu.Lock()
	r.req = req
	r.mu.Unlock()
	onData([]byte("before\n"))
	select {
	case <-r.more:
		onData([]byte("after\n"))
	case <-ctx.Done():
		return execproto.Frame{Done: true, Error: "aborted", Code: "aborted", FullOutputPath: req.Spill}, nil
	}
	select {
	case <-r.end:
		code := 0
		return execproto.Frame{Done: true, Exit: &code}, nil
	case <-ctx.Done():
		return execproto.Frame{Done: true, Error: "aborted", Code: "aborted"}, nil
	}
}

func startFg(t *testing.T, run ToolRunner, bg BackgroundTasks, fg *Foreground) (*http.Client, *fakeRecorder) {
	t.Helper()
	dir, err := os.MkdirTemp("", "agwsockfg")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	rec := &fakeRecorder{}
	srv, err := Listen(dir, NewPiHandlerFg("p-test", &fakeBackend{chat: "chat-1"}, 1<<20, run, rec, bg, fg))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(dir, SocketName))
	}}, Timeout: 10 * time.Second}, rec
}

// bashStream starts bash and returns the NDJSON lines; the first one has already been read.
func bashStream(t *testing.T, c *http.Client, id string, timeout float64) (first string, rest func() []string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"toolCallId": id, "tool": "bash", "req": map[string]any{"op": "bash", "command": "long", "cwd": "/workspace", "timeout": timeout}})
	resp, err := c.Post("http://agw/tool/bash", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(resp.Body)
	first, _ = br.ReadString('\n')
	return first, func() []string {
		defer resp.Body.Close()
		var out []string
		for {
			l, err := br.ReadString('\n')
			if l != "" {
				out = append(out, strings.TrimSpace(l))
			}
			if err != nil {
				return out
			}
		}
	}
}

func lastFrame(t *testing.T, lines []string) execproto.Frame {
	t.Helper()
	var f execproto.Frame
	if len(lines) == 0 || json.Unmarshal([]byte(lines[len(lines)-1]), &f) != nil || !f.Done {
		t.Fatalf("no final frame: %q", lines)
	}
	return f
}

func TestForegroundStop(t *testing.T) {
	run := &stepRunner{more: make(chan struct{}), end: make(chan struct{})}
	fg := NewForeground()
	c, rec := startFg(t, run, &fakeBg{}, fg)
	first, rest := bashStream(t, c, "call_s", 0)
	if !strings.Contains(first, base64.StdEncoding.EncodeToString([]byte("before\n"))) {
		t.Fatalf("first line: %q", first)
	}
	if got := fg.Running("chat-1"); len(got) != 1 || got[0] != "call_s" {
		t.Fatalf("running: %v", got)
	}
	if err := fg.Stop("other", "call_s"); err != ErrNoForeground {
		t.Fatalf("foreign chat: %v", err)
	}
	if err := fg.Stop("chat-1", "call_s"); err != nil {
		t.Fatal(err)
	}
	f := lastFrame(t, rest())
	if f.Code != "stopped" || f.Error != "Command stopped by the user" {
		t.Fatalf("final frame: %+v", f)
	}
	if r := rec.all(); len(r) != 1 || !strings.Contains(r[0].Error, "stopped") || r[0].OutputExcerpt != "before\n" {
		t.Fatalf("log: %+v", r)
	}
	if len(fg.Running("chat-1")) != 0 {
		t.Fatal("still in the registry")
	}
}

func TestForegroundToBackground(t *testing.T) {
	run := &stepRunner{more: make(chan struct{}), end: make(chan struct{})}
	fg, bg := NewForeground(), &fakeBg{}
	c, rec := startFg(t, run, bg, fg)
	_, rest := bashStream(t, c, "call_b", 0.3)
	task, err := fg.Background(context.Background(), "chat-1", "call_b")
	if err != nil || task.ID != "bg-7" {
		t.Fatalf("conversion: %+v %v", task, err)
	}
	f := lastFrame(t, rest())
	if f.Code != "backgrounded" || f.Background != "bg-7" || f.Error != "" {
		t.Fatalf("final frame to pi: %+v", f)
	}
	if run.req.Timeout != 0 {
		t.Fatalf("timeout passed to agw-exec: %v", run.req.Timeout)
	}
	if run.req.Env[ToolCallEnv] != "call_b" || run.req.Env[SessionEnv] == "" {
		t.Fatalf("environment for agw-artifact: %v", run.req.Env)
	}
	// The timeout (0.3 s) no longer applies after the conversion; further output goes to the task.
	time.Sleep(500 * time.Millisecond)
	close(run.more)
	close(run.end)
	deadline := time.Now().Add(3 * time.Second)
	for {
		bg.mu.Lock()
		done, out, logPath := bg.finished, string(bg.adoptedOut), bg.adoptLog
		bg.mu.Unlock()
		if done != nil {
			if done.Exit == nil || *done.Exit != 0 || out != "before\nafter\n" || logPath != execproto.SpillPath("call_b") {
				t.Fatalf("task: %+v %q %q", done, out, logPath)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("task not ended")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if r := rec.all(); len(r) != 1 || !strings.Contains(r[0].Error, "bg-7") {
		t.Fatalf("log: %+v", r)
	}
	if _, err := fg.Background(context.Background(), "chat-1", "call_b"); err != ErrNoForeground {
		t.Fatalf("second conversion: %v", err)
	}
}

func TestForegroundTimeoutByOrchestrator(t *testing.T) {
	run := &stepRunner{more: make(chan struct{}), end: make(chan struct{})}
	c, _ := startFg(t, run, &fakeBg{}, NewForeground())
	_, rest := bashStream(t, c, "call_t", 0.2)
	if f := lastFrame(t, rest()); f.Code != "timeout" || f.Error != "timeout" {
		t.Fatalf("timeout: %+v", f)
	}
}

// A second operation with the same ID does not take control away from the first; user_bash is
// not registered at all.
func TestForegroundDuplicateID(t *testing.T) {
	fg := NewForeground()
	var first, second bool
	a := fg.add("call_x", "chat-1", func() { first = true })
	fg.add("call_x", "chat-1", func() { second = true })
	fg.add("user_bash", "chat-1", func() {})
	if got := fg.Running("chat-1"); len(got) != 1 || got[0] != "call_x" {
		t.Fatalf("registry: %v", got)
	}
	if err := fg.Stop("chat-1", "call_x"); err != nil || !first || second {
		t.Fatalf("stop hit the wrong operation: %v first=%v second=%v", err, first, second)
	}
	fg.remove("call_x", a)
	if len(fg.Running("chat-1")) != 0 {
		t.Fatal("not removed")
	}
}

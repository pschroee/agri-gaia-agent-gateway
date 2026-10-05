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

// stepRunner spielt einen laufenden bash-Befehl: gibt „vorher“ aus, wartet auf more (dann „nachher“)
// und endet mit Exit 0 bei end oder mit „aborted“, sobald der Kontext endet.
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
	onData([]byte("vorher\n"))
	select {
	case <-r.more:
		onData([]byte("nachher\n"))
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

// bashStream startet bash und liefert die NDJSON-Zeilen; die erste ist schon gelesen.
func bashStream(t *testing.T, c *http.Client, id string, timeout float64) (first string, rest func() []string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"toolCallId": id, "tool": "bash", "req": map[string]any{"op": "bash", "command": "lange", "cwd": "/workspace", "timeout": timeout}})
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
		t.Fatalf("kein Abschluss: %q", lines)
	}
	return f
}

func TestForegroundStop(t *testing.T) {
	run := &stepRunner{more: make(chan struct{}), end: make(chan struct{})}
	fg := NewForeground()
	c, rec := startFg(t, run, &fakeBg{}, fg)
	first, rest := bashStream(t, c, "call_s", 0)
	if !strings.Contains(first, base64.StdEncoding.EncodeToString([]byte("vorher\n"))) {
		t.Fatalf("erste Zeile: %q", first)
	}
	if got := fg.Running("chat-1"); len(got) != 1 || got[0] != "call_s" {
		t.Fatalf("laufend: %v", got)
	}
	if err := fg.Stop("fremd", "call_s"); err != ErrNoForeground {
		t.Fatalf("fremder Chat: %v", err)
	}
	if err := fg.Stop("chat-1", "call_s"); err != nil {
		t.Fatal(err)
	}
	f := lastFrame(t, rest())
	if f.Code != "stopped" || f.Error != "Command stopped by the user" {
		t.Fatalf("Abschluss: %+v", f)
	}
	if r := rec.all(); len(r) != 1 || !strings.Contains(r[0].Error, "stopped") || r[0].OutputExcerpt != "vorher\n" {
		t.Fatalf("Protokoll: %+v", r)
	}
	if len(fg.Running("chat-1")) != 0 {
		t.Fatal("noch im Register")
	}
}

func TestForegroundToBackground(t *testing.T) {
	run := &stepRunner{more: make(chan struct{}), end: make(chan struct{})}
	fg, bg := NewForeground(), &fakeBg{}
	c, rec := startFg(t, run, bg, fg)
	_, rest := bashStream(t, c, "call_b", 0.3)
	task, err := fg.Background(context.Background(), "chat-1", "call_b")
	if err != nil || task.ID != "bg-7" {
		t.Fatalf("Umwandlung: %+v %v", task, err)
	}
	f := lastFrame(t, rest())
	if f.Code != "backgrounded" || f.Background != "bg-7" || f.Error != "" {
		t.Fatalf("Abschluss an pi: %+v", f)
	}
	if run.req.Timeout != 0 {
		t.Fatalf("Zeitgrenze an agw-exec: %v", run.req.Timeout)
	}
	if run.req.Env[ToolCallEnv] != "call_b" || run.req.Env[SessionEnv] == "" {
		t.Fatalf("Umgebung für agw-artifact: %v", run.req.Env)
	}
	// Die Zeitgrenze (0,3 s) gilt nach der Umwandlung nicht mehr; weitere Ausgabe geht an die Aufgabe.
	time.Sleep(500 * time.Millisecond)
	close(run.more)
	close(run.end)
	deadline := time.Now().Add(3 * time.Second)
	for {
		bg.mu.Lock()
		done, out, logPath := bg.finished, string(bg.adoptedOut), bg.adoptLog
		bg.mu.Unlock()
		if done != nil {
			if done.Exit == nil || *done.Exit != 0 || out != "vorher\nnachher\n" || logPath != execproto.SpillPath("call_b") {
				t.Fatalf("Aufgabe: %+v %q %q", done, out, logPath)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Aufgabe nicht beendet")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if r := rec.all(); len(r) != 1 || !strings.Contains(r[0].Error, "bg-7") {
		t.Fatalf("Protokoll: %+v", r)
	}
	if _, err := fg.Background(context.Background(), "chat-1", "call_b"); err != ErrNoForeground {
		t.Fatalf("zweite Umwandlung: %v", err)
	}
}

func TestForegroundTimeoutByOrchestrator(t *testing.T) {
	run := &stepRunner{more: make(chan struct{}), end: make(chan struct{})}
	c, _ := startFg(t, run, &fakeBg{}, NewForeground())
	_, rest := bashStream(t, c, "call_t", 0.2)
	if f := lastFrame(t, rest()); f.Code != "timeout" || f.Error != "timeout" {
		t.Fatalf("Zeitgrenze: %+v", f)
	}
}

// Eine zweite Operation mit derselben Kennung nimmt der ersten die Steuerung nicht; user_bash wird
// gar nicht eingetragen.
func TestForegroundDuplicateID(t *testing.T) {
	fg := NewForeground()
	var first, second bool
	a := fg.add("call_x", "chat-1", func() { first = true })
	fg.add("call_x", "chat-1", func() { second = true })
	fg.add("user_bash", "chat-1", func() {})
	if got := fg.Running("chat-1"); len(got) != 1 || got[0] != "call_x" {
		t.Fatalf("Register: %v", got)
	}
	if err := fg.Stop("chat-1", "call_x"); err != nil || !first || second {
		t.Fatalf("Stopp traf die falsche Operation: %v erste=%v zweite=%v", err, first, second)
	}
	fg.remove("call_x", a)
	if len(fg.Running("chat-1")) != 0 {
		t.Fatal("nicht entfernt")
	}
}

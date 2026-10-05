package sock

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agw/internal/bgtask"
	"agw/internal/execproto"
	"agw/internal/store"
)

type fakeBg struct {
	starts []bgtask.StartParams
	stops  []string

	mu          sync.Mutex
	adoptedOut  []byte
	adoptLog    string
	adoptCancel context.CancelFunc
	finished    *execproto.Frame
}

func (f *fakeBg) Start(_ context.Context, p bgtask.StartParams) (store.BackgroundTask, error) {
	f.starts = append(f.starts, p)
	if strings.Contains(p.Command, "full") {
		return store.BackgroundTask{}, bgtask.ErrLimit
	}
	return store.BackgroundTask{ID: "bg-1", Seq: 1, ChatID: p.ChatID, State: store.BgRunning, Command: p.Command, LogPath: "/tmp/agw-bg/bg-1.log"}, nil
}
func (f *fakeBg) Output(_ context.Context, chatID, id string) (store.BackgroundTask, string, error) {
	if id != "bg-1" {
		return store.BackgroundTask{}, "", bgtask.ErrUnknown
	}
	code := 0
	return store.BackgroundTask{ID: id, Seq: 1, ChatID: chatID, State: store.BgExited, ExitCode: &code}, "done-bg\n", nil
}
func (f *fakeBg) Stop(_ context.Context, chatID, id, by string) (store.BackgroundTask, error) {
	f.stops = append(f.stops, id+":"+by)
	return store.BackgroundTask{ID: id, Seq: 1, ChatID: chatID, State: store.BgStopped, StoppedBy: by}, nil
}
func (f *fakeBg) Max() int { return 5 }

func (f *fakeBg) Adopt(_ context.Context, p bgtask.StartParams, logPath string, soFar []byte, cancel context.CancelFunc) (*bgtask.Adopted, error) {
	f.mu.Lock()
	f.adoptedOut = append(f.adoptedOut, soFar...)
	f.adoptLog, f.adoptCancel = logPath, cancel
	f.mu.Unlock()
	return &bgtask.Adopted{Task: store.BackgroundTask{ID: "bg-7", Seq: 7, ChatID: p.ChatID, Command: p.Command, State: store.BgRunning},
		Write: func(b []byte) { f.mu.Lock(); f.adoptedOut = append(f.adoptedOut, b...); f.mu.Unlock() },
		Finish: func(fr execproto.Frame, err error) {
			f.mu.Lock()
			f.finished = &fr
			f.mu.Unlock()
		}}, nil
}

func startPiBg(t *testing.T, b *fakeBackend, rec *fakeRecorder, bg BackgroundTasks) *http.Client {
	t.Helper()
	dir, err := os.MkdirTemp("", "agwsockbg")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	srv, err := Listen(dir, NewPiHandlerBg("p-test", b, 1<<20, &fakeRunner{}, rec, bg))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(dir, SocketName))
	}}, Timeout: 10 * time.Second}
}

func TestBackgroundEndpoints(t *testing.T) {
	b := &fakeBackend{chat: "chat-1"}
	rec := &fakeRecorder{}
	bg := &fakeBg{}
	c := startPiBg(t, b, rec, bg)
	sub := "/agent/sessions/x/9017da63-d08d-43ab-b978-e86cb7afc45b/run-0/session.jsonl"
	start := map[string]any{"toolCallId": "call_00_bg", "tool": "bash", "sessionFile": sub,
		"req": map[string]any{"op": "bg", "command": "sleep 8; echo done-bg", "cwd": "/workspace", "env": map[string]string{"PI_SESSION_ID": "s"}}}
	resp, body := postJSON(t, c, "/tool/bg/start", start)
	var res BgResponse
	_ = json.Unmarshal([]byte(body), &res)
	if resp.StatusCode != 200 || res.Task == nil || res.Task.ID != "bg-1" || res.Max != 5 || res.Error != "" {
		t.Fatalf("Start: %d %s", resp.StatusCode, body)
	}
	if p := bg.starts[0]; p.ChatID != "chat-1" || p.Session != "9017da63-d08d-43ab-b978-e86cb7afc45b" || p.ToolCallID != "call_00_bg" || p.Env["PI_SESSION_ID"] != "s" {
		t.Fatalf("start parameters: %+v", p)
	}
	// Limit reached: error as the result, logged nevertheless
	start["req"] = map[string]any{"op": "bg", "command": "full", "cwd": "/workspace"}
	start["toolCallId"] = "call_01_bg"
	_, body = postJSON(t, c, "/tool/bg/start", start)
	if !strings.Contains(body, "too many background tasks") {
		t.Fatalf("limit: %s", body)
	}
	// Invalid: wrong tool, invalid request, wrong endpoint
	for path, v := range map[string]map[string]any{
		"/tool/bg/start":  {"toolCallId": "c", "tool": "read", "req": map[string]any{"command": "x", "cwd": "/workspace"}},
		"/tool/bg/output": {"toolCallId": "c", "tool": "bg_stop", "id": "bg-1"},
		"/tool/bg/stop":   {"toolCallId": "", "tool": "bg_stop", "id": "bg-1"},
	} {
		if resp, body := postJSON(t, c, path, v); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s accepted: %d %s", path, resp.StatusCode, body)
		}
	}
	if resp, _ := postJSON(t, c, "/tool/bg/start", map[string]any{"toolCallId": "c", "tool": "bash", "req": map[string]any{"command": "x", "cwd": "rel"}}); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("relative working directory accepted")
	}
	_, body = postJSON(t, c, "/tool/bg/output", map[string]any{"toolCallId": "call_02", "tool": "bg_output", "id": "bg-1", "tailLines": 20})
	if !strings.Contains(body, "done-bg") || !strings.Contains(body, `"state":"exited"`) {
		t.Fatalf("Output: %s", body)
	}
	_, body = postJSON(t, c, "/tool/bg/output", map[string]any{"toolCallId": "call_03", "tool": "bg_output", "id": "bg-9"})
	if !strings.Contains(body, "unknown background task") {
		t.Fatalf("unknown: %s", body)
	}
	_, body = postJSON(t, c, "/tool/bg/stop", map[string]any{"toolCallId": "call_04", "tool": "bg_stop", "id": "bg-1"})
	if !strings.Contains(body, `"stopped_by":"agent"`) || bg.stops[0] != "bg-1:agent" {
		t.Fatalf("Stop: %s %v", body, bg.stops)
	}
	// Log: every call recorded, with tool, operation and session
	recs := rec.all()
	want := []string{"call_00_bg bash bg_start", "call_01_bg bash bg_start", "call_02 bg_output bg_output", "call_03 bg_output bg_output", "call_04 bg_stop bg_stop"}
	if len(recs) != len(want) {
		t.Fatalf("entries: %+v", recs)
	}
	for i, r := range recs {
		if got := r.ToolCallID + " " + r.Tool + " " + r.Op; got != want[i] {
			t.Errorf("entry %d: %s", i, got)
		}
	}
	if recs[0].Session != "9017da63-d08d-43ab-b978-e86cb7afc45b" || !strings.Contains(string(recs[0].Args), `"run_in_background":true`) || !strings.Contains(string(recs[0].Args), `"id":"bg-1"`) {
		t.Fatalf("start in the log: %+v %s", recs[0], recs[0].Args)
	}
	if recs[1].Error == "" || recs[3].Error == "" || recs[2].ExitCode == nil {
		t.Fatalf("errors in the log: %+v", recs)
	}
	// Without a registry: 501
	c2 := startPi(t, b, &fakeRunner{}, &fakeRecorder{})
	if resp, _ := postJSON(t, c2, "/tool/bg/output", map[string]any{"toolCallId": "c", "tool": "bg_output", "id": "bg-1"}); resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("without registry: %d", resp.StatusCode)
	}
	for _, tool := range []string{"bg_output", "bg_stop"} {
		found := false
		for _, e := range ExecutedTools() {
			found = found || e == tool
		}
		if !found {
			t.Errorf("%s missing from ExecutedTools", tool)
		}
	}
}

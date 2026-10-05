package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeServer simulates the orchestrator: REST endpoints and a scriptable SSE stream
// that only starts after a message has been sent.
type fakeServer struct {
	t          *testing.T
	srv        *httptest.Server
	subscribed atomic.Bool
	posted     chan string
	decided    chan decision
	noSlot     bool
	running    bool
	script     []string // SSE events after sending; "WAIT_DECISION" waits for a decision

	detail    string // response of GET /api/chats/{id}, empty = default
	sendReply string // response of POST …/messages, empty = sent

	mu          sync.Mutex
	createReq   map[string]any
	decisions   []decision
	commands    []string
	autocompact []bool
}

func newFakeServer(t *testing.T) *fakeServer {
	f := &fakeServer{t: t, posted: make(chan string, 4), decided: make(chan decision, 4)}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/models", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `[{"id":"deepseek/deepseek-flash","provider":"deepseek","model":"deepseek-flash","name":"DeepSeek Flash","default":true,"pricing":{"input":0.27,"output":1.1,"cache_read":0.07,"cache_write":0,"currency":"USD","note":"peak tariff","retrieved":"2026-09-28"},"tariff":{"peak_windows_utc":[{"days":"mon-fri","from":"01:00","to":"09:00"}],"offpeak_factor":0.5},"peak_now":false},{"id":"x/y","provider":"x","model":"y","name":"Without price","default":false}]`)
	})
	mux.HandleFunc("GET /api/pool", func(w http.ResponseWriter, r *http.Request) {
		since := time.Now().Add(-90 * time.Second).UTC().Format(time.RFC3339)
		fmt.Fprintf(w, `{"slots":[{"id":"p-1","variant":"cli","state":"idle","container_id":"abcdef123456","container_name":"agw-p-1","image":"agw-basis","created_at":%q},{"id":"p-2","variant":"cli","state":"assigned","container_id":"123456abcdef","container_name":"agw-p-2","image":"agw-basis","created_at":%q,"assigned_at":%q,"chat_id":"c1","chat_title":"Test","activity":{"kind":"tool","tool":"bash","since":%q},"internet":false}],"targets":{"cli":2,"mcp":1},"totals":{"cost":0.0123,"tokens":{"input":100,"output":50,"cache_read":10,"total":160},"chats_active":1}}`, since, since, since, since)
	})
	mux.HandleFunc("POST /api/chats", func(w http.ResponseWriter, r *http.Request) {
		if f.noSlot {
			w.WriteHeader(http.StatusServiceUnavailable)
			io.WriteString(w, `{"error":"no idle slot"}`)
			return
		}
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.createReq = req
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"id":"c1","title":"new","model":"deepseek/deepseek-flash","variant":"cli","state":"active","tokens":{"input":0,"output":0,"cache_read":0,"total":0}}`)
	})
	mux.HandleFunc("GET /api/chats/{id}", func(w http.ResponseWriter, r *http.Request) {
		if f.detail != "" {
			io.WriteString(w, f.detail)
			return
		}
		fmt.Fprintf(w, `{"chat":{"id":"c1","title":"new","model":"deepseek/deepseek-flash","variant":"cli","state":"active","running":%v,"tokens":{"input":1200,"output":300,"cache_read":0,"total":1500},"cost":0.0042,"artifact_count":1,"pending_approvals":0},
"messages":[{"seq":1,"role":"user","message":{"role":"user","content":[{"type":"text","text":"Count files"}]}},
{"seq":2,"role":"assistant","message":{"role":"assistant","content":[{"type":"thinking","thinking":"hm"},{"type":"text","text":"Let me check."},{"type":"toolCall","id":"t1","name":"bash","arguments":{"command":"ls | wc -l"}}]}},
{"seq":3,"role":"toolResult","message":{"role":"toolResult","toolCallId":"t1","toolName":"bash","content":[{"type":"text","text":"3\n"}],"isError":false}}],
"artifacts":[{"chat_id":"c1","kind":"output","name":"report.md","size":120,"via":"cli","created_at":"2026-09-29T10:00:00Z"}],
"approvals":[{"id":"a9","chat_id":"c1","kind":"artifact_upload","via":"mcp","name":"data.csv","size":42,"state":"pending"}],"socket_calls":[]}`, f.running)
	})
	mux.HandleFunc("GET /api/chats/{id}/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		// An old, completed run before sending must not end the waiting.
		io.WriteString(w, ": ping\n\ndata: {\"kind\":\"pi\",\"data\":{\"type\":\"agent_settled\"}}\n\n")
		fl.Flush()
		f.subscribed.Store(true)
		select {
		case <-f.posted:
		case <-r.Context().Done():
			return
		case <-time.After(5 * time.Second):
			return
		}
		for _, line := range f.script {
			if line == "WAIT_DECISION" {
				select {
				case <-f.decided:
				case <-r.Context().Done():
					return
				case <-time.After(5 * time.Second):
					return
				}
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", line)
			fl.Flush()
		}
		<-r.Context().Done()
	})
	mux.HandleFunc("POST /api/chats/{id}/messages", func(w http.ResponseWriter, r *http.Request) {
		if !f.subscribed.Load() {
			t.Errorf("message sent before the event stream was subscribed")
		}
		var b map[string]string
		json.NewDecoder(r.Body).Decode(&b)
		f.posted <- b["text"]
		if f.sendReply != "" {
			io.WriteString(w, f.sendReply)
			return
		}
		io.WriteString(w, `{"ok":true,"resumed":false}`)
	})
	mux.HandleFunc("POST /api/chats/{id}/commands", func(w http.ResponseWriter, r *http.Request) {
		// All tests that send here wait with --wait: then the subscription must come first.
		if !f.subscribed.Load() {
			t.Errorf("command sent before the event stream was subscribed")
		}
		var b map[string]string
		json.NewDecoder(r.Body).Decode(&b)
		f.mu.Lock()
		f.commands = append(f.commands, b["command"])
		f.mu.Unlock()
		select {
		case f.posted <- b["command"]:
		default:
		}
		io.WriteString(w, `{"ok":true,"resumed":false}`)
	})
	mux.HandleFunc("GET /api/chats/{id}/commands", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `[{"name":"compact","description":"summarise the context now","source":"builtin","args":"[instructions]"},{"name":"autocompact","description":"automation on/off","source":"builtin","args":"on|off"},{"name":"skill:report","description":"write a report","source":"skill"}]`)
	})
	mux.HandleFunc("POST /api/chats/{id}/autocompact", func(w http.ResponseWriter, r *http.Request) {
		var b map[string]bool
		json.NewDecoder(r.Body).Decode(&b)
		f.mu.Lock()
		f.autocompact = append(f.autocompact, b["enabled"])
		f.mu.Unlock()
		fmt.Fprintf(w, `{"id":"c1","title":"new","state":"active","auto_compact":%v,"tokens":{"total":0}}`, b["enabled"])
	})
	mux.HandleFunc("POST /api/approvals/{id}", func(w http.ResponseWriter, r *http.Request) {
		var b map[string]bool
		json.NewDecoder(r.Body).Decode(&b)
		d := decision{r.PathValue("id"), b["approve"]}
		f.mu.Lock()
		f.decisions = append(f.decisions, d)
		f.mu.Unlock()
		f.decided <- d
		fmt.Fprintf(w, `{"id":%q,"chat_id":"c1","state":"approved"}`, d.id)
	})
	mux.HandleFunc("GET /api/approvals", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") != "pending" {
			t.Errorf("expected state=pending: %s", r.URL.RawQuery)
		}
		io.WriteString(w, `[{"id":"a1","chat_id":"c1","name":"x.md","size":3,"via":"cli","state":"pending","created_at":"2026-09-29T10:00:00Z"},{"id":"a2","chat_id":"c2","name":"y.md","size":4,"via":"mcp","state":"pending","created_at":"2026-09-29T10:00:00Z"}]`)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeServer) run(args ...string) (code int, stdout, stderr string) {
	return f.runIn("", args...)
}

func (f *fakeServer) runIn(stdin string, args ...string) (code int, stdout, stderr string) {
	var out, errw strings.Builder
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	env := func(k string) string {
		if k == "AGW_URL" {
			return f.srv.URL
		}
		return ""
	}
	code = realMain(ctx, args, strings.NewReader(stdin), &out, &errw, env)
	return code, out.String(), errw.String()
}

var standardScript = []string{
	`{"kind":"pi","data":{"type":"agent_start"}}`,
	`{"kind":"pi","data":{"type":"message_start","message":{"role":"assistant","content":[]}}}`,
	`{"kind":"pi","data":{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"Hello"}}}`,
	`{"kind":"pi","data":{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"Hello"}]}}}`,
	`{"kind":"pi","data":{"type":"tool_execution_start","toolCallId":"t1","toolName":"bash","args":{"command":"agw-artifact put report.md"}}}`,
	`{"kind":"approval","data":{"id":"a1","chat_id":"c1","kind":"artifact_upload","via":"cli","name":"report.md","size":120,"state":"pending"}}`,
	"WAIT_DECISION",
	`{"kind":"approval","data":{"id":"a1","chat_id":"c1","kind":"artifact_upload","via":"cli","name":"report.md","size":120,"state":"approved"}}`,
	`{"kind":"pi","data":{"type":"tool_execution_end","toolCallId":"t1","toolName":"bash","result":{"content":[{"type":"text","text":"uploaded"}]},"isError":false}}`,
	`{"kind":"pi","data":{"type":"message_start","message":{"role":"assistant","content":[]}}}`,
	`{"kind":"pi","data":{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":" Done."}}}`,
	`{"kind":"pi","data":{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":" Done."}]}}}`,
	`{"kind":"pi","data":{"type":"agent_end"}}`,
	`{"kind":"pi","data":{"type":"agent_settled"}}`,
}

func TestRunAutoApprove(t *testing.T) {
	f := newFakeServer(t)
	f.script = standardScript
	code, out, errw := f.run("run", "--auto-approve", "--variant", "mcp", "--internet=false", "Write a report")
	if code != 0 {
		t.Fatalf("exit code %d, stderr: %s", code, errw)
	}
	if out != "Hello\n Done.\n" {
		t.Errorf("stdout = %q", out)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.decisions) != 1 || f.decisions[0] != (decision{"a1", true}) {
		t.Errorf("decisions = %+v", f.decisions)
	}
	if f.createReq["variant"] != "mcp" || f.createReq["internet"] != false {
		t.Errorf("create = %v", f.createReq)
	}
	if _, ok := f.createReq["message"]; ok {
		t.Errorf("run must not send the message when creating (events would be lost): %v", f.createReq)
	}
	for _, want := range []string{`▶ bash {"command":"agw-artifact put report.md"}`, "c1", "1500", "0.0042"} {
		if !strings.Contains(errw, want) {
			t.Errorf("stderr without %q:\n%s", want, errw)
		}
	}
}

func TestRunInteractiveReject(t *testing.T) {
	f := newFakeServer(t)
	f.script = standardScript
	code, _, errw := f.runIn("n\n", "run", "task")
	if code != 0 {
		t.Fatalf("exit code %d, stderr: %s", code, errw)
	}
	if !strings.Contains(errw, "Approve artifact report.md (120 bytes)? [y/n]") {
		t.Errorf("question missing: %s", errw)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.decisions) != 1 || f.decisions[0].approve {
		t.Errorf("decisions = %+v", f.decisions)
	}
}

func TestSendWait(t *testing.T) {
	f := newFakeServer(t)
	f.script = []string{
		`{"kind":"pi","data":{"type":"agent_start"}}`,
		`{"kind":"pi","data":{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"Reply"}}}`,
		`{"kind":"pi","data":{"type":"agent_settled"}}`,
	}
	code, out, errw := f.run("chat", "send", "c1", "Hello", "you", "--wait")
	if code != 0 {
		t.Fatalf("exit code %d: %s", code, errw)
	}
	if !strings.HasPrefix(out, "Reply") {
		t.Errorf("stdout = %q", out)
	}
}

// If the agent is running, the orchestrator enqueues; --wait waits for the turn after the
// handover, not for the end of the running one. Steps when resuming appear on stderr.
func TestSendWaitQueued(t *testing.T) {
	f := newFakeServer(t)
	f.running = true
	f.sendReply = `{"ok":true,"resumed":false,"queued":true,"queue_id":"q1"}`
	f.script = []string{
		`{"kind":"pi","data":{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"old "}}}`,
		`{"kind":"pi","data":{"type":"agent_settled"}}`,
		`{"kind":"queue","data":{"entries":[],"change":"delivered","ids":["q1"],"text":"Hello you"}}`,
		`{"kind":"pi","data":{"type":"agent_start"}}`,
		`{"kind":"pi","data":{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"New"}}}`,
		`{"kind":"pi","data":{"type":"agent_settled"}}`,
	}
	code, out, errw := f.run("chat", "send", "c1", "Hello", "you", "--wait")
	if code != 0 {
		t.Fatalf("exit code %d: %s", code, errw)
	}
	if !strings.Contains(out, "New") {
		t.Errorf("stdout = %q (reply after the handover missing)", out)
	}
	if !strings.Contains(errw, "Queued") || !strings.Contains(errw, "handed over (1)") {
		t.Errorf("stderr = %q", errw)
	}
}

func TestResumeLine(t *testing.T) {
	size, files := int64(1258291), 14
	cases := []struct {
		in   resumeStep
		want string
	}{
		{resumeStep{Phase: "acquire", Status: "running"}, ""},
		{resumeStep{Phase: "workspace", Status: "done", Size: &size, Files: &files, Ms: 420}, "resume: workspace (1.2 MB, 14 files) · 0.4 s"},
		{resumeStep{Phase: "ready", Status: "done", Ms: 3400}, "chat resumed in a fresh sandbox (3.4 s)."},
		{resumeStep{Phase: "failed", Status: "error", Detail: "no free slot in the pool"}, "resume failed: no free slot in the pool"},
	}
	for _, c := range cases {
		if got := resumeLine(c.in); got != c.want {
			t.Errorf("resumeLine(%+v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestChatNewNoSlot(t *testing.T) {
	f := newFakeServer(t)
	f.noSlot = true
	code, _, errw := f.run("chat", "new", "Hello")
	if code == 0 {
		t.Fatal("exit code 0 despite 503")
	}
	if !strings.Contains(errw, "No free slot in the pool") {
		t.Errorf("stderr = %q", errw)
	}
}

func TestChatNewPrintsID(t *testing.T) {
	f := newFakeServer(t)
	code, out, errw := f.run("chat", "new", "--model", "x/y", "--title", "T", "Go")
	if code != 0 || strings.TrimSpace(out) != "c1" {
		t.Fatalf("code=%d out=%q err=%q", code, out, errw)
	}
	if f.createReq["message"] != "Go" || f.createReq["model"] != "x/y" {
		t.Errorf("create = %v", f.createReq)
	}
	if _, ok := f.createReq["internet"]; ok {
		t.Errorf("without --internet the field may be absent: %v", f.createReq)
	}
}

func TestModelsTable(t *testing.T) {
	f := newFakeServer(t)
	code, out, _ := f.run("models")
	if code != 0 {
		t.Fatal(code)
	}
	for _, want := range []string{"ID", "deepseek/deepseek-flash", "DeepSeek Flash", "yes", "0.27", "1.10", "0.07", "peak tariff"} {
		if !strings.Contains(out, want) {
			t.Errorf("table without %q:\n%s", want, out)
		}
	}
	code, out, _ = f.run("models", "--json")
	var v []map[string]any
	if code != 0 || json.Unmarshal([]byte(out), &v) != nil || len(v) != 2 {
		t.Errorf("--json: %d %q", code, out)
	}
}

func TestPoolOutput(t *testing.T) {
	f := newFakeServer(t)
	code, out, _ := f.run("pool")
	if code != 0 {
		t.Fatal(code)
	}
	for _, want := range []string{"cli", "target 2", "free 1", "assigned 1", "mcp", "target 1", "0.0123", "160", "Slot", "Activity", "p-2", "Running bash", "Test", "1m"} {
		if !strings.Contains(out, want) {
			t.Errorf("output without %q:\n%s", want, out)
		}
	}
}

func TestChatShow(t *testing.T) {
	f := newFakeServer(t)
	code, out, errw := f.run("chat", "show", "c1")
	if code != 0 {
		t.Fatal(errw)
	}
	for _, want := range []string{"Count files", "Let me check.", `▶ bash {"command":"ls | wc -l"}`, "report.md", "data.csv", "a9"} {
		if !strings.Contains(out, want) {
			t.Errorf("output without %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "hm") {
		t.Errorf("thinking without --thinking: %s", out)
	}
}

func TestApprovalsFilterChat(t *testing.T) {
	f := newFakeServer(t)
	code, out, _ := f.run("approvals", "--chat", "c1")
	if code != 0 || !strings.Contains(out, "a1") || strings.Contains(out, "a2") {
		t.Errorf("code=%d out=%s", code, out)
	}
}

func TestApproveReject(t *testing.T) {
	f := newFakeServer(t)
	if code, _, e := f.run("approve", "a1"); code != 0 {
		t.Fatal(e)
	}
	if code, _, e := f.run("reject", "a2"); code != 0 {
		t.Fatal(e)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.decisions) != 2 || f.decisions[0] != (decision{"a1", true}) || f.decisions[1] != (decision{"a2", false}) {
		t.Errorf("decisions = %+v", f.decisions)
	}
}

func TestUsageErrors(t *testing.T) {
	f := newFakeServer(t)
	if code, _, _ := f.run(); code != 2 {
		t.Errorf("without a command: %d", code)
	}
	if code, _, e := f.run("doesnotexist"); code != 2 || !strings.Contains(e, "unknown command") {
		t.Errorf("unknown: %d %q", code, e)
	}
	if code, _, _ := f.run("chat", "internet", "c1", "maybe"); code != 2 {
		t.Errorf("internet without on/off: %d", code)
	}
	if code, _, _ := f.run("run", "--auto-approve", "--auto-reject", "x"); code != 2 {
		t.Errorf("both auto switches: %d", code)
	}
}

func TestURLFlagOverridesEnv(t *testing.T) {
	f := newFakeServer(t)
	var out, errw strings.Builder
	code := realMain(context.Background(), []string{"--url", f.srv.URL, "models"}, strings.NewReader(""), &out, &errw,
		func(string) string { return "http://127.0.0.1:1" })
	if code != 0 {
		t.Fatalf("%d %s", code, errw.String())
	}
}

func TestDownloadToFile(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/chats/{id}/artifacts/{name}", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "content:"+r.PathValue("name")+":"+r.URL.Query().Get("kind"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	dst := filepath.Join(t.TempDir(), "x.md")
	var out, errw strings.Builder
	code := realMain(context.Background(), []string{"chat", "download", "c1", "b.md", "--kind", "input", "-o", dst},
		strings.NewReader(""), &out, &errw, func(k string) string {
			if k == "AGW_URL" {
				return srv.URL
			}
			return ""
		})
	b, _ := os.ReadFile(dst)
	if code != 0 || string(b) != "content:b.md:input" {
		t.Errorf("code=%d file=%q err=%s", code, b, errw.String())
	}
}

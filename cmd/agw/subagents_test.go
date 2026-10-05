package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"agw/internal/agwclient"
)

const detailWithSubagents = `{"chat":{"id":"c1","title":"new","model":"deepseek/deepseek-flash","variant":"cli","state":"active","max_subagents":2,"subagents":1,"llm_calls":5,"cost":0.05,"cost_other":0.01,"tokens":{"input":1200,"output":300,"cache_read":0,"total":1500}},
"messages":[],"artifacts":[],"approvals":[],"socket_calls":[],
"subagent_entries":[
{"chat_id":"c1","run_id":"r1","entry_id":"e1","agent":"scout","kind":"task","payload":{"text":"Search the repo for all places where tokens are checked, and report on them in detail with file names and lines"},"confirmed":false,"created_at":"2026-09-29T10:00:00Z"},
{"chat_id":"c1","run_id":"r1","entry_id":"e2:0","agent":"scout","kind":"tool_call","payload":{"name":"bash","arguments":"{\"command\":\"grep -rn token\"}"},"response_id":"x1","confirmed":true,"created_at":"2026-09-29T10:00:01Z"},
{"chat_id":"c1","run_id":"r1","entry_id":"e3","agent":"scout","kind":"tool_result","payload":{"name":"bash","text":"a.go:1: token\nb.go:2: token","is_error":false},"confirmed":false,"created_at":"2026-09-29T10:00:02Z"},
{"chat_id":"c1","run_id":"r1","entry_id":"e4:0","agent":"scout","kind":"tool_call","payload":{"name":"read","arguments":"{\"path\":\"c.go\"}"},"response_id":"x2","confirmed":false,"created_at":"2026-09-29T10:00:03Z"},
{"chat_id":"c1","run_id":"r1","entry_id":"e5","agent":"scout","kind":"tool_result","payload":{"name":"read","text":"not found","is_error":true},"confirmed":false,"created_at":"2026-09-29T10:00:04Z"}
]}`

type subServer struct {
	srv    *httptest.Server
	mu     sync.Mutex
	maxSet []int
}

func newSubServer(t *testing.T) *subServer {
	s := &subServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/chats/{id}", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, detailWithSubagents)
	})
	mux.HandleFunc("GET /api/models", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `[{"id":"deepseek/deepseek-flash","provider":"deepseek","model":"deepseek-flash","name":"DeepSeek Flash","default":true,"tariff":{"peak_windows_utc":[{"days":"mon-fri","from":"01:00","to":"09:00"}],"offpeak_factor":0.5}}]`)
	})
	mux.HandleFunc("POST /api/chats/{id}/subagents", func(w http.ResponseWriter, r *http.Request) {
		var b map[string]int
		json.NewDecoder(r.Body).Decode(&b)
		s.mu.Lock()
		s.maxSet = append(s.maxSet, b["max"])
		s.mu.Unlock()
		if b["max"] > 8 {
			w.WriteHeader(400)
			io.WriteString(w, `{"error":"max_subagents must be between 0 and 8"}`)
			return
		}
		io.WriteString(w, `{"id":"c1","state":"active","max_subagents":`+strings.TrimSpace(jsonInt(b["max"]))+`,"subagents":1,"tokens":{"total":0}}`)
	})
	mux.HandleFunc("GET /api/chats/{id}/llm_calls", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `[
{"id":1,"slot_id":"p-1","source_ip":"10.0.0.2","model":"deepseek-flash","response_id":"m1","status":200,"input":1000,"output":200,"cache_read":50,"cache_write":0,"cost":0.004,"peak":true,"tool_calls":[{"name":"subagent","arguments":"{}"}],"started_at":"2026-09-29T10:00:00Z","duration_ms":1500,"main":true},
{"id":2,"slot_id":"p-1","source_ip":"10.0.0.2","model":"deepseek-flash","response_id":"x1","status":200,"input":500,"output":100,"cache_read":0,"cache_write":0,"cost":0.001,"peak":false,"tool_calls":[{"name":"bash","arguments":"{}"},{"name":"read","arguments":"{}"}],"started_at":"2026-09-29T10:00:01Z","duration_ms":800,"main":false},
{"id":3,"slot_id":"p-1","source_ip":"10.0.0.2","model":"deepseek-flash","response_id":"","status":429,"input":0,"output":0,"cache_read":0,"cache_write":0,"cost":0,"peak":false,"tool_calls":null,"started_at":"2026-09-29T10:00:02Z","duration_ms":1,"main":false}]`)
	})
	s.srv = httptest.NewServer(mux)
	t.Cleanup(s.srv.Close)
	return s
}

func jsonInt(n int) string { b, _ := json.Marshal(n); return string(b) }

func (s *subServer) run(args ...string) (int, string, string) {
	var out, errw strings.Builder
	code := realMain(context.Background(), args, strings.NewReader(""), &out, &errw, func(k string) string {
		if k == "AGW_URL" {
			return s.srv.URL
		}
		return ""
	})
	return code, out.String(), errw.String()
}

func TestChatSubagentsShow(t *testing.T) {
	s := newSubServer(t)
	code, out, errw := s.run("chat", "subagents", "c1")
	if code != 0 {
		t.Fatalf("code %d: %s", code, errw)
	}
	for _, want := range []string{"limit 2", "started 1", "scout", "task: Search the repo",
		`▶ bash {"command":"grep -rn token"}`, "✓ a.go:1: token", "confirmed", "sandbox only", `▶ read {"path":"c.go"}`, "✗ not found"} {
		if !strings.Contains(out, want) {
			t.Errorf("output without %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "report on them in detail with file names and lines") {
		t.Errorf("task not truncated:\n%s", out)
	}
	if len(s.maxSet) != 0 {
		t.Errorf("showing must not set anything: %v", s.maxSet)
	}
}

func TestChatSubagentsSet(t *testing.T) {
	s := newSubServer(t)
	code, out, errw := s.run("chat", "subagents", "c1", "4")
	if code != 0 {
		t.Fatalf("code %d: %s", code, errw)
	}
	if !strings.Contains(out, "4") || !strings.Contains(out, "Subagent") {
		t.Errorf("output = %q", out)
	}
	if len(s.maxSet) != 1 || s.maxSet[0] != 4 {
		t.Errorf("set = %v", s.maxSet)
	}
	if code, _, _ := s.run("chat", "subagents", "c1", "many"); code != 2 {
		t.Errorf("not a number: code %d", code)
	}
	if code, _, _ := s.run("chat", "subagents", "c1", "-1"); code != 2 {
		t.Errorf("negative number: code %d", code)
	}
	code, _, errw = s.run("chat", "subagents", "c1", "9")
	if code != 1 || !strings.Contains(errw, "between 0 and 8") {
		t.Errorf("server error: code %d, %q", code, errw)
	}
}

func TestChatCalls(t *testing.T) {
	s := newSubServer(t)
	code, out, errw := s.run("chat", "calls", "c1")
	if code != 0 {
		t.Fatalf("code %d: %s", code, errw)
	}
	for _, want := range []string{"Time", "main", "sub", "1,000", "0.0040 USD", "peak tariff", "off-peak tariff", "bash, read", "subagent", "429", "3 model calls", "0.0050 USD", "of which sub"} {
		if !strings.Contains(out, want) {
			t.Errorf("output without %q:\n%s", want, out)
		}
	}
	code, out, _ = s.run("chat", "calls", "c1", "--json")
	var v []map[string]any
	if code != 0 || json.Unmarshal([]byte(out), &v) != nil || len(v) != 3 {
		t.Errorf("--json: %d %q", code, out)
	}
}

func TestChatShowSubagentHeader(t *testing.T) {
	s := newSubServer(t)
	code, out, errw := s.run("chat", "show", "c1")
	if code != 0 {
		t.Fatalf("code %d: %s", code, errw)
	}
	for _, want := range []string{"subagents 1/2", "model calls 5", "cost 0.0500 USD", "0.0100 USD outside the main replies"} {
		if !strings.Contains(out, want) {
			t.Errorf("output without %q:\n%s", want, out)
		}
	}
}

var subagentScript = []string{
	`{"kind":"pi","data":{"type":"agent_start"}}`,
	`{"kind":"subagent","data":{"chat_id":"c1","run_id":"r1","entry_id":"e1","agent":"scout","kind":"task","payload":{"text":"Search tokens"},"confirmed":false}}`,
	`{"kind":"subagent","data":{"chat_id":"c1","run_id":"r1","entry_id":"e2:0","agent":"scout","kind":"tool_call","payload":{"name":"bash","arguments":"{\"command\":\"ls\"}"},"response_id":"x1","confirmed":true}}`,
	`{"kind":"subagent","data":{"chat_id":"c1","run_id":"r1","entry_id":"e3","agent":"scout","kind":"tool_result","payload":{"name":"bash","text":"a.txt\nb.txt","is_error":false},"confirmed":false}}`,
	`{"kind":"llm_call","data":{"id":2,"slot_id":"p-1","model":"deepseek-flash","status":200,"input":500,"output":100,"cost":0.0012,"peak":false,"tool_calls":[{"name":"bash","arguments":"{}"}],"main":false}}`,
	`{"kind":"socket_call","data":{"id":9,"chat_id":"c1","slot_id":"p-1","via":"cli","op":"extension_ui","detail":"confirm: delete file?","result":"rejected"}}`,
	`{"kind":"socket_call","data":{"id":10,"chat_id":"c1","slot_id":"p-1","via":"cli","op":"agent_limit","detail":"at most 3 concurrent agents","result":"refused"}}`,
	`{"kind":"pi","data":{"type":"agent_settled"}}`,
}

func TestRunSubagentStream(t *testing.T) {
	f := newFakeServer(t)
	f.script = subagentScript
	f.detail = detailWithSubagents
	code, _, errw := f.run("run", "--max-subagents", "1", "task")
	if code != 0 {
		t.Fatalf("code %d: %s", code, errw)
	}
	f.mu.Lock()
	if f.createReq["max_subagents"] != float64(1) {
		t.Errorf("create = %v", f.createReq)
	}
	f.mu.Unlock()
	for _, want := range []string{"  ↳ scout: task: Search tokens", `  ↳ scout: ▶ bash {"command":"ls"}`, "  ↳ scout: ✓ a.txt", "extension", "delete file?",
		"cost 0.0500 USD", "0.0100 USD outside the main replies"} {
		if !strings.Contains(errw, want) {
			t.Errorf("stderr without %q:\n%s", want, errw)
		}
	}
	if strings.Contains(errw, "model call sub") || strings.Contains(errw, "concurrent agents") {
		t.Errorf("llm_call/agent_limit shown without --verbose:\n%s", errw)
	}

	f = newFakeServer(t)
	f.script = subagentScript
	f.detail = detailWithSubagents
	code, _, errw = f.run("run", "--verbose", "task")
	if code != 0 {
		t.Fatalf("code %d: %s", code, errw)
	}
	for _, want := range []string{"model call sub", "0.0012 USD", "proxy refused a model call", "at most 3 concurrent agents"} {
		if !strings.Contains(errw, want) {
			t.Errorf("--verbose: stderr without %q:\n%s", want, errw)
		}
	}
	if _, ok := f.createReq["max_subagents"]; ok {
		t.Errorf("without --max-subagents the field may be absent: %v", f.createReq)
	}
}

func TestChatNewMaxSubagents(t *testing.T) {
	f := newFakeServer(t)
	code, _, errw := f.run("chat", "new", "--max-subagents", "0", "Go")
	if code != 0 {
		t.Fatal(errw)
	}
	if v, ok := f.createReq["max_subagents"]; !ok || v != float64(0) {
		t.Errorf("create = %v", f.createReq)
	}
	if code, _, _ := f.run("chat", "new", "--max-subagents", "-2"); code != 2 {
		t.Errorf("negative: code %d", code)
	}
}

func TestStreamSubagentLimitSocketCall(t *testing.T) {
	s, _, errw, _ := newTestStreamer(approvalShow, "")
	s.verbose = true
	feed(t, s, true, agwEv("socket_call", `{"op":"subagent_limit","via":"cli","detail":"3 started, 2 allowed","result":"aborted"}`))
	if !strings.Contains(errw.String(), "subagent limit exceeded (3 started, 2 allowed)") {
		t.Errorf("stderr = %q", errw.String())
	}
}

func agwEv(kind, js string) agwclient.Event { return agwclient.Event{Kind: kind, Data: []byte(js)} }

package llmproxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"agw/internal/config"
)

const deepseekSSE = `data: {"id":"resp-1","model":"deepseek-flash","choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}

data: {"id":"resp-1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"bash","arguments":""}}]}}]}

data: {"id":"resp-1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"command\":"}}]}}]}

data: {"id":"resp-1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"ls\"}"}}]}}]}

data: {"id":"resp-1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1000,"completion_tokens":50,"total_tokens":1050,"prompt_cache_hit_tokens":800,"prompt_cache_miss_tokens":200}}

data: [DONE]

`

func TestParseSSE(t *testing.T) {
	m := newMeter("openai-completions")
	m.Write([]byte(deepseekSSE[:100])) // in chunks, as when streaming
	m.Write([]byte(deepseekSSE[100:]))
	r := m.Result()
	if r.ResponseID != "resp-1" || r.Usage.Input != 200 || r.Usage.CacheRead != 800 || r.Usage.Output != 50 {
		t.Fatalf("result: %+v", r)
	}
	if len(r.ToolCalls) != 1 || r.ToolCalls[0].Name != "bash" || r.ToolCalls[0].Arguments != `{"command":"ls"}` || r.ToolCalls[0].ID != "call_1" {
		t.Fatalf("tool calls: %+v", r.ToolCalls)
	}
}

// Two parallel calls: IDs come only in the first chunk per index; later
// chunks without ID do not overwrite them.
func TestParseSSEParallelToolCallIDs(t *testing.T) {
	m := newMeter("openai-completions")
	m.Write([]byte(`data: {"id":"r","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_00_a","function":{"name":"read","arguments":""}},{"index":1,"id":"call_01_b","function":{"name":"bash","arguments":"{}"}}]}}]}` + "\n"))
	m.Write([]byte(`data: {"id":"r","choices":[{"delta":{"tool_calls":[{"index":0,"id":"","function":{"arguments":"{}"}}]}}]}` + "\n"))
	r := m.Result()
	if len(r.ToolCalls) != 2 || r.ToolCalls[0].ID != "call_00_a" || r.ToolCalls[1].ID != "call_01_b" || r.ToolCalls[0].Arguments != "{}" {
		t.Fatalf("IDs: %+v", r.ToolCalls)
	}
}

func TestParseJSONWithCachedTokensDetails(t *testing.T) {
	m := newMeter("openai-completions")
	m.Write([]byte(`{"id":"r2","choices":[{"message":{"role":"assistant","content":"hi","tool_calls":[{"id":"c","type":"function","function":{"name":"read","arguments":"{\"path\":\"a\"}"}}]}}],"usage":{"prompt_tokens":300,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":100}}}`))
	r := m.Result()
	if r.ResponseID != "r2" || r.Usage.Input != 200 || r.Usage.CacheRead != 100 || r.Usage.Output != 7 || len(r.ToolCalls) != 1 || r.ToolCalls[0].Name != "read" || r.ToolCalls[0].ID != "c" {
		t.Fatalf("JSON: %+v", r)
	}
}

type fakeRecorder struct {
	mu    sync.Mutex
	calls []Call
	chat  string
	max   int
	limit []string
}

func (f *fakeRecorder) Attribute(ip string) Attribution {
	if ip == "" {
		return Attribution{}
	}
	m := f.max
	if m == 0 {
		m = 4
	}
	return Attribution{ChatID: f.chat, SlotID: "p-1", MaxConcurrent: m}
}
func (f *fakeRecorder) LimitHit(chatID string, max int) {
	f.mu.Lock()
	f.limit = append(f.limit, chatID)
	f.mu.Unlock()
}
func (f *fakeRecorder) Record(c Call) {
	f.mu.Lock()
	f.calls = append(f.calls, c)
	f.mu.Unlock()
}

func meteredProxy(t *testing.T, rec *fakeRecorder) (*httptest.Server, *int) {
	t.Helper()
	hits := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		io.WriteString(w, deepseekSSE)
	}))
	t.Cleanup(up.Close)
	t.Setenv("TEST_KEY", "sk")
	cat, _ := config.ParseCatalog([]byte(`{"default":"p/m1","providers":[{"id":"p","upstream":"` + up.URL + `","api":"openai-completions","api_key_env":"TEST_KEY",
	  "models":[{"id":"m1","pricing":{"input":1,"output":2,"cache_read":0.5}}]}]}`))
	px := New(cat)
	px.SetRecorder(rec)
	s := httptest.NewServer(px)
	t.Cleanup(s.Close)
	return s, &hits
}

// Every call is attributed to the slot's chat and billed per tariff.
func TestRecordsCallWithCost(t *testing.T) {
	rec := &fakeRecorder{chat: "chat-1"}
	s, _ := meteredProxy(t, rec)
	resp, err := http.Post(s.URL+"/llm/p/chat/completions", "application/json", strings.NewReader(`{"model":"m1","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		rec.mu.Lock()
		n := len(rec.calls)
		rec.mu.Unlock()
		if n == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.calls) != 1 {
		t.Fatalf("calls: %d", len(rec.calls))
	}
	c := rec.calls[0]
	want := (200*1.0 + 50*2.0 + 800*0.5) / 1e6
	if c.ChatID != "chat-1" || c.SlotID != "p-1" || c.Model != "p/m1" || c.ResponseID != "resp-1" || c.Status != 200 || c.Cost < want-1e-12 || c.Cost > want+1e-12 {
		t.Fatalf("call: %+v (expected cost %v)", c, want)
	}
	if len(c.ToolCalls) != 1 || c.ToolCalls[0].Name != "bash" {
		t.Fatalf("tool calls: %+v", c.ToolCalls)
	}
}

// Calls that belong to no assigned slot are refused.
func TestRejectsUnattributedCaller(t *testing.T) {
	rec := &fakeRecorder{chat: ""}
	s, hits := meteredProxy(t, rec)
	resp, _ := http.Post(s.URL+"/llm/p/chat/completions", "application/json", strings.NewReader(`{"model":"m1"}`))
	if resp.StatusCode != http.StatusForbidden || *hits != 0 {
		t.Fatalf("unattributed: %d, upstream %d", resp.StatusCode, *hits)
	}
}

// Hard limit: at most MaxConcurrent concurrent calls per chat
// (main agent plus allowed subagents), independent of the sandbox.
func TestConcurrencyLimitPerChat(t *testing.T) {
	release := make(chan struct{})
	var mu sync.Mutex
	inFlight := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		inFlight++
		mu.Unlock()
		<-release
		w.WriteHeader(200)
		io.WriteString(w, `{"id":"x","usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer up.Close()
	t.Setenv("TEST_KEY", "sk")
	cat, _ := config.ParseCatalog([]byte(`{"default":"p/m1","providers":[{"id":"p","upstream":"` + up.URL + `","api":"openai-completions","api_key_env":"TEST_KEY","models":[{"id":"m1"}]}]}`))
	px := New(cat)
	rec := &fakeRecorder{chat: "c", max: 2}
	px.SetRecorder(rec)
	s := httptest.NewServer(px)
	defer s.Close()
	codes := make(chan int, 3)
	for i := 0; i < 3; i++ {
		go func() {
			resp, err := http.Post(s.URL+"/llm/p/chat/completions", "application/json", strings.NewReader(`{"model":"m1"}`))
			if err != nil {
				codes <- 0
				return
			}
			io.ReadAll(resp.Body)
			resp.Body.Close()
			codes <- resp.StatusCode
		}()
	}
	// The third call must return immediately with 429, the other two are still pending.
	select {
	case c := <-codes:
		if c != http.StatusTooManyRequests {
			t.Fatalf("first finished call: %d, expected 429", c)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("limit does not take effect")
	}
	close(release)
	for i := 0; i < 2; i++ {
		if c := <-codes; c != 200 {
			t.Fatalf("allowed call: %d", c)
		}
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.limit) != 1 {
		t.Fatalf("limit violation not reported: %v", rec.limit)
	}
	// After the end there is room again.
	resp, _ := http.Post(s.URL+"/llm/p/chat/completions", "application/json", strings.NewReader(`{"model":"m1"}`))
	if resp.StatusCode != 200 {
		t.Fatalf("after release: %d", resp.StatusCode)
	}
}

// L9: chunks without index (some providers) are matched by id; a chunk without both
// index and id belongs to the most recently started call.
func TestParseSSEToolCallsWithoutIndex(t *testing.T) {
	m := newMeter("openai-completions")
	m.Write([]byte(`data: {"id":"r","choices":[{"delta":{"tool_calls":[{"id":"call_a","function":{"name":"read","arguments":"{\"pa"}}]}}]}` + "\n"))
	m.Write([]byte(`data: {"id":"r","choices":[{"delta":{"tool_calls":[{"function":{"arguments":"th\":1}"}}]}}]}` + "\n"))
	m.Write([]byte(`data: {"id":"r","choices":[{"delta":{"tool_calls":[{"id":"call_b","function":{"name":"bash","arguments":"{}"}}]}}]}` + "\n"))
	m.Write([]byte(`data: {"id":"r","choices":[{"delta":{"tool_calls":[{"id":"call_a","function":{"arguments":""}}]}}]}` + "\n"))
	r := m.Result()
	if len(r.ToolCalls) != 2 || r.ToolCalls[0].ID != "call_a" || r.ToolCalls[0].Arguments != `{"path":1}` || r.ToolCalls[1].ID != "call_b" || r.ToolCalls[1].Name != "bash" {
		t.Fatalf("without index: %+v", r.ToolCalls)
	}
}

// M1: the proxy records whether the response arrived completely (finish_reason). An aborted
// stream contains tool calls that pi never executes; they are no bypass.
func TestMeterFinishReasonAndCompleteness(t *testing.T) {
	m := newMeter("openai-completions")
	m.Write([]byte(deepseekSSE))
	if r := m.Result(); r.FinishReason != "tool_calls" || !r.Complete {
		t.Fatalf("complete: %q %v", r.FinishReason, r.Complete)
	}
	cut := deepseekSSE[:strings.Index(deepseekSSE, `"finish_reason"`)-40]
	m = newMeter("openai-completions")
	m.Write([]byte(cut))
	if r := m.Result(); r.Complete || r.FinishReason != "" || len(r.ToolCalls) != 1 {
		t.Fatalf("aborted: %+v", r)
	}
	m = newMeter("openai-completions")
	m.Write([]byte(`{"id":"r2","choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}]}`))
	if r := m.Result(); r.FinishReason != "stop" || !r.Complete {
		t.Fatalf("JSON: %+v", r)
	}
	m = newMeter("anthropic-messages")
	m.Write([]byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":3}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	if r := m.Result(); r.FinishReason != "tool_use" || !r.Complete {
		t.Fatalf("Anthropic: %+v", r)
	}
}

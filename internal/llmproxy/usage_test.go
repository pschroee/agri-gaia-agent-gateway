// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package llmproxy

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agw/internal/config"
)

// Issue #151: the metered values must land in the recorded call for streamed and non-streamed answers alike.
// The double at the start is what made them disappear: the sandbox accepts gzip, the proxy forwarded that
// header, and the compressed body was unreadable to the meter while pi decoded it.
const usageSSE = `data: {"id":"r1","model":"deepseek-flash-2026","system_fingerprint":"fp_1","choices":[{"index":0,"delta":{"content":"hi"}}]}` + "\n\n" +
	`data: {"id":"r1","model":"deepseek-flash-2026","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1000,"completion_tokens":50,"total_tokens":1050,"prompt_cache_hit_tokens":800}}` + "\n\n" +
	"data: [DONE]\n\n"

const usageJSON = `{"id":"r2","model":"deepseek-flash-2026","system_fingerprint":"fp_1","choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1000,"completion_tokens":50,"total_tokens":1050,"prompt_cache_hit_tokens":800}}`

func usageProxy(t *testing.T, rec *fakeRecorder, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	up := httptest.NewServer(h)
	t.Cleanup(up.Close)
	t.Setenv("TEST_KEY", "sk")
	cat, err := config.ParseCatalog([]byte(`{"default":"p/m1","providers":[{"id":"p","upstream":"` + up.URL + `","api":"openai-completions","api_key_env":"TEST_KEY",
	  "models":[{"id":"m1","pricing":{"input":1,"output":2,"cache_read":0.5}}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	px := New(cat)
	px.SetRecorder(rec)
	s := httptest.NewServer(px)
	t.Cleanup(s.Close)
	return s
}

func postMetered(t *testing.T, srv *httptest.Server) {
	t.Helper()
	resp, err := http.Post(srv.URL+"/llm/p/chat/completions", "application/json",
		strings.NewReader(`{"model":"m1","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("call: %d %s", resp.StatusCode, b)
	}
}

// plain answers the body as it is.
func plain(contentType, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(200)
		io.WriteString(w, body)
	}
}

// gzipped answers the body with Content-Encoding: gzip, as the provider does when the client accepts it.
func gzipped(contentType, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(200)
		zw := gzip.NewWriter(w)
		io.WriteString(zw, body)
		zw.Close()
	}
}

// checkUsage asserts the counting rule: input without cache reads, output as reported, the cache reads apart.
func checkUsage(t *testing.T, calls []Call) {
	t.Helper()
	if len(calls) != 1 {
		t.Fatalf("one call expected: %+v", calls)
	}
	c := calls[0]
	if c.Usage.Input != 200 || c.Usage.Output != 50 || c.Usage.CacheRead != 800 {
		t.Fatalf("usage: %+v", c.Usage)
	}
	if c.ModelReported != "deepseek-flash-2026 fp_1" {
		t.Fatalf("reported model: %q", c.ModelReported)
	}
}

func TestUsageRecordedStreamed(t *testing.T) {
	rec := &fakeRecorder{chat: "chat-1"}
	srv := usageProxy(t, rec, plain("text/event-stream", usageSSE))
	postMetered(t, srv)
	checkUsage(t, rec.calls)
}

func TestUsageRecordedNotStreamed(t *testing.T) {
	rec := &fakeRecorder{chat: "chat-1"}
	srv := usageProxy(t, rec, plain("application/json", usageJSON))
	postMetered(t, srv)
	checkUsage(t, rec.calls)
}

// The regression: a compressed answer must not pass the meter unread.
func TestUsageRecordedCompressed(t *testing.T) {
	for _, tc := range []struct {
		name, contentType, body string
	}{
		{"streamed", "text/event-stream", usageSSE},
		{"not streamed", "application/json", usageJSON},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &fakeRecorder{chat: "chat-1"}
			srv := usageProxy(t, rec, gzipped(tc.contentType, tc.body))
			postMetered(t, srv)
			checkUsage(t, rec.calls)
		})
	}
}

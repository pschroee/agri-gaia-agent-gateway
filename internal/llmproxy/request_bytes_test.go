// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package llmproxy

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The proxy measures the request body it forwards unchanged (issue #69).
func TestRecordsRequestBytes(t *testing.T) {
	rec := &fakeRecorder{chat: "chat-1"}
	s, _ := meteredProxy(t, rec)
	body := `{"model":"m1","stream":true,"messages":[` + strings.Repeat(`"x",`, 999) + `"x"]}`
	resp, err := http.Post(s.URL+"/llm/p/chat/completions", "application/json", strings.NewReader(body))
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
	if got := rec.calls[0].RequestBytes; got != int64(len(body)) {
		t.Fatalf("request bytes: got %d, want %d", got, len(body))
	}
}

// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package sock

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"agw/internal/platform"
)

// waitBackend holds every platform call until release is closed or the call's context ends, like a call that
// waits for an approval.
type waitBackend struct {
	fakeBackend
	release chan struct{}
	started chan struct{}
	mu2     sync.Mutex
	ctxErr  error
	ended   chan struct{}
}

func newWaitBackend() *waitBackend {
	return &waitBackend{fakeBackend: fakeBackend{chat: "chat-w"}, release: make(chan struct{}), started: make(chan struct{}, 1), ended: make(chan struct{})}
}

func (w *waitBackend) PlatformCall(ctx context.Context, chatID, slotID, via string, req platform.Request) (platform.Result, error) {
	w.started <- struct{}{}
	defer close(w.ended)
	select {
	case <-w.release:
		return platform.Result{Status: "ok", HTTPStatus: 200, Body: `{"decided":"approved"}`}, nil
	case <-ctx.Done():
		w.mu2.Lock()
		w.ctxErr = ctx.Err()
		w.mu2.Unlock()
		return platform.Result{Status: "error", Message: "cancelled"}, nil
	}
}

// piServer serves pi's socket routes that matter here: /mcp and /tool/cancel, with the extension header trusted.
func piServer(t *testing.T, b Backend, trusted bool) *httptest.Server {
	t.Helper()
	h := newHandler("p-test", b, 1<<20)
	h.trustedRef = trusted
	mux := http.NewServeMux()
	mux.Handle("/mcp", h.mcp)
	mux.HandleFunc("POST /tool/cancel", h.cancelCall)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func mcpPlatformRequest(t *testing.T, url, toolCall string) *http.Request {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "platform_list_datasets", "arguments": map[string]any{}}})
	req, _ := http.NewRequest(http.MethodPost, url+"/mcp", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Protocol-Version", "2025-06-18")
	if toolCall != "" {
		req.Header.Set("X-Agw-Pi-Tool-Call", toolCall)
	}
	return req
}

func holdMCPCall(t *testing.T, hold time.Duration) {
	t.Helper()
	b := newWaitBackend()
	srv := piServer(t, b, true)
	type out struct {
		text string
		err  error
	}
	done := make(chan out, 1)
	go func() {
		resp, err := http.DefaultClient.Do(mcpPlatformRequest(t, srv.URL, "call_long"))
		if err != nil {
			done <- out{err: err}
			return
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		done <- out{text: string(raw)}
	}()
	<-b.started
	time.Sleep(hold)
	close(b.release)
	r := <-done
	if r.err != nil || !strings.Contains(r.text, "approved") {
		t.Fatalf("the call that waited %s must get the decision: %q %v", hold, r.text, r.err)
	}
}

// #251 (B3): the MCP path has no time limit of its own. The short variant runs always and proves that nothing in
// the socket server, the SDK handler or the gateway cuts a waiting call; the full one (130 s, past the 115 s of the
// demo run) runs with AGW_LONG_TESTS=1.
func TestMCPApprovalWaitShortStaysOpen(t *testing.T) { holdMCPCall(t, 2500*time.Millisecond) }

func TestMCPApprovalWaitBeyondTwoMinutes(t *testing.T) {
	if os.Getenv("AGW_LONG_TESTS") == "" {
		t.Skip("takes 130 s; set AGW_LONG_TESTS=1")
	}
	holdMCPCall(t, 130*time.Second)
}

// #251 (B3): the SDK handler does not see the client hang up. POST /tool/cancel (sent by pi's extension when it
// aborts the call) ends the waiting call with a cancelled context, which is what turns its approval into cancelled.
func TestToolCancelEndsWaitingMCPCall(t *testing.T) {
	b := newWaitBackend()
	srv := piServer(t, b, true)
	go func() {
		resp, err := http.DefaultClient.Do(mcpPlatformRequest(t, srv.URL, "call_abort"))
		if err == nil {
			resp.Body.Close()
		}
	}()
	<-b.started
	resp, err := http.Post(srv.URL+"/tool/cancel", "application/json", strings.NewReader(`{"toolCallId":"call_abort"}`))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]bool
	_ = json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()
	if !got["cancelled"] {
		t.Fatalf("cancel did not find the call: %v", got)
	}
	select {
	case <-b.ended:
	case <-time.After(3 * time.Second):
		t.Fatal("the waiting call did not stop")
	}
	b.mu2.Lock()
	defer b.mu2.Unlock()
	if b.ctxErr != context.Canceled {
		t.Fatalf("context error %v, want context.Canceled", b.ctxErr)
	}
	// An unknown or finished call is no error.
	resp, err = http.Post(srv.URL+"/tool/cancel", "application/json", strings.NewReader(`{"toolCallId":"call_none"}`))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("unknown call: %v %v", resp, err)
	}
	resp.Body.Close()
	resp, _ = http.Post(srv.URL+"/tool/cancel", "application/json", strings.NewReader(`{"toolCallId":""}`))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty id: %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// Only pi's socket takes the header: on the execution sandbox's socket a call cannot be cancelled this way.
func TestToolCancelIgnoredOnUntrustedSocket(t *testing.T) {
	b := newWaitBackend()
	srv := piServer(t, b, false)
	go func() {
		resp, err := http.DefaultClient.Do(mcpPlatformRequest(t, srv.URL, "call_x"))
		if err == nil {
			resp.Body.Close()
		}
	}()
	<-b.started
	resp, _ := http.Post(srv.URL+"/tool/cancel", "application/json", strings.NewReader(`{"toolCallId":"call_x"}`))
	var got map[string]bool
	_ = json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()
	if got["cancelled"] {
		t.Fatal("an untrusted socket must not register cancellable calls")
	}
	close(b.release)
}

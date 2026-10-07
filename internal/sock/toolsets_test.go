// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package sock

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// listenHandler serves h on a socket in a temporary directory.
func listenHandler(t *testing.T, h http.Handler) *http.Client {
	t.Helper()
	dir, err := os.MkdirTemp("", "agwsockts")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	srv, err := Listen(dir, h)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(dir, SocketName))
	}}, Timeout: 5 * time.Second}
}

// Issue #29: in a slot with several bindings (here cli,mcp,api) every platform call is attributed to
// the path it came through, not to the chat's combination: MCP and the HTTP tool at pi's socket as
// mcp and api, agw-platform and curl from bash at the sandbox's socket as cli. All of them reach the
// same check (PlatformCall) for the same chat.
func TestPlatformPathsInCombination(t *testing.T) {
	b := &platformBackend{fakeBackend: fakeBackend{chat: "chat-c", decide: "rejected"}}
	pi := listenHandler(t, NewPiHandler("p-test", b, 1<<20, &fakeRunner{}, &fakeRecorder{}))
	exec := listenHandler(t, NewHandlerRun("p-test", b, 1<<20, nil))

	if res := mcpCall(t, pi, "tools/call", map[string]any{"name": "platform_list_datasets", "arguments": map[string]any{}}); res["isError"] == true {
		t.Fatalf("MCP: %v", res)
	}
	if code, body, _ := apiCall(t, pi, "GET", "/platform-api/models", "", "", nil); code != 200 {
		t.Fatalf("platform_http: %d %s", code, body)
	}
	if code, r := cliPlatform(t, exec, "datasets", `{}`); code != 200 || r.Status != "ok" {
		t.Fatalf("agw-platform: %d %+v", code, r)
	}
	if code, body, _ := apiCall(t, exec, "PATCH", "/platform-api/datasets/5", "application/json", `{"name":"x"}`, nil); code != 403 {
		t.Fatalf("curl: %d %s", code, body)
	}
	want := []string{"chat-c|mcp|GET /datasets|", "chat-c|api|GET /models|", "chat-c|cli|GET /datasets|", "chat-c|cli|PATCH /datasets/5|{\"name\":\"x\"}"}
	b.mu.Lock()
	got := strings.Join(b.reqs, ";")
	b.mu.Unlock()
	if got != strings.Join(want, ";") {
		t.Fatalf("to the backend:\n got  %s\n want %s", got, strings.Join(want, ";"))
	}
}

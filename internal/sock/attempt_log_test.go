// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package sock

import (
	"slices"
	"testing"
)

// BM11 (no_attempt): an attempt at a platform path stays in the call log with its method and path in every
// binding, also when the gateway refuses it before the platform (a blocked path, a chat stopped for failed calls).
func TestRefusedAttemptKeepsMethodAndPath(t *testing.T) {
	b := &countingBackend{platformBackend: platformBackend{fakeBackend: fakeBackend{chat: "chat-a", decide: "approved"}}, stopAt: 99, status: 200}
	c := start(t, b)
	cliPlatform(t, c, "request", `{"method":"get","path":"/urls/basic-auth"}`)
	mcpCall(t, c, "tools/call", map[string]any{"name": "platform_request", "arguments": map[string]any{"method": "GET", "path": "/urls/basic-auth"}})
	apiCall(t, c, "GET", "/platform-api/urls/basic-auth", "", "", nil)
	b.cmu.Lock()
	b.stopped = true
	b.cmu.Unlock()
	cliPlatform(t, c, "request", `{"method":"PATCH","path":"/datasets/3/toggle-public"}`)
	mcpCall(t, c, "tools/call", map[string]any{"name": "platform_request", "arguments": map[string]any{"method": "PATCH", "path": "/datasets/4/toggle-public"}})
	apiCall(t, c, "PATCH", "/platform-api/datasets/5/toggle-public", "", "", nil)

	b.mu.Lock()
	defer b.mu.Unlock()
	for _, want := range []string{
		"GET /urls/basic-auth", "PATCH /datasets/3/toggle-public", "PATCH /datasets/4/toggle-public", "PATCH /datasets/5/toggle-public",
	} {
		n := 0
		for _, d := range b.details {
			if d == want {
				n++
			}
		}
		if want == "GET /urls/basic-auth" && n != 3 || want != "GET /urls/basic-auth" && n != 1 {
			t.Fatalf("detail %q logged %d times; details: %v", want, n, b.details)
		}
	}
	if slices.Contains(b.details, "request") {
		t.Fatalf("a request without method and path in the log: %v", b.details)
	}
}

// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package sock

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
)

func postInternetOff(t *testing.T, c *http.Client) (int, UploadResult) {
	t.Helper()
	resp, err := c.Post("http://agw/internet/off", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var r UploadResult
	_ = json.NewDecoder(resp.Body).Decode(&r)
	return resp.StatusCode, r
}

func (f *fakeBackend) snapshot() (calls, uploads []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls), slices.Clone(f.uploads)
}

// Issue #34: the agent switches internet off through all three bindings. CLI (agw-internet off) at the
// sandbox's socket as cli, MCP disable_internet as mcp, the REST tool disable_internet at pi's socket as api.
// Each call is logged as internet_off; the second one finds internet already off and still succeeds.
func TestInternetOffAllBindings(t *testing.T) {
	b := &fakeBackend{chat: "chat-7", internetOn: true}
	exec := listenHandler(t, NewHandlerRun("p-test", b, 1<<20, nil))
	pi := listenHandler(t, NewPiHandler("p-test", b, 1<<20, &fakeRunner{}, &fakeRecorder{}))

	// CLI: switches off.
	code, r := postInternetOff(t, exec)
	if code != 200 || r.Status != InternetOff {
		t.Fatalf("CLI: %d %+v", code, r)
	}
	// MCP: already off, still a success (no error).
	res := mcpCall(t, pi, "tools/call", map[string]any{"name": "disable_internet", "arguments": map[string]any{}})
	txt := res["content"].([]any)[0].(map[string]any)["text"].(string)
	if res["isError"] == true || !strings.HasPrefix(txt, "already_off") {
		t.Fatalf("MCP: %v", res)
	}
	// REST binding at pi's socket: on again, then off.
	b.mu.Lock()
	b.internetOn = true
	b.mu.Unlock()
	code, r = postInternetOff(t, pi)
	if code != 200 || r.Status != InternetOff {
		t.Fatalf("api: %d %+v", code, r)
	}
	code, r = postInternetOff(t, pi)
	if code != 200 || r.Status != InternetAlreadyOff {
		t.Fatalf("api already off: %d %+v", code, r)
	}

	calls, ups := b.snapshot()
	wantUps := []string{"internet_off|chat-7|cli", "internet_off|chat-7|mcp", "internet_off|chat-7|api", "internet_off|chat-7|api"}
	if !slices.Equal(ups, wantUps) {
		t.Fatalf("to the backend: %v", ups)
	}
	wantCalls := []string{"cli:internet_off:off", "mcp:internet_off:already off", "api:internet_off:off", "api:internet_off:already off"}
	if !slices.Equal(calls, wantCalls) {
		t.Fatalf("log: %v", calls)
	}
}

// An unassigned slot cannot switch anything: 409 (MCP: an error), logged as refused, nothing reaches the backend.
func TestInternetOffUnassigned(t *testing.T) {
	b := &fakeBackend{}
	exec := listenHandler(t, NewHandlerRun("p-test", b, 1<<20, nil))
	pi := listenHandler(t, NewPiHandler("p-test", b, 1<<20, &fakeRunner{}, &fakeRecorder{}))
	if code, _ := postInternetOff(t, exec); code != http.StatusConflict {
		t.Fatalf("CLI: %d", code)
	}
	if code, _ := postInternetOff(t, pi); code != http.StatusConflict {
		t.Fatalf("api: %d", code)
	}
	if res := mcpCall(t, pi, "tools/call", map[string]any{"name": "disable_internet", "arguments": map[string]any{}}); res["isError"] != true {
		t.Fatalf("MCP: %v", res)
	}
	calls, ups := b.snapshot()
	if len(ups) != 0 {
		t.Fatalf("passed on: %v", ups)
	}
	if !slices.Equal(calls, []string{"cli:internet_off:refused: not assigned", "api:internet_off:refused: not assigned", "mcp:internet_off:refused: not assigned"}) {
		t.Fatalf("log: %v", calls)
	}
}

// The REST binding requests internet at pi's socket; the channel is api (from the socket, not from the agent).
func TestInternetRequestViaRESTBinding(t *testing.T) {
	b := &fakeBackend{chat: "chat-8", decide: "approved"}
	pi := listenHandler(t, NewPiHandler("p-test", b, 1<<20, &fakeRunner{}, &fakeRecorder{}))
	resp, err := pi.Post("http://agw/internet", "application/json", strings.NewReader(`{"reason":"pip install torch"}`))
	if err != nil {
		t.Fatal(err)
	}
	var r UploadResult
	_ = json.NewDecoder(resp.Body).Decode(&r)
	resp.Body.Close()
	if r.Status != "approved" {
		t.Fatalf("api: %+v", r)
	}
	calls, ups := b.snapshot()
	if !slices.Equal(ups, []string{"internet|chat-8|api|pip install torch"}) || !slices.Equal(calls, []string{"api:internet:approved"}) {
		t.Fatalf("backend %v, log %v", ups, calls)
	}
}

// The MCP tool list (also unassigned) offers disable_internet next to request_internet.
func TestMCPListsDisableInternet(t *testing.T) {
	c := start(t, &fakeBackend{})
	res := mcpCall(t, c, "tools/list", map[string]any{})
	names := map[string]bool{}
	for _, tl := range res["tools"].([]any) {
		names[tl.(map[string]any)["name"].(string)] = true
	}
	if !names["disable_internet"] || !names["request_internet"] {
		t.Fatalf("tools: %v", names)
	}
}

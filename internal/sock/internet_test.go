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

	"agw/internal/netgrant"
	"agw/internal/store"
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
	resp, err := pi.Post("http://agw/internet", "application/json", strings.NewReader(`{"kind":"search","reason":"pip install torch"}`))
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

// Issue #175: a request needs a valid kind. Without one (the old form) the socket answers 400 and nothing reaches the
// backend; the refusal is logged. With one, kind, list and proposed limits arrive as the spec.
func TestInternetRequestKinds(t *testing.T) {
	b := &fakeBackend{chat: "chat-9", decide: "approved"}
	exec := listenHandler(t, NewHandlerRun("p-test", b, 1<<20, nil))
	post := func(body string) (int, string) {
		resp, err := exec.Post("http://agw/internet", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var m map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&m)
		s, _ := m["error"].(string)
		return resp.StatusCode, s
	}
	for _, body := range []string{`{"reason":"pip install x"}`, `{"kind":"","reason":"x"}`, `{"kind":"everything","reason":"x"}`} {
		code, msg := post(body)
		if code != http.StatusBadRequest || !strings.Contains(msg, "search, domains, packages, full") {
			t.Fatalf("%s: %d %q", body, code, msg)
		}
	}
	if _, ups := b.snapshot(); len(ups) != 0 {
		t.Fatalf("passed on: %v", ups)
	}
	if code, _ := post(`{"kind":"domains","reason":"flowers","domains":["download.tensorflow.org"],"minutes":45,"megabytes":800}`); code != 200 {
		t.Fatalf("domains: %d", code)
	}
	if len(b.specs) != 1 {
		t.Fatalf("specs: %+v", b.specs)
	}
	if sp := b.specs[0]; sp.Kind != netgrant.Domains || sp.Reason != "flowers" || len(sp.Domains) != 1 || sp.Domains[0] != "download.tensorflow.org" || sp.Minutes != 45 || sp.MB != 800 {
		t.Fatalf("spec: %+v", sp)
	}
}

// GET /internet lists the active grants; POST /internet/off passes an optional grant id on.
func TestInternetStatusAndOffByGrant(t *testing.T) {
	b := &fakeBackend{chat: "chat-10", internetOn: true, grants: []store.Grant{{ID: 7, Kind: "domains", Domains: []string{"a.example.org"}}}}
	exec := listenHandler(t, NewHandlerRun("p-test", b, 1<<20, nil))
	resp, err := exec.Get("http://agw/internet")
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Grants []store.Grant `json:"grants"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if resp.StatusCode != 200 || len(out.Grants) != 1 || out.Grants[0].ID != 7 {
		t.Fatalf("status: %d %+v", resp.StatusCode, out)
	}
	resp, err = exec.Post("http://agw/internet/off", "application/json", strings.NewReader(`{"grant_id":7}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(b.offGrants) != 1 || b.offGrants[0] != 7 {
		t.Fatalf("off grants: %v", b.offGrants)
	}
	calls, _ := b.snapshot()
	if !slices.Contains(calls, "cli:internet_off:off") || !slices.Contains(b.details, "grant 7") {
		t.Fatalf("log: %v %v", calls, b.details)
	}
}

// The MCP tool request_internet carries kind, reason and list; a missing kind is refused before the backend.
func TestMCPRequestInternetKind(t *testing.T) {
	b := &fakeBackend{chat: "chat-11", decide: "approved"}
	pi := listenHandler(t, NewPiHandler("p-test", b, 1<<20, &fakeRunner{}, &fakeRecorder{}))
	res := mcpCall(t, pi, "tools/call", map[string]any{"name": "request_internet", "arguments": map[string]any{"reason": "x"}})
	txt := res["content"].([]any)[0].(map[string]any)["text"].(string)
	// The tool's schema requires kind, so the call is rejected before it reaches the handler.
	if res["isError"] != true || !strings.Contains(txt, "kind") {
		t.Fatalf("no kind: %v", res)
	}
	res = mcpCall(t, pi, "tools/call", map[string]any{"name": "request_internet", "arguments": map[string]any{"kind": "domains", "reason": "x", "domains": []string{"a.example.org"}}})
	txt = res["content"].([]any)[0].(map[string]any)["text"].(string)
	if res["isError"] == true || !strings.HasPrefix(txt, "approved:") || len(b.specs) != 1 || b.specs[0].Domains[0] != "a.example.org" {
		t.Fatalf("domains: %v %+v", res, b.specs)
	}
}

// refusalBackend adds the refusals of the chat to the fake backend (issue #183).
type refusalBackend struct {
	*fakeBackend
	rs []store.WebRequest
}

func (r refusalBackend) RecentRefusals(string) []store.WebRequest { return r.rs }

func getInternet(t *testing.T, c *http.Client) map[string]any {
	t.Helper()
	resp, err := c.Get("http://agw/internet")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestInternetStatusListsRefusals(t *testing.T) {
	base := &fakeBackend{chat: "chat-7"}
	with := refusalBackend{base, []store.WebRequest{{Host: "pypi.org", Package: "torch", Reason: netgrant.CodeTooLarge, Denied: "blocked: pip package torch is too large for this sandbox"}}}
	out := getInternet(t, listenHandler(t, NewHandlerRun("p-test", with, 1<<20, nil)))
	bl, _ := out["blocked"].([]any)
	if len(bl) != 1 || bl[0].(map[string]any)["reason"] != "too_large" || !strings.Contains(bl[0].(map[string]any)["denied"].(string), "too large") {
		t.Fatalf("blocked: %v", out)
	}
	// no refusals (or a backend without the interface): the field is missing
	for _, b := range []Backend{refusalBackend{base, nil}, base} {
		if out := getInternet(t, listenHandler(t, NewHandlerRun("p-test", b, 1<<20, nil))); out["blocked"] != nil {
			t.Fatalf("blocked without refusals: %v", out)
		}
	}
}

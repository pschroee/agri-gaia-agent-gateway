// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package sock

import (
	"context"
	"net/url"
	"strings"
	"sync"
	"testing"

	"agw/internal/platform"
)

// countingBackend answers every platform call with an HTTP 404 and counts like chat.Manager does (#217): the
// outcomes it is told about, and a stop once stopAt failures arrived.
type countingBackend struct {
	platformBackend
	cmu     sync.Mutex
	keys    []string
	stopAt  int
	stopped bool
	status  int
}

func (c *countingBackend) PlatformCall(ctx context.Context, chatID, slotID, via string, req platform.Request) (platform.Result, error) {
	return platform.Result{Status: "ok", HTTPStatus: c.status, Body: `{"detail":"not found"}`}, nil
}

func (c *countingBackend) FailedCallsStopped(string) string {
	c.cmu.Lock()
	defer c.cmu.Unlock()
	if c.stopped {
		return "refused: the gateway stopped this chat after 3 failed calls; no further calls until the user writes"
	}
	return ""
}

func (c *countingBackend) NoteCallOutcome(_ context.Context, _, _, key string, res platform.Result, err error) string {
	c.cmu.Lock()
	defer c.cmu.Unlock()
	if platform.OutcomeOf(res, err) != platform.OutcomeFailed {
		return ""
	}
	c.keys = append(c.keys, key)
	if len(c.keys) >= c.stopAt {
		c.stopped = true
		return "stopped by the gateway: STOP-NOTE"
	}
	return ""
}

// The same key in every binding: get_dataset on the command line and MCP, GET /datasets/9 over REST.
func TestCountedAcrossBindings(t *testing.T) {
	b := &countingBackend{platformBackend: platformBackend{fakeBackend: fakeBackend{chat: "chat-c", decide: "approved"}}, stopAt: 99, status: 404}
	c := start(t, b)
	cliPlatform(t, c, "dataset", `{"dataset_id": 9}`)
	mcpCall(t, c, "tools/call", map[string]any{"name": "platform_get_dataset", "arguments": map[string]any{"dataset_id": 9}})
	apiCall(t, c, "GET", "/platform-api/datasets/9", "", "", nil)
	if got := strings.Join(b.keys, ","); got != "get_dataset,get_dataset,get_dataset" {
		t.Fatalf("keys: %s", got)
	}
}

// A refusal of the gateway in the REST binding (a path without a tool) counts as a failed call.
func TestRESTRefusalCounts(t *testing.T) {
	b := &countingBackend{platformBackend: platformBackend{fakeBackend: fakeBackend{chat: "chat-c", decide: "approved"}}, stopAt: 99, status: 200}
	c := start(t, b)
	code, body, _ := apiCall(t, c, "GET", "/platform-api/_agw/nothing-here", "", "", nil)
	if code != 400 || len(b.keys) != 1 {
		t.Fatalf("refusal: %d %s, counted %v", code, body, b.keys)
	}
}

// The stop notice reaches the agent in every binding; afterwards the chat is refused without running the call.
func TestStopNoteInEveryBinding(t *testing.T) {
	b := &countingBackend{platformBackend: platformBackend{fakeBackend: fakeBackend{chat: "chat-c", decide: "approved"}}, stopAt: 1, status: 404}
	c := start(t, b)
	// REST: HTTP 404 carries the note in X-Agw-Note.
	_, _, hdr := apiCall(t, c, "GET", "/platform-api/datasets/9", "", "", nil)
	if n, _ := url.PathUnescape(hdr.Get("X-Agw-Note")); !strings.Contains(n, "STOP-NOTE") {
		t.Fatalf("REST note: %q", hdr.Get("X-Agw-Note"))
	}
	// Now stopped: CLI, MCP and REST are refused, the backend is not called.
	_, r := cliPlatform(t, c, "dataset", `{"dataset_id": 9}`)
	if r.Status != "error" || !strings.Contains(r.Text(), "the gateway stopped this chat") {
		t.Fatalf("CLI: %+v", r)
	}
	res := mcpCall(t, c, "tools/call", map[string]any{"name": "platform_get_dataset", "arguments": map[string]any{"dataset_id": 9}})
	txt := res["content"].([]any)[0].(map[string]any)["text"].(string)
	if res["isError"] != true || !strings.Contains(txt, "the gateway stopped this chat") {
		t.Fatalf("MCP: %v", res)
	}
	code, body, _ := apiCall(t, c, "GET", "/platform-api/datasets/9", "", "", nil)
	if code != 400 || !strings.Contains(body, "the gateway stopped this chat") {
		t.Fatalf("REST after the stop: %d %s", code, body)
	}
	// And the stop notice in the text of a fresh chat's CLI call.
	b2 := &countingBackend{platformBackend: platformBackend{fakeBackend: fakeBackend{chat: "chat-d", decide: "approved"}}, stopAt: 1, status: 404}
	c2 := start(t, b2)
	_, r2 := cliPlatform(t, c2, "dataset", `{"dataset_id": 9}`)
	if !strings.Contains(r2.Text(), "STOP-NOTE") {
		t.Fatalf("CLI text: %q", r2.Text())
	}
	res = mcpCall(t, start(t, &countingBackend{platformBackend: platformBackend{fakeBackend: fakeBackend{chat: "chat-e", decide: "approved"}}, stopAt: 1, status: 404}),
		"tools/call", map[string]any{"name": "platform_get_dataset", "arguments": map[string]any{"dataset_id": 9}})
	if txt := res["content"].([]any)[0].(map[string]any)["text"].(string); !strings.Contains(txt, "STOP-NOTE") {
		t.Fatalf("MCP text: %q", txt)
	}
}

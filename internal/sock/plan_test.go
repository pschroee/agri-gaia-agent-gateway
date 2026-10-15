// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package sock

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"agw/internal/plan"
	"agw/internal/platform"
)

const planBody = `{"title":"t","steps":[{"method":"DELETE","path":"/models/5"}]}`

func TestProposePlanAllBindings(t *testing.T) {
	b := &platformBackend{fakeBackend: fakeBackend{chat: "chat-pl", decide: "approved"}}
	c := start(t, b)
	if code, r := cliPlatform(t, c, "propose-plan", `{"plan":`+planBody+`}`); code != 200 || r.Status != "ok" {
		t.Fatalf("CLI: %d %+v", code, r)
	}
	res := mcpCall(t, c, "tools/call", map[string]any{"name": "platform_propose_plan", "arguments": map[string]any{
		"plan": map[string]any{"title": "t", "steps": []any{map[string]any{"method": "DELETE", "path": "/models/5"}}}}})
	if res["isError"] == true {
		t.Fatalf("MCP: %v", res)
	}
	if code, _, _ := apiCall(t, c, "POST", "/platform-api/_agw/plan", "application/json", planBody, nil); code != 200 {
		t.Fatalf("REST: %d", code)
	}
	if len(b.reqs) != 3 {
		t.Fatalf("calls at the backend: %v", b.reqs)
	}
	var canon []string
	for _, rq := range b.reqs {
		parts := strings.SplitN(rq, "|", 4)
		if len(parts) != 4 || parts[2] != "POST /_agw/plan" {
			t.Fatalf("not a plan proposal: %q", rq)
		}
		cb, err := plan.Canonical([]byte(parts[3]))
		if err != nil {
			t.Fatal(err)
		}
		canon = append(canon, string(cb))
	}
	if canon[0] != canon[1] || canon[1] != canon[2] {
		t.Fatalf("bodies differ over the bindings: %v", canon)
	}
}

func TestPinPlanFiles(t *testing.T) {
	b := &uploadBackend{platformBackend: platformBackend{fakeBackend: fakeBackend{chat: "chat-pin", decide: "approved"}}}
	c := startRun(t, b, &fakeRunner{file: []byte("PNGDATA")})
	const claimed = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	body := `{"title":"upload","steps":[{"tool":"upload_model","args":{"name":"m","description":"d","format":"onnx","model_file":"/workspace/m.onnx"},"file_sha256":["` + claimed + `"]}]}`
	if code, r := cliPlatform(t, c, "propose-plan", `{"plan":`+body+`}`); code != 200 || r.Status != "ok" {
		t.Fatalf("propose: %d %+v", code, r)
	}
	if len(b.reqs) != 1 {
		t.Fatalf("calls at the backend: %v", b.reqs)
	}
	got, err := plan.ParseProposal([]byte(strings.SplitN(b.reqs[0], "|", 4)[3]))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("PNGDATA"))
	if len(got.Steps[0].FileSHA256) != 1 || got.Steps[0].FileSHA256[0] != hex.EncodeToString(sum[:]) {
		t.Fatalf("hash not pinned from the read file: %v", got.Steps[0].FileSHA256)
	}
}

// invBackend records the tool invocation of every platform call (issue #70, option b).
type invBackend struct {
	platformBackend
	invs []string
}

func (b *invBackend) PlatformCall(ctx context.Context, chatID, slotID, via string, req platform.Request) (platform.Result, error) {
	inv, ok := platform.InvocationFrom(ctx)
	b.mu.Lock()
	if ok {
		b.invs = append(b.invs, via+" "+req.String()+" tool="+inv.Tool)
	} else {
		b.invs = append(b.invs, via+" "+req.String()+" none")
	}
	b.mu.Unlock()
	if strings.HasPrefix(req.Path, "/models/") && req.Method == "GET" {
		return platform.Result{Status: "ok", HTTPStatus: 200, Body: `{"id":5,"owner":"u","name":"m1","format":"onnx","input_name":"i","input_datatype":"float32","input_shape":[1,3],"input_semantics":"NCHW","output_name":"o","output_datatype":"float32","output_shape":[1,2],"output_labels":["0","1"]}`}, nil
	}
	return b.platformBackend.PlatformCall(ctx, chatID, slotID, via, req)
}

// Only a tool with steps carries an invocation, over CLI and MCP; a raw request over REST never does.
func TestInvocationOnlyForTools(t *testing.T) {
	b := &invBackend{platformBackend: platformBackend{fakeBackend: fakeBackend{chat: "chat-inv", decide: "approved"}}}
	c := start(t, b)
	cliPlatform(t, c, "update-model", `{"model_id":"5","name":"m2"}`)
	mcpCall(t, c, "tools/call", map[string]any{"name": "platform_update_model", "arguments": map[string]any{"model_id": 5, "name": "m3"}})
	apiCall(t, c, "PATCH", "/platform-api/models/5", "application/json", `{"name":"m4"}`, nil)
	var viaTool, none int
	for _, s := range b.invs {
		switch {
		case strings.HasSuffix(s, "tool=update_model"):
			viaTool++
		case strings.HasSuffix(s, " none"):
			none++
		}
	}
	// update_model reads, writes and reads again: all three calls of CLI and of MCP carry the invocation; the
	// raw REST request is the only call without one and it is the PATCH.
	if viaTool != 6 || none != 1 || !strings.HasSuffix(b.invs[len(b.invs)-1], "PATCH /models/5 none") {
		t.Fatalf("invocations: %v", b.invs)
	}
}

// mcpCallHeader is mcpCall with extra headers: the pi extension names the tool call of the session in
// X-Agw-Tool-Call when it posts to /mcp (issue L2/#119).
func mcpCallHeader(t *testing.T, c *http.Client, method string, params any, hdr map[string]string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	req, _ := http.NewRequest(http.MethodPost, "http://agw/mcp", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Protocol-Version", "2025-06-18")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var msg map[string]any
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatalf("MCP response not JSON (%d): %s", resp.StatusCode, raw)
	}
	if msg["error"] != nil {
		t.Fatalf("MCP error: %v", msg["error"])
	}
}

// F28 (issue L2/#119): a plan proposed over any binding reaches the store with the tool call of the session, so the
// card can place it in the chat and the activity can point back at it. CLI and REST take it from the header
// X-Agw-Tool-Call (agw-platform and the REST extension set it), MCP from the header the pi extension sends on
// POST /mcp. The manager stores what arrives here as plans.tool_call_id.
func TestPlanToolCallPerBinding(t *testing.T) {
	b := &platformBackend{fakeBackend: fakeBackend{chat: "chat-tc", decide: "approved"}}
	c := start(t, b)

	// CLI.
	req, _ := http.NewRequest(http.MethodPost, "http://agw/platform/propose-plan", strings.NewReader(`{"plan":`+planBody+`}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Agw-Tool-Call", "call_cli")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CLI: %d", resp.StatusCode)
	}
	// MCP.
	mcpCallHeader(t, c, "tools/call", map[string]any{"name": "platform_propose_plan", "arguments": map[string]any{
		"plan": map[string]any{"title": "t", "steps": []any{map[string]any{"method": "DELETE", "path": "/models/5"}}}}},
		map[string]string{"X-Agw-Tool-Call": "call_mcp"})
	// REST.
	if code, _, _ := apiCall(t, c, "POST", "/platform-api/_agw/plan", "application/json", planBody, map[string]string{"X-Agw-Tool-Call": "call_api"}); code != http.StatusOK {
		t.Fatalf("REST: %d", code)
	}

	b.mu.Lock()
	got := append([]string(nil), b.toolCalls...)
	b.mu.Unlock()
	want := []string{"call_cli", "call_mcp", "call_api"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("tool calls at the backend: %v, want %v", got, want)
	}
}

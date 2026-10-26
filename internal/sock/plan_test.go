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
	"time"

	"agw/internal/delegation"
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
	body := `{"title":"upload","steps":[{"tool":"upload_model","args":{"name":"m","description":"d","format":"onnx","model_file":"/workspace/m.onnx"},"file_sha256":["` + claimed + `"],"file_bytes":[1]}]}`
	if code, r := cliPlatform(t, c, "propose-plan", `{"plan":`+body+`}`); code != 200 || r.Status != "ok" {
		t.Fatalf("propose: %d %+v", code, r)
	}
	if len(b.reqs) != 1 {
		t.Fatalf("calls at the backend: %v", b.reqs)
	}
	got, err := plan.ParseProposal([]byte(strings.SplitN(b.reqs[0], "|", 4)[3]), plan.Limits{MaxDuration: 8 * time.Hour, MaxLLMCost: 5})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("PNGDATA"))
	if len(got.Steps[0].FileSHA256) != 1 || got.Steps[0].FileSHA256[0] != hex.EncodeToString(sum[:]) {
		t.Fatalf("hash not pinned from the read file: %v", got.Steps[0].FileSHA256)
	}
	if len(got.Steps[0].FileBytes) != 1 || got.Steps[0].FileBytes[0] != int64(len("PNGDATA")) {
		t.Fatalf("size not pinned from the read file (the agent's own value must be dropped): %v", got.Steps[0].FileBytes)
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

// A-E1: an autonomous proposal reaches the manager unchanged over CLI, MCP and REST (the grant is derived
// from it, so the three bindings must carry the same body). That the derived grant bounds every binding
// alike is checked in internal/chat (the sock backend fakes PlatformCall and never runs the guard).
func TestAutonomousPlanSameOverCLIMCPREST(t *testing.T) {
	b := &platformBackend{fakeBackend: fakeBackend{chat: "chat-auto", decide: "approved"}}
	c := start(t, b)
	body := `{"title":"Autonom","mode":"autonomous","budget":{"max_duration_minutes":60,"max_llm_cost_usd":1,"max_auto_turns":5},"steps":[{"method":"POST","path":"/datasets"}]}`
	if code, r := cliPlatform(t, c, "propose-plan", `{"plan":`+body+`}`); code != 200 || r.Status != "ok" {
		t.Fatalf("CLI: %d %+v", code, r)
	}
	res := mcpCall(t, c, "tools/call", map[string]any{"name": "platform_propose_plan", "arguments": map[string]any{
		"plan": map[string]any{"title": "Autonom", "mode": "autonomous",
			"budget": map[string]any{"max_duration_minutes": 60, "max_llm_cost_usd": 1, "max_auto_turns": 5},
			"steps":  []any{map[string]any{"method": "POST", "path": "/datasets"}}}}})
	if res["isError"] == true {
		t.Fatalf("MCP: %v", res)
	}
	if code, _, _ := apiCall(t, c, "POST", "/platform-api/_agw/plan", "application/json", body, nil); code != 200 {
		t.Fatalf("REST: %d", code)
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
	if len(canon) != 3 || canon[0] != canon[1] || canon[1] != canon[2] {
		t.Fatalf("bodies differ over the bindings: %v", canon)
	}
}

// planCheckBackend answers a plan proposal like Manager.ProposePlan does for the parts that matter here
// (issue #218): it parses the pinned body and builds the steps, and refuses with every problem at once.
type planCheckBackend struct{ platformBackend }

func (b *planCheckBackend) PlatformCall(ctx context.Context, chatID, slotID, via string, req platform.Request) (platform.Result, error) {
	res, err := b.platformBackend.PlatformCall(ctx, chatID, slotID, via, req)
	if req.Path != platform.PlanPath {
		return res, err
	}
	p, perr := plan.ParseProposal(req.Body, plan.Limits{MaxDuration: 8 * time.Hour, MaxLLMCost: 5})
	var berr error
	if len(p.Steps) > 0 {
		_, berr = plan.Build(p, func(r platform.Request) (string, string, bool) {
			a := delegation.Classify(r)
			return string(a.Effect), a.Action, a.ConfirmEach
		})
	}
	if e := plan.Join(perr, berr); e != nil {
		return platform.Result{Status: "error", Message: e.Error()}, nil
	}
	return res, err
}

// recordedPlan is the body of the n-th proposal that reached the backend.
func recordedPlan(t *testing.T, b *planCheckBackend, n int) plan.Proposal {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.reqs) <= n {
		t.Fatalf("proposals at the backend: %v", b.reqs)
	}
	p, _ := plan.ParseProposal([]byte(strings.SplitN(b.reqs[n], "|", 4)[3]), plan.Limits{MaxDuration: 8 * time.Hour, MaxLLMCost: 5})
	return p
}

// With 31 files in step 1 and an unknown tool in step 2, the answer names the tool, not "0 file_sha256 for 31
// uploads" (the demo chat, attempt 3): the files of step 1 are pinned all the same.
func TestMCPProposePlanPinsDespiteOtherError(t *testing.T) {
	b := &planCheckBackend{platformBackend{fakeBackend: fakeBackend{chat: "chat-pin2", decide: "approved"}}}
	c := startRun(t, b, &fakeRunner{file: []byte("PNG")})
	files := make([]any, 31)
	for i := range files {
		files[i] = "/workspace/img" + string(rune('a'+i/26)) + string(rune('a'+i%26)) + ".png"
	}
	res := mcpCall(t, c, "tools/call", map[string]any{"name": "platform_propose_plan", "arguments": map[string]any{
		"plan": map[string]any{"title": "t", "steps": []any{
			map[string]any{"tool": "upload_dataset", "args": map[string]any{"name": "n", "description": "d", "files": files}},
			map[string]any{"tool": "nonsense_tool"}}}}})
	txt := mcpText(t, res)
	if res["isError"] != true || !strings.Contains(txt, "step 2: unknown tool") || strings.Contains(txt, "file_sha256") {
		t.Fatalf("answer: %v", txt)
	}
	p := recordedPlan(t, b, 0)
	if len(p.Steps) != 2 || len(p.Steps[0].FileSHA256) != 31 || len(p.Steps[0].FileBytes) != 31 {
		t.Fatalf("the 31 files must be pinned: %+v", p.Steps[0])
	}
}

// The steps as JSON text reach the orchestrator unpacked, and the go-sdk does not turn them away against the schema.
func TestMCPProposePlanStepsAsText(t *testing.T) {
	b := &planCheckBackend{platformBackend{fakeBackend: fakeBackend{chat: "chat-text", decide: "approved"}}}
	c := start(t, b)
	res := mcpCall(t, c, "tools/call", map[string]any{"name": "platform_propose_plan", "arguments": map[string]any{
		"plan": map[string]any{"title": "t", "steps": `[{"method":"DELETE","path":"/models/5"}]`}}})
	if res["isError"] == true {
		t.Fatalf("steps as text refused: %v", mcpText(t, res))
	}
	if p := recordedPlan(t, b, 0); len(p.Steps) != 1 || p.Steps[0].Path != "/models/5" {
		t.Fatalf("steps not unpacked: %+v", p)
	}
	// The whole plan as text is unpacked as well.
	res = mcpCall(t, c, "tools/call", map[string]any{"name": "platform_propose_plan", "arguments": map[string]any{
		"plan": `{"title":"t","steps":[{"method":"DELETE","path":"/models/6"}]}`}})
	if res["isError"] == true {
		t.Fatalf("plan as text refused: %v", mcpText(t, res))
	}
}

// badPlan has three problems: a budget above the limit, an unknown tool, a placeholder on a step that only changes
// an object. Every binding answers with all three in one message.
const badPlan = `{"title":"t","mode":"autonomous","budget":{"max_auto_turns":80},"steps":[
 {"tool":"model_from_training","args":{"train_container_id":7}},
 {"tool":"nonsense_tool"},
 {"tool":"update_model","args":{"model_id":"{{step1.id}}","name":"a"}},
 {"tool":"update_model","args":{"model_id":"{{step3.id}}","name":"b"}}]}`

var badPlanWants = []string{"plan refused, 3 problems", "max_auto_turns is 80, at most 50", "step 2: unknown tool", "step 4: ", "so write"}

func TestRESTProposePlanCollects(t *testing.T) {
	b := &planCheckBackend{platformBackend{fakeBackend: fakeBackend{chat: "chat-rest-c", decide: "approved"}}}
	c := start(t, b)
	_, body, _ := apiCall(t, c, "POST", "/platform-api/_agw/plan", "application/json", badPlan, nil)
	for _, want := range badPlanWants {
		if !strings.Contains(body, want) {
			t.Errorf("REST answer misses %q: %s", want, body)
		}
	}
}

func TestCLIProposePlanCollects(t *testing.T) {
	b := &planCheckBackend{platformBackend{fakeBackend: fakeBackend{chat: "chat-cli-c", decide: "approved"}}}
	c := start(t, b)
	_, r := cliPlatform(t, c, "propose-plan", `{"plan":`+badPlan+`}`)
	for _, want := range badPlanWants {
		if !strings.Contains(r.Message, want) {
			t.Errorf("CLI answer misses %q: %+v", want, r)
		}
	}
	res := mcpCall(t, c, "tools/call", map[string]any{"name": "platform_propose_plan", "arguments": map[string]any{"plan": json.RawMessage(badPlan)}})
	for _, want := range badPlanWants {
		if !strings.Contains(mcpText(t, res), want) {
			t.Errorf("MCP answer misses %q: %s", want, mcpText(t, res))
		}
	}
}

// A path given twice and the annotation file in files as well are pinned once (#233): the proposal is accepted
// and the step carries two pins, not four.
func TestMCPProposePlanDuplicateAnnotation(t *testing.T) {
	b := &planCheckBackend{platformBackend{fakeBackend: fakeBackend{chat: "chat-dup", decide: "approved"}}}
	c := startRun(t, b, &fakeRunner{file: []byte("PNG")})
	res := mcpCall(t, c, "tools/call", map[string]any{"name": "platform_propose_plan", "arguments": map[string]any{
		"plan": map[string]any{"title": "t", "steps": []any{
			map[string]any{"tool": "upload_dataset", "args": map[string]any{"name": "n", "description": "d",
				"files":           []any{"/workspace/a.png", "/workspace/a.png", "/workspace/annotations.xml"},
				"annotation_file": "/workspace/annotations.xml"}}}}}})
	if res["isError"] == true {
		t.Fatalf("proposal refused: %v", mcpText(t, res))
	}
	p := recordedPlan(t, b, 0)
	if len(p.Steps) != 1 || len(p.Steps[0].FileSHA256) != 2 || len(p.Steps[0].FileBytes) != 2 {
		t.Fatalf("want 2 pinned files: %+v", p.Steps[0])
	}
}

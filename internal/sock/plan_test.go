// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package sock

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"agw/internal/plan"
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

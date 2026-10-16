// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// The check tools are in the table (and therefore in every binding that lists the platform tools, with
// the CLI command from the same entry, R0.17).
func TestAssessToolsInTable(t *testing.T) {
	tool, ok := Lookup("assess_status")
	if !ok {
		t.Fatal("the tool assess_status is missing")
	}
	if tool.CLI != "assess-status" {
		t.Errorf("CLI name: %q", tool.CLI)
	}
	if tool.Write {
		t.Error("a status read is not a write")
	}
	if !strings.Contains(tool.Desc, "The report's header is the verdict to start your reply with.") {
		t.Errorf("description: %q", tool.Desc)
	}
	if tool.MCPName() != "platform_assess_status" {
		t.Errorf("MCP name: %q", tool.MCPName())
	}
	if len(tool.Params) != 1 || tool.Params[0].Name != "check_id" || !tool.Params[0].Required {
		t.Fatalf("parameters: %+v", tool.Params)
	}
	// The call goes to the check path with the id in it.
	req, err := tool.Build(json.RawMessage(`{"check_id":"11111111-2222-3333-4444-555555555555"}`))
	if err != nil {
		t.Fatal(err)
	}
	if req.Method != http.MethodGet || req.Path != AssessPrefix+"/checks/11111111-2222-3333-4444-555555555555" {
		t.Errorf("call: %s %s", req.Method, req.Path)
	}
	// A missing or misshapen id is refused before anything is sent.
	if _, err := tool.Build(json.RawMessage(`{}`)); err == nil {
		t.Error("a call without a check id is built")
	}
	if _, err := tool.Build(json.RawMessage(`{"check_id":"cheque 1"}`)); err == nil {
		t.Error("a call with a misshapen check id is built")
	}
}

// The check paths belong to the check tools: the raw request tool refuses them (R0), because the gateway
// decides what a check runs on and the agent must not name the measured set.
func TestAssessPathRefusedForRawCalls(t *testing.T) {
	raw, ok := Lookup("request")
	if !ok {
		t.Fatal("the request tool is missing")
	}
	_, err := raw.Build(json.RawMessage(`{"method":"GET","path":"` + AssessPrefix + `/checks/11111111-2222-3333-4444-555555555555"}`))
	if err == nil || !strings.Contains(err.Error(), "check calls go through the assess tools") {
		t.Fatalf("raw call to a check path: %v", err)
	}
	if _, err := raw.Build(json.RawMessage(`{"method":"POST","path":"` + AssessPrefix + `/checks"}`)); err == nil {
		t.Error("a raw write of the check paths must be refused")
	}
	if why := AssessCallRefusal(Request{Method: http.MethodGet, Path: AssessPrefix}); why == "" {
		t.Error("the refusal of the check prefix itself is missing")
	}
	for _, p := range []string{"/datasets", "/_agw/rights", "/_agw/assessment", "/_agw/assessments"} {
		if why := AssessCallRefusal(Request{Method: http.MethodGet, Path: p}); why != "" {
			t.Errorf("path %s refused: %s", p, why)
		}
	}
}

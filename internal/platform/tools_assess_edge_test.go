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

// The tool of the deployment check (R3.1) is in the table, names its CLI command and sends its call to
// the check path with the deployment in the path and the model in the query.
func TestDeploymentCheckTool(t *testing.T) {
	tool, ok := Lookup("deployment_check")
	if !ok {
		t.Fatal("the tool deployment_check is missing")
	}
	if tool.CLI != "deployment-check" {
		t.Errorf("CLI name: %q", tool.CLI)
	}
	if tool.Write {
		t.Error("the test requests of a check are a read")
	}
	if tool.MCPName() != "platform_deployment_check" {
		t.Errorf("MCP name: %q", tool.MCPName())
	}
	if !strings.Contains(tool.Desc, "Use it after platform_deployment_status shows deployed.") ||
		!strings.Contains(tool.Desc, "accuracy comes from assess_model") {
		t.Errorf("description: %q", tool.Desc)
	}
	if len(tool.Params) != 2 || tool.Params[0].Name != "deployment_id" || !tool.Params[0].Required ||
		tool.Params[1].Name != "model_id" || !tool.Params[1].Required {
		t.Fatalf("parameters: %+v", tool.Params)
	}
	req, err := tool.Build(json.RawMessage(`{"deployment_id":9,"model_id":5}`))
	if err != nil {
		t.Fatal(err)
	}
	if req.Method != http.MethodGet || req.Path != AssessDeploymentPath+"9" || req.Query["model_id"] != "5" {
		t.Errorf("call: %s %s %v", req.Method, req.Path, req.Query)
	}
	// A missing or misshapen identifier is refused before anything is sent.
	for _, args := range []string{`{}`, `{"deployment_id":9}`, `{"model_id":5}`,
		`{"deployment_id":0,"model_id":5}`, `{"deployment_id":9,"model_id":"drei"}`} {
		if _, err := tool.Build(json.RawMessage(args)); err == nil {
			t.Errorf("a call with %s is built", args)
		}
	}
}

// The deployment check goes through the check tools as well: the raw request tool refuses its path (R3,
// pattern of R0).
func TestDeploymentCheckPathRefusedForRawCalls(t *testing.T) {
	raw, ok := Lookup("request")
	if !ok {
		t.Fatal("the request tool is missing")
	}
	_, err := raw.Build(json.RawMessage(`{"method":"GET","path":"` + AssessDeploymentPath + `9"}`))
	if err == nil || !strings.Contains(err.Error(), "check calls go through the assess tools") {
		t.Fatalf("raw call to the deployment check: %v", err)
	}
}

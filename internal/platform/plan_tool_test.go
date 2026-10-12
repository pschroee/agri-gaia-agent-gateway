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

func TestPlanToolBuildsPlanPath(t *testing.T) {
	for _, name := range []string{"propose_plan", "platform_propose_plan", "propose-plan"} {
		tool, ok := Lookup(name)
		if !ok || !tool.Write {
			t.Fatalf("Lookup(%q) = %v, write %v", name, ok, tool.Write)
		}
	}
	tool, _ := Lookup("propose_plan")
	req, err := tool.Build(json.RawMessage(`{"plan":{"title":"t","steps":[{"method":"DELETE","path":"/models/5"}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if req.Method != http.MethodPost || req.Path != PlanPath {
		t.Fatalf("request: %s %s", req.Method, req.Path)
	}
	if !strings.Contains(string(req.Body), `"steps":[{"method":"DELETE","path":"/models/5"}]`) {
		t.Fatalf("body: %s", req.Body)
	}
	if got := tool.Schema()["required"]; got == nil {
		t.Fatal("plan parameter is not required in the schema")
	}
}

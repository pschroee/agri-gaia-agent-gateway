// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"
)

// between returns the text between two markers, empty if one is missing.
func between(s, from, to string) string {
	i := strings.Index(s, from)
	if i < 0 {
		return ""
	}
	s = s[i+len(from):]
	j := strings.Index(s, to)
	if j < 0 {
		return ""
	}
	return s[:j]
}

func names(list string) []string {
	var out []string
	for _, n := range strings.Split(list, ",") {
		if n = strings.TrimSpace(n); n != "" {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// The description names every tool that can be a step: all writing tools but propose_plan itself (issue #218).
// A new writing tool that is not in the text fails here.
func TestPlanDescriptionListsWriteTools(t *testing.T) {
	var want []string
	for _, tool := range Tools {
		if tool.Write && tool.Name != "propose_plan" {
			want = append(want, tool.Name)
		}
	}
	sort.Strings(want)
	got := names(between(PlanToolDescription, "Tools that can be steps: ", ". Reading calls"))
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("the description lists\n  %v\nthe writing tools are\n  %v", got, want)
	}
}

// The MCP input schema of propose_plan has every field of the plan, with the bounds the gateway enforces.
func TestPlanSchemaInMCP(t *testing.T) {
	tool := mustLookup(t, "propose_plan")
	raw, err := json.Marshal(tool.Schema())
	if err != nil {
		t.Fatal(err)
	}
	var s map[string]any
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	at := func(v any, path ...string) any {
		for _, p := range path {
			m, ok := v.(map[string]any)
			if !ok {
				t.Fatalf("no object at %s in %s", p, raw)
			}
			v = m[p]
		}
		return v
	}
	plan := at(s, "properties", "plan")
	if at(plan, "type") != "object" || at(plan, "additionalProperties") != false {
		t.Errorf("plan: %v", plan)
	}
	if at(plan, "properties", "steps", "items", "properties", "tool") == nil {
		t.Errorf("the schema lacks steps.items.properties.tool: %s", raw)
	}
	if at(plan, "properties", "steps", "items", "properties", "body") == nil || at(plan, "properties", "replaces") == nil {
		t.Errorf("fields missing: %s", raw)
	}
	if got := at(plan, "properties", "budget", "properties", "max_auto_turns", "maximum"); got != float64(50) {
		t.Errorf("max_auto_turns maximum: %v", got)
	}
	// pi checks MCP arguments against the schema before the gateway sees them: steps as JSON text must pass (#218).
	if typ, _ := at(plan, "properties", "steps", "type").([]any); len(typ) != 2 || typ[0] != "array" || typ[1] != "string" {
		t.Errorf("steps type: %v, want [array string]", at(plan, "properties", "steps", "type"))
	}
	if got := at(plan, "properties", "steps", "maxItems"); got != float64(10) {
		t.Errorf("maxItems: %v", got)
	}
	if got := at(plan, "properties", "steps", "items", "properties", "max_attempts", "maximum"); got != float64(3) {
		t.Errorf("max_attempts maximum: %v", got)
	}
	for _, hidden := range []string{"file_sha256", "file_bytes", "file_error"} {
		if strings.Contains(string(raw), hidden) {
			t.Errorf("%s belongs to the socket, not to the schema", hidden)
		}
	}
	if req, _ := at(s, "required").([]any); len(req) != 1 || req[0] != "plan" {
		t.Errorf("required: %v", req)
	}
	// A tool without a schema of its own keeps the generated one.
	if other := mustLookup(t, "start_training").Schema(); at(other, "properties", "train_container_id", "type") != "integer" {
		t.Errorf("start_training: %v", other)
	}
}

func TestGatewayIndexDetailPlanSchema(t *testing.T) {
	got := GatewayIndexDetail("/_agw/plan")
	if !strings.Contains(got, "schema: {") || !strings.Contains(got, `"max_auto_turns"`) || !strings.Contains(got, `"required":["title","steps"]`) {
		t.Fatalf("detail:\n%s", got)
	}
	if strings.Contains(GatewayIndexDetail("/_agw/rights"), "schema:") {
		t.Fatal("a tool without a schema prints none")
	}
}

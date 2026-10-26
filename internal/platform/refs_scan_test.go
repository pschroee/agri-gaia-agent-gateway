// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"agw/internal/toolset"
)

// sourceLiterals calls f for every string literal of the non-test Go files under internal/ and cmd/.
func sourceLiterals(t *testing.T, f func(file string, line int, text string)) {
	t.Helper()
	fset := token.NewFileSet()
	for _, root := range []string{"../../internal", "../../cmd"} {
		err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return err
			}
			file, perr := parser.ParseFile(fset, p, nil, 0)
			if perr != nil {
				t.Fatalf("%s: %v", p, perr)
			}
			ast.Inspect(file, func(n ast.Node) bool {
				if b, ok := n.(*ast.BasicLit); ok && b.Kind == token.STRING {
					if s, uerr := strconv.Unquote(b.Value); uerr == nil {
						f(filepath.ToSlash(strings.TrimPrefix(p, "../../")), fset.Position(b.Pos()).Line, s)
					}
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

// Every marker in the source names a tool of the table (Render leaves an unknown one in place).
func TestToolMarkersKnown(t *testing.T) {
	n := 0
	sourceLiterals(t, func(file string, line int, s string) {
		for _, m := range toolRefRe.FindAllStringSubmatch(s, -1) {
			n++
			if _, ok := toolByName(m[1]); !ok {
				t.Errorf("%s:%d: the marker names no tool: %s", file, line, m[0])
			}
		}
	})
	if n < 100 {
		t.Errorf("only %d markers found; the scan misses the sources", n)
	}
}

// Where a bare tool name is allowed in a literal, with the reason.
var bareNameAllowed = map[string]string{
	"internal/store/":                   "SQL tables (annotation_functions, notebook_runs)",
	"internal/delegation/annotation.go": "labels of conformance cases",
	"internal/platform/plan_tool.go":    "the step syntax of a plan names the tool by its name in every binding",
	"internal/platform/refs.go":         "the forms of the tools",
	"internal/worker/worker.go":         "capabilityTool, sendFile and internetSwitch name the tools per binding",
}

// A text for the agent names a tool by the marker, never by its name: the same tool has a different name in each
// binding. A literal that is exactly a name (a field, a map key, a case) is code, not text.
func TestAgentTextsUseToolRefs(t *testing.T) {
	var alts []string
	seen := map[string]bool{}
	for _, tool := range Tools {
		for _, n := range []string{tool.Name, tool.CLI} {
			if !seen[n] && strings.ContainsAny(n, "_-") {
				seen[n] = true
				alts = append(alts, regexp.QuoteMeta(n))
			}
		}
	}
	sort.Slice(alts, func(i, j int) bool { return len(alts[i]) > len(alts[j]) })
	var all []string
	for _, tool := range Tools {
		all = append(all, regexp.QuoteMeta(tool.Name))
	}
	bare := regexp.MustCompile(`(^|[^A-Za-z0-9_/.{-])(` + strings.Join(alts, "|") + `)($|[^A-Za-z0-9_/}-])`)
	prefixed := regexp.MustCompile(`(^|[^A-Za-z0-9_])(mcp_)?platform_(` + strings.Join(all, "|") + `)($|[^A-Za-z0-9_])`)
	exact := regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	sourceLiterals(t, func(file string, line int, s string) {
		for prefix := range bareNameAllowed {
			if strings.HasPrefix(file, prefix) {
				return
			}
		}
		s = toolRefRe.ReplaceAllString(s, "")
		if exact.MatchString(strings.TrimSpace(s)) {
			return
		}
		if m := bare.FindStringSubmatch(s); m != nil {
			t.Errorf("%s:%d: tool name %q outside a marker (use %s)", file, line, m[2], ToolRef(strings.ReplaceAll(m[2], "-", "_")))
		}
		if m := prefixed.FindStringSubmatch(s); m != nil {
			t.Errorf("%s:%d: %q outside a marker", file, line, strings.TrimSpace(m[0]))
		}
	})
}

// After rendering, no description of a tool carries a marker, in any binding, and the MCP form of a reference is
// the MCP name.
func TestDescriptionsRenderedPerBinding(t *testing.T) {
	for _, b := range toolset.Order {
		for _, tool := range Tools {
			r := tool.RenderedFor(b, toolset.Of(b))
			if strings.Contains(r.Desc, "{{tool:") {
				t.Errorf("%s in %s: marker in the description", tool.Name, b)
			}
			for _, p := range r.Params {
				if strings.Contains(p.Desc, "{{tool:") {
					t.Errorf("%s in %s: marker in %s", tool.Name, b, p.Name)
				}
			}
		}
	}
	create, _ := toolByName("create_training")
	if d := create.RenderedFor(toolset.MCP, toolset.Of(toolset.MCP)).Desc; !strings.Contains(d, "mcp_platform_train_options") {
		t.Errorf("MCP description of create_training: %s", d)
	}
	if d := create.RenderedFor(toolset.CLI, toolset.Of(toolset.CLI)).Desc; !strings.Contains(d, "agw-platform train-options") {
		t.Errorf("CLI description of create_training: %s", d)
	}
	// The original keeps its markers: the table is shared.
	if !strings.Contains(create.Desc, "{{tool:train_options}}") {
		t.Error("RenderedFor changed the tool of the table")
	}
}

// The tool names in the example of a plan are step syntax and stay bare; each one must be a tool of the table (the
// text once named model_from_training, which no step accepts).
func TestPlanToolDescriptionToolsExist(t *testing.T) {
	params := map[string]bool{"max_attempts": true, "max_duration_minutes": true, "max_llm_cost_usd": true, "max_auto_turns": true,
		"dataset_id": true, "train_container_id": true, "plan_id": true, "model_id": true, "image_id": true, "deployment_id": true}
	for _, name := range regexp.MustCompile(`[a-z]+(?:_[a-z]+)+`).FindAllString(PlanToolDescription, -1) {
		if params[name] {
			continue
		}
		if _, ok := Lookup(name); !ok {
			t.Errorf("PlanToolDescription names %q, which is no tool", name)
		}
	}
	if !strings.Contains(PlanToolDescription, "{{tool:capabilities}}") {
		t.Error("PlanToolDescription does not refer to capabilities by marker")
	}
}

// The static text of the HTTP tool names paths, not tools.
func TestAPIExtNamesNoTools(t *testing.T) {
	raw, err := os.ReadFile("../../images/agw-basis/ext/api.ts")
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range Tools {
		if !strings.Contains(tool.Name, "_") {
			continue
		}
		if regexp.MustCompile(`(^|[^A-Za-z0-9_])(platform_|mcp_platform_)?` + regexp.QuoteMeta(tool.Name) + `($|[^A-Za-z0-9_])`).Match(raw) {
			t.Errorf("api.ts names the tool %s", tool.Name)
		}
	}
}

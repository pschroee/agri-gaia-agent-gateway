// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package main

import (
	"strings"
	"testing"

	"agw/internal/platform"
)

// The help of propose-plan prints the schema of the plan under its argument (issue #218); a tool without a
// schema of its own prints none.
func TestToolUsagePrintsSchema(t *testing.T) {
	tool, _ := platform.Lookup("propose-plan")
	var b strings.Builder
	toolUsage(&b, tool)
	out := b.String()
	if !strings.Contains(out, "\n    schema: {") || !strings.Contains(out, `"max_auto_turns"`) {
		t.Fatalf("usage:\n%s", out)
	}
	if strings.Count(out, "schema:") != 1 || strings.Contains(out, "\\u003c") {
		t.Fatalf("one compact schema line without HTML escapes expected:\n%s", out)
	}
	other, _ := platform.Lookup("start-training")
	b.Reset()
	toolUsage(&b, other)
	if strings.Contains(b.String(), "schema:") {
		t.Fatalf("start-training: %s", b.String())
	}
}

// The help of build-image prints the enum of its runtime argument (#230).
func TestBuildImageHelpShowsRuntimeSchema(t *testing.T) {
	tool, _ := platform.Lookup("build-image")
	var b strings.Builder
	toolUsage(&b, tool)
	out := b.String()
	if !strings.Contains(out, "--runtime") || !strings.Contains(out, `schema: {"enum":["triton","ovms"],"type":"string"}`) {
		t.Fatalf("usage:\n%s", out)
	}
}

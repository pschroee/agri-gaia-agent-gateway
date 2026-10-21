// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"strings"
	"testing"

	"agw/internal/platform"
)

// #200: the help of a command and the text the command prints name other tools as agw-platform commands,
// never as MCP names or markers.
func TestHelpUsesCLIForms(t *testing.T) {
	tool, ok := platform.Lookup("create-training")
	if !ok {
		t.Fatal("create-training")
	}
	var b bytes.Buffer
	toolUsage(&b, tool)
	help := b.String()
	if !strings.Contains(help, "agw-platform train-options") || strings.Contains(help, "platform_") || strings.Contains(help, "{{tool:") {
		t.Errorf("create-training --help:\n%s", help)
	}
	b.Reset()
	platformUsage(&b)
	if !strings.Contains(b.String(), "(agw-platform rights shows them)") || strings.Contains(b.String(), "{{tool:") {
		t.Errorf("agw-platform --help:\n%s", b.String())
	}
	denied := cliRender(platform.Result{Status: "denied", Message: "x"}.Text())
	if !strings.Contains(denied, "(agw-platform rights shows which rights apply)") {
		t.Errorf("denied text: %s", denied)
	}
}

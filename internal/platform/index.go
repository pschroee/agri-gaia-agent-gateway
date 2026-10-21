// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

import (
	"fmt"
	"strings"
)

// Synopsis is the argument part of the command line of the tool, the same text in
// `agw-platform --help` and in the command index of the system note (#198).
func (t Tool) Synopsis() string {
	var b strings.Builder
	afterList := false // after a list everything else only works as an option
	for _, p := range t.Params {
		switch {
		case p.Required && p.Type == "array":
			fmt.Fprintf(&b, " <%s …>", p.Name)
			afterList = true
		case p.Required:
			fmt.Fprintf(&b, " <%s>", p.Name)
		case p.Type == "string" && !afterList:
			fmt.Fprintf(&b, " [%s]", p.Name)
		case p.Name == "query":
			b.WriteString(" [--query k=v …]")
		case p.Type == "array":
			fmt.Fprintf(&b, " [--%s x …]", p.Name)
		default:
			fmt.Fprintf(&b, " [--%s …]", p.Name)
		}
	}
	return b.String()
}

// CLIIndex lists every agw-platform command with its arguments, one line per tool of the
// table, writing ones marked "(approval)". The system note of the CLI binding carries it (#198).
func CLIIndex() string {
	var b strings.Builder
	for _, t := range Tools {
		fmt.Fprintf(&b, "agw-platform %s%s", t.CLI, t.Synopsis())
		if t.Write {
			b.WriteString(" (approval)")
		}
		b.WriteString("\n")
	}
	return b.String()
}

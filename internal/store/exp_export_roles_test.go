// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package store

import "testing"

// BM1: the role is the last part of the cell identifier and may be any role name, hyphens included.
func TestExportRoleFromCell(t *testing.T) {
	cases := []struct{ cell, binding, protection, attack, role string }{
		{"cli-service-no-deepseek-flash", "cli", "service", "no", "deepseek-flash"},
		{"cli-service-yes-main", "cli", "service", "yes", "main"},
		{"cli-mcp-none-yes-main", "cli-mcp", "none", "yes", "main"},
		{"mcp-cli-service-no-deepseek-flash", "mcp-cli", "service", "no", "deepseek-flash"},
		{"api-service-none-second", "api", "service", "none", "second"},
		{"cli-service-no", "cli", "service", "no", ""},
		{"cli-service-no-Main", "cli", "service", "no", ""},
		{"nonsense", "", "", "", ""},
	}
	for _, c := range cases {
		b, p, a, r := cellParts(c.cell)
		if b != c.binding || p != c.protection || a != c.attack || r != c.role {
			t.Errorf("%s: got %q %q %q %q, want %q %q %q %q", c.cell, b, p, a, r, c.binding, c.protection, c.attack, c.role)
		}
	}
}

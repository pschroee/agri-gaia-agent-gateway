// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package testpg

import (
	"regexp"
	"testing"
)

// The store accepts only lower-case identifiers, and Postgres cuts names at 63 bytes.
var schemaName = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

func TestSchemaIsUsableAndUnique(t *testing.T) {
	seen := make(map[string]bool, 1000)
	for i := 0; i < 1000; i++ {
		name := Schema("test")
		if !schemaName.MatchString(name) {
			t.Fatalf("not a schema name the store accepts: %q", name)
		}
		if len(name) > 63 {
			t.Fatalf("longer than a Postgres identifier: %q", name)
		}
		if seen[name] {
			t.Fatalf("name handed out twice: %q", name)
		}
		seen[name] = true
	}
}

func TestSchemaKeepsThePrefix(t *testing.T) {
	if name := Schema("apitest_plans"); len(name) < len("apitest_plans")+1 || name[:len("apitest_plans")] != "apitest_plans" {
		t.Fatalf("prefix lost: %q", name)
	}
}

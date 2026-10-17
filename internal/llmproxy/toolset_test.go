// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package llmproxy

import "testing"

// X5.6: the same tool list in a different key order has the same checksum; a request without tools has none.
func TestToolsetDigestStable(t *testing.T) {
	a := []byte(`{"model":"m","tools":[{"name":"bash","description":"run","parameters":{"type":"object","properties":{"command":{"type":"string"}}}}]}`)
	b := []byte(`{"tools":[{"parameters":{"properties":{"command":{"type":"string"}},"type":"object"},"description":"run","name":"bash"}],"model":"m"}`)
	shaA, toolsA, okA := toolsetOf(a)
	shaB, toolsB, okB := toolsetOf(b)
	if !okA || !okB || shaA == "" || shaA != shaB || string(toolsA) != string(toolsB) {
		t.Fatalf("digest differs:\n%s\n%s\n%s\n%s", shaA, shaB, toolsA, toolsB)
	}
	if _, _, ok := toolsetOf([]byte(`{"model":"m"}`)); ok {
		t.Fatal("a request without tools must not produce a toolset")
	}
	if string(toolsA) != `[{"description":"run","name":"bash","parameters":{"properties":{"command":{"type":"string"}},"type":"object"}}]` {
		t.Fatalf("canonical form: %s", toolsA)
	}
}

// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package search

import (
	"strings"
	"testing"
)

func TestUnifiedDiff(t *testing.T) {
	old := "a\nb\nc\nd\ne\nf\ng\nh\n"
	d, long := UnifiedDiff(old, "a\nb\nc\nX\ne\nf\ng\nh\n")
	if long || !strings.Contains(d, "-d") || !strings.Contains(d, "+X") || !strings.Contains(d, "@@") {
		t.Fatalf("diff: %q long=%v", d, long)
	}
	if d, _ := UnifiedDiff(old, old); d != "" {
		t.Errorf("equal texts give a diff: %q", d)
	}
	var big, big2 strings.Builder
	for i := 0; i < 1000; i++ {
		big.WriteString("line\n")
		big2.WriteString("other\n")
	}
	if _, long := UnifiedDiff(big.String(), big2.String()); !long {
		t.Error("a diff of 2000 lines is not too long")
	}
}

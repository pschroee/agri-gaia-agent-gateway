// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package worker

import (
	"strings"
	"testing"

	"agw/internal/toolset"
)

// Issue #75: the agent named sandbox paths (/workspace/…) and internal commands in its replies, which the user
// cannot use. The rule is part of the system note of every binding, once, with its wrong and right example and the
// exception for an explicit question about technical details.
func TestSystemNoteUserFacingRule(t *testing.T) {
	for _, ts := range toolset.All() {
		note := SystemNoteFor(ts.Key(), 4)
		if strings.Count(note, userFacingRule) != 1 {
			t.Errorf("%s: user-facing rule not exactly once", ts.Key())
		}
		for _, w := range []string{"Wrong: \"Source: /workspace/report.typ\"", "Right: \"I can also send you the Typst source (report.typ)", "explicitly asks for technical details"} {
			if !strings.Contains(note, w) {
				t.Errorf("%s: system note without %q", ts.Key(), w)
			}
		}
	}
}

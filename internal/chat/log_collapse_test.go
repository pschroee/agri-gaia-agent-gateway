// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"strings"
	"testing"

	"agw/internal/store"
)

// Log tails stored before issue #178 still hold redrawn bars; they leave the gateway collapsed.
func TestOutgoingLogTailsCollapse(t *testing.T) {
	raw := "start\nEpoch 1:   0%|          | 0/17 [00:00<?, ?it/s]\rEpoch 1: 100%|##########| 17/17 [00:03<00:00,  4.57it/s]\nend"
	check := func(what, s string) {
		t.Helper()
		if strings.Contains(s, "\r") || strings.Count(s, "Epoch 1") != 1 || !strings.Contains(s, "end") {
			t.Errorf("%s: %q", what, s)
		}
	}
	check("job", redactWatchText(store.WatchJob{LogTail: raw}).LogTail)
	p := redactPlanText(store.Plan{Steps: []store.PlanStep{{Idx: 1, LogTail: raw}}})
	check("plan step", p.Steps[0].LogTail)
}

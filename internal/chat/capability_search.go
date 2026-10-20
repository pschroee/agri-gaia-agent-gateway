// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"fmt"
)

// The capability "training search" (issue #168, G1.11): can the agent propose a search with a budget?

const searchWithout = "run single trainings and compare them yourself"

func init() { registerCapability("training search", capabilityTrainingSearch) }

func capabilityTrainingSearch(ctx context.Context, m *Manager, chatID string) Capability {
	if !m.searchEnabled() || m.opt.Platform == nil {
		return Capability{Detail: "not configured on this instance", Without: searchWithout}
	}
	return Capability{Available: true, Without: searchWithout, Detail: fmt.Sprintf(
		"yes: one approval for a search space and a budget, then trial after trial; %d compute calls left in this chat (one per trial), "+
			"at most %d hours, and a split dataset is needed for a final test set", m.computeLeft(ctx, chatID), m.searchMaxHours())}
}

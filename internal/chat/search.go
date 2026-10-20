// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"

	"agw/internal/platform"
)

// ProposeSearch takes a proposal of a training search (POST /_agw/search, issue #168): one approval of a search
// space and a budget, inside which the agent then runs trial after trial. The seam answers that the search is
// not available; TS-S replaces this function (the pattern is ProposePlan).
func (m *Manager) ProposeSearch(ctx context.Context, chatID, slotID, via string, req platform.Request) (platform.Result, error) {
	_, _, _, _ = ctx, chatID, slotID, via
	return platform.Result{Status: platform.StatusUnavailable, Message: platform.UnavailableText(
		"training search", "this gateway has no search service yet", "Run single trainings instead")}, nil
}

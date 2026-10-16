// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package assess

import (
	"context"
	"fmt"
)

// WithoutChecks is what the agent should do on a setup that cannot check a result (R0.23, G1.13).
const WithoutChecks = withoutChecks

// ChecksState answers the capability "checks" (G1.11, R0.23): whether this setup can check a result
// right now, and how many checks of this chat are left. The remaining number is state and belongs into
// the answer, not into the system note (G1.9). The chat package registers the entry and adds what to do
// without it.
func (s *Service) ChecksState(ctx context.Context, chatID string) (available bool, detail string) {
	if !s.Configured() {
		return false, "not configured on this instance"
	}
	left := s.Max
	if used, err := s.St.CountAssessments(ctx, chatID); err == nil {
		left = s.Max - used
		if left < 0 {
			left = 0
		}
	}
	return true, fmt.Sprintf("classification models on held-out images in a gateway container "+
		"(accuracy, per-class figures, majority baseline); %d of %d checks left in this chat", left, s.Max)
}

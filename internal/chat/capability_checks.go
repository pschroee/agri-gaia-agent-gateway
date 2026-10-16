// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import "context"

// The capability "checks" (R0, G1.11): whether this setup checks the result of a long step — a training,
// an own model, a deployment — and how many checks this chat has left. The entry lives in its own file;
// the service behind it is wired in main.go (internal/assess) and registered as the doer of the check
// paths.

// Checker is the check service of this setup (internal/assess). Without it (nil) the entry says that
// results are not checked, and the agent reports the platform's own figure as an unchecked one.
type Checker interface {
	// ChecksState reports whether this setup can check a result right now, with a reason when it cannot,
	// and names how many checks are left in this chat.
	ChecksState(ctx context.Context, chatID string) (available bool, detail string)
}

// checksWithout is what the agent should do without the capability (G1.13, R0.23); the service and the
// tools carry the same sentence.
const checksWithout = "report the platform's own figure from the training log and say that the gateway did not check the result"

func init() { registerCapability("checks", capabilityChecks) }

// capabilityChecks: can this setup check a result of this chat right now? The remaining number is state
// and is read from the checks' table, so the answer is kept for a minute (G1.11).
func capabilityChecks(ctx context.Context, m *Manager, chatID string) Capability {
	if m.opt.Checks == nil {
		return Capability{Detail: "not configured on this instance", Without: checksWithout}
	}
	return m.cachedCapability(chatID, "checks", func() Capability {
		available, detail := m.opt.Checks.ChecksState(ctx, chatID)
		return Capability{Available: available, Detail: detail, Without: checksWithout}
	})
}

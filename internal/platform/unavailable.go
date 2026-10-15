// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

import "fmt"

// StatusUnavailable is the status of a call that the gateway does not send because the capability it
// needs is not part of this setup (G1.13, e.g. a /_agw/ path nobody answers). It is not an error: the
// agent should work around it instead of repeating the call, and it costs neither a compute call nor an
// approval. The tool capabilities says what this setup offers.
const StatusUnavailable = "unavailable"

// UnavailableText is the fixed wording of such an answer (G1.13):
// "<capability> is not available in this setup: <reason>. <what to do instead>. capabilities shows what this setup offers."
func UnavailableText(capability, reason, instead string) string {
	return fmt.Sprintf("%s is not available in this setup: %s. %s. capabilities shows what this setup offers.", capability, reason, instead)
}

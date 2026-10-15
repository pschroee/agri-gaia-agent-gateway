// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package notebook

import "context"

// NotebookState answers the capability "notebook" (G1.11): can this chat run its own code in its
// notebook server right now, and if not, why not. available is false with a reason the agent can act
// on ("not configured on this instance", "JupyterHub not reachable", "open JupyterHub once in the
// browser"); the chat package adds what to do instead and caches the answer for a minute (the entry
// reads the hub).
func (n *Notebooks) NotebookState(ctx context.Context, chatID string) (available bool, detail string) {
	if n == nil || n.hub == nil || n.cfg.APIURL == "" {
		return false, "not configured on this instance"
	}
	user, err := n.hubUser(ctx, chatID)
	if err != nil {
		return false, "the user of this chat cannot be named (the platform token of the chat is not readable: " + err.Error() + ")"
	}
	known, err := n.hub.HasUser(ctx, user)
	if err != nil {
		return false, "JupyterHub not reachable (" + err.Error() + ")"
	}
	if !known {
		return false, "open JupyterHub once in the browser (the hub creates the user at the first login)"
	}
	server := n.Server(chatID)
	state, err := n.hub.ServerState(ctx, user, server)
	if err != nil {
		return false, "JupyterHub not reachable (" + err.Error() + ")"
	}
	return true, "notebook server " + server + " (" + state + "), 2 CPUs, 8 GB, no internet, no platform access"
}

// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import "context"

// The capability "notebook" (own code C-E3, G1.11): whether this chat can run its own code in its
// notebook server in JupyterHub. The entry lives in its own file; the service behind it is wired in
// main.go only when AGW_JUPYTERHUB_API_URL is set.

// Notebooker is the notebook service of this setup (internal/notebook). Without it (nil) no doer is
// registered for the notebook paths, and every notebook tool answers "not configured on this
// instance" through the manager's virtual switch (G1.5, G1.13).
type Notebooker interface {
	// NotebookState reports whether this chat can run its own code in its notebook server right now,
	// with a reason when it cannot.
	NotebookState(ctx context.Context, chatID string) (available bool, detail string)
	// StopChat stops the chat's notebook server; the files of the runs stay. The trash uses it.
	StopChat(ctx context.Context, chatID string) error
}

// notebookWithout is what the agent should do without a notebook server (G1.13); the runner and the
// tools carry the same sentence.
const notebookWithout = "train small models in the sandbox (numpy, pandas, no torch) or use a platform training template"

func init() { registerCapability("notebook", capabilityNotebook) }

// capabilityNotebook: can this chat run its own code in its notebook server? The answer reads the
// hub (does it know the user, does the server run), so it is kept for a minute (G1.11).
func capabilityNotebook(ctx context.Context, m *Manager, chatID string) Capability {
	if m.opt.Notebook == nil {
		return Capability{Detail: "not configured on this instance", Without: notebookWithout}
	}
	return m.cachedCapability(chatID, "notebook", func() Capability {
		available, detail := m.opt.Notebook.NotebookState(ctx, chatID)
		return Capability{Available: available, Detail: detail, Without: notebookWithout}
	})
}

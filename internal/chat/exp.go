// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"

	"agw/internal/store"
)

// The experiment runner's bookkeeping (X1.5). The runner is a separate process and reaches it over
// /api/exp/*; the manager keeps the store behind the same seam as the rest of the API.
func (m *Manager) StartExpRun(ctx context.Context, run store.ExpRun) error {
	return m.st.StartExpRun(ctx, run)
}

func (m *Manager) SetExpRunChat(ctx context.Context, id, chatID string) error {
	return m.st.SetExpRunChat(ctx, id, chatID)
}

func (m *Manager) EndExpRun(ctx context.Context, id string, end store.ExpRunEnd) error {
	return m.st.EndExpRun(ctx, id, end)
}

func (m *Manager) SupersedeExpRun(ctx context.Context, id, by string) error {
	return m.st.SupersedeExpRun(ctx, id, by)
}

func (m *Manager) ExpRuns(ctx context.Context, plan, cell string) ([]store.ExpRun, error) {
	return m.st.ListExpRuns(ctx, plan, cell)
}

func (m *Manager) ExpRun(ctx context.Context, id string) (store.ExpRun, error) {
	return m.st.GetExpRun(ctx, id)
}

// DelegationObjects returns the provenance register of a chat (X1.10: cleanup only by these ids).
func (m *Manager) DelegationObjects(ctx context.Context, chatID string) ([]store.DelegationObject, error) {
	return m.st.ListDelegationObjects(ctx, chatID)
}

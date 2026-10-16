// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"

	"agw/internal/store"
)

// The check rows of a chat (R0) are read here: the API's check route (issue #112) shows them, the
// gateway's own service reads them through the store directly. Reading never wakes a chat or takes a
// slot — the card must be able to load the reports of a dormant chat.

// Assessments returns a chat's result checks, oldest first.
func (m *Manager) Assessments(ctx context.Context, chatID string) ([]store.Assessment, error) {
	return m.st.Assessments(ctx, chatID)
}

// Assessment reads one check of a chat (ErrNotFound: another chat's or unknown).
func (m *Manager) Assessment(ctx context.Context, chatID, id string) (store.Assessment, error) {
	return m.st.Assessment(ctx, chatID, id)
}

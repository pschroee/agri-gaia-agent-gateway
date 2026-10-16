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

// UploadedModelOfFile answers the model a chat uploaded from a file with this SHA-256 (R2.6): the check card
// names the model behind a check of a file. ok false: this chat uploaded no such model. Reading only, from
// the gateway's own table — the card must not call the platform for a dormant chat.
func (m *Manager) UploadedModelOfFile(ctx context.Context, chatID, sha256 string) (store.ModelOrigin, bool, error) {
	return m.st.ModelOriginOfSHA(ctx, chatID, sha256)
}

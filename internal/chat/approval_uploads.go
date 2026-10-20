// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"encoding/json"
)

// ApprovalUploadEntries returns the list of every file of an upload approval of the chat (issue #164): the card
// fetches it when the user opens the list. It is not part of the approval, so no approval event carries it.
func (m *Manager) ApprovalUploadEntries(ctx context.Context, chatID, approvalID string) (json.RawMessage, error) {
	return m.st.GetApprovalUploadEntries(ctx, chatID, approvalID)
}

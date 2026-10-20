// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package api

import (
	"net/http"

	"agw/internal/store"
)

// approvalUploads answers GET /api/chats/{id}/approvals/{aid}/uploads: the list of every file (field, path, size,
// SHA-256) of an upload approval, as {"entries": [...]} (issue #164). The approval event carries only the summary
// and the hash over this list.
func (s *Server) approvalUploads(w http.ResponseWriter, r *http.Request) {
	entries, err := s.M.ApprovalUploadEntries(r.Context(), r.PathValue("id"), r.PathValue("aid"))
	if err != nil {
		fail(w, err)
		return
	}
	if entries == nil {
		writeErr(w, http.StatusNotFound, store.ErrNotFound.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"entries": entries})
}

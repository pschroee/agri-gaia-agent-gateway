// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package api

import "net/http"

// plans lists the plans of a chat, oldest first (issue #70). Foreign and unknown chats are 404.
func (s *Server) plans(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.M.ChatOwner(r.Context(), id); err != nil {
		fail(w, err)
		return
	}
	ps, err := s.M.Plans(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, ps)
}

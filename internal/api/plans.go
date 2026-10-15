// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package api

import (
	"context"
	"errors"
	"net/http"

	"agw/internal/chat"
	"agw/internal/store"
)

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

// planControl is a pause, resume or cancel of an autonomous plan (A-E1). The chat and its owner are
// checked by the route; a plan of another chat or an unknown plan is 404, a plan that is not in the
// required state is 409. The answer is the plan in its new state.
func (s *Server) planControl(f func(ctx context.Context, chatID, planID string) (store.Plan, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		chatID, planID := r.PathValue("id"), r.PathValue("plan")
		p, err := f(r.Context(), chatID, planID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeErr(w, http.StatusNotFound, "plan not found")
				return
			}
			var st *chat.PlanStateError
			if errors.As(err, &st) {
				writeJSON(w, http.StatusConflict, map[string]string{"error": st.Error(), "state": st.State})
				return
			}
			fail(w, err)
			return
		}
		writeJSON(w, 200, p)
	}
}

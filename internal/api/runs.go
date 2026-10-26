// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package api

import (
	"net/http"

	"agw/internal/chat"
)

// experimentRuns lists the training searches, single training runs and notebook runs of the user's chats on one
// page (API.md, Experiments across chats). In oidc mode only the user's own chats count; a foreign or unknown chat in
// ?chat= gives an empty page.
func (s *Server) experimentRuns(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	oq, bad := parseOverviewQuery(q, chat.RunSorts)
	if bad != "" {
		writeErr(w, http.StatusBadRequest, bad)
		return
	}
	states, bad := parseList(q, "state", chat.RunStates)
	if bad != "" {
		writeErr(w, http.StatusBadRequest, bad)
		return
	}
	kinds, bad := parseList(q, "kind", chat.RunKinds)
	if bad != "" {
		writeErr(w, http.StatusBadRequest, bad)
		return
	}
	f := chat.RunFilter{ChatID: oq.ChatID, Since: oq.Since, Until: oq.Until, States: states, Kinds: kinds,
		Sort: oq.Sort, Order: oq.Order, Offset: oq.Offset, Limit: oq.Limit}
	if s.OIDC != nil {
		u, ok := UserFrom(r.Context())
		if !ok || u.Sub == "" {
			writeErr(w, http.StatusUnauthorized, "not logged in")
			return
		}
		f.Owner = &u.Sub
	}
	page, err := s.M.ExperimentRuns(r.Context(), f)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

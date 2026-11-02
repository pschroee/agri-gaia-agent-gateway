// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package api

// GET /api/exp/workspace?chat=<id> (BM3): the stored archive of the agent's working directory of one experiment run,
// streamed as it lies in the object storage (tar.gz). Only the separate experiment key reaches it (auth middleware, the
// route is under /api/exp/). The CLI writes it next to the export when asked for --workspaces.

import (
	"io"
	"net/http"
	"strconv"
)

// expWorkspace streams the archive. 404 when the chat is no experiment run or no archive was saved.
func (s *Server) expWorkspace(w http.ResponseWriter, r *http.Request) {
	chatID := r.URL.Query().Get("chat")
	if chatID == "" {
		writeErr(w, http.StatusBadRequest, "chat is required")
		return
	}
	var opener WorkspaceOpener = s.Workspaces
	if opener == nil && s.M != nil {
		opener = s.M
	}
	if opener == nil {
		writeErr(w, http.StatusServiceUnavailable, "workspaces not available")
		return
	}
	rc, ws, err := opener.OpenExpWorkspace(r.Context(), chatID)
	if err != nil {
		fail(w, err)
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("X-Agw-Workspace-SHA256", ws.SHA256)
	w.Header().Set("X-Agw-Workspace-Files", strconv.Itoa(ws.Files))
	if ws.ArchiveSize > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(ws.ArchiveSize, 10))
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, rc)
}

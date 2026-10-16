// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package api

import (
	"encoding/json"
	"net/http"
	"time"

	"agw/internal/store"
)

// The check route of a chat (issue #112, H113): the check card loads the whole reports of a chat from
// here, not only the head line the watcher published. Own chats only; the route reads the database and
// never wakes the chat.
//
// The answer carries no member of the checked set: which images were measured is the gateway's alone
// (P11), and the report names sample sizes, never images, file names or SHAs of the hold-out set.

// assessmentRow is one line of the answer: the fields of the store's row, with the progress only while
// the check runs (L4) and the report as the gateway wrote it.
type assessmentRow struct {
	CheckID string `json:"check_id"`
	Kind    string `json:"kind"`
	Subject string `json:"subject"`
	Final   bool   `json:"final"`
	State   string `json:"state"`
	// Report: the whole report of the check, null while it runs.
	Report   map[string]any `json:"report"`
	Progress map[string]any `json:"progress"`
	Error    string         `json:"error,omitempty"`
	// ToolCallID: the call that started the check, so the card can place it in the history.
	ToolCallID string     `json:"tool_call_id,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	EndedAt    *time.Time `json:"ended_at,omitempty"`
}

// assessments is GET /api/chats/{id}/assessments: the checks of one chat, oldest first.
func (s *Server) assessments(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.M.ChatOwner(r.Context(), id); err != nil {
		fail(w, err)
		return
	}
	rows, err := s.M.Assessments(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	out := make([]assessmentRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, assessmentLine(row))
	}
	writeJSON(w, 200, map[string]any{"checks": out})
}

// assessmentLine turns a stored check into the line of the route.
func assessmentLine(row store.Assessment) assessmentRow {
	line := assessmentRow{
		CheckID: row.ID, Kind: row.Kind, Subject: row.Subject, Final: row.Final, State: row.State,
		Error: row.Error, ToolCallID: row.ToolCallID, CreatedAt: row.CreatedAt,
		StartedAt: row.StartedAt, EndedAt: row.EndedAt,
	}
	if len(row.Report) > 0 {
		var rep map[string]any
		if json.Unmarshal(row.Report, &rep) == nil {
			line.Report = rep
		}
	}
	// The progress is only interesting while a check runs; afterwards it is stale (L4).
	if row.State == store.AssessRunning && len(row.Progress) > 0 {
		var p map[string]any
		if json.Unmarshal(row.Progress, &p) == nil {
			line.Progress = p
		}
	}
	return line
}

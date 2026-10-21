// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"agw/internal/assess"
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
	// UploadedModel: for a check of a file (subject sha:<hex>) the model this file was uploaded as, once the
	// chat uploaded it (R2.6, L14): the card shows which model stands behind a check of a file. The route
	// reads it from the gateway's own table and never from the platform, so the name is only there when the
	// check itself read the model — a check that ran before the upload names the id alone.
	UploadedModel *uploadedModelRef `json:"uploaded_model,omitempty"`
}

// uploadedModelRef names the model a check's file was uploaded as (R2.6).
type uploadedModelRef struct {
	ID   int64  `json:"id"`
	Name string `json:"name,omitempty"`
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
		row.Report = json.RawMessage(s.M.RenderNote(r.Context(), id, string(row.Report))) // references to tools (#200)
		line := assessmentLine(row)
		line.UploadedModel = s.uploadedModelOf(r.Context(), id, row)
		out = append(out, line)
	}
	writeJSON(w, 200, map[string]any{"checks": out})
}

// uploadedModelOf answers the model behind a check of a file (R2.6, L14): a row whose subject is sha:<hex>
// is a check of a file the agent may have uploaded as a model afterwards. Without such a model the field
// stays away; the name comes from the report, which read it when the check ran after the upload.
func (s *Server) uploadedModelOf(ctx context.Context, chatID string, row store.Assessment) *uploadedModelRef {
	sha, ok := assess.SHAFromSubject(row.Subject)
	if !ok {
		return nil
	}
	o, found, err := s.M.UploadedModelOfFile(ctx, chatID, sha)
	if err != nil || !found {
		return nil
	}
	return &uploadedModelRef{ID: o.ModelID, Name: modelNameOf(row.Report, o.ModelID)}
}

// modelNameOf reads the name of one model out of a stored report (figures.model, R2.6); empty when the
// report does not name this model.
func modelNameOf(raw json.RawMessage, modelID int64) string {
	var rep struct {
		Figures struct {
			Model struct {
				ID   int64  `json:"id"`
				Name string `json:"name"`
			} `json:"model"`
		} `json:"figures"`
	}
	if len(raw) == 0 || modelID <= 0 || json.Unmarshal(raw, &rep) != nil || rep.Figures.Model.ID != modelID {
		return ""
	}
	return rep.Figures.Model.Name
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

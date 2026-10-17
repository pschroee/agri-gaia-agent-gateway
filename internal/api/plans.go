// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"agw/internal/chat"
	"agw/internal/platform"
	"agw/internal/store"
)

// redactPlanText keeps tokens in the progress texts of a plan's steps away from the browser (issue #141):
// the messages come from the platform, and rows stored before the gateway redacted them still carry one.
func redactPlanText(p store.Plan) store.Plan {
	for i := range p.Steps {
		p.Steps[i].ProgressMsg = platform.RedactText(p.Steps[i].ProgressMsg)
		p.Steps[i].LogTail = platform.RedactText(p.Steps[i].LogTail)
		p.Steps[i].Result = platform.RedactText(p.Steps[i].Result)
	}
	return p
}

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
	for i := range ps {
		ps[i] = redactPlanText(ps[i])
	}
	writeJSON(w, 200, ps)
}

// jobs lists the observed platform jobs of a chat (A-E2), newest first: the cards outside plans read
// them (own code E4, pre-annotation V4, edge E4). The chat and its owner are checked by the route.
func (s *Server) jobs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.M.ChatOwner(r.Context(), id); err != nil {
		fail(w, err)
		return
	}
	list, err := s.M.Jobs(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, list)
}

// stopPlanStep stops the platform job a waiting step of a plan observes (A-E2, D2.9): the user's own
// action, run with the user's token, not a tool of the agent. The answer is the plan in its new state;
// a step that is not waiting or a job that cannot be stopped is 409.
func (s *Server) stopPlanStep(w http.ResponseWriter, r *http.Request) {
	chatID, planID := r.PathValue("id"), r.PathValue("plan")
	idx, err := strconv.Atoi(r.PathValue("idx"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "step is not a number")
		return
	}
	p, err := s.M.StopPlanStep(r.Context(), chatID, planID, idx)
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
	writeJSON(w, 200, redactPlanText(p))
}

// stopJob stops a single observed job of a chat (A-E2, D2.9): the user's action for the cards outside
// plans. The answer is the job; a job that cannot be stopped is 409.
func (s *Server) stopJob(w http.ResponseWriter, r *http.Request) {
	chatID, jobID := r.PathValue("id"), r.PathValue("job")
	job, err := s.M.StopJob(r.Context(), chatID, jobID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, "job not found")
	case errors.Is(err, chat.ErrNotStoppable), errors.Is(err, chat.ErrNotRunning):
		writeErr(w, http.StatusConflict, err.Error())
	case err != nil:
		fail(w, err)
	default:
		writeJSON(w, 200, job)
	}
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
		writeJSON(w, 200, redactPlanText(p))
	}
}

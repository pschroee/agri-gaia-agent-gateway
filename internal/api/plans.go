// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"agw/internal/chat"
	"agw/internal/plan"
	"agw/internal/platform"
	"agw/internal/store"
	"agw/internal/watch"
)

// redactPlanText keeps tokens in the progress texts of a plan's steps away from the browser (issue #141):
// the messages come from the platform, and rows stored before the gateway redacted them still carry one.
func redactPlanText(p store.Plan) store.Plan {
	for i := range p.Steps {
		p.Steps[i].ProgressMsg = platform.RedactText(p.Steps[i].ProgressMsg)
		p.Steps[i].LogTail = watch.Tail(platform.RedactText(p.Steps[i].LogTail))
		p.Steps[i].Result = platform.RedactText(p.Steps[i].Result)
	}
	return plan.WithUploads(p)
}

// planStepUploads answers GET /api/chats/{id}/plans/{plan}/steps/{idx}/uploads: the list of every file (field,
// path, size, SHA-256) of an upload step, as {"entries": [...]} (issue #172). The plan carries only the summary and
// the hash over this list.
func (s *Server) planStepUploads(w http.ResponseWriter, r *http.Request) {
	idx, err := strconv.Atoi(r.PathValue("idx"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "step is not a number")
		return
	}
	entries, err := s.M.PlanStepUploadEntries(r.Context(), r.PathValue("id"), r.PathValue("plan"), idx)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "no uploads for this step")
			return
		}
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"entries": entries})
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

// plansAcross lists the plans of the user's chats across all chats, running ones first (GET /api/plans, issue
// #223). In oidc mode the store filters by the owner, so another user's plans never leave the database; a
// foreign chat in ?chat= gives an empty page, like an unknown one.
func (s *Server) plansAcross(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	oq, bad := parseOverviewQuery(q, nil)
	if bad != "" {
		writeErr(w, http.StatusBadRequest, bad)
		return
	}
	states, bad := parseList(q, "state", store.PlanStates)
	if bad != "" {
		writeErr(w, http.StatusBadRequest, bad)
		return
	}
	mode := q.Get("mode")
	if mode != "" && mode != "interactive" && mode != "autonomous" {
		writeErr(w, http.StatusBadRequest, "mode: interactive or autonomous")
		return
	}
	f := store.PlanListFilter{States: states, Mode: mode, ChatID: oq.ChatID, Limit: oq.Limit, Offset: oq.Offset}
	if s.OIDC != nil {
		u, ok := UserFrom(r.Context())
		if !ok || u.Sub == "" {
			writeErr(w, http.StatusUnauthorized, "not logged in")
			return
		}
		f.Owner = &u.Sub
	}
	items, total, running, err := s.M.PlansAcross(r.Context(), f)
	if err != nil {
		fail(w, err)
		return
	}
	for i := range items {
		items[i].Plan = redactPlanText(items[i].Plan)
	}
	writeJSON(w, 200, map[string]any{"plans": items, "total": total, "running": running})
}

// planEvents is the stream of plans across the user's chats (GET /api/plans/events, issue #223): every state
// change and every progress of a plan of a chat that is not in the trash, as "plan" with the chat title. It is a
// stream of its own, because a plan event comes with every poll of a job and would burden every page that
// listens on GET /api/events.
func (s *Server) planEvents(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, "streaming not possible")
		return
	}
	if s.OIDC != nil {
		if u, ok := UserFrom(ctx); !ok || u.Sub == "" {
			writeErr(w, http.StatusUnauthorized, "not logged in")
			return
		}
	}
	ch, cancel := s.M.SubscribeKinds("plan")
	defer cancel()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	_, _ = fmt.Fprintf(w, "retry: %d\n: connected\n\n", eventsRetryMs)
	fl.Flush()
	type chatInfo struct {
		mine  bool
		title string
	}
	known := map[string]chatInfo{}
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ping.C:
			_, _ = io.WriteString(w, ": ping\n\n")
			fl.Flush()
		case ev := <-ch:
			p, isPlan := ev.Data.(store.Plan)
			if !isPlan {
				continue
			}
			info, seen := known[ev.ChatID]
			if !seen {
				own, err := s.owns(ctx, ev.ChatID)
				if err != nil {
					return // the client reconnects and loads the list again
				}
				info.mine = own
				if own {
					title, trashed, err := s.M.ChatTitle(ctx, ev.ChatID)
					if err != nil {
						return
					}
					info.mine = !trashed
					info.title = title
				}
				known[ev.ChatID] = info
			}
			if !info.mine {
				continue
			}
			b, err := json.Marshal(map[string]any{"kind": "plan", "chat_id": ev.ChatID,
				"data": store.PlanListItem{Plan: redactPlanText(p), ChatTitle: info.title}})
			if err != nil {
				continue
			}
			_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
			fl.Flush()
		}
	}
}

// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"agw/internal/chat"
	"agw/internal/store"
)

// The routes of the training search (issue #168): the book of trials of a chat, one trial with its series, the
// export, and the user's four controls. The controls are the user's, never the agent's: the chat and its owner are
// checked by the route, and a search of another chat is 404.

func (s *Server) experiments(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.M.ChatOwner(r.Context(), id); err != nil {
		fail(w, err)
		return
	}
	searches, err := s.M.Searches(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	trials, err := s.M.Trials(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"searches": searches, "trials": trials})
}

func (s *Server) trialDetail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.M.ChatOwner(r.Context(), id); err != nil {
		fail(w, err)
		return
	}
	d, err := s.M.TrialDetail(r.Context(), id, r.PathValue("trial"))
	if err != nil {
		searchFail(w, err)
		return
	}
	writeJSON(w, 200, d)
}

func (s *Server) searchExport(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.M.ChatOwner(r.Context(), id); err != nil {
		fail(w, err)
		return
	}
	format := r.URL.Query().Get("format")
	if format == "" {
		format = "json"
	}
	if format != "json" && format != "csv" {
		writeErr(w, http.StatusBadRequest, "format must be json or csv")
		return
	}
	exp, err := s.M.ExportSearch(r.Context(), id, r.PathValue("search"))
	if err != nil {
		searchFail(w, err)
		return
	}
	name := "attachment; filename=\"" + r.PathValue("search") + "." + format + "\""
	if format == "csv" {
		b, err := exp.CSV()
		if err != nil {
			fail(w, err)
			return
		}
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", name)
		_, _ = w.Write(b)
		return
	}
	w.Header().Set("Content-Disposition", name)
	writeJSON(w, 200, exp)
}

// searchFail maps the errors of the search to HTTP answers.
func searchFail(w http.ResponseWriter, err error) {
	var in *chat.SearchInputError
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not found")
	case errors.Is(err, chat.ErrSearchNotRunning):
		writeErr(w, http.StatusConflict, err.Error())
	case errors.As(err, &in):
		writeErr(w, http.StatusBadRequest, in.Msg)
	default:
		fail(w, err)
	}
}

// searchControl wraps a control: the body is read (small), the call made, the search in its new state answered.
func (s *Server) searchControl(f func(r *http.Request, chatID, searchID string, body []byte) (chat.SearchView, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<10))
		if err != nil {
			writeErr(w, http.StatusBadRequest, "body too large or unreadable")
			return
		}
		v, err := f(r, r.PathValue("id"), r.PathValue("search"), body)
		if err != nil {
			searchFail(w, err)
			return
		}
		writeJSON(w, 200, v)
	}
}

func (s *Server) registerSearchRoutes(chatRoute, writes func(pattern string, h http.HandlerFunc)) {
	chatRoute("GET /api/chats/{id}/experiments", s.experiments)
	chatRoute("GET /api/chats/{id}/experiments/trials/{trial}", s.trialDetail)
	chatRoute("GET /api/chats/{id}/searches/{search}/export", s.searchExport)
	writes("POST /api/chats/{id}/searches/{search}/stop", s.searchControl(func(r *http.Request, chatID, searchID string, body []byte) (chat.SearchView, error) {
		var b struct {
			Scope string `json:"scope"`
		}
		if err := decodeStrict(body, &b); err != nil {
			return chat.SearchView{}, &chat.SearchInputError{Msg: "body: {\"scope\":\"search\"|\"trial\"}"}
		}
		return s.M.StopSearch(r.Context(), chatID, searchID, b.Scope)
	}))
	writes("POST /api/chats/{id}/searches/{search}/budget", s.searchControl(func(r *http.Request, chatID, searchID string, body []byte) (chat.SearchView, error) {
		return s.M.SetSearchBudget(r.Context(), chatID, searchID, body)
	}))
	writes("POST /api/chats/{id}/searches/{search}/hint", s.searchControl(func(r *http.Request, chatID, searchID string, body []byte) (chat.SearchView, error) {
		var b struct {
			Text string `json:"text"`
		}
		if err := decodeStrict(body, &b); err != nil {
			return chat.SearchView{}, &chat.SearchInputError{Msg: "body: {\"text\":\"...\"}"}
		}
		return s.M.HintSearch(r.Context(), chatID, searchID, b.Text)
	}))
	writes("POST /api/chats/{id}/searches/{search}/adopt", s.searchControl(func(r *http.Request, chatID, searchID string, body []byte) (chat.SearchView, error) {
		var b struct {
			Trial int `json:"trial"`
		}
		if err := decodeStrict(body, &b); err != nil || b.Trial < 1 {
			return chat.SearchView{}, &chat.SearchInputError{Msg: "body: {\"trial\":2}"}
		}
		return s.M.AdoptTrial(r.Context(), chatID, searchID, b.Trial)
	}))
}

// decodeStrict decodes a JSON body and refuses fields it does not know.
func decodeStrict(body []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	if d.More() {
		return errors.New("data after the JSON value")
	}
	return nil
}

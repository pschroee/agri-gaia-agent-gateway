// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"agw/internal/platform"
	"agw/internal/store"
	"agw/internal/version"
)

// The runner's routes (X1). They are reachable with the experiment key only; the key itself is
// checked in auth. The runner creates, reads and closes runs, executes the declarative check and
// removes what a run created — all with the experiment account's platform token, never with the
// browser session of the author.

// expOnly refuses a request that did not come in with the experiment key. The key's route list
// already limits the surface; this keeps a logged-in user from opening the runner's own routes.
func (s *Server) expOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !ExpKeyFrom(r.Context()) {
			writeErr(w, http.StatusForbidden, "the experiment routes need the experiment key")
			return
		}
		next(w, r)
	}
}

func (s *Server) registerExp(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/exp/environment", s.expOnly(s.expEnvironment))
	mux.HandleFunc("GET /api/exp/runs", s.expOnly(s.expRuns))
	mux.HandleFunc("GET /api/exp/usage", s.expOnly(s.expUsage))
	mux.HandleFunc("GET /api/exp/run", s.expOnly(s.expRun))
	mux.HandleFunc("POST /api/exp/runs/start", s.expOnly(s.expStartRun))
	mux.HandleFunc("POST /api/exp/runs/chat", s.expOnly(s.expSetRunChat))
	mux.HandleFunc("POST /api/exp/runs/end", s.expOnly(s.expEndRun))
	mux.HandleFunc("POST /api/exp/runs/supersede", s.expOnly(s.expSupersedeRun))
	mux.HandleFunc("POST /api/exp/runs/stop", s.expOnly(s.expStopRun))
	mux.HandleFunc("GET /api/exp/objects", s.expOnly(s.expObjects))
	mux.HandleFunc("POST /api/exp/call", s.expOnly(s.expCall))
	mux.HandleFunc("POST /api/exp/chats/abort", s.expOnly(s.expAbortChat))
}

func (s *Server) expRuns(w http.ResponseWriter, r *http.Request) {
	runs, err := s.M.ExpRuns(r.Context(), r.URL.Query().Get("plan"), r.URL.Query().Get("cell"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, runs)
}

// expUsage answers what a plan has consumed so far (#161): the runner checks the plan-wide token
// and duration limits before every run.
func (s *Server) expUsage(w http.ResponseWriter, r *http.Request) {
	plan := r.URL.Query().Get("plan")
	if plan == "" {
		writeErr(w, http.StatusBadRequest, "plan is missing")
		return
	}
	usage, err := s.M.ExpUsage(r.Context(), plan)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, usage)
}

func (s *Server) expRun(w http.ResponseWriter, r *http.Request) {
	run, err := s.M.ExpRun(r.Context(), r.URL.Query().Get("id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, run)
}

func (s *Server) expStartRun(w http.ResponseWriter, r *http.Request) {
	var run store.ExpRun
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&run); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	// X8.1: the gateway itself states the version it runs with; a client value is not authoritative.
	run.GatewayVersion = version.String()
	if err := s.M.StartExpRun(r.Context(), run); err != nil {
		if errors.Is(err, store.ErrExpRunExists) {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, run)
}

func (s *Server) expSetRunChat(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID   string `json:"id"`
		Chat string `json:"chat"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	ok, err := s.owns(r.Context(), req.Chat)
	if err != nil {
		fail(w, err)
		return
	}
	if !ok {
		writeErr(w, http.StatusNotFound, store.ErrNotFound.Error())
		return
	}
	// X8.2: the image IDs of the slot that carries the run and the checksums of the environment are
	// recorded with the run. A slot that is not visible leaves them empty; the runner's lock check then
	// reports the deviation instead of the gateway inventing a value.
	env := s.environment()
	imagePi, imageExec := "", ""
	if found, ok := env.SlotForChat(req.Chat); ok {
		imagePi, imageExec = found.ImagePi, found.ImageExec
	}
	if err := s.M.SetExpRunChat(r.Context(), req.ID, req.Chat, store.ExpRunEnvironment{
		ImagePi: imagePi, ImageExec: imageExec,
		CatalogSHA256: env.CatalogSHA256, SkillsSHA256: env.SkillsSHA256,
	}); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

func (s *Server) expEndRun(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID            string          `json:"id"`
		Status        string          `json:"status"`
		EndReason     string          `json:"end_reason"`
		Pass          *bool           `json:"pass"`
		CheckEvidence json.RawMessage `json:"check_evidence"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	err := s.M.EndExpRun(r.Context(), req.ID, store.ExpRunEnd{Status: req.Status, EndReason: req.EndReason,
		Pass: req.Pass, CheckEvidence: req.CheckEvidence})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

func (s *Server) expSupersedeRun(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID           string `json:"id"`
		SupersededBy string `json:"superseded_by"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if err := s.M.SupersedeExpRun(r.Context(), req.ID, req.SupersededBy); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

// expStopRun ends a run from outside (agw exp abort): the chat is aborted and the run is closed as
// aborted. A run that is already finished keeps its outcome.
func (s *Server) expStopRun(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	run, err := s.M.ExpRun(r.Context(), req.ID)
	if err != nil {
		fail(w, err)
		return
	}
	if run.ChatID != "" {
		ok, err := s.owns(r.Context(), run.ChatID)
		if err != nil {
			fail(w, err)
			return
		}
		if !ok {
			writeErr(w, http.StatusNotFound, store.ErrNotFound.Error())
			return
		}
		if _, err := s.M.Abort(r.Context(), run.ChatID); err != nil {
			fail(w, err)
			return
		}
	}
	if run.Status == store.ExpStarted {
		if err := s.M.EndExpRun(r.Context(), run.ID, store.ExpRunEnd{Status: store.ExpAborted, EndReason: "aborted by agw exp abort"}); err != nil {
			fail(w, err)
			return
		}
	}
	run, err = s.M.ExpRun(r.Context(), run.ID)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, run)
}

// expObjects answers the provenance register of a chat (X1.10): the runner removes only these ids.
func (s *Server) expObjects(w http.ResponseWriter, r *http.Request) {
	chatID := r.URL.Query().Get("chat")
	if chatID == "" {
		writeErr(w, http.StatusBadRequest, "chat is missing")
		return
	}
	ok, err := s.owns(r.Context(), chatID)
	if err != nil {
		fail(w, err)
		return
	}
	if !ok {
		writeErr(w, http.StatusNotFound, store.ErrNotFound.Error())
		return
	}
	objs, err := s.M.DelegationObjects(r.Context(), chatID)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, objs)
}

// expCall is the check's and the cleanup's way to the platform (X1.9, X1.10): a read or delete with
// the experiment account's token for a chat this account owns. Never a write: the check only reads,
// and the cleanup deletes objects by identifier.
func (s *Server) expCall(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Chat   string            `json:"chat"`
		Method string            `json:"method"`
		Path   string            `json:"path"`
		Query  map[string]string `json:"query,omitempty"`
		Body   json.RawMessage   `json:"body,omitempty"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if req.Method != http.MethodGet && req.Method != http.MethodDelete {
		writeErr(w, http.StatusForbidden, "the experiment runner only reads and deletes")
		return
	}
	if req.Chat == "" {
		writeErr(w, http.StatusBadRequest, "chat is missing")
		return
	}
	ok, err := s.owns(r.Context(), req.Chat)
	if err != nil {
		fail(w, err)
		return
	}
	if !ok {
		writeErr(w, http.StatusNotFound, store.ErrNotFound.Error())
		return
	}
	if s.Platform == nil {
		writeErr(w, http.StatusNotImplemented, "the platform binding is off")
		return
	}
	res, err := s.Platform.Do(r.Context(), req.Chat, platform.Request{
		Method: req.Method, Path: req.Path, Query: req.Query, Body: req.Body, Full: true,
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{
		"status": res.Status, "http_status": res.HTTPStatus, "body": res.Body, "message": res.Message,
	})
}

// expAbortChat stops the running turn of a chat of the experiment account (a limit of X3).
func (s *Server) expAbortChat(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Chat string `json:"chat"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if req.Chat == "" {
		writeErr(w, http.StatusBadRequest, "chat is missing")
		return
	}
	ok, err := s.owns(r.Context(), req.Chat)
	if err != nil {
		fail(w, err)
		return
	}
	if !ok {
		writeErr(w, http.StatusNotFound, store.ErrNotFound.Error())
		return
	}
	if _, err := s.M.Abort(r.Context(), req.Chat); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

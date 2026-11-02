// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package api

import (
	"context"
	"io"
	"net/http"
	"strconv"

	"agw/internal/platform"
	"agw/internal/store"
)

// expDownload hands the experiment runner a dataset archive that a run's chat created (BM1): the runner collects the
// result of a task before it cleans up. Only datasets in the chat's provenance register can be downloaded, so the
// route is no general proxy to the platform. The archive is streamed, never held in memory.
//
//	GET /api/exp/download?chat=<id>&dataset=<id>
func (s *Server) expDownload(w http.ResponseWriter, r *http.Request) {
	chatID := r.URL.Query().Get("chat")
	dataset := r.URL.Query().Get("dataset")
	if chatID == "" || dataset == "" {
		writeErr(w, http.StatusBadRequest, "chat and dataset are required")
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
	if s.Platform == nil {
		writeErr(w, http.StatusNotImplemented, "the platform binding is off")
		return
	}
	serveDatasetDownload(w, r, chatID, dataset,
		func(ctx context.Context) ([]store.DelegationObject, error) { return s.M.DelegationObjects(ctx, chatID) },
		s.Platform.Stream)
}

// serveDatasetDownload checks the register and copies the archive on. objects and stream are the two
// things it needs, so that a test does not need a database or a platform.
func serveDatasetDownload(w http.ResponseWriter, r *http.Request, chatID, dataset string,
	objects func(context.Context) ([]store.DelegationObject, error),
	stream func(context.Context, string, platform.Request) (io.ReadCloser, int64, error)) {
	id, err := strconv.ParseInt(dataset, 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, "dataset must be a dataset identifier")
		return
	}
	objs, err := objects(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	created := false
	for _, o := range objs {
		if o.Resource == "dataset" && o.ObjectID == strconv.FormatInt(id, 10) {
			created = true
			break
		}
	}
	if !created {
		writeErr(w, http.StatusForbidden, "not created by this chat")
		return
	}
	body, size, err := stream(r.Context(), chatID, platform.Request{Method: http.MethodGet, Path: "/datasets/" + strconv.FormatInt(id, 10) + "/download"})
	if err != nil {
		fail(w, err)
		return
	}
	defer body.Close()
	w.Header().Set("Content-Type", "application/zip")
	if size >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, body)
}

// expDataset answers GET /datasets/{id} for the runner without a chat (BM1): the check of the base datasets runs
// before a run's chat exists. It only reads one dataset and answers in the shape of /api/exp/call.
//
//	GET /api/exp/dataset?id=<id>
func (s *Server) expDataset(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, "id must be a dataset identifier")
		return
	}
	if s.Platform == nil {
		writeErr(w, http.StatusNotImplemented, "the platform binding is off")
		return
	}
	res, err := s.Platform.Do(r.Context(), platform.ExpRunnerChat, platform.Request{Method: http.MethodGet, Path: "/datasets/" + strconv.FormatInt(id, 10), Full: true})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{
		"status": res.Status, "http_status": res.HTTPStatus, "body": res.Body, "message": res.Message,
	})
}

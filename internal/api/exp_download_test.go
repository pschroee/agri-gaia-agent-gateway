// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agw/internal/platform"
	"agw/internal/store"
)

// BM1: the download route hands out only datasets in the chat's provenance register.
func TestExpDownloadOnlyOwnObjects(t *testing.T) {
	objects := func(context.Context) ([]store.DelegationObject, error) {
		return []store.DelegationObject{{Resource: "dataset", ObjectID: "77"}, {Resource: "model", ObjectID: "88"}}, nil
	}
	var asked []string
	stream := func(_ context.Context, chat string, r platform.Request) (io.ReadCloser, int64, error) {
		asked = append(asked, chat+" "+r.Method+" "+r.Path)
		return io.NopCloser(strings.NewReader("PK-archive")), 10, nil
	}
	call := func(dataset string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/exp/download?chat=c1&dataset="+dataset, nil)
		serveDatasetDownload(rec, req, "c1", dataset, objects, stream)
		return rec
	}

	own := call("77")
	if own.Code != http.StatusOK || own.Body.String() != "PK-archive" || own.Header().Get("Content-Length") != "10" {
		t.Fatalf("own dataset: %d %q %v", own.Code, own.Body.String(), own.Header())
	}
	if len(asked) != 1 || asked[0] != "c1 GET /datasets/77/download" {
		t.Fatalf("platform calls: %v", asked)
	}

	for _, foreign := range []string{"78", "88"} { // not in the register; a model with that number is no dataset
		rec := call(foreign)
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "not created by this chat") {
			t.Fatalf("dataset %s: %d %q", foreign, rec.Code, rec.Body.String())
		}
	}
	if len(asked) != 1 {
		t.Fatalf("a refused request must not reach the platform: %v", asked)
	}
	if rec := call("abc"); rec.Code != http.StatusBadRequest {
		t.Fatalf("non-numeric dataset: %d", rec.Code)
	}
}

// SPDX-FileCopyrightText: 2026 Philipp Schröer
// SPDX-License-Identifier: MIT

package annotation

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestCVATAcceptsItsOwnMediaType guards the Accept header of every CVAT call. CVAT renders only its
// own media type (application/vnd.cvat+json); a request that asks for application/json alone is
// answered with 406 "Could not satisfy the request Accept header" before authentication is looked
// at. The fake below behaves like CVAT in that respect, so the test fails if the header ever goes
// back to application/json alone.
func TestCVATAcceptsItsOwnMediaType(t *testing.T) {
	var accept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accept = r.Header.Get("Accept")
		if !strings.Contains(r.Header.Get("Accept"), "*/*") {
			w.WriteHeader(http.StatusNotAcceptable)
			io.WriteString(w, `{"detail":"Could not satisfy the request Accept header."}`)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.cvat+json")
		io.WriteString(w, `{"count":0,"results":[]}`)
	}))
	defer srv.Close()

	c, err := NewCVAT(srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.do(context.Background(), session{key: "k"}, http.MethodGet, "/api/tasks", nil, nil, 1<<20); err != nil {
		t.Fatalf("CVAT refused the call: %v (Accept was %q)", err, accept)
	}
	if !strings.Contains(accept, "application/json") {
		t.Errorf("Accept = %q, want application/json as the preference", accept)
	}
}

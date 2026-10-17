// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// X5.7: /api/exp/* takes only the separate experiment key, never the normal API token.
func TestExpExportTokenOnly(t *testing.T) {
	const expToken = "exp0123456789abcdef0123456789abcd"
	s := &Server{Token: testToken, ExpAPIToken: expToken, AllowedHosts: []string{"127.0.0.1:18480"}}
	h := s.auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	cases := []struct {
		name, bearer string
		want         int
	}{
		{"normal API token", testToken, 401},
		{"no token", "", 401},
		{"experiment token", expToken, 204},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/api/exp/export?plan=p&table=runs", nil)
		r.Host = "127.0.0.1:18480"
		if c.bearer != "" {
			r.Header.Set("Authorization", "Bearer "+c.bearer)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != c.want {
			t.Errorf("%s: %d, expected %d", c.name, w.Code, c.want)
		}
	}
}

// The table check runs before any store access.
func TestExpExportUnknownTable(t *testing.T) {
	s := &Server{ExpAPIToken: "exp0123456789abcdef0123456789abcd"}
	r := httptest.NewRequest("GET", "/api/exp/export?plan=p&table=secrets", nil)
	w := httptest.NewRecorder()
	s.expExport(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code: %d", w.Code)
	}
}

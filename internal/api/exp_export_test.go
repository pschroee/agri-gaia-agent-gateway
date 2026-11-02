// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"agw/internal/chat"
	"agw/internal/store"
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

// A run whose chat was deleted has no reconcile rows and does not stop the export of the other runs.
func TestReconcileTableSkipsDeletedChat(t *testing.T) {
	runs := []store.PlanRun{{RunID: "a", ChatID: "gone"}, {RunID: "b", ChatID: "here"}, {RunID: "c"}}
	read := func(_ context.Context, id string) (chat.Reconciliation, error) {
		if id == "gone" {
			return chat.Reconciliation{}, fmt.Errorf("chat: %w", store.ErrNotFound)
		}
		return chat.Reconciliation{Calls: []chat.ReconciledCall{{ToolCallID: "t1"}}}, nil
	}
	tbl, err := reconcileTable(context.Background(), runs, read)
	if err != nil {
		t.Fatal(err)
	}
	if len(tbl.Rows) != 1 || tbl.Rows[0][0] != "b" {
		t.Fatalf("rows: %v", tbl.Rows)
	}
	boom := func(context.Context, string) (chat.Reconciliation, error) {
		return chat.Reconciliation{}, errors.New("db down")
	}
	if _, err := reconcileTable(context.Background(), runs, boom); err == nil {
		t.Fatal("other errors must still stop the export")
	}
}

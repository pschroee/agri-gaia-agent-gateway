// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agw/internal/store"
)

type fakeWorkspaces struct {
	archives map[string]string // chat -> archive bytes; a chat that is not listed has no archive
}

func (f fakeWorkspaces) OpenExpWorkspace(_ context.Context, chatID string) (io.ReadCloser, store.Workspace, error) {
	body, ok := f.archives[chatID]
	if !ok {
		return nil, store.Workspace{}, fmt.Errorf("%w: no workspace archive (never backed up)", store.ErrNotFound)
	}
	w := store.Workspace{ChatID: chatID}
	w.SHA256, w.Files, w.ArchiveSize = "abc123", 3, int64(len(body))
	return io.NopCloser(strings.NewReader(body)), w, nil
}

// BM3: the archive route takes only the experiment key; a chat without an archive answers 404.
func TestExpWorkspaceOnlyExperimentKey(t *testing.T) {
	const expToken = "exp0123456789abcdef0123456789abcd"
	s := &Server{Token: testToken, ExpAPIToken: expToken, AllowedHosts: []string{"127.0.0.1:18480"},
		Workspaces: fakeWorkspaces{archives: map[string]string{"c-with": "ARCHIVE"}}}
	h := s.auth(http.HandlerFunc(s.expWorkspace))
	cases := []struct {
		name, bearer, chat string
		want               int
	}{
		{"normal API token", testToken, "c-with", 401},
		{"no token", "", "c-with", 401},
		{"experiment token, archive", expToken, "c-with", 200},
		{"experiment token, no archive", expToken, "c-without", 404},
		{"experiment token, no chat", expToken, "", 400},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/api/exp/workspace?chat="+c.chat, nil)
		r.Host = "127.0.0.1:18480"
		if c.bearer != "" {
			r.Header.Set("Authorization", "Bearer "+c.bearer)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != c.want {
			t.Errorf("%s: %d, expected %d (%s)", c.name, w.Code, c.want, w.Body.String())
		}
		if c.want == 200 {
			if w.Body.String() != "ARCHIVE" || w.Header().Get("X-Agw-Workspace-SHA256") != "abc123" || w.Header().Get("X-Agw-Workspace-Files") != "3" {
				t.Errorf("%s: body %q headers %v", c.name, w.Body.String(), w.Header())
			}
		}
		if c.want == 404 && !strings.Contains(w.Body.String(), "never backed up") {
			t.Errorf("%s: the answer must say why: %s", c.name, w.Body.String())
		}
	}
}

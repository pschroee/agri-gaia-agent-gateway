// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agw/internal/oidc"
	"agw/internal/store"
)

const expTestKey = "0123456789abcdef0123456789abcdef-exp"

// expAuth builds the middleware with an experiment key that maps to sub-exp. The issuer is never
// contacted: SessionUser only reads the cookie, and the experiment path does not use it.
func expAuth(t *testing.T, owners Owners) http.Handler {
	t.Helper()
	auth, err := oidc.New(oidc.Config{Issuer: "http://127.0.0.1:1/realms/test", ClientID: "agw-agent", PublicURL: "http://127.0.0.1:18480"})
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{
		OIDC:        auth,
		ExpAPIToken: expTestKey,
		OwnerSource: owners,
		ExpAccount: func(context.Context) (oidc.User, error) {
			return oidc.User{Sub: "sub-exp", Username: "agent-exp"}, nil
		},
	}
	return s.auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !ExpKeyFrom(r.Context()) {
			w.WriteHeader(http.StatusTeapot)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
}

type fakeOwners map[string]string

func (f fakeOwners) ChatOwner(_ context.Context, chatID string) (string, error) {
	if owner, ok := f[chatID]; ok {
		return owner, nil
	}
	return "", store.ErrNotFound
}

func (f fakeOwners) ApprovalChat(context.Context, string) (string, error) {
	return "", store.ErrNotFound
}

// X1.3: the experiment key reaches only the routes of the smallest surface, everything else is 403.
func TestExpTokenRefusedOnOtherRoutes(t *testing.T) {
	h := expAuth(t, fakeOwners{})
	cases := []struct {
		method, path string
		want         int
	}{
		{"POST", "/api/chats", http.StatusNoContent},
		{"GET", "/api/chats/abc", http.StatusNoContent},
		{"GET", "/api/chats/abc/events", http.StatusNoContent},
		{"POST", "/api/exp/call", http.StatusNoContent},
		{"GET", "/api/approvals", http.StatusForbidden},
		{"GET", "/api/chats", http.StatusForbidden},
		{"POST", "/api/chats/abc/messages", http.StatusForbidden},
		{"POST", "/api/chats/abc/abort", http.StatusForbidden},
		{"GET", "/api/trash", http.StatusForbidden},
	}
	for _, c := range cases {
		r := httptest.NewRequest(c.method, "http://127.0.0.1:18480"+c.path, nil)
		r.Header.Set("Authorization", "Bearer "+expTestKey)
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != c.want {
			t.Errorf("%s %s: %d, want %d (%s)", c.method, c.path, w.Code, c.want, w.Body.String())
		}
	}
	// Without any key the route is unauthorized, as before.
	r := httptest.NewRequest("GET", "http://127.0.0.1:18480/api/exp/call", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("without key: %d", w.Code)
	}
}

// The key only creates chats that carry an experiment profile (X1.3).
func TestExpTokenOnlyExperimentChats(t *testing.T) {
	s := &Server{}
	r := httptest.NewRequest("POST", "/api/chats", strings.NewReader(`{"title":"x"}`))
	r = r.WithContext(withExpKey(context.Background()))
	w := httptest.NewRecorder()
	s.createChat(w, r)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "experiment") {
		t.Fatalf("without experiment: %d %s", w.Code, w.Body.String())
	}
}

// A chat of another user stays invisible to the experiment key (X1.3).
func TestExpTokenCannotReadUserChats(t *testing.T) {
	auth, err := oidc.New(oidc.Config{Issuer: "http://127.0.0.1:1/realms/test", ClientID: "agw-agent", PublicURL: "http://127.0.0.1:18480"})
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{
		OIDC:        auth,
		ExpAPIToken: expTestKey,
		OwnerSource: fakeOwners{"c-exp": "sub-exp", "c-anna": "sub-anna"},
		ExpAccount: func(context.Context) (oidc.User, error) {
			return oidc.User{Sub: "sub-exp", Username: "agent-exp"}, nil
		},
	}
	h := s.auth(s.ownChat(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	get := func(chat string) int {
		r := httptest.NewRequest("GET", "http://127.0.0.1:18480/api/chats/"+chat, nil)
		r.Header.Set("Authorization", "Bearer "+expTestKey)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	if got := get("c-exp"); got != http.StatusNoContent {
		t.Fatalf("own chat: %d", got)
	}
	if got := get("c-anna"); got != http.StatusNotFound {
		t.Fatalf("foreign chat: %d, want 404", got)
	}
}

// A request with a short or wrong key never enters the experiment path.
func TestExpTokenRejectsWrongKey(t *testing.T) {
	h := expAuth(t, fakeOwners{})
	for _, key := range []string{"", "short", expTestKey + "x"} {
		r := httptest.NewRequest("POST", "http://127.0.0.1:18480/api/exp/call", nil)
		if key != "" {
			r.Header.Set("Authorization", "Bearer "+key)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("key %q: %d, want 401", key, w.Code)
		}
	}
}

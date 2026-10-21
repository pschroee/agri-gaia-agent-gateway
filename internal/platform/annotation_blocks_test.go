// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The paths of V1 the agent may never reach: /cvat (its login answers with the user's CVAT key and
// session) and the two raw annotation routes.
func TestCVATBlockedForAgent(t *testing.T) {
	for _, tc := range []struct{ method, path, want string }{
		{"POST", "/cvat/auth/login", "annotation_ tools"},
		{"GET", "/cvat/user/exists", "annotation_ tools"},
		{"POST", "/Cvat/auth/login", "annotation_ tools"},
		{"POST", "/datasets/5/annotate", "use {{tool:annotation_task}}"},
		{"POST", "/datasets/auto-annotation", "use {{tool:annotation_function}}"},
	} {
		if _, err := Normalize(Request{Method: tc.method, Path: tc.path}); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s %s: %v", tc.method, tc.path, err)
		}
	}
	// Reading a dataset stays allowed; only the annotate route is refused.
	if _, err := Normalize(Request{Method: http.MethodGet, Path: "/datasets/5"}); err != nil {
		t.Errorf("GET /datasets/5: %v", err)
	}
}

// The gateway itself reaches the login (the annotation service logs the user in, E1): DoTrusted skips
// the blockers of the agent and hands the raw answer back, which the service reads the session from.
func TestCVATLoginTrustedOnly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			fmt.Fprintf(w, `{"access_token":%q,"expires_in":3600}`, jwt(map[string]any{"sub": "u-1"}))
			return
		}
		fmt.Fprint(w, `{"key":"the-key","csrftoken":"the-csrf","sessionid":"the-session"}`)
	}))
	t.Cleanup(srv.Close)
	c, err := New(Config{APIURL: srv.URL, TokenURL: srv.URL + "/token", User: "u", Password: "p"})
	if err != nil {
		t.Fatal(err)
	}
	login := Request{Method: http.MethodPost, Path: "/cvat/auth/login"}
	res, raw, err := c.DoTrusted(context.Background(), "chat-a", login, 1<<10)
	if err != nil || res.Status != "ok" || res.HTTPStatus != http.StatusOK {
		t.Fatalf("DoTrusted: %+v %v", res, err)
	}
	if !strings.Contains(string(raw), "the-key") || !strings.Contains(string(raw), "the-session") {
		t.Errorf("the raw answer is missing values the service needs: %s", raw)
	}
	// The body the agent could see is redacted like every other answer (csrftoken holds "token").
	if strings.Contains(res.Body, "the-csrf") {
		t.Errorf("the body was not redacted: %s", res.Body)
	}
	// The fixed block list applies to the gateway's own calls as well.
	if res, _, err := c.DoTrusted(context.Background(), "chat-a", Request{Method: http.MethodGet, Path: "/urls/basic-auth"}, 1<<10); err != nil || res.Status != "error" || !strings.Contains(res.Message, "blocked") {
		t.Errorf("a blocked path passed DoTrusted: %+v %v", res, err)
	}
}

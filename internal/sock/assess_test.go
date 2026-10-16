// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package sock

import (
	"strings"
	"testing"
)

// The REST binding never reaches the check paths: the gateway decides what a check runs on, and the
// measured set stays hidden (R0).
func TestRESTRefusesAssessPaths(t *testing.T) {
	b := &platformBackend{fakeBackend: fakeBackend{chat: "chat-ras", decide: "approved"}}
	c := start(t, b)
	for _, call := range []struct{ m, p, body string }{
		{"GET", "/platform-api/_agw/assess/checks/11111111-2222-3333-4444-555555555555", ""},
		{"GET", "/platform-api/_agw/assess/checks", ""},
		{"POST", "/platform-api/_agw/assess/checks", "{}"},
	} {
		code, body, _ := apiCall(t, c, call.m, call.p, "", call.body, nil)
		if code != 400 || !strings.Contains(body, "check calls go through the assess tools") {
			t.Errorf("%s %s: %d %s", call.m, call.p, code, body)
		}
	}
	// Nothing of it reached the backend, and every refusal is in the log.
	if len(b.reqs) != 0 {
		t.Errorf("the check paths reached the backend: %v", b.reqs)
	}
	refused := 0
	for _, l := range b.calls {
		if strings.HasPrefix(l, "cli:platform:refused: check calls go through") {
			refused++
		}
	}
	if refused != 3 {
		t.Errorf("refusals in the log: %d of 3 (%v)", refused, b.calls)
	}
}

// The raw tool request refuses the check paths too.
func TestRequestToolRefusesAssessPaths(t *testing.T) {
	b := &platformBackend{fakeBackend: fakeBackend{chat: "chat-raq", decide: "approved"}}
	c := start(t, b)
	for _, args := range []string{
		`{"method":"GET","path":"/_agw/assess/checks/11111111-2222-3333-4444-555555555555"}`,
		`{"method":"POST","path":"/_agw/assess/checks"}`,
	} {
		code, r := cliPlatform(t, c, "request", args)
		if code != 200 || r.Status != "error" || !strings.Contains(r.Message, "check calls go through the assess tools") {
			t.Errorf("%s: %d %+v", args, code, r)
		}
	}
	if len(b.reqs) != 0 {
		t.Errorf("the check paths reached the backend: %v", b.reqs)
	}
	// A path that only shares a prefix is not a check path.
	code, r := cliPlatform(t, c, "request", `{"method":"GET","path":"/datasets"}`)
	if code != 200 || r.Status != "ok" {
		t.Errorf("a normal call: %d %+v", code, r)
	}
}

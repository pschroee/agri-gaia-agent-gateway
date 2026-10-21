// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package sock

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// CONNECT and absolute URLs go to the proxy, /index/ and /npm/ to the package gate, everything else to the
// socket's own routes; without a handler the request is refused.
func TestWithRelayRoutes(t *testing.T) {
	mark := func(s string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Header().Set("X-Hit", s) })
	}
	h := WithRelay(mark("socket"), Relay{Proxy: mark("proxy"), Pkg: mark("pkg")})
	cases := []struct {
		method, target, want string
	}{
		{"CONNECT", "example.org:443", "proxy"},
		{"GET", "http://example.org/x", "proxy"},
		{"GET", "/index/torch/", "pkg"},
		{"GET", "/npm/left-pad", "pkg"},
		{"GET", "/internet", "socket"},
		{"POST", "/artifacts", "socket"},
	}
	for _, c := range cases {
		req := httptest.NewRequest(c.method, c.target, nil)
		if c.method == "CONNECT" {
			req.URL.Host = c.target
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if got := rec.Header().Get("X-Hit"); got != c.want {
			t.Errorf("%s %s: %q, want %q", c.method, c.target, got, c.want)
		}
	}
	none := WithRelay(mark("socket"), Relay{})
	for _, target := range []string{"http://example.org/x", "/index/torch/"} {
		rec := httptest.NewRecorder()
		none.ServeHTTP(rec, httptest.NewRequest("GET", target, nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s without a handler: %d", target, rec.Code)
		}
	}
}

func TestForegroundSole(t *testing.T) {
	f := NewForeground()
	if f.Sole("c") != "" {
		t.Fatal("none running")
	}
	a := f.add("t1", "c", func() {})
	if f.Sole("c") != "t1" {
		t.Fatal("one running")
	}
	f.add("t2", "c", func() {})
	if f.Sole("c") != "" {
		t.Fatal("two running are ambiguous")
	}
	f.remove("t1", a)
	if f.Sole("c") != "t2" || f.Sole("other") != "" {
		t.Fatal("after one ended / other chat")
	}
	var nilFg *Foreground
	if nilFg.Sole("c") != "" {
		t.Fatal("nil register")
	}
}

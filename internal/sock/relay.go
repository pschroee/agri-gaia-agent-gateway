// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package sock

import (
	"net/http"
	"strings"
)

// Relay holds what the execution sandbox's socket hands its internet traffic to (issue #175): the relay in the
// sandbox (agw-exec serve, 127.0.0.1:3128) forwards the bytes of every connection unchanged to the socket.
// Proxy takes CONNECT and requests with an absolute URL (the web proxy, origin exec), Pkg takes /index/… and /npm/…
// (the package gate). Either may be nil: the request is then refused.
type Relay struct {
	Proxy http.Handler
	Pkg   http.Handler
}

// WithRelay puts the relay in front of the socket's own routes.
func WithRelay(next http.Handler, r Relay) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case req.Method == http.MethodConnect || req.URL.IsAbs() || req.URL.Host != "":
			if r.Proxy == nil {
				http.Error(w, "internet relay not configured", http.StatusServiceUnavailable)
				return
			}
			r.Proxy.ServeHTTP(w, req)
		case strings.HasPrefix(req.URL.Path, "/index/") || strings.HasPrefix(req.URL.Path, "/npm/"):
			if r.Pkg == nil {
				http.Error(w, "package gate not configured", http.StatusServiceUnavailable)
				return
			}
			r.Pkg.ServeHTTP(w, req)
		default:
			next.ServeHTTP(w, req)
		}
	})
}

// Sole returns the tool call of the chat's only running foreground command; "" when there is none or several (the
// connection then belongs to no single call).
func (f *Foreground) Sole(chat string) string {
	if f == nil {
		return ""
	}
	ids := f.Running(chat)
	if len(ids) == 1 {
		return ids[0]
	}
	return ""
}

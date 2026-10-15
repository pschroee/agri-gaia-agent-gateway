// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"agw/internal/platform"
)

// The CLI reports a missing capability with its own exit code (G1.13), so that a script can tell it
// apart from an error and from a refusal by the user.
func TestPlatformCmdUnavailable(t *testing.T) {
	var (
		mu   sync.Mutex
		body []byte
	)
	set := func(status string) {
		mu.Lock()
		defer mu.Unlock()
		b, err := json.Marshal(platform.Result{Status: status, Message: platform.UnavailableText(
			"notebook", "/_agw/notebook is not configured on this instance", "Ask the user how to proceed without it")})
		if err != nil {
			t.Fatal(err)
		}
		body = b
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	dir := t.TempDir()
	sock := filepath.Join(dir, "agw.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}}}

	for _, c := range []struct {
		status string
		want   int
	}{
		{platform.StatusUnavailable, exitUnavailable},
		{"denied", exitDenied},
		{"rejected", exitRejected},
		{"ok", 0},
		{"error", 1},
	} {
		set(c.status)
		if code, err := platformCmd(hc, []string{"capabilities"}); code != c.want {
			t.Fatalf("status %s: exit %d (error %v), want %d", c.status, code, err, c.want)
		}
	}
}

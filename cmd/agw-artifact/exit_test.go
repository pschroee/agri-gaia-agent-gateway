// SPDX-FileCopyrightText: 2026 Philipp Schröer
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// unixClient serves h on a Unix socket and returns a client that dials it.
func unixClient(t *testing.T, h http.Handler) *http.Client {
	t.Helper()
	dir, err := os.MkdirTemp("", "agwcli")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := dir + "/agw.sock"
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(h)
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}}}
}

func TestReportExitSendsCodeAndCall(t *testing.T) {
	t.Setenv("PI_AGW_TOOL_CALL_ID", "tc9")
	var mu sync.Mutex
	var path, call, body string
	hc := unixClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		path, call, body = r.URL.Path, r.Header.Get("X-Agw-Tool-Call"), string(b)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	reportExit(hc, "agw-internet", 2)
	mu.Lock()
	defer mu.Unlock()
	if path != "/cli/exit" || call != "tc9" || !strings.Contains(body, `"cli":"agw-internet"`) || !strings.Contains(body, `"exit":2`) {
		t.Fatalf("got %q %q %q", path, call, body)
	}
}

func TestReportExitTimeout(t *testing.T) {
	release := make(chan struct{})
	hc := unixClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(release) })
	start := time.Now()
	reportExit(hc, "agw-platform", 1)
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("took %v", d)
	}
}

func TestUsageErrorReports(t *testing.T) {
	hc := unixClient(t, http.NotFoundHandler())
	code, _ := internet(hc, []string{"packages", "falsch"})
	if code != exitUsage {
		t.Fatalf("code %d, want %d", code, exitUsage)
	}
	// main sends this code through exit(), which reports every code other than 0.
	var got int32
	var mu sync.Mutex
	hc = unixClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got++
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	reportExit(hc, "agw-internet", code)
	mu.Lock()
	defer mu.Unlock()
	if got != 1 {
		t.Fatalf("reports: %d", got)
	}
}

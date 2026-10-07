// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
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
)

func TestParseInternetArgs(t *testing.T) {
	cases := []struct {
		args           []string
		action, reason string
	}{
		{[]string{"pip install scikit-learn"}, internetRequest, "pip install scikit-learn"},
		{[]string{"pip", "install", "torch"}, internetRequest, "pip install torch"},
		{[]string{"off"}, internetOff, ""},
		{[]string{"OFF"}, internetOff, ""},
		{[]string{" off "}, internetOff, ""},
		{[]string{"off-site download of a dataset"}, internetRequest, "off-site download of a dataset"},
		{[]string{"offline docs are not enough"}, internetRequest, "offline docs are not enough"},
		{[]string{"--", "off"}, internetRequest, "off"},
		{[]string{"--", "-v is needed", "for curl"}, internetRequest, "-v is needed for curl"},
		{[]string{"--help"}, internetHelpCmd, ""},
		{[]string{"-h"}, internetHelpCmd, ""},
	}
	for _, c := range cases {
		a, r, err := parseInternetArgs(c.args)
		if err != nil || a != c.action || r != c.reason {
			t.Errorf("%q: %s %q %v, want %s %q", c.args, a, r, err, c.action, c.reason)
		}
	}
	for _, bad := range [][]string{nil, {""}, {"  "}, {"--"}, {"--", " "}, {"off", "done with pip"}} {
		if a, _, err := parseInternetArgs(bad); err == nil {
			t.Errorf("%q: expected an error, got %s", bad, a)
		}
	}
}

// agw-internet off sends POST /internet/off without a body and succeeds also for "already off"; a reason that is
// literally "off" goes through "--" as a request.
func TestInternetCommandOverSocket(t *testing.T) {
	var mu sync.Mutex
	var got []string
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, r.Method+" "+r.URL.Path+" "+string(body))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/internet/off":
			_, _ = io.WriteString(w, `{"status":"already_off","name":"internet","message":"internet access was already off"}`)
		case "/internet":
			_, _ = io.WriteString(w, `{"status":"rejected","name":"internet","message":"rejected by the user"}`)
		default:
			http.NotFound(w, r)
		}
	}))
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
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}}}

	if code, err := internet(hc, []string{"off"}); code != 0 || err != nil {
		t.Fatalf("off: %d %v", code, err)
	}
	if code, err := internet(hc, []string{"--", "off"}); code != exitRejected || err != nil {
		t.Fatalf("request with reason off: %d %v", code, err)
	}
	if code, _ := internet(hc, []string{"off", "now"}); code != 1 {
		t.Fatalf("off with arguments: %d", code)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{"POST /internet/off ", `POST /internet {"reason":"off"}`}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("requests: %q", got)
	}
}

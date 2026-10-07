// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestParseUploadArgs(t *testing.T) {
	cases := []struct {
		args       []string
		file, name string
	}{
		{[]string{"out/data.csv"}, "out/data.csv", "data.csv"},
		{[]string{"chart.png", "--name", "Chart.png"}, "chart.png", "Chart.png"},
		{[]string{"--name", "r.pdf", "report.pdf"}, "report.pdf", "r.pdf"},
	}
	for _, c := range cases {
		f, n, err := parseUploadArgs(c.args)
		if err != nil || f != c.file || n != c.name {
			t.Errorf("%q: %q %q %v", c.args, f, n, err)
		}
	}
	if _, _, err := parseUploadArgs(nil); err == nil {
		t.Error("no file accepted")
	}
}

func TestPrintUpload(t *testing.T) {
	var b bytes.Buffer
	if code := printUpload(&b, uploadResult{Status: "stored", Text: "sent: \"a.csv\" is in the chat for the user"}); code != 0 || !strings.HasPrefix(b.String(), "sent: ") {
		t.Fatalf("stored: %d %q", code, b.String())
	}
	b.Reset()
	if code := printUpload(&b, uploadResult{Status: "rejected", Name: "a.csv", Message: "checksum does not match"}); code != 1 || !strings.Contains(b.String(), "checksum does not match") {
		t.Fatalf("rejected: %d %q", code, b.String())
	}
}

// Issue #62: agw-artifact upload sends a file inside the workspace with its checksum and no further question; files
// outside, symbolic links and FIFOs never reach the socket.
func TestUploadOverSocket(t *testing.T) {
	var mu sync.Mutex
	var got []string
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		sum := sha256.Sum256(body)
		mu.Lock()
		got = append(got, r.Method+" "+r.URL.RequestURI()+" "+string(body)+" "+r.Header.Get("X-Agw-Sha256")+" "+r.Header.Get("X-Agw-Tool-Call"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"stored","name":"data.csv","size":4,"sha256":"`+hex.EncodeToString(sum[:])+`","text":"sent: \"data.csv\" is in the chat for the user"}`)
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

	ws := t.TempDir()
	old := workspace
	workspace = ws
	t.Cleanup(func() { workspace = old })
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "data.csv"), []byte("a,b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "secret.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(other, "secret.txt"), filepath.Join(ws, "link.txt")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PI_AGW_TOOL_CALL_ID", "call_up1")

	if code, err := upload(hc, []string{filepath.Join(ws, "data.csv")}); code != 0 || err != nil {
		t.Fatalf("upload: %d %v", code, err)
	}
	for _, bad := range []string{filepath.Join(other, "secret.txt"), filepath.Join(ws, "link.txt"), filepath.Join(ws, "missing.csv")} {
		if code, err := upload(hc, []string{bad}); code != 1 || err == nil {
			t.Fatalf("%s: %d %v", bad, code, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	want := sha256.Sum256([]byte("a,b\n"))
	if len(got) != 1 || got[0] != "POST /artifacts?name=data.csv a,b\n "+hex.EncodeToString(want[:])+" call_up1" {
		t.Fatalf("requests: %q", got)
	}
}

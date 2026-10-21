// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCLIIndexListsEveryTool(t *testing.T) {
	idx := CLIIndex()
	for _, tool := range Tools {
		prefix := "agw-platform " + tool.CLI + tool.Synopsis()
		n := 0
		for _, line := range strings.Split(idx, "\n") {
			if line == prefix || line == prefix+" (approval)" {
				n++
			}
		}
		if n != 1 {
			t.Errorf("tool %s: %d lines in the index, want 1", tool.Name, n)
		}
	}
	if !strings.Contains(idx, "agw-platform rename-dataset <dataset_id> <name> (approval)\n") {
		t.Errorf("rename-dataset line missing or not marked:\n%s", idx)
	}
	t.Logf("command index: %d bytes", len(idx))
}

func TestAPIIndexFromDropsBlocked(t *testing.T) {
	raw, err := os.ReadFile("testdata/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	idx, err := APIIndexFrom(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{" /users", " /urls", " /service", " /network", " /integrated-services/uploadservice"} {
		if strings.Contains(idx, bad) {
			t.Errorf("blocked path %q in the index", bad)
		}
	}
	if !strings.Contains(idx, "PATCH /datasets/{dataset_id}") {
		t.Error("PATCH /datasets/{dataset_id} missing")
	}
	all, _ := openAPIDigest(raw, "")
	if strings.Contains(idx, "(130 operations") || !strings.Contains(idx, " operations; * = required") {
		t.Errorf("closing line not counted anew:\n%s", idx[len(idx)-200:])
	}
	if len(idx) >= len(all) {
		t.Errorf("index (%d) not smaller than the digest (%d)", len(idx), len(all))
	}
	t.Logf("api index: %d bytes", len(idx))
}

func TestAPIIndexLimits(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"paths":{`)
	for i := 0; i < 400; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`"/p` + strings.Repeat("x", i%5) + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26)) + `":{"get":{"summary":"` + strings.Repeat("long ", 120) + `\u001b[31m red\u0007"}}`)
	}
	b.WriteString("}}")
	idx, err := APIIndexFrom([]byte(b.String()))
	if err != nil {
		t.Fatal(err)
	}
	if len(idx) > apiIndexMax {
		t.Errorf("index is %d bytes, limit %d", len(idx), apiIndexMax)
	}
	if strings.ContainsAny(idx, "\x1b\x07") {
		t.Error("control characters in the index")
	}
	for _, line := range strings.Split(idx, "\n") {
		if len([]rune(line)) > apiIndexLine {
			t.Fatalf("line of %d runes: %.60s…", len([]rune(line)), line)
		}
	}
	if !strings.Contains(idx, apiIndexTruncMsg) {
		t.Error("truncation not announced")
	}
}

func TestAPIIndexFetchWithoutAuth(t *testing.T) {
	raw, err := os.ReadFile("testdata/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	var calls int32
	status := http.StatusInternalServerError
	body := raw
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		if r.Header.Get("Authorization") != "" || r.URL.Path != "/openapi.json" {
			t.Errorf("request %s with Authorization %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	now := time.Now()
	x := NewAPIIndex(srv.URL, nil)
	x.now = func() time.Time { return now }
	ctx := context.Background()
	if x.Get(ctx) != "" || x.SHA256() != "" {
		t.Fatal("HTTP 500 must leave the index empty")
	}
	now = now.Add(30 * time.Second)
	if x.Get(ctx) != "" || atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("retry before 60 s: %d calls", calls)
	}
	now = now.Add(31 * time.Second)
	status = http.StatusOK
	got := x.Get(ctx)
	if got == "" || atomic.LoadInt32(&calls) != 2 || len(x.SHA256()) != 64 {
		t.Fatalf("after 60 s: %d bytes, %d calls", len(got), calls)
	}
	now = now.Add(time.Hour)
	if x.Get(ctx) != got || atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("a filled index must not be fetched again: %d calls", calls)
	}
	// More than 2 MiB is an error.
	status, body = http.StatusOK, []byte(strings.Repeat(" ", apiIndexBodyMax+10))
	y := NewAPIIndex(srv.URL, nil)
	if y.Get(ctx) != "" {
		t.Error("an oversized description must not be accepted")
	}
}

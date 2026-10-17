// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package exectrace

import (
	"testing"
	"time"
)

// X6.4: failed network and file access, permission errors and unknown lines.
func TestParseFailedSyscalls(t *testing.T) {
	raw := []byte(`325 1759234567.123456 openat(AT_FDCWD, "/workspace/fehlt.txt", O_RDONLY) = -1 ENOENT (No such file or directory)
325 1759234567.200001 connect(3, {sa_family=AF_INET, sin_port=htons(1), sin_addr=inet_addr("127.0.0.1")}, 16) = -1 ECONNREFUSED (Connection refused)
326 1759234567.300002 openat(AT_FDCWD, "/root/x", O_RDONLY) = -1 EACCES (Permission denied)
325 1759234567.400003 execve("/nonexistent/tool", ["tool"], 0x7ffe /* 20 vars */) = -1 ENOENT (No such file or directory)
325 1759234567.500004 +++ exited with 0 +++

`)
	got := Parse(raw)
	if len(got) != 5 {
		t.Fatalf("rows: %d %+v", len(got), got)
	}
	if got[0].PID != 325 || got[0].Name != "openat" || got[0].Errno != "ENOENT" ||
		got[0].Args != `AT_FDCWD, "/workspace/fehlt.txt", O_RDONLY` {
		t.Fatalf("openat: %+v", got[0])
	}
	want := time.Unix(1759234567, 123456000)
	if !got[0].TS.Equal(want) {
		t.Fatalf("ts: %v, want %v", got[0].TS, want)
	}
	if got[1].Name != "connect" || got[1].Errno != "ECONNREFUSED" {
		t.Fatalf("connect: %+v", got[1])
	}
	if got[2].PID != 326 || got[2].Errno != "EACCES" {
		t.Fatalf("eacces: %+v", got[2])
	}
	if got[3].Name != "execve" || got[3].Errno != "ENOENT" {
		t.Fatalf("execve: %+v", got[3])
	}
	// What the parser does not know stays visible; it does not vanish.
	if got[4].Name != Unknown || got[4].Args != "325 1759234567.500004 +++ exited with 0 +++" {
		t.Fatalf("unknown: %+v", got[4])
	}
}

// Arguments may contain parentheses; the match anchors on "= -1".
func TestParseArgsWithParentheses(t *testing.T) {
	got := Parse([]byte(`7 1759234567.123456 openat(AT_FDCWD, "/tmp/a)b(c", O_RDONLY) = -1 ENOENT (No such file or directory)`))
	if len(got) != 1 || got[0].Args != `AT_FDCWD, "/tmp/a)b(c", O_RDONLY` {
		t.Fatalf("args: %+v", got)
	}
}

func TestParseEmpty(t *testing.T) {
	if got := Parse(nil); got != nil {
		t.Fatalf("nil: %+v", got)
	}
	if got := Parse([]byte("\n\n")); len(got) != 0 {
		t.Fatalf("blank lines: %+v", got)
	}
}

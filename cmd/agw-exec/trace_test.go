// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// X6.1: a traced operation starts under strace as root; the helper receives the uid to drop
// to. The tracer keeps its uid so that it can attach without CAP_SYS_PTRACE.
func TestSpawnWithTraceArgs(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGW_EXEC_TRACE", "failed")
	t.Setenv("AGW_EXEC_TRACE_DIR", dir)
	s := newServer(io.Discard, "/usr/local/bin/agw-exec", 10001, 10001)
	if !s.trace {
		t.Fatal("trace not enabled")
	}
	cmd := s.command(7, nil)
	if filepath.Base(cmd.Args[0]) != "strace" {
		t.Fatalf("command: %v", cmd.Args)
	}
	for _, a := range []string{"-f", "-qq", "--status=failed", "-ttt", "trace=" + tracedSyscalls} {
		if !slices.Contains(cmd.Args, a) {
			t.Fatalf("missing %q in %v", a, cmd.Args)
		}
	}
	wantTrace := filepath.Join(dir, "op-7.trace")
	if i := slices.Index(cmd.Args, "-o"); i < 0 || i+1 >= len(cmd.Args) || cmd.Args[i+1] != wantTrace {
		t.Fatalf("trace file in %v, want %s", cmd.Args, wantTrace)
	}
	if !slices.Contains(cmd.Args, s.self) {
		t.Fatalf("helper missing in %v", cmd.Args)
	}
	if cmd.SysProcAttr.Credential != nil {
		t.Fatal("strace must start as root; the helper drops the uid itself")
	}
	for _, kv := range []string{"AGW_EXEC_SETUID=10001", "AGW_EXEC_SETGID=10001"} {
		if !slices.Contains(cmd.Env, kv) {
			t.Fatalf("missing %q in %v", kv, cmd.Env)
		}
	}
}

// Without tracing nothing changes: the helper starts as the agent user.
func TestSpawnWithoutTrace(t *testing.T) {
	t.Setenv("AGW_EXEC_TRACE", "")
	s := newServer(io.Discard, "/usr/local/bin/agw-exec", 10001, 10001)
	cmd := s.command(7, nil)
	if !slices.Equal(cmd.Args, []string{s.self, "op"}) {
		t.Fatalf("command: %v", cmd.Args)
	}
	if cmd.SysProcAttr.Credential == nil || cmd.SysProcAttr.Credential.Uid != 10001 || cmd.SysProcAttr.Credential.Gid != 10001 {
		t.Fatalf("credential: %+v", cmd.SysProcAttr.Credential)
	}
	if slices.Contains(cmd.Env, "AGW_EXEC_SETUID=10001") {
		t.Fatalf("trace variables without tracing: %v", cmd.Env)
	}
}

// X6.3: the supervisor reads the file after the operation and removes it.
func TestTraceReadAndRemoved(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "op-1.trace")
	want := "7 1759234567.123456 openat(AT_FDCWD, \"/workspace/x\", O_RDONLY) = -1 ENOENT (No such file or directory)\n"
	if err := os.WriteFile(path, []byte(want), 0600); err != nil {
		t.Fatal(err)
	}
	b, truncated := readTrace(path)
	if string(b) != want || truncated {
		t.Fatalf("read: %q truncated=%v", b, truncated)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if b, truncated := readTrace(path); b != nil || truncated {
		t.Fatalf("after removal: %q truncated=%v", b, truncated)
	}
}

// X6.3: more than 1 MiB is cut, and the last line stays whole for the parser.
func TestTraceTruncatedAt1MiB(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "op-2.trace")
	line := []byte("7 1759234567.123456 openat(AT_FDCWD, \"/workspace/x\", O_RDONLY) = -1 ENOENT (No such file or directory)\n")
	var buf bytes.Buffer
	for buf.Len() <= traceMaxBytes {
		buf.Write(line)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	b, truncated := readTrace(path)
	if !truncated {
		t.Fatal("not marked as truncated")
	}
	if len(b) > traceMaxBytes {
		t.Fatalf("too long: %d bytes", len(b))
	}
	if len(b) == 0 || !strings.HasSuffix(string(b), "\n") {
		t.Fatalf("partial last line: %q", b[len(b)-40:])
	}
}

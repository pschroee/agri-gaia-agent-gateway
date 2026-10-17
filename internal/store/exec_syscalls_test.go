// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"testing"
	"time"
)

// X6.4: the trace of an execution becomes one exec_syscalls row per failed syscall, with the
// execution and the run, and the export carries them (X5.7).
func TestExecSyscallsStoredAndExported(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c, err := s.CreateChat(ctx, NewChat{Title: "trace", Model: "model-a", Variant: "exp:cli",
		Experiment: &Experiment{Cell: "cli-service-none-model-a", Run: "pilot/cli-service-none-model-a/t1/1"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.StartExpRun(ctx, ExpRun{ID: "pilot/cli-service-none-model-a/t1/1", Key: "pilot/cli-service-none-model-a/t1/1",
		Plan: "pilot", Cell: "cli-service-none-model-a", Task: "t1", Rep: 1, ChatID: c.ID, Model: "model-a"}); err != nil {
		t.Fatal(err)
	}
	trace := `325 1759234567.123456 openat(AT_FDCWD, "/workspace/fehlt.txt", O_RDONLY) = -1 ENOENT (No such file or directory)
325 1759234567.200001 connect(3, {sa_family=AF_INET, sin_port=htons(1), sin_addr=inet_addr("127.0.0.1")}, 16) = -1 ECONNREFUSED (Connection refused)
325 1759234567.300002 not a strace line
`
	e, err := s.AddToolExecution(ctx, ToolExecution{ChatID: c.ID, SlotID: "slot-1", ToolCallID: "call-1", Tool: "bash",
		Op: "bash", Args: []byte(`{"command":"cat fehlt.txt"}`), StartedAt: time.Now(), Trace: []byte(trace), TraceTruncated: true})
	if err != nil {
		t.Fatal(err)
	}
	if e.ID == 0 {
		t.Fatal("execution not stored")
	}
	tbl, err := s.ExportTable(ctx, "pilot", "syscalls")
	if err != nil {
		t.Fatal(err)
	}
	if len(tbl.Rows) != 3 {
		t.Fatalf("rows: %d %+v", len(tbl.Rows), tbl.Rows)
	}
	idx := func(name string) int {
		for i, col := range tbl.Columns {
			if col == name {
				return i
			}
		}
		t.Fatalf("column %q missing (%v)", name, tbl.Columns)
		return -1
	}
	if tbl.Rows[0][idx("run_id")] != "pilot/cli-service-none-model-a/t1/1" ||
		tbl.Rows[0][idx("execution_id")] != e.ID {
		t.Fatalf("assignment: %+v", tbl.Rows[0])
	}
	if tbl.Rows[0][idx("syscall")] != "openat" || tbl.Rows[0][idx("errno")] != "ENOENT" {
		t.Fatalf("openat: %+v", tbl.Rows[0])
	}
	if tbl.Rows[1][idx("syscall")] != "connect" || tbl.Rows[1][idx("errno")] != "ECONNREFUSED" {
		t.Fatalf("connect: %+v", tbl.Rows[1])
	}
	if tbl.Rows[2][idx("syscall")] != "?" {
		t.Fatalf("unknown line: %+v", tbl.Rows[2])
	}
	if tbl.Rows[0][idx("trace_truncated")] != true {
		t.Fatalf("truncation flag: %+v", tbl.Rows[0])
	}
}

// X6/#145: a background task's trace is attached to the tool_executions row that was written at
// its start; a repeated (late) call replaces the rows instead of duplicating them.
func TestAttachExecTraceToBackgroundStart(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c, err := s.CreateChat(ctx, NewChat{Title: "bg trace", Model: "model-a", Variant: "exp:cli",
		Experiment: &Experiment{Cell: "cli-service-none-model-a", Run: "pilot/cli-service-none-model-a/t1/1"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.StartExpRun(ctx, ExpRun{ID: "pilot/cli-service-none-model-a/t1/1", Key: "pilot/cli-service-none-model-a/t1/1",
		Plan: "pilot", Cell: "cli-service-none-model-a", Task: "t1", Rep: 1, ChatID: c.ID, Model: "model-a"}); err != nil {
		t.Fatal(err)
	}
	// The socket writes this row when the background task starts.
	e, err := s.AddToolExecution(ctx, ToolExecution{ChatID: c.ID, SlotID: "slot-1", Session: "main", ToolCallID: "call_bg",
		Tool: "bash", Op: "bg_start", Args: []byte(`{"run_in_background":true}`), StartedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	trace := "7 1759234567.123456 openat(AT_FDCWD, \"/workspace/fehlt.txt\", O_RDONLY) = -1 ENOENT (No such file or directory)\n"
	ok, err := s.AttachExecTrace(ctx, c.ID, "call_bg", "bg-1", "slot-1", []byte(trace), true)
	if err != nil || !ok {
		t.Fatalf("attach: %v %v", ok, err)
	}
	tbl, err := s.ExportTable(ctx, "pilot", "syscalls")
	if err != nil || len(tbl.Rows) != 1 {
		t.Fatalf("export: %+v %v", tbl, err)
	}
	idx := func(name string) int {
		for i, col := range tbl.Columns {
			if col == name {
				return i
			}
		}
		t.Fatalf("column %q missing", name)
		return -1
	}
	if tbl.Rows[0][idx("execution_id")] != e.ID || tbl.Rows[0][idx("syscall")] != "openat" ||
		tbl.Rows[0][idx("errno")] != "ENOENT" || tbl.Rows[0][idx("trace_truncated")] != true {
		t.Fatalf("row: %+v", tbl.Rows[0])
	}
	// A late Done frame of the already ended row replaces the trace and does not duplicate rows.
	trace2 := "7 1759234568.000001 connect(3, {sa_family=AF_INET, sin_port=htons(1), sin_addr=inet_addr(\"127.0.0.1\")}, 16) = -1 ECONNREFUSED (Connection refused)\n"
	ok, err = s.AttachExecTrace(ctx, c.ID, "call_bg", "bg-1", "slot-1", []byte(trace2), false)
	if err != nil || !ok {
		t.Fatalf("second attach: %v %v", ok, err)
	}
	tbl, err = s.ExportTable(ctx, "pilot", "syscalls")
	if err != nil || len(tbl.Rows) != 1 || tbl.Rows[0][idx("syscall")] != "connect" ||
		tbl.Rows[0][idx("errno")] != "ECONNREFUSED" || tbl.Rows[0][idx("trace_truncated")] != false {
		t.Fatalf("replaced row: %+v %v", tbl.Rows, err)
	}
	// Unknown execution: the caller can retry.
	if ok, err := s.AttachExecTrace(ctx, c.ID, "call_other", "bg-9", "slot-1", []byte(trace), false); err != nil || ok {
		t.Fatalf("unknown row: %v %v", ok, err)
	}
}

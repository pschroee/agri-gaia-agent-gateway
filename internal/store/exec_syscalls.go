// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	_ "embed"
	"time"

	"github.com/jackc/pgx/v5"

	"agw/internal/exectrace"
)

//go:embed exec_trace_schema.sql
var execTraceSchemaSQL string

func init() { registerSchema(execTraceSchemaSQL) }

// insertExecSyscalls parses the trace of an operation (X6.4) and stores one row per failed
// syscall with the execution it belongs to. It runs in the transaction that writes the
// tool_executions row; the caller rolls back when this fails.
func insertExecSyscalls(ctx context.Context, tx pgx.Tx, e ToolExecution, executionID int64) error {
	calls := exectrace.Parse(e.Trace)
	if len(calls) == 0 {
		return nil
	}
	var chat any
	if e.ChatID != "" {
		chat = e.ChatID
	}
	now := time.Now()
	rows := make([][]any, len(calls))
	for i, c := range calls {
		ts := c.TS
		if ts.IsZero() {
			ts = now
		}
		rows[i] = []any{chat, e.SlotID, executionID, c.PID, noNUL(c.Name), noNUL(c.Args), noNUL(c.Errno),
			ts, e.TraceTruncated}
	}
	_, err := tx.CopyFrom(ctx, pgx.Identifier{"exec_syscalls"},
		[]string{"chat_id", "slot_id", "execution_id", "pid", "syscall", "args", "errno", "ts", "trace_truncated"},
		pgx.CopyFromRows(rows))
	return err
}

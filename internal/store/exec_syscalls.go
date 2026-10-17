// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	_ "embed"
	"errors"
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

// AttachExecTrace attaches the trace of a background task to the tool_executions row it already
// has (X6, #145): the row is written when the task starts, the Done frame arrives later. A row
// that has already ended is updated as well; a repeated call replaces the rows instead of
// duplicating them. false means the row does not exist (yet), so the caller can retry.
func (s *Store) AttachExecTrace(ctx context.Context, chatID, toolCallID, taskID, slotID string, trace []byte, truncated bool) (bool, error) {
	if len(trace) == 0 {
		return false, nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var id int64
	var slot string
	// A task started with run_in_background has the row of its bg_start; one that was turned into
	// a background task from a foreground command has the row of the original bash call.
	err = tx.QueryRow(ctx, `SELECT id, slot_id FROM tool_executions
WHERE chat_id=$1 AND tool_call_id=$2
  AND (op='bg_start' OR (op='bash' AND error LIKE '%moved to background as ' || $3))
ORDER BY id DESC LIMIT 1`, chatID, toolCallID, taskID).Scan(&id, &slot)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if slot == "" {
		slot = slotID
	}
	if _, err := tx.Exec(ctx, `DELETE FROM exec_syscalls WHERE execution_id=$1`, id); err != nil {
		return false, err
	}
	if err := insertExecSyscalls(ctx, tx, ToolExecution{ChatID: chatID, SlotID: slot, Trace: trace, TraceTruncated: truncated}, id); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

-- SPDX-FileCopyrightText: 2026 Philipp Schröer
--
-- SPDX-License-Identifier: MIT

-- Failed network and file syscalls of an operation in the execution sandbox (X6.4).
-- One row per line of the trace; execution_id is the tool_executions row of the operation.
-- Registered through registerSchema, so this file is applied after schema.sql at every start
-- and stays idempotent.

CREATE TABLE IF NOT EXISTS exec_syscalls (
    id              bigserial PRIMARY KEY,
    chat_id         uuid,                            -- no FK: the row outlives a deleted chat (see trash.go)
    slot_id         text NOT NULL DEFAULT '',
    execution_id    bigint NOT NULL,                 -- tool_executions.id
    pid             integer NOT NULL DEFAULT 0,      -- pid in the execution sandbox
    syscall         text NOT NULL DEFAULT '',        -- openat, connect, ...; "?" for unparsable lines
    args            text NOT NULL DEFAULT '',        -- arguments as strace printed them, or the raw line
    errno           text NOT NULL DEFAULT '',        -- ENOENT, ECONNREFUSED, ...
    ts              timestamptz NOT NULL DEFAULT now(),
    trace_truncated boolean NOT NULL DEFAULT false   -- the trace was cut at 1 MiB (X6.3)
);
CREATE INDEX IF NOT EXISTS exec_syscalls_execution ON exec_syscalls (execution_id);
CREATE INDEX IF NOT EXISTS exec_syscalls_chat ON exec_syscalls (chat_id);

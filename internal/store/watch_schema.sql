-- SPDX-FileCopyrightText: 2026 Philipp Schröer
--
-- SPDX-License-Identifier: MIT

-- Observed platform jobs (autonomy A-E2). Registered through registerSchema, so this file is applied
-- after schema.sql at every start and stays idempotent.
--
-- One table for every observed job, inside and outside plans: the progress of a plan step is the
-- progress of its job (plan_steps.watch_job), and a job outside a plan has an empty plan_id. The
-- kinds (platform_task, training_run, image_build, notebook_run, annotation_run, edge_deployment)
-- poll the platform with the user's token; progress never comes from the agent (D2.1).

CREATE TABLE IF NOT EXISTS watch_jobs (
    id           text PRIMARY KEY,                 -- "w" + 12 hex
    chat_id      uuid NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
    plan_id      text NOT NULL DEFAULT '',         -- '' outside plans
    step_idx     integer NOT NULL DEFAULT 0,
    tool_call_id text NOT NULL DEFAULT '',         -- store.ToolCallFrom(ctx), for cards at the tool call
    kind         text NOT NULL,
    ref          text NOT NULL,                    -- what the kind polls, e.g. the task or container id
    state        text NOT NULL DEFAULT 'running',  -- running | done | failed
    progress     double precision,                 -- percent, NULL while unknown
    message      text NOT NULL DEFAULT '',         -- one line from the platform
    eta          timestamptz,                      -- estimated end, NULL while unknown (D2.4)
    log_tail     text NOT NULL DEFAULT '',          -- at most 20 lines / 4000 runes (D2.5)
    outputs      text NOT NULL DEFAULT '[]',        -- JSON [{resource,id,label}]
    fail_reads   integer NOT NULL DEFAULT 0,        -- reading calls that failed in a row (D2.3)
    started_at   timestamptz NOT NULL DEFAULT now(),
    checked_at   timestamptz,
    ended_at     timestamptz
);
CREATE INDEX IF NOT EXISTS watch_jobs_open ON watch_jobs (state) WHERE state = 'running';
CREATE INDEX IF NOT EXISTS watch_jobs_chat ON watch_jobs (chat_id, started_at DESC);

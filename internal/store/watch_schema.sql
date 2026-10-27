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
    outputs      text NOT NULL DEFAULT '[]',        -- JSON [{resource,id,label,size}]; size 0 while the kind knows none
    fail_reads   integer NOT NULL DEFAULT 0,        -- reading calls that failed in a row (D2.3)
    started_at   timestamptz NOT NULL DEFAULT now(),
    checked_at   timestamptz,
    ended_at     timestamptz
);
CREATE INDEX IF NOT EXISTS watch_jobs_open ON watch_jobs (state) WHERE state = 'running';
CREATE INDEX IF NOT EXISTS watch_jobs_chat ON watch_jobs (chat_id, started_at DESC);

-- The address the user can open for a job (own code C-E4): the kind notebook_run fills it with the run in the
-- user's JupyterHub, every other kind leaves it empty. It appears with the first poll of the job.
ALTER TABLE watch_jobs ADD COLUMN IF NOT EXISTS link text NOT NULL DEFAULT '';

-- Who stopped a job (own code C-E4, the words of the aborts of issue #120/L3): "user" for the user's stop route,
-- "agent" for a stop of the agent's own tools, empty while nobody stopped it. Written once, by the call that
-- stops; a progress update leaves it alone.
ALTER TABLE watch_jobs ADD COLUMN IF NOT EXISTS stopped_by text NOT NULL DEFAULT '';

-- The run's own start and end, as the run wrote them (issue #203): the kind notebook_run takes them from the
-- markers of its starter, every other kind leaves them NULL. started_at/ended_at stay the gateway's clock
-- (job created, job seen ended).
ALTER TABLE watch_jobs ADD COLUMN IF NOT EXISTS run_started_at timestamptz;
ALTER TABLE watch_jobs ADD COLUMN IF NOT EXISTS run_ended_at timestamptz;
-- Last training epoch as JSON {epoch, epochs, values, at} (issue #221); empty until a poll reports one.
ALTER TABLE watch_jobs ADD COLUMN IF NOT EXISTS last_epoch text NOT NULL DEFAULT '';

-- History of the phases of a job (issue #246): one row when the phase or the state of a job changes, at most 30
-- per job (the oldest intermediate phases fall away, the first and the last stay).
CREATE TABLE IF NOT EXISTS watch_job_events (
    seq     bigserial PRIMARY KEY,
    job_id  text NOT NULL REFERENCES watch_jobs(id) ON DELETE CASCADE,
    at      timestamptz NOT NULL DEFAULT now(),
    text    text NOT NULL,
    state   text NOT NULL DEFAULT 'running',
    percent double precision
);
CREATE INDEX IF NOT EXISTS watch_job_events_job ON watch_job_events (job_id, seq);

-- SPDX-FileCopyrightText: 2026 Philipp Schröer
--
-- SPDX-License-Identifier: MIT

-- Plans with one approval (issue #70). Executed after schema.sql at every start; idempotent.
-- Text instead of jsonb for bodies and queries: jsonb would reorder keys and normalise numbers,
-- and the exact match of a call against its step depends on the stored bytes.

CREATE TABLE IF NOT EXISTS plans (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    chat_id      uuid NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
    approval_id  text NOT NULL DEFAULT '',
    title        text NOT NULL,
    session      text NOT NULL DEFAULT '',
    tool_call_id text NOT NULL DEFAULT '',
    via          text NOT NULL,
    state        text NOT NULL DEFAULT 'proposed',  -- proposed | approved | rejected | expired | completed | failed | superseded
    created_at   timestamptz NOT NULL DEFAULT now(),
    decided_at   timestamptz,
    expires_at   timestamptz
);
CREATE INDEX IF NOT EXISTS plans_chat ON plans (chat_id, created_at);

CREATE TABLE IF NOT EXISTS plan_steps (
    plan_id          uuid NOT NULL REFERENCES plans(id) ON DELETE CASCADE,
    idx              integer NOT NULL,                  -- from 1
    description      text NOT NULL DEFAULT '',
    tool             text NOT NULL DEFAULT '',          -- empty for a raw call
    method           text NOT NULL,
    path             text NOT NULL,                     -- may contain {{stepN.id}}
    query            text NOT NULL DEFAULT '',
    body             text NOT NULL DEFAULT '',
    body_sha256      text NOT NULL DEFAULT '',
    form             text NOT NULL DEFAULT '',
    form_sha256      text NOT NULL DEFAULT '',
    files            text NOT NULL DEFAULT '[]',        -- JSON [{field,path,sha256}]
    effect           text NOT NULL,                     -- read | write | compute | irreversible
    state            text NOT NULL DEFAULT 'open',      -- open | running | done | failed
    result           text NOT NULL DEFAULT '',
    created_resource text NOT NULL DEFAULT '',
    created_id       text NOT NULL DEFAULT '',
    done_at          timestamptz,
    PRIMARY KEY (plan_id, idx)
);

-- Arguments of a step that is a platform tool with steps (issue #70, option b): the step is matched on the tool name
-- and these arguments, not on the HTTP request the tool builds. Empty for a raw call or a tool without steps.
ALTER TABLE plan_steps ADD COLUMN IF NOT EXISTS args text NOT NULL DEFAULT '';

-- What the gateway says a step does (issue #72): written from the stored call and platform state read at proposal
-- time, never from the agent's text. Empty for steps from before #72.
ALTER TABLE plan_steps ADD COLUMN IF NOT EXISTS summary text NOT NULL DEFAULT '';
-- The same for an approval (single platform calls and plans); empty for other kinds and older entries.
ALTER TABLE approvals ADD COLUMN IF NOT EXISTS summary text;

-- The summary as it was first proposed (issue L2/#119): a re-read at the moment of the approval overwrites summary,
-- and this column keeps the first text so that the card can show "As proposed" next to the current one. NULL until
-- the first change; then it holds the text the user saw when the approval was raised.
ALTER TABLE plan_steps ADD COLUMN IF NOT EXISTS summary_proposed text;

-- Autonomous plans with a budget and a delegation derived from their steps (A-E1). A plan keeps its mode
-- 'interactive' unless the agent asks for "mode": "autonomous"; only then do the budget columns and the
-- grant carry values. pause_reason names why a plan is paused (abort, budget_cost, user); replaces is the
-- plan this one superseded. ended_at marks the end of an autonomous plan (completed, failed, cancelled).
ALTER TABLE plans ADD COLUMN IF NOT EXISTS mode text NOT NULL DEFAULT 'interactive';
ALTER TABLE plans ADD COLUMN IF NOT EXISTS max_duration_s integer NOT NULL DEFAULT 1800;
ALTER TABLE plans ADD COLUMN IF NOT EXISTS max_llm_cost double precision NOT NULL DEFAULT 0;
ALTER TABLE plans ADD COLUMN IF NOT EXISTS max_auto_turns integer NOT NULL DEFAULT 0;
ALTER TABLE plans ADD COLUMN IF NOT EXISTS auto_turns integer NOT NULL DEFAULT 0;
ALTER TABLE plans ADD COLUMN IF NOT EXISTS grant_json text NOT NULL DEFAULT '';
ALTER TABLE plans ADD COLUMN IF NOT EXISTS pause_reason text NOT NULL DEFAULT '';
ALTER TABLE plans ADD COLUMN IF NOT EXISTS replaces text NOT NULL DEFAULT '';
ALTER TABLE plans ADD COLUMN IF NOT EXISTS ended_at timestamptz;

-- max_attempts: how often an autonomous plan may repeat a failed step (1 to 3, D1.6); attempts counts the
-- claims so far (incremented by ClaimPlanStep).
ALTER TABLE plan_steps ADD COLUMN IF NOT EXISTS max_attempts integer NOT NULL DEFAULT 1;
ALTER TABLE plan_steps ADD COLUMN IF NOT EXISTS attempts integer NOT NULL DEFAULT 0;

-- confirm_each: the route of the step asks for every single call (G1.1, from delegation.Classify at
-- proposal time). Such a step is never covered by a plan, also not after the plan was loaded again —
-- without this column a reloaded plan would cover a call that has to ask the user every time (A-E2).
ALTER TABLE plan_steps ADD COLUMN IF NOT EXISTS confirm_each boolean NOT NULL DEFAULT false;

-- watch_job names the observed platform job the step waits for (A-E2): the step is "waiting" (a value of
-- the state column, no new type) until the job ends, and the row of watch_jobs carries state, percent,
-- message, ETA and log tail of that job.
ALTER TABLE plan_steps ADD COLUMN IF NOT EXISTS watch_job text NOT NULL DEFAULT '';
-- parked_approval names the approval a non-covered step of an autonomous plan waits for (D2.8): the
-- approval stays open until the plan expires, and the gateway repeats the stored call on approval.
ALTER TABLE plan_steps ADD COLUMN IF NOT EXISTS parked_approval text NOT NULL DEFAULT '';

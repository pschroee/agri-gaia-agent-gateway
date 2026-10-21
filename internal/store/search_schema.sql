-- SPDX-FileCopyrightText: 2026 Philipp Schröer
--
-- SPDX-License-Identifier: MIT

-- Training searches with a budget and the book of trials (issue #168). Registered by the store seam
-- (registerSchema, G1.6): applied after schema.sql at every start, idempotent.

CREATE TABLE IF NOT EXISTS searches (
    id          text PRIMARY KEY,                -- "s" + 12 hex
    chat_id     uuid NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
    title       text NOT NULL,
    state       text NOT NULL DEFAULT 'proposed', -- proposed | running | ended | rejected | expired
    path        text NOT NULL,                    -- template | notebook
    objective   jsonb NOT NULL,                   -- {"metric":"val_acc","mode":"max"}
    space       jsonb NOT NULL,
    budget      jsonb NOT NULL,
    base        jsonb NOT NULL DEFAULT '{}',      -- template: {container_id, dataset_id, architecture, config}
    code        jsonb NOT NULL DEFAULT '{}',      -- notebook: {version, entry, files[{name,sha256,size}], dataset_ids, args, timeout_minutes}
    holdout     jsonb NOT NULL DEFAULT '{}',      -- {available, eval_dataset_id, final_state, check_id, result}
    note        text NOT NULL DEFAULT '',
    end_reason  text NOT NULL DEFAULT '',
    winner_idx  integer,
    session     text NOT NULL DEFAULT '',
    approval_id text NOT NULL DEFAULT '',
    tool_call_id text NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now(),
    started_at  timestamptz,
    ends_by     timestamptz,
    ended_at    timestamptz
);
CREATE INDEX IF NOT EXISTS searches_chat ON searches (chat_id, created_at);
-- At most one running search per chat: a second one cannot start, whatever the code does.
CREATE UNIQUE INDEX IF NOT EXISTS searches_one_running ON searches (chat_id) WHERE state = 'running';

-- Every training or notebook run of a chat: the trials of a search carry search_id, single runs do not.
CREATE TABLE IF NOT EXISTS experiment_trials (
    id          text PRIMARY KEY,                 -- "t" + 12 hex
    chat_id     uuid NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
    search_id   text REFERENCES searches(id) ON DELETE CASCADE,
    seq         integer NOT NULL,                 -- number within the chat, from 1
    idx         integer NOT NULL DEFAULT 0,       -- number within the search, from 1; 0 for a single run
    path        text NOT NULL,                    -- template | notebook
    state       text NOT NULL DEFAULT 'declared', -- declared | running | done | failed | stopped | refused
    params      jsonb NOT NULL DEFAULT '{}',
    note        text NOT NULL DEFAULT '',
    ref         text NOT NULL DEFAULT '',         -- template: training container id; notebook: run id
    job_id      text NOT NULL DEFAULT '',
    code_version integer NOT NULL DEFAULT 0,
    source      text NOT NULL DEFAULT '',
    epoch       integer,
    epochs      integer,
    score       double precision,
    score_epoch integer,
    gpu         jsonb,
    error       text NOT NULL DEFAULT '',
    stopped_by  text NOT NULL DEFAULT '',
    declared_at timestamptz NOT NULL DEFAULT now(),
    started_at  timestamptz,
    ended_at    timestamptz,
    UNIQUE (chat_id, seq)
);
CREATE INDEX IF NOT EXISTS experiment_trials_search ON experiment_trials (search_id, idx);
CREATE INDEX IF NOT EXISTS experiment_trials_job ON experiment_trials (job_id) WHERE job_id <> '';
CREATE INDEX IF NOT EXISTS experiment_trials_ref ON experiment_trials (chat_id, path, ref);

CREATE TABLE IF NOT EXISTS trial_epochs (
    trial_id text NOT NULL REFERENCES experiment_trials(id) ON DELETE CASCADE,
    epoch    integer NOT NULL,
    at       timestamptz NOT NULL,
    data     jsonb NOT NULL,
    PRIMARY KEY (trial_id, epoch)
);

CREATE TABLE IF NOT EXISTS trial_gpu (
    trial_id     text NOT NULL REFERENCES experiment_trials(id) ON DELETE CASCADE,
    at           timestamptz NOT NULL,
    util         real,
    mem_used_mb  integer,
    mem_total_mb integer,
    PRIMARY KEY (trial_id, at)
);

CREATE TABLE IF NOT EXISTS search_events (
    id        bigserial PRIMARY KEY,
    search_id text NOT NULL REFERENCES searches(id) ON DELETE CASCADE,
    at        timestamptz NOT NULL DEFAULT now(),
    kind      text NOT NULL, -- approved | trial_declared | trial_refused | code_approved | budget_changed | hint | stopped | adopted | ended | final
    data      jsonb NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS search_events_search ON search_events (search_id, id);

-- The structured data of the approval of a search, or of a changed code inside a search (issue #168): built by
-- the chat package and written as it is. NULL for every other kind of approval.
ALTER TABLE approvals ADD COLUMN IF NOT EXISTS search jsonb;

-- Turns the agent was woken for without the user, counted against the wake limit of the search (issue #168).
ALTER TABLE searches ADD COLUMN IF NOT EXISTS auto_turns integer NOT NULL DEFAULT 0;

-- A message of the user that is a hint for a training search carries the search's id as metadata (issue #168).
ALTER TABLE chat_queue ADD COLUMN IF NOT EXISTS search_hint text NOT NULL DEFAULT '';

-- What the run of a trial writes (issue #168): figures "" unknown, "none" the template writes none, "ok"; gpu_available
-- says whether the run sampled a GPU.
ALTER TABLE experiment_trials ADD COLUMN IF NOT EXISTS figures text NOT NULL DEFAULT '';
ALTER TABLE experiment_trials ADD COLUMN IF NOT EXISTS gpu_available boolean;

-- issue #194: a notebook run without figures is not a trial; remove the ones the first rule made.
DELETE FROM experiment_trials t WHERE t.search_id IS NULL AND t.path = 'notebook'
  AND t.state IN ('done','failed','stopped')
  AND NOT EXISTS (SELECT 1 FROM trial_epochs e WHERE e.trial_id = t.id);

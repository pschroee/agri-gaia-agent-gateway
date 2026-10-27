-- SPDX-FileCopyrightText: 2026 Philipp Schröer
--
-- SPDX-License-Identifier: MIT

-- Experiment runs (X1.5). One row per attempt of one run; the logical run is run_key
-- (<plan>/<cell>/<task>/<rep>). An attempt that died stays visible: the next attempt supersedes it
-- (superseded_by) and the row keeps status "aborted". Registered through registerSchema, so this
-- file is applied after schema.sql at every start and stays idempotent.

CREATE TABLE IF NOT EXISTS exp_runs (
    id              text PRIMARY KEY,               -- run_key, then run_key + "#<attempt>"
    run_key         text NOT NULL,                  -- deterministic <plan>/<cell>/<task>/<rep>
    plan            text NOT NULL,                  -- plan name (file name without extension)
    plan_sha256     text NOT NULL DEFAULT '',       -- digest of the material the run used
    cell            text NOT NULL,
    task            text NOT NULL,
    rep             integer NOT NULL,
    seed            bigint NOT NULL DEFAULT 0,      -- the plan's seed, kept per run
    position        integer NOT NULL DEFAULT 0,     -- place in the runner's order (X1.6)
    chat_id         uuid,                           -- the run's chat; no FK: the row outlives the chat
    status          text NOT NULL DEFAULT 'started',-- started | done | limit_cost | limit_tokens |
                                                    -- limit_duration | error | aborted
    end_reason      text NOT NULL DEFAULT '',       -- detail of the end (an error text, a limit)
    pass            boolean,                        -- result of the declarative check; NULL: not checked
    check_evidence  jsonb,                          -- answers of the check (X1.9)
    started_at      timestamptz NOT NULL DEFAULT now(),
    ended_at        timestamptz,
    superseded_by   text NOT NULL DEFAULT '',       -- id of the attempt that replaced this one
    gateway_version text NOT NULL DEFAULT '',       -- X8.1, the build the gateway ran with
    model           text NOT NULL DEFAULT '',
    image_pi        text NOT NULL DEFAULT '',       -- X8.2, image ID the run's slot used
    image_exec      text NOT NULL DEFAULT '',
    catalog_sha256  text NOT NULL DEFAULT '',
    skills_sha256   text NOT NULL DEFAULT '',
    inference_runtime text NOT NULL DEFAULT ''      -- #237, AGW_INFERENCE_RUNTIME the run was made with
);
CREATE INDEX IF NOT EXISTS exp_runs_plan ON exp_runs (plan, cell, position);
CREATE INDEX IF NOT EXISTS exp_runs_key ON exp_runs (run_key);

-- Existing databases get the columns of X8.2; the statement is idempotent like the rest of the schema.
ALTER TABLE exp_runs ADD COLUMN IF NOT EXISTS image_pi text NOT NULL DEFAULT '';
ALTER TABLE exp_runs ADD COLUMN IF NOT EXISTS image_exec text NOT NULL DEFAULT '';
ALTER TABLE exp_runs ADD COLUMN IF NOT EXISTS catalog_sha256 text NOT NULL DEFAULT '';
ALTER TABLE exp_runs ADD COLUMN IF NOT EXISTS skills_sha256 text NOT NULL DEFAULT '';
ALTER TABLE exp_runs ADD COLUMN IF NOT EXISTS inference_runtime text NOT NULL DEFAULT '';

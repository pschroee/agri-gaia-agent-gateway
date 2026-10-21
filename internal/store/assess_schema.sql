-- SPDX-FileCopyrightText: 2026 Philipp Schröer
--
-- SPDX-License-Identifier: MIT

-- Result checks of the gateway (R0): one row per check of a chat, with its state and its report.
-- Registered through registerSchema, so this file is applied after schema.sql at every start and stays
-- idempotent.
--
-- The unique partial index carries the rule of P4 into the database: at most one final check per chat
-- and evaluation dataset. Two checks started at the same moment cannot both get through; a failed check
-- leaves the index (state <> 'failed'), so a new attempt is possible after a failure.

CREATE TABLE IF NOT EXISTS assessments (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    chat_id         uuid NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
    kind            text NOT NULL,                                    -- training_model | own_model | deployment
    subject         text NOT NULL,                                    -- model:<id> | sha:<hex> | deployment:<name>
    eval_dataset_id bigint,                                           -- the dataset whose held-out images were measured
    final           boolean NOT NULL DEFAULT false,                   -- measured at the hold-out set (P4)
    state           text NOT NULL DEFAULT 'queued',                   -- queued | running | finished | failed
    progress        jsonb,                                            -- {set, images, note} while running
    report          jsonb,
    error           text NOT NULL DEFAULT '',
    tool_call_id    text NOT NULL DEFAULT '',                         -- the call that started the check
    created_at      timestamptz NOT NULL DEFAULT now(),
    started_at      timestamptz,                                      -- set when the check starts running
    ended_at        timestamptz
);
CREATE INDEX IF NOT EXISTS assessments_chat ON assessments (chat_id, created_at DESC);
-- holdout_measured (#181): false when a final check ended before the hold-out images went through the
-- model (class order not derivable); such a check does not use up the final test.
ALTER TABLE assessments ADD COLUMN IF NOT EXISTS holdout_measured boolean NOT NULL DEFAULT true;
DROP INDEX IF EXISTS assessments_final_once;
CREATE UNIQUE INDEX IF NOT EXISTS assessments_final_once2 ON assessments (chat_id, eval_dataset_id)
    WHERE final AND holdout_measured AND state <> 'failed';

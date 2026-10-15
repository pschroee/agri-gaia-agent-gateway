-- SPDX-FileCopyrightText: 2026 Philipp Schröer
--
-- SPDX-License-Identifier: MIT

-- Runs of a chat's notebook server in JupyterHub (own code C-E3). Registered by the store seam
-- (registerSchema, G1.6): applied after schema.sql at every start, idempotent.

CREATE TABLE IF NOT EXISTS notebook_runs (
    id          text PRIMARY KEY,
    chat_id     uuid NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
    hub_user    text NOT NULL,
    server      text NOT NULL,
    entry       text NOT NULL,
    files       jsonb NOT NULL,
    dataset_ids bigint[] NOT NULL DEFAULT '{}',
    timeout_min int NOT NULL,
    started_at  timestamptz NOT NULL DEFAULT now(),
    finished_at timestamptz,
    exit_code   int
);
CREATE INDEX IF NOT EXISTS notebook_runs_chat ON notebook_runs (chat_id, started_at DESC);

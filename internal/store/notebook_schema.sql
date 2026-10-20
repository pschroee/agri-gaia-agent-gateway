-- SPDX-FileCopyrightText: 2026 Philipp Schröer
--
-- SPDX-License-Identifier: MIT

-- Runs of a chat's notebook server in JupyterHub (own code C-E3). Registered by the store seam
-- (registerSchema, G1.6): applied after schema.sql at every start, idempotent.

-- The structured data of the approval of a run (own code C-E4): entry, files with size, SHA-256 and text,
-- datasets and limits, built by the notebook service. The frontend shows it instead of the text preview;
-- NULL for every other kind of approval and for approvals from before C-E4.
ALTER TABLE approvals ADD COLUMN IF NOT EXISTS notebook jsonb;

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

-- The reaper stopped the server after this run, or found it stopped (set on the chat's last run only).
-- A new run is a new row with NULL, so the server becomes a candidate again.
ALTER TABLE notebook_runs ADD COLUMN IF NOT EXISTS server_stopped_at timestamptz;

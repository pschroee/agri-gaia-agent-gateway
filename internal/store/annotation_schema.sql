-- SPDX-FileCopyrightText: 2026 Philipp Schröer
--
-- SPDX-License-Identifier: MIT

-- Pre-annotation runs of the agent (V1). Registered through registerSchema, so this file is applied
-- after schema.sql at every start and stays idempotent.
--
-- The progress of a run is not here: it belongs to watch_jobs (autonomy A-E2), which the loop of
-- internal/watch keeps up to date. A row of this table holds what the run was started with and how it
-- ended; id is the request id of the CVAT queue (rq job).

CREATE TABLE IF NOT EXISTS annotation_runs (
    id             text PRIMARY KEY,
    chat_id        uuid NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
    dataset_id     bigint NOT NULL,
    task_id        bigint NOT NULL,
    function       text NOT NULL,
    min_confidence integer NOT NULL,
    mapping        text NOT NULL DEFAULT '{}',
    existing       text NOT NULL,
    state          text NOT NULL DEFAULT 'queued',  -- queued | started | finished | failed | expired
    error          text NOT NULL DEFAULT '',
    created_at     timestamptz NOT NULL DEFAULT now(),
    ended_at       timestamptz
);
CREATE INDEX IF NOT EXISTS annotation_runs_chat ON annotation_runs (chat_id, created_at);

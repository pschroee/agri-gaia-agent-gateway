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

-- Pre-annotation functions the agent built from a model of the platform (V2). The name is the
-- identifier: it is built from the user, the model and the hash of the model file, and it is what
-- CVAT and nuclio use. The register holds the build and how it ended; CVAT's catalogue is the truth
-- about a function that runs (state ready), and the listing of the agent compares the two.
--
-- The function itself belongs to the user, not to the chat (it is visible to every CVAT user and
-- cannot be removed by the agent, E14); chat_id says where it was built. The gateway never deletes a
-- function, so the row stays after the chat is gone only if somebody deletes it by hand.

CREATE TABLE IF NOT EXISTS annotation_functions (
    name             text PRIMARY KEY,
    chat_id          uuid NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
    username         text NOT NULL,
    model_id         bigint NOT NULL,
    model_sha256     text NOT NULL,
    kind             text NOT NULL,
    labels           text NOT NULL DEFAULT '[]',      -- JSON array, in the order of the output
    input_size       integer NOT NULL,
    template_version integer NOT NULL,
    platform_task    text NOT NULL DEFAULT '',        -- the build task of the platform, '' when unknown
    state            text NOT NULL DEFAULT 'deploying', -- deploying | ready | failed | missing
    created_at       timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS annotation_functions_user ON annotation_functions (username, created_at);

-- Background jobs of pre-annotation (V3): a check (calibrate | uncertain | verify) and a fill. The job runs in
-- the gateway (minutes of CPU time, one at a time) instead of inside a tool call; params holds the call
-- the agent made, result the answer of a finished job and done/total what the observation kind
-- annotation_job reports. A restart ends every job that was queued or running (store.RecoverAnnotationJobs).

CREATE TABLE IF NOT EXISTS annotation_jobs (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    chat_id    uuid NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
    kind       text NOT NULL,                            -- calibrate | uncertain | fill | verify
    dataset_id bigint NOT NULL,
    task_id    bigint NOT NULL,
    function   text NOT NULL,
    params     text NOT NULL DEFAULT '{}',               -- JSON, the call the agent made
    state      text NOT NULL DEFAULT 'queued',           -- queued | running | finished | failed
    done       integer NOT NULL DEFAULT 0,
    total      integer NOT NULL DEFAULT 0,
    result     text NOT NULL DEFAULT '',
    error      text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    ended_at   timestamptz
);
CREATE INDEX IF NOT EXISTS annotation_jobs_chat ON annotation_jobs (chat_id, created_at DESC);

-- The three sets of manually annotated images of a task (V3b, E22): the calibration set the mapping and
-- the threshold are chosen on, the validation set the rounds of V3c are compared on, and the held-out
-- set the verdict falls on. A frame is put into its set when it is first seen and never changes it, so
-- that a later manual annotation joins without moving the frames that were already measured on. The
-- task id is unique in CVAT, so it stands alone in the key of the table.

CREATE TABLE IF NOT EXISTS annotation_splits (
    task_id bigint NOT NULL,
    chat_id uuid NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
    frame   integer NOT NULL,
    part    text NOT NULL,                          -- calibration | validation | holdout
    PRIMARY KEY (task_id, frame)
);

-- The register of the annotations a run or a fill wrote (V3b, E21). Only what stands here may be taken
-- back, and only as long as CVAT still carries it unchanged: fingerprint is the SHA-256 over what the
-- object was written with, so a correction of the user in CVAT shows up as a change and the object is
-- left alone. kind is what CVAT needs for its delete call (tag | shape | track).

CREATE TABLE IF NOT EXISTS annotation_objects (
    source_id   text NOT NULL,                      -- the run id of CVAT or the job id of the gateway
    chat_id     uuid NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
    task_id     bigint NOT NULL,
    kind        text NOT NULL,                      -- tag | shape | track
    cvat_id     bigint NOT NULL,
    frame       integer NOT NULL,
    label_id    bigint NOT NULL,
    fingerprint text NOT NULL,
    PRIMARY KEY (task_id, kind, cvat_id)
);
CREATE INDEX IF NOT EXISTS annotation_objects_source ON annotation_objects (chat_id, source_id);

-- Counters and one-off marks of a CVAT task in one chat (V3b, V3c): the rounds and retrains of the
-- correction loop (V3c) and when the held-out images were used for a final verdict. The held-out set of
-- a task is measured at most once per chat (E22), so the mark stands per chat and task, not per source.

CREATE TABLE IF NOT EXISTS annotation_tasks (
    chat_id           uuid NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
    task_id           bigint NOT NULL,
    rounds            integer NOT NULL DEFAULT 0,
    retrains          integer NOT NULL DEFAULT 0,
    final_verified_at timestamptz,
    PRIMARY KEY (chat_id, task_id)
);

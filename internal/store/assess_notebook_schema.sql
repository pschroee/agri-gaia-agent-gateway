-- SPDX-FileCopyrightText: 2026 Philipp Schröer
--
-- SPDX-License-Identifier: MIT

-- Own models from the notebook server (R2): the files a notebook run handed to the agent, and the SHA
-- of the file a model was uploaded from. Registered through registerSchema, so this file is applied
-- after schema.sql and after the tables of R0/R1 at every start and stays idempotent.
--
-- notebook_fetches (R2.1): one row per file notebook_fetch pulled out of a run, with the SHA-256 of the
-- bytes the gateway read then. It is what a check on a file of the sandbox is measured against: only a
-- file of a run of this chat has an origin the gateway saw (P2). The path inside the run is kept for the
-- report; the SHA and the run are the key.
--
-- model_origins.source_sha (R2.6): the SHA-256 of the file a model was uploaded from. A check of that
-- file keeps its subject sha:<hex>; this column is how the model finds it again — and how the check card
-- names the model behind a file check. The column is added in assess_training_schema.sql, which creates
-- the table: the schema files run in file order, and assess_notebook.go registers before
-- assess_training.go, so the ALTER cannot stand here.

CREATE TABLE IF NOT EXISTS notebook_fetches (
    chat_id    uuid NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
    run_id     text NOT NULL,                            -- the run the file came out of
    path       text NOT NULL,                            -- the path inside that run
    sha256     text NOT NULL,                            -- SHA-256 of the bytes the gateway read
    size       bigint NOT NULL,
    fetched_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (chat_id, sha256, run_id)
);
CREATE INDEX IF NOT EXISTS notebook_fetches_chat ON notebook_fetches (chat_id, fetched_at DESC);

-- model_meta (#181): outputs/model.json of the run, read when a .onnx file is fetched.
ALTER TABLE notebook_fetches ADD COLUMN IF NOT EXISTS model_meta jsonb;

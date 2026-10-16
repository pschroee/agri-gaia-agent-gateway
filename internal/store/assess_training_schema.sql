-- SPDX-FileCopyrightText: 2026 Philipp Schröer
--
-- SPDX-License-Identifier: MIT

-- Training checks of the gateway (R1), two tables that belong to the result checks (R0): the splits a
-- chat made before a training, and where a model came from. Registered through registerSchema, so this
-- file is applied after schema.sql at every start and stays idempotent.
--
-- assess_splits (R1.5): one row per dataset the gateway split. The training dataset is the key: it is
-- what a later check looks up to find the source (the dataset whose images were kept back).
--
-- model_origins (R1.6): what the gateway saw in this chat — which training container and which datasets
-- a model came from. The row is written by the gateway alone (never by the agent), and only such a row
-- tells a check that the model was trained here; without it the verdict is not_measured (P2).

CREATE TABLE IF NOT EXISTS assess_splits (
    chat_id           uuid NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
    source_dataset_id bigint NOT NULL,                    -- the dataset the split read (downloaded)
    train_dataset_id  bigint PRIMARY KEY,                 -- the dataset the split created
    train_images      integer NOT NULL,                   -- images that went into the training dataset
    kept_images       integer NOT NULL,                   -- images kept back for checks
    created_at        timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS assess_splits_chat ON assess_splits (chat_id, source_dataset_id);

CREATE TABLE IF NOT EXISTS model_origins (
    model_id           bigint PRIMARY KEY,                -- the platform model the gateway saw being created
    chat_id            uuid NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
    source             text NOT NULL,                     -- training | notebook
    train_container_id bigint,                            -- the training the model came from (source training)
    seen_dataset_ids   bigint[] NOT NULL,                 -- datasets the model could have seen (P2)
    created_at         timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS model_origins_chat ON model_origins (chat_id, created_at DESC);

-- The SHA-256 of the file a model was uploaded from. Filled by the notebook path (R2.6) only; NULL for a
-- model from a training (R1). It stands here, with the table, because the schema files are applied in
-- file order and assess_notebook.go registers before assess_training.go — an ALTER in the notebook file
-- ran before this table existed.
ALTER TABLE model_origins ADD COLUMN IF NOT EXISTS source_sha text;
CREATE INDEX IF NOT EXISTS model_origins_sha ON model_origins (chat_id, source_sha);

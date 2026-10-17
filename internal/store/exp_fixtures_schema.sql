-- SPDX-FileCopyrightText: 2026 Philipp Schröer
--
-- SPDX-License-Identifier: MIT

-- Platform objects seeded for an experiment cell (X4, issue #129). Written before a cell and removed
-- after it, always by object_id: never by name (CLAUDE.md). A removed row stays as evidence that the
-- object existed and how its removal went. Text instead of enum for kind, owner and remove_result so
-- that a new fixture kind does not need a migration.
CREATE TABLE IF NOT EXISTS exp_fixtures (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    plan          text NOT NULL,
    cell          text NOT NULL,
    manifest_id   text NOT NULL,
    kind          text NOT NULL,
    owner         text NOT NULL,
    object_id     text NOT NULL DEFAULT '',
    field_sha256  jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at    timestamptz NOT NULL DEFAULT now(),
    removed_at    timestamptz,
    remove_result text NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS exp_fixtures_plan_cell ON exp_fixtures (plan, cell);

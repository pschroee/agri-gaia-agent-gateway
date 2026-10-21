-- SPDX-FileCopyrightText: 2026 Philipp Schröer
--
-- SPDX-License-Identifier: MIT

-- Internet grants (issue #175): what the user allowed the agent to reach, one row per grant with its kind, list,
-- limits and end, plus the log columns of web_requests. Registered by the store seam (registerSchema, G1.6):
-- applied after schema.sql at every start, idempotent.

CREATE TABLE IF NOT EXISTS internet_grants (
  id bigserial PRIMARY KEY, chat_id uuid REFERENCES chats(id) ON DELETE CASCADE,
  approval_id uuid, kind text NOT NULL CHECK (kind IN ('search','domains','packages','full')),
  origin text NOT NULL,            -- agent | user | default
  via text NOT NULL DEFAULT '', session text NOT NULL DEFAULT '', tool_call_id text NOT NULL DEFAULT '',
  reason text NOT NULL DEFAULT '', domains text[] NOT NULL DEFAULT '{}', packages jsonb NOT NULL DEFAULT '[]',
  state text NOT NULL,             -- pending | active | ended | rejected | expired
  max_minutes int NOT NULL, max_bytes bigint NOT NULL, bytes bigint NOT NULL DEFAULT 0, blocked int NOT NULL DEFAULT 0,
  created_at timestamptz NOT NULL DEFAULT now(), started_at timestamptz, expires_at timestamptz,
  ended_at timestamptz, ended_reason text NOT NULL DEFAULT '');  -- time | bytes | installed | user | agent | error
CREATE INDEX IF NOT EXISTS internet_grants_chat ON internet_grants (chat_id, id);
CREATE INDEX IF NOT EXISTS internet_grants_active ON internet_grants (state) WHERE state = 'active';
CREATE TABLE IF NOT EXISTS internet_grant_packages (
  grant_id bigint REFERENCES internet_grants(id) ON DELETE CASCADE, ecosystem text NOT NULL, name text NOT NULL,
  parent text NOT NULL DEFAULT '',  -- '' = requested by the agent, else the package that declared it
  installed_at timestamptz, PRIMARY KEY (grant_id, ecosystem, name));
ALTER TABLE web_requests ADD COLUMN IF NOT EXISTS grant_id bigint, ADD COLUMN IF NOT EXISTS origin text NOT NULL DEFAULT 'pi',
  ADD COLUMN IF NOT EXISTS package text NOT NULL DEFAULT '', ADD COLUMN IF NOT EXISTS version text NOT NULL DEFAULT '',
  ADD COLUMN IF NOT EXISTS parent text NOT NULL DEFAULT '', ADD COLUMN IF NOT EXISTS tool_call_id text NOT NULL DEFAULT '',
  ADD COLUMN IF NOT EXISTS count int NOT NULL DEFAULT 1;
CREATE INDEX IF NOT EXISTS web_requests_grant ON web_requests (grant_id, id) WHERE grant_id IS NOT NULL;
ALTER TABLE approvals ADD COLUMN IF NOT EXISTS internet jsonb;

-- Stage 2 (package gate, refusal codes, service of a row).
ALTER TABLE web_requests ADD COLUMN IF NOT EXISTS reason text NOT NULL DEFAULT '', ADD COLUMN IF NOT EXISTS service text NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS web_requests_chat_denied ON web_requests (chat_id, id DESC) WHERE denied <> '';
ALTER TABLE internet_grant_packages ADD COLUMN IF NOT EXISTS version text NOT NULL DEFAULT '', ADD COLUMN IF NOT EXISTS extras text NOT NULL DEFAULT '',
  ADD COLUMN IF NOT EXISTS downloaded_at timestamptz, ADD COLUMN IF NOT EXISTS installed_version text NOT NULL DEFAULT '';

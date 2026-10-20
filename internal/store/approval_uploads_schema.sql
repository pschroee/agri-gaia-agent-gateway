-- SPDX-FileCopyrightText: 2026 Philipp Schröer
--
-- SPDX-License-Identifier: MIT

-- The files of an upload approval (issue #164). Registered by the store seam (registerSchema): applied after
-- schema.sql at every start, idempotent.

-- The summary of the files (number, size, folders, file types, annotation file, manifest SHA-256), shown on the
-- card; NULL for every approval that uploads nothing.
ALTER TABLE approvals ADD COLUMN IF NOT EXISTS uploads jsonb;

-- The list of every file (field, path, size, SHA-256) that the manifest SHA-256 binds. Kept apart from the summary
-- because it can hold 2 000 entries: no approval event or listing reads it.
ALTER TABLE approvals ADD COLUMN IF NOT EXISTS upload_entries jsonb;

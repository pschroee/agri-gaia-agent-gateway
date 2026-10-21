-- SPDX-FileCopyrightText: 2026 Philipp Schröer
--
-- SPDX-License-Identifier: MIT

-- Index of the activity list with kind=internet|all. Registered through registerSchema, so this file is applied
-- after schema.sql at every start and stays idempotent. The first version of the index did not cover the ends of
-- grants (op internet_end, issue #175); it is dropped once, if its definition lacks that op, and created anew.
DO $$
BEGIN
	IF to_regclass(quote_ident(current_schema()) || '.socket_calls_activity') IS NOT NULL
		AND pg_get_indexdef(to_regclass(quote_ident(current_schema()) || '.socket_calls_activity')) NOT LIKE '%internet_end%' THEN
		DROP INDEX socket_calls_activity;
	END IF;
END $$;
CREATE INDEX IF NOT EXISTS socket_calls_activity ON socket_calls (id)
	WHERE op IN ('platform', 'internet', 'internet_off', 'internet_set', 'internet_end');

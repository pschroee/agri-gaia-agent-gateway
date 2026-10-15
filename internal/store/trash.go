// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// TrashItem is a chat in the trash as the trash list shows it (issue #76).
type TrashItem struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Model     string    `json:"model"`
	Variant   string    `json:"variant"`
	UpdatedAt time.Time `json:"updated_at"`
	TrashedAt time.Time `json:"trashed_at"`
	Messages  int       `json:"messages"`
	Bytes     int64     `json:"bytes"`
}

// MoveToTrash puts a chat into the trash at the given time and makes it dormant. It reports whether the chat was
// moved (false: unknown or already in the trash). Data stays; the caller tears the sandbox down.
func (s *Store) MoveToTrash(ctx context.Context, chatID string, at time.Time) (bool, error) {
	if !isUUID(chatID) {
		return false, ErrNotFound
	}
	tag, err := s.pool.Exec(ctx, `UPDATE chats SET trashed_at=$2, state=$3 WHERE id=$1 AND trashed_at IS NULL`, chatID, at, StateDormant)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// Restore takes a chat out of the trash and puts it on top of the history again. It reports whether the chat was
// in the trash.
func (s *Store) Restore(ctx context.Context, chatID string) (bool, error) {
	if !isUUID(chatID) {
		return false, ErrNotFound
	}
	tag, err := s.pool.Exec(ctx, `UPDATE chats SET trashed_at=NULL, updated_at=now() WHERE id=$1 AND trashed_at IS NOT NULL`, chatID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// ChatTrashed reports whether a chat is in the trash; unknown chats give ErrNotFound.
func (s *Store) ChatTrashed(ctx context.Context, chatID string) (bool, error) {
	if !isUUID(chatID) {
		return false, ErrNotFound
	}
	var trashed bool
	err := s.pool.QueryRow(ctx, `SELECT trashed_at IS NOT NULL FROM chats WHERE id = $1`, chatID).Scan(&trashed)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrNotFound
	}
	return trashed, err
}

// ListTrash returns the chats in the trash, most recently moved first. owner nil: all chats (token mode).
func (s *Store) ListTrash(ctx context.Context, owner *string) ([]TrashItem, error) {
	args := []any{}
	q := `SELECT c.id::text, c.title, c.model, c.variant, c.updated_at, c.trashed_at,
  (SELECT count(*) FROM chat_messages m WHERE m.chat_id = c.id AND m.role IN ('user', 'assistant')),
  COALESCE((SELECT sum(a.size) FROM artifacts a WHERE a.chat_id = c.id), 0)::bigint
  + COALESCE((SELECT sum(i.size) FROM chat_images i WHERE i.chat_id = c.id), 0)::bigint
  + COALESCE((SELECT w.archive_size FROM chat_workspaces w WHERE w.chat_id = c.id), 0)::bigint
FROM chats c WHERE c.trashed_at IS NOT NULL`
	if owner != nil {
		args = append(args, *owner)
		q += ` AND c.owner = $1`
	}
	q += ` ORDER BY c.trashed_at DESC, c.id`
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TrashItem{}
	for rows.Next() {
		var t TrashItem
		if err := rows.Scan(&t.ID, &t.Title, &t.Model, &t.Variant, &t.UpdatedAt, &t.TrashedAt, &t.Messages, &t.Bytes); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// TrashedBefore returns the IDs of the chats that went into the trash before the given time, oldest first.
func (s *Store) TrashedBefore(ctx context.Context, before time.Time) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT id::text FROM chats WHERE trashed_at IS NOT NULL AND trashed_at < $1 ORDER BY trashed_at, id`, before)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ChatBlobKeys lists the object keys of everything a chat keeps in the object store (artifacts, display images,
// workspace backup, pending approvals). Read before DeleteChatAnonymised, whose cascade removes the rows.
func (s *Store) ChatBlobKeys(ctx context.Context, chatID string) ([]string, error) {
	if !isUUID(chatID) {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `
SELECT object_key FROM artifacts WHERE chat_id = $1
UNION SELECT object_key FROM chat_images WHERE chat_id = $1
UNION SELECT object_key FROM chat_workspaces WHERE chat_id = $1 AND object_key IS NOT NULL
UNION SELECT pending_key FROM approvals WHERE chat_id = $1 AND pending_key NOT IN ('', '-')`, chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// DeleteChatAnonymised deletes a chat for good (issue #76, decision of the author). In one transaction it first
// moves the call logs of the chat to a tombstone (deleted_chats) and removes their content, and only then deletes
// the chat: the cascade would otherwise wipe the measurement data. Kept per entry: time, kind, effect class, result
// class, duration, tokens, bytes. Removed: titles, arguments, previews, bodies, texts of tool calls.
// The object store is not touched here; the caller deletes the blobs listed by ChatBlobKeys afterwards.
func (s *Store) DeleteChatAnonymised(ctx context.Context, chatID string) (string, error) {
	if !isUUID(chatID) {
		return "", ErrNotFound
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var owner *string
	var model, variant string
	err = tx.QueryRow(ctx, `SELECT NULLIF(owner, ''), model, variant FROM chats WHERE id = $1 FOR UPDATE`, chatID).Scan(&owner, &model, &variant)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	var tomb string
	if err := tx.QueryRow(ctx, `INSERT INTO deleted_chats (owner, model, variant) VALUES ($1, $2, $3) RETURNING id::text`,
		owner, model, variant).Scan(&tomb); err != nil {
		return "", err
	}
	// An internet request whose approval expired was logged as "rejected"; the approval goes with the chat, so the
	// outcome is written down now (activity classifies "expired" from the text alone).
	if _, err := tx.Exec(ctx, `
UPDATE socket_calls sc SET result = 'expired'
WHERE sc.chat_id = $1 AND sc.op = 'internet' AND sc.result LIKE 'rejected%'
  AND sc.tool_call_id <> '' AND sc.result NOT LIKE 'already on%'
  AND (SELECT a.state FROM approvals a
       WHERE a.chat_id = sc.chat_id AND a.tool_call_id = sc.tool_call_id AND a.kind = 'internet_access'
         AND a.created_at <= sc.created_at
       ORDER BY a.created_at DESC LIMIT 1) = 'expired'`, chatID); err != nil {
		return "", err
	}
	// The text before the first colon is the class (see outcomeSQL and internetSQL); the violation suffix stays.
	const suffix = ` · violation, logged only`
	steps := []string{
		`UPDATE socket_calls SET chat_id = NULL, deleted_chat = $2,
  detail = CASE WHEN op = 'platform' THEN split_part(split_part(detail, '?', 1), ' (', 1) ELSE '' END,
  result = CASE WHEN position('` + suffix + `' IN result) > 0
    THEN split_part(replace(result, '` + suffix + `', ''), ':', 1) || '` + suffix + `'
    ELSE split_part(result, ':', 1) END
WHERE chat_id = $1`,
		`UPDATE tool_executions SET chat_id = NULL, deleted_chat = $2, args = '{}', output_excerpt = '', output_sha256 = '',
  error = CASE WHEN error <> '' THEN 'error' ELSE '' END
WHERE chat_id = $1`,
		`UPDATE llm_calls SET chat_id = NULL, deleted_chat = $2,
  tool_calls = CASE WHEN jsonb_typeof(tool_calls) = 'array'
    THEN COALESCE((SELECT jsonb_agg(jsonb_build_object('id', t->>'id', 'name', t->>'name')) FROM jsonb_array_elements(tool_calls) t), '[]'::jsonb)
    ELSE '[]'::jsonb END
WHERE chat_id = $1`,
		`UPDATE aux_llm_calls SET chat_id = NULL, deleted_chat = $2, error = CASE WHEN error <> '' THEN 'error' ELSE '' END
WHERE chat_id = $1`,
		`UPDATE web_requests SET chat_id = NULL, deleted_chat = $2, path = '' WHERE chat_id = $1`,
	}
	for _, q := range steps {
		if _, err := tx.Exec(ctx, q, chatID, tomb); err != nil {
			return "", err
		}
	}
	// The cascade removes messages, runs, artifacts, approvals, subagents, images, workspace, background tasks,
	// queue, turns, delegation objects, plans, annotation runs (V1) and the session.
	if _, err := tx.Exec(ctx, `DELETE FROM chats WHERE id = $1`, chatID); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return tomb, nil
}

// ExpireOpenPlans expires the plans of a chat that are still proposed or approved; it reports how many.
func (s *Store) ExpireOpenPlans(ctx context.Context, chatID string) (int64, error) {
	if !isUUID(chatID) {
		return 0, nil
	}
	tag, err := s.pool.Exec(ctx, `UPDATE plans SET state = 'expired', decided_at = now() WHERE chat_id = $1 AND state IN ('proposed', 'approved')`, chatID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// StopRunningBackground stops the running background tasks of a chat by the user (state stopped, stopped_by user)
// without a notice to the agent: the chat is in the trash.
func (s *Store) StopRunningBackground(ctx context.Context, chatID, reason string) ([]BackgroundTask, error) {
	if !isUUID(chatID) {
		return []BackgroundTask{}, nil
	}
	rows, err := s.pool.Query(ctx, `
UPDATE background_tasks SET state = 'stopped', stopped_by = 'user', error = $2, ended_at = now(), notice_pending = false
WHERE chat_id = $1 AND state = 'running' RETURNING `+bgCols, chatID, reason)
	if err != nil {
		return nil, err
	}
	return scanBg(rows)
}

// DeleteUndelivered removes the queued messages that have not gone to the agent yet; it reports how many.
func (s *Store) DeleteUndelivered(ctx context.Context, chatID string) (int64, error) {
	if !isUUID(chatID) {
		return 0, nil
	}
	tag, err := s.pool.Exec(ctx, `DELETE FROM chat_queue WHERE chat_id = $1 AND delivered_at IS NULL`, chatID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

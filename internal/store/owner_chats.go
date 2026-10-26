// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package store

import "context"

// OwnerChatTitles returns the titles of the chats that are not in the trash, by chat id. owner nil means every
// chat (token mode); otherwise only the chats of that owner. Shared by the overviews across chats.
func (s *Store) OwnerChatTitles(ctx context.Context, owner *string) (map[string]string, error) {
	q := `SELECT id::text, title FROM chats WHERE trashed_at IS NULL`
	args := []any{}
	if owner != nil {
		q += ` AND owner = $1`
		args = append(args, *owner)
	}
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, title string
		if err := rows.Scan(&id, &title); err != nil {
			return nil, err
		}
		out[id] = title
	}
	return out, rows.Err()
}

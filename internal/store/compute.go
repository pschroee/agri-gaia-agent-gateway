// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package store

import "context"

// ComputeCalls returns how many compute calls the platform has accepted in a chat.
func (s *Store) ComputeCalls(ctx context.Context, chatID string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT compute_calls FROM chats WHERE id=$1`, chatID).Scan(&n)
	return n, err
}

// ReserveCompute counts one more compute call against the chat's limit, only while it is below limit.
// ok is false when the limit is reached; nothing is counted then.
func (s *Store) ReserveCompute(ctx context.Context, chatID string, limit int) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE chats SET compute_calls = compute_calls + 1 WHERE id=$1 AND compute_calls < $2`, chatID, limit)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// ReleaseCompute gives back a reserved compute call that the platform did not accept. The count never
// goes below zero.
func (s *Store) ReleaseCompute(ctx context.Context, chatID string) error {
	_, err := s.pool.Exec(ctx, `UPDATE chats SET compute_calls = compute_calls - 1 WHERE id=$1 AND compute_calls > 0`, chatID)
	return err
}

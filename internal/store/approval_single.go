// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"time"
)

// CountSingleApprovalsSince counts the single approvals of a writing platform call that were granted in a chat at or
// after since (issue #254). Approvals of plans, of the trials of a search and of the steps of an autonomous plan
// (parked) are not single approvals and are not counted.
func (s *Store) CountSingleApprovalsSince(ctx context.Context, chatID string, since time.Time) (int, error) {
	if !isUUID(chatID) {
		return 0, ErrNotFound
	}
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM approvals a
WHERE a.chat_id=$1 AND a.kind='platform_write' AND a.state='approved' AND a.created_at >= $2 AND a.search IS NULL
  AND NOT EXISTS (SELECT 1 FROM plan_steps ps WHERE ps.parked_approval = a.id::text)`, chatID, since).Scan(&n)
	return n, err
}

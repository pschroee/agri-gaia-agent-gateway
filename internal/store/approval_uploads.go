// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
)

//go:embed approval_uploads_schema.sql
var approvalUploadsSchemaSQL string

func init() { registerSchema(approvalUploadsSchemaSQL) }

// GetApprovalUploadEntries returns the list of every file of an upload approval of the chat (JSON array); nil if
// the approval has none. ErrNotFound if the approval does not exist or belongs to another chat.
func (s *Store) GetApprovalUploadEntries(ctx context.Context, chatID, id string) (json.RawMessage, error) {
	if !isUUID(id) || !isUUID(chatID) {
		return nil, ErrNotFound
	}
	var entries string
	err := s.pool.QueryRow(ctx, `SELECT COALESCE(upload_entries::text,'') FROM approvals WHERE id=$1 AND chat_id=$2`, id, chatID).Scan(&entries)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if entries == "" {
		return nil, nil
	}
	return json.RawMessage(entries), nil
}

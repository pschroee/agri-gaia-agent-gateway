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

// Messages the user wrote directly to a subagent (issue #81). The gateway stores each one before it is relayed;
// the sandbox only reports how it went (status), it never changes the text.
const (
	SubagentMsgSending   = "sending"
	SubagentMsgSent      = "sent"
	SubagentMsgDelivered = "delivered"
	SubagentMsgFailed    = "failed"
)

// SubagentMessage is one message of the user to a subagent, as the API and the event stream show it.
type SubagentMessage struct {
	ID          int64      `json:"id"`
	ChatID      string     `json:"chat_id"`
	RunID       string     `json:"run_id"`
	TargetRunID string     `json:"target_run_id,omitempty"`
	Ref         string     `json:"ref"`
	Mode        string     `json:"mode"`
	Text        string     `json:"text"`
	Status      string     `json:"status"`
	Error       string     `json:"error,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	DeliveredAt *time.Time `json:"delivered_at,omitempty"`
}

const subagentMsgCols = `id, chat_id::text, run_id, target_run_id, ref, mode, text, status, error, created_at, delivered_at`

func scanSubagentMessage(row interface{ Scan(...any) error }) (SubagentMessage, error) {
	var m SubagentMessage
	err := row.Scan(&m.ID, &m.ChatID, &m.RunID, &m.TargetRunID, &m.Ref, &m.Mode, &m.Text, &m.Status, &m.Error, &m.CreatedAt, &m.DeliveredAt)
	return m, err
}

// CreateSubagentMessage stores a new message (status sending) and returns it with its ID and time.
func (s *Store) CreateSubagentMessage(ctx context.Context, m SubagentMessage) (SubagentMessage, error) {
	if !isUUID(m.ChatID) {
		return m, ErrNotFound
	}
	row := s.pool.QueryRow(ctx, `INSERT INTO subagent_messages (chat_id, run_id, target_run_id, ref, mode, text, status)
VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING `+subagentMsgCols,
		m.ChatID, m.RunID, m.TargetRunID, m.Ref, m.Mode, m.Text, m.Status)
	return scanSubagentMessage(row)
}

// SetSubagentMessageStatus changes the status of a message of a chat; mode and targetRunID are set when not empty
// (a resume that falls back to a restart, the new run).
// delivered_at is set once the status becomes delivered. ErrNotFound: unknown ref or other chat.
func (s *Store) SetSubagentMessageStatus(ctx context.Context, chatID, ref, status, errText, mode, targetRunID string) (SubagentMessage, error) {
	if !isUUID(chatID) {
		return SubagentMessage{}, ErrNotFound
	}
	row := s.pool.QueryRow(ctx, `UPDATE subagent_messages SET status = $3, error = $4,
  mode = CASE WHEN $5 = '' THEN mode ELSE $5 END,
  target_run_id = CASE WHEN $6 = '' THEN target_run_id ELSE $6 END,
  delivered_at = CASE WHEN $3 = 'delivered' THEN now() ELSE delivered_at END
WHERE chat_id = $1 AND ref = $2 RETURNING `+subagentMsgCols, chatID, ref, status, errText, mode, targetRunID)
	m, err := scanSubagentMessage(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return m, ErrNotFound
	}
	return m, err
}

// ListSubagentMessages returns the messages of a chat in the order they were written.
func (s *Store) ListSubagentMessages(ctx context.Context, chatID string) ([]SubagentMessage, error) {
	out := []SubagentMessage{}
	if !isUUID(chatID) {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT `+subagentMsgCols+` FROM subagent_messages WHERE chat_id = $1 ORDER BY id`, chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		m, err := scanSubagentMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// SubagentMessageByRef returns the message with this ref of the chat; ErrNotFound otherwise.
func (s *Store) SubagentMessageByRef(ctx context.Context, chatID, ref string) (SubagentMessage, error) {
	if !isUUID(chatID) {
		return SubagentMessage{}, ErrNotFound
	}
	m, err := scanSubagentMessage(s.pool.QueryRow(ctx, `SELECT `+subagentMsgCols+` FROM subagent_messages WHERE chat_id = $1 AND ref = $2`, chatID, ref))
	if errors.Is(err, pgx.ErrNoRows) {
		return m, ErrNotFound
	}
	return m, err
}

// PendingSubagentMessage reports whether a message to this run is still on its way (sending or sent).
func (s *Store) PendingSubagentMessage(ctx context.Context, chatID, runID string) (bool, error) {
	if !isUUID(chatID) {
		return false, nil
	}
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM subagent_messages WHERE chat_id = $1 AND run_id = $2 AND status IN ('sending','sent')`,
		chatID, runID).Scan(&n)
	return n > 0, err
}

// CountSubagentMessages counts the messages of a chat written since the given time (rate limit).
func (s *Store) CountSubagentMessages(ctx context.Context, chatID string, since time.Time) (int, error) {
	if !isUUID(chatID) {
		return 0, nil
	}
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM subagent_messages WHERE chat_id = $1 AND created_at >= $2`, chatID, since).Scan(&n)
	return n, err
}

// SubagentRunOf returns a run of a chat with its stored name and state. The run must belong to the chat: it is
// known from subagent_runs or from its entries (runs of older chats have entries only). ErrNotFound otherwise.
func (s *Store) SubagentRunOf(ctx context.Context, chatID, runID string) (SubagentRun, error) {
	r := SubagentRun{ChatID: chatID, RunID: runID}
	if !isUUID(chatID) {
		return r, ErrNotFound
	}
	err := s.pool.QueryRow(ctx, `SELECT agent, label, state, pi_run_id, parent_run_id, continues_run_id, started_at, ended_at, updated_at, stopped_by
FROM subagent_runs WHERE chat_id = $1 AND run_id = $2`, chatID, runID).
		Scan(&r.Agent, &r.Label, &r.State, &r.PiRunID, &r.ParentRunID, &r.ContinuesRunID, &r.StartedAt, &r.EndedAt, &r.UpdatedAt, &r.StoppedBy)
	if err == nil {
		return r, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return r, err
	}
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM subagent_entries WHERE chat_id = $1 AND run_id = $2`, chatID, runID).Scan(&n); err != nil {
		return r, err
	}
	if n == 0 {
		return r, ErrNotFound
	}
	// runs of older chats have no status row: the agent is the one of their entries
	err = s.pool.QueryRow(ctx, `SELECT agent FROM subagent_entries WHERE chat_id = $1 AND run_id = $2 AND agent <> '' ORDER BY created_at LIMIT 1`,
		chatID, runID).Scan(&r.Agent)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return r, err
	}
	return r, nil
}

// SetRunContinues records that run continues run from. Only the gateway calls it; UpsertSubagentRun never
// changes this column.
func (s *Store) SetRunContinues(ctx context.Context, chatID, runID, from string) error {
	if !isUUID(chatID) {
		return ErrNotFound
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO subagent_runs (chat_id, run_id, continues_run_id) VALUES ($1,$2,$3)
ON CONFLICT (chat_id, run_id) DO UPDATE SET continues_run_id = EXCLUDED.continues_run_id`, chatID, runID, from)
	return err
}

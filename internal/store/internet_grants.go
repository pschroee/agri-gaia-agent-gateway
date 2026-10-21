// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

//go:embed internet_grants_schema.sql
var internetGrantsSchemaSQL string

func init() { registerSchema(internetGrantsSchemaSQL) }

// States of an internet grant.
const (
	GrantPending  = "pending"
	GrantActive   = "active"
	GrantEnded    = "ended"
	GrantRejected = "rejected"
	GrantExpired  = "expired"
)

// Origins of a grant: asked for by the agent, switched on by the user, or the default of a new chat.
const (
	GrantByAgent   = "agent"
	GrantByUser    = "user"
	GrantByDefault = "default"
)

// Reasons a grant ended (ended_reason).
const (
	EndTime      = "time"
	EndBytes     = "bytes"
	EndInstalled = "installed"
	EndUser      = "user"
	EndAgent     = "agent"
	EndError     = "error"
)

// Grant is one internet grant of a chat (internet_grants). Several can be active at once; what a chat may reach
// is their union.
type Grant struct {
	ID          int64           `json:"id"`
	ChatID      string          `json:"chat_id"`
	ApprovalID  string          `json:"approval_id,omitempty"`
	Kind        string          `json:"kind"`
	Origin      string          `json:"origin"`
	Via         string          `json:"via,omitempty"`
	Session     string          `json:"session,omitempty"`
	ToolCallID  string          `json:"tool_call_id,omitempty"`
	Reason      string          `json:"reason,omitempty"` // the agent's note, never verified
	Domains     []string        `json:"domains"`
	Packages    json.RawMessage `json:"packages"`
	State       string          `json:"state"`
	MaxMinutes  int             `json:"max_minutes"`
	MaxBytes    int64           `json:"max_bytes"`
	Bytes       int64           `json:"bytes"`
	Blocked     int             `json:"blocked"`
	CreatedAt   time.Time       `json:"created_at"`
	StartedAt   *time.Time      `json:"started_at,omitempty"`
	ExpiresAt   *time.Time      `json:"expires_at,omitempty"`
	EndedAt     *time.Time      `json:"ended_at,omitempty"`
	EndedReason string          `json:"ended_reason,omitempty"`
}

// GrantPackage is a package a packages grant covers (stage 2): requested by the agent (Parent empty) or declared
// by a package that is covered.
type GrantPackage struct {
	GrantID     int64      `json:"grant_id"`
	Ecosystem   string     `json:"ecosystem"`
	Name        string     `json:"name"`
	Parent      string     `json:"parent,omitempty"`
	InstalledAt *time.Time `json:"installed_at,omitempty"`
}

const grantCols = `id, chat_id::text, COALESCE(approval_id::text,''), kind, origin, via, session, tool_call_id, reason, domains, packages::text, state,
	max_minutes, max_bytes, bytes, blocked, created_at, started_at, expires_at, ended_at, ended_reason`

func scanGrant(row pgx.Row) (Grant, error) {
	var g Grant
	var pk string
	err := row.Scan(&g.ID, &g.ChatID, &g.ApprovalID, &g.Kind, &g.Origin, &g.Via, &g.Session, &g.ToolCallID, &g.Reason, &g.Domains, &pk, &g.State,
		&g.MaxMinutes, &g.MaxBytes, &g.Bytes, &g.Blocked, &g.CreatedAt, &g.StartedAt, &g.ExpiresAt, &g.EndedAt, &g.EndedReason)
	if errors.Is(err, pgx.ErrNoRows) {
		return g, ErrNotFound
	}
	if err != nil {
		return g, err
	}
	if g.Domains == nil {
		g.Domains = []string{}
	}
	g.Packages = json.RawMessage(pk)
	if pk == "" {
		g.Packages = json.RawMessage("[]")
	}
	return g, nil
}

// CreateGrant inserts a grant. State is GrantPending (waiting for the user) or GrantActive (the user's switch, the
// default of a new chat); an active grant needs StartedAt and ExpiresAt.
func (s *Store) CreateGrant(ctx context.Context, g Grant) (Grant, error) {
	if g.Domains == nil {
		g.Domains = []string{}
	}
	pk := string(g.Packages)
	if pk == "" {
		pk = "[]"
	}
	return scanGrant(s.pool.QueryRow(ctx, `
INSERT INTO internet_grants (chat_id, approval_id, kind, origin, via, session, tool_call_id, reason, domains, packages, state, max_minutes, max_bytes, started_at, expires_at)
VALUES ($1, NULLIF($2,'')::uuid, $3, $4, $5, $6, $7, $8, $9, $10::jsonb, $11, $12, $13, $14, $15) RETURNING `+grantCols,
		g.ChatID, g.ApprovalID, g.Kind, g.Origin, g.Via, g.Session, g.ToolCallID, noNUL(g.Reason), g.Domains, pk, g.State, g.MaxMinutes, g.MaxBytes, g.StartedAt, g.ExpiresAt))
}

// SetGrantApproval records the approval a pending grant waits for.
func (s *Store) SetGrantApproval(ctx context.Context, id int64, approvalID string) error {
	return s.exec1(ctx, `UPDATE internet_grants SET approval_id = $2::uuid WHERE id = $1`, id, approvalID)
}

// ActivateGrant starts a pending grant (the user approved): ok is false if it was not pending any more.
func (s *Store) ActivateGrant(ctx context.Context, id int64, started, expires time.Time) (Grant, bool, error) {
	g, err := scanGrant(s.pool.QueryRow(ctx, `UPDATE internet_grants SET state='active', started_at=$2, expires_at=$3
		WHERE id=$1 AND state='pending' RETURNING `+grantCols, id, started, expires))
	if err == nil {
		return g, true, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return g, false, err
	}
	g, err = s.GetGrant(ctx, id)
	return g, false, err
}

// CloseGrant sets a pending grant to GrantRejected or GrantExpired; ok is false if it was not pending any more.
func (s *Store) CloseGrant(ctx context.Context, id int64, state string) (bool, error) {
	if state != GrantRejected && state != GrantExpired {
		return false, fmt.Errorf("state %q", state)
	}
	tag, err := s.pool.Exec(ctx, `UPDATE internet_grants SET state=$2, ended_at=now() WHERE id=$1 AND state='pending'`, id, state)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// EndGrant ends an active grant for the reason (EndTime, EndBytes, …) and stores the final byte count if it is
// higher than the stored one; ok is false if the grant was not active.
func (s *Store) EndGrant(ctx context.Context, id int64, reason string, bytes int64) (Grant, bool, error) {
	g, err := scanGrant(s.pool.QueryRow(ctx, `UPDATE internet_grants SET state='ended', ended_at=now(), ended_reason=$2, bytes=GREATEST(bytes,$3)
		WHERE id=$1 AND state='active' RETURNING `+grantCols, id, reason, bytes))
	if err == nil {
		return g, true, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return g, false, err
	}
	g, err = s.GetGrant(ctx, id)
	return g, false, err
}

// GetGrant reads one grant.
func (s *Store) GetGrant(ctx context.Context, id int64) (Grant, error) {
	return scanGrant(s.pool.QueryRow(ctx, `SELECT `+grantCols+` FROM internet_grants WHERE id=$1`, id))
}

// ChatGrant reads one grant of a chat; ErrNotFound if the grant belongs to another chat.
func (s *Store) ChatGrant(ctx context.Context, chatID string, id int64) (Grant, error) {
	if !isUUID(chatID) {
		return Grant{}, ErrNotFound
	}
	return scanGrant(s.pool.QueryRow(ctx, `SELECT `+grantCols+` FROM internet_grants WHERE id=$1 AND chat_id=$2`, id, chatID))
}

func (s *Store) queryGrants(ctx context.Context, where string, args ...any) ([]Grant, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+grantCols+` FROM internet_grants WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Grant{}
	for rows.Next() {
		g, err := scanGrant(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// Grants lists all grants of a chat, newest first.
func (s *Store) Grants(ctx context.Context, chatID string) ([]Grant, error) {
	if !isUUID(chatID) {
		return nil, ErrNotFound
	}
	return s.queryGrants(ctx, `chat_id=$1 ORDER BY id DESC`, chatID)
}

// ActiveGrants lists the active grants of a chat, oldest first.
func (s *Store) ActiveGrants(ctx context.Context, chatID string) ([]Grant, error) {
	if !isUUID(chatID) {
		return nil, ErrNotFound
	}
	return s.queryGrants(ctx, `chat_id=$1 AND state='active' ORDER BY id`, chatID)
}

// AllActiveGrants lists the active grants of every chat (the manager keeps them in memory, loaded at the start).
func (s *Store) AllActiveGrants(ctx context.Context) ([]Grant, error) {
	return s.queryGrants(ctx, `state='active' ORDER BY id`)
}

// SetGrantBytes stores the running byte count of an active grant.
func (s *Store) SetGrantBytes(ctx context.Context, id, bytes int64) error {
	_, err := s.pool.Exec(ctx, `UPDATE internet_grants SET bytes=GREATEST(bytes,$2) WHERE id=$1 AND state='active'`, id, bytes)
	return err
}

// BumpBlocked counts one refused connection on the grant.
func (s *Store) BumpBlocked(ctx context.Context, id int64) error {
	_, err := s.pool.Exec(ctx, `UPDATE internet_grants SET blocked=blocked+1 WHERE id=$1`, id)
	return err
}

// AddGrantPackage records a package a packages grant covers; the first entry for a name wins (stage 2).
func (s *Store) AddGrantPackage(ctx context.Context, p GrantPackage) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO internet_grant_packages (grant_id, ecosystem, name, parent) VALUES ($1,$2,$3,$4)
		ON CONFLICT DO NOTHING`, p.GrantID, p.Ecosystem, p.Name, p.Parent)
	return err
}

// MarkInstalled sets the time a covered package was found installed (stage 2).
func (s *Store) MarkInstalled(ctx context.Context, grantID int64, ecosystem, name string) error {
	_, err := s.pool.Exec(ctx, `UPDATE internet_grant_packages SET installed_at=now()
		WHERE grant_id=$1 AND ecosystem=$2 AND name=$3 AND installed_at IS NULL`, grantID, ecosystem, name)
	return err
}

// GrantPackages lists the packages of a grant, requested ones first.
func (s *Store) GrantPackages(ctx context.Context, grantID int64) ([]GrantPackage, error) {
	rows, err := s.pool.Query(ctx, `SELECT grant_id, ecosystem, name, parent, installed_at FROM internet_grant_packages
		WHERE grant_id=$1 ORDER BY (parent <> ''), ecosystem, name`, grantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []GrantPackage{}
	for rows.Next() {
		var p GrantPackage
		if err := rows.Scan(&p.GrantID, &p.Ecosystem, &p.Name, &p.Parent, &p.InstalledAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// WebConnectionFilter selects rows of web_requests across chats (GET /api/activity/connections). Zero values do
// not filter.
type WebConnectionFilter struct {
	// Owner limits to the chats of this user; nil: all chats (token mode).
	Owner   *string
	ChatID  string
	GrantID int64
	// Result: "allowed" or "blocked" (empty: both).
	Result string
	// Origin: pi, exec or pkg (empty: all).
	Origin       string
	Since, Until time.Time
	Before       int64
	Limit        int
}

// WebConnections lists connections newest first (one more than Limit, so that the caller can tell whether another
// page follows). Rows of chats deleted for good are not listed: their chat_id is gone.
func (s *Store) WebConnections(ctx context.Context, f WebConnectionFilter) ([]WebRequest, error) {
	if f.Limit <= 0 {
		f.Limit = 100
	}
	var where []string
	var args []any
	add := func(cond string, v any) {
		args = append(args, v)
		where = append(where, fmt.Sprintf(cond, len(args)))
	}
	where = append(where, "w.chat_id IS NOT NULL")
	if f.Owner != nil {
		add("w.chat_id IN (SELECT id FROM chats WHERE owner = $%d)", *f.Owner)
	}
	if f.ChatID != "" {
		if !isUUID(f.ChatID) {
			return []WebRequest{}, nil
		}
		add("w.chat_id = $%d::uuid", f.ChatID)
	}
	if f.GrantID > 0 {
		add("w.grant_id = $%d", f.GrantID)
	}
	switch f.Result {
	case "allowed":
		where = append(where, "w.denied = ''")
	case "blocked":
		where = append(where, "w.denied <> ''")
	}
	if f.Origin != "" {
		add("w.origin = $%d", f.Origin)
	}
	if !f.Since.IsZero() {
		add("w.started_at >= $%d", f.Since)
	}
	if !f.Until.IsZero() {
		add("w.started_at < $%d", f.Until)
	}
	if f.Before > 0 {
		add("w.id < $%d", f.Before)
	}
	args = append(args, f.Limit+1)
	q := `SELECT ` + webCols("w") + ` FROM web_requests w WHERE ` + strings.Join(where, " AND ") +
		fmt.Sprintf(` ORDER BY w.id DESC LIMIT $%d`, len(args))
	return s.queryWeb(ctx, q, args...)
}

// ActivityChats returns what the activity views need of the given chats (title, model, binding), by id.
func (s *Store) ActivityChats(ctx context.Context, ids []string) (map[string]ActivityChat, error) {
	out := map[string]ActivityChat{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT id::text, title, model, variant, delegation FROM chats WHERE id::text = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var c ActivityChat
		var del []byte
		if err := rows.Scan(&c.ID, &c.Title, &c.Model, &c.Variant, &del); err != nil {
			return nil, err
		}
		if len(del) > 0 {
			c.Delegation = del
		}
		out[c.ID] = c
	}
	return out, rows.Err()
}

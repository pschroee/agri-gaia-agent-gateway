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

	"agw/internal/netgrant"
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

// GrantPackage is a package a packages grant covers: requested by the agent (Parent empty) or declared by a package
// that is covered (stage 2).
type GrantPackage struct {
	GrantID          int64      `json:"grant_id"`
	Ecosystem        string     `json:"ecosystem"`
	Name             string     `json:"name"`
	Parent           string     `json:"parent,omitempty"`
	Version          string     `json:"version,omitempty"` // exact pin of a root, empty: any version
	Extras           string     `json:"extras,omitempty"`  // comma separated, normalised
	DownloadedAt     *time.Time `json:"downloaded_at,omitempty"`
	InstalledVersion string     `json:"installed_version,omitempty"`
	InstalledAt      *time.Time `json:"installed_at,omitempty"`
}

// Package states shown to the user: waiting (nothing fetched yet), downloaded (the first file went through the
// gate), installed (found installed in the sandbox).
const (
	PkgWaiting    = "waiting"
	PkgDownloaded = "downloaded"
	PkgInstalled  = "installed"
)

// PackageState is one package of a packages grant as the API shows it (Grant.Packages of a packages grant).
type PackageState struct {
	Ecosystem        string     `json:"ecosystem"`
	Name             string     `json:"name"`
	Version          string     `json:"version,omitempty"`
	Parent           string     `json:"parent,omitempty"`
	State            string     `json:"state"`
	InstalledVersion string     `json:"installed_version,omitempty"`
	InstalledAt      *time.Time `json:"installed_at,omitempty"`
}

// State derives the state of the package.
func (p GrantPackage) State() PackageState {
	st := PackageState{Ecosystem: p.Ecosystem, Name: p.Name, Version: p.Version, Parent: p.Parent, State: PkgWaiting,
		InstalledVersion: p.InstalledVersion, InstalledAt: p.InstalledAt}
	switch {
	case p.InstalledAt != nil:
		st.State = PkgInstalled
	case p.DownloadedAt != nil:
		st.State = PkgDownloaded
	}
	return st
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
	_, err := s.pool.Exec(ctx, `INSERT INTO internet_grant_packages (grant_id, ecosystem, name, parent, version, extras) VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT DO NOTHING`, p.GrantID, p.Ecosystem, p.Name, p.Parent, p.Version, p.Extras)
	return err
}

// MarkDownloaded sets the time the first file of a covered package went through the gate.
func (s *Store) MarkDownloaded(ctx context.Context, grantID int64, ecosystem, name string) error {
	_, err := s.pool.Exec(ctx, `UPDATE internet_grant_packages SET downloaded_at=now()
		WHERE grant_id=$1 AND ecosystem=$2 AND name=$3 AND downloaded_at IS NULL`, grantID, ecosystem, name)
	return err
}

// MarkInstalled sets the time and version a covered package was found installed.
func (s *Store) MarkInstalled(ctx context.Context, grantID int64, ecosystem, name, version string) error {
	_, err := s.pool.Exec(ctx, `UPDATE internet_grant_packages SET installed_at=now(), installed_version=$4
		WHERE grant_id=$1 AND ecosystem=$2 AND name=$3 AND installed_at IS NULL`, grantID, ecosystem, name, version)
	return err
}

// GrantPackages lists the packages of a grant, requested ones first.
func (s *Store) GrantPackages(ctx context.Context, grantID int64) ([]GrantPackage, error) {
	rows, err := s.pool.Query(ctx, `SELECT grant_id, ecosystem, name, parent, version, extras, downloaded_at, installed_version, installed_at
		FROM internet_grant_packages WHERE grant_id=$1 ORDER BY (parent <> ''), ecosystem, name`, grantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []GrantPackage{}
	for rows.Next() {
		var p GrantPackage
		if err := rows.Scan(&p.GrantID, &p.Ecosystem, &p.Name, &p.Parent, &p.Version, &p.Extras, &p.DownloadedAt, &p.InstalledVersion, &p.InstalledAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GrantsByID reads grants by ID (the activity views).
func (s *Store) GrantsByID(ctx context.Context, ids []int64) (map[int64]Grant, error) {
	out := map[int64]Grant{}
	if len(ids) == 0 {
		return out, nil
	}
	gs, err := s.queryGrants(ctx, `id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	for _, g := range gs {
		out[g.ID] = g
	}
	return out, nil
}

// GrantsByApproval reads grants by the approval they waited for (approval ID -> grant).
func (s *Store) GrantsByApproval(ctx context.Context, approvalIDs []string) (map[string]Grant, error) {
	out := map[string]Grant{}
	if len(approvalIDs) == 0 {
		return out, nil
	}
	gs, err := s.queryGrants(ctx, `approval_id::text = ANY($1)`, approvalIDs)
	if err != nil {
		return nil, err
	}
	for _, g := range gs {
		out[g.ApprovalID] = g
	}
	return out, nil
}

// ChatBlocked counts the refused connections of a chat since the time (a collapsed row counts with its count) and
// returns the latest ones, newest first, at most last.
func (s *Store) ChatBlocked(ctx context.Context, chatID string, since time.Time, last int) (int, []WebRequest, error) {
	if !isUUID(chatID) {
		return 0, nil, nil
	}
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT COALESCE(sum(count),0)::int FROM web_requests WHERE chat_id=$1 AND denied <> '' AND started_at >= $2`,
		chatID, since).Scan(&n); err != nil {
		return 0, nil, err
	}
	if n == 0 || last <= 0 {
		return n, []WebRequest{}, nil
	}
	rows, err := s.queryWeb(ctx, `SELECT `+webCols("w")+` FROM web_requests w WHERE w.chat_id=$1 AND w.denied <> '' AND w.started_at >= $2 ORDER BY w.id DESC LIMIT $3`,
		chatID, since, last)
	return n, rows, err
}

// WebSummary totals the connections of a filter: rows counted by their count, bytes, and refused ones.
type WebSummary struct {
	Count     int   `json:"count"`
	BytesDown int64 `json:"bytes_down"`
	BytesUp   int64 `json:"bytes_up"`
	Blocked   int   `json:"blocked"`
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

func webConnWhere(f WebConnectionFilter, withCursor bool) ([]string, []any) {
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
	if withCursor && f.Before > 0 {
		add("w.id < $%d", f.Before)
	}
	return where, args
}

// WebConnections lists connections newest first (one more than Limit, so that the caller can tell whether another
// page follows). Rows of chats deleted for good are not listed: their chat_id is gone.
func (s *Store) WebConnections(ctx context.Context, f WebConnectionFilter) ([]WebRequest, error) {
	if f.Limit <= 0 {
		f.Limit = 100
	}
	if f.ChatID != "" && !isUUID(f.ChatID) {
		return []WebRequest{}, nil
	}
	where, args := webConnWhere(f, true)
	args = append(args, f.Limit+1)
	q := `SELECT ` + webCols("w") + ` FROM web_requests w WHERE ` + strings.Join(where, " AND ") +
		fmt.Sprintf(` ORDER BY w.id DESC LIMIT $%d`, len(args))
	return s.queryWeb(ctx, q, args...)
}

// WebConnectionsSummary totals everything the filter selects, ignoring the cursor and the limit. Total is the number
// of rows (for "show N more"); Count counts a collapsed row with its count.
func (s *Store) WebConnectionsSummary(ctx context.Context, f WebConnectionFilter) (sum WebSummary, total int, err error) {
	if f.ChatID != "" && !isUUID(f.ChatID) {
		return sum, 0, nil
	}
	where, args := webConnWhere(f, false)
	err = s.pool.QueryRow(ctx, `SELECT count(*), COALESCE(sum(w.count),0), COALESCE(sum(w.bytes_down),0), COALESCE(sum(w.bytes_up),0),
		COALESCE(sum(w.count) FILTER (WHERE w.denied <> ''),0) FROM web_requests w WHERE `+strings.Join(where, " AND "), args...).
		Scan(&total, &sum.Count, &sum.BytesDown, &sum.BytesUp, &sum.Blocked)
	return sum, total, err
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

// UnlistedBytes is the data a full grant moved that the web proxy did not list: the grant's byte count (Docker's
// counters) minus the bytes of its listed connections. Programs that ignore the proxy leave only this trace.
type UnlistedBytes struct {
	GrantID int64  `json:"grant_id"`
	ChatID  string `json:"chat_id"`
	Bytes   int64  `json:"bytes"`
}

// FullGrantsUnlisted lists, for the full grants in the filter's scope (owner, chat, grant, period), the bytes that
// are in no listed connection. Grants with nothing unlisted are left out.
func (s *Store) FullGrantsUnlisted(ctx context.Context, f WebConnectionFilter) ([]UnlistedBytes, error) {
	if f.ChatID != "" && !isUUID(f.ChatID) {
		return []UnlistedBytes{}, nil
	}
	var where []string
	var args []any
	add := func(cond string, v any) {
		args = append(args, v)
		where = append(where, fmt.Sprintf(cond, len(args)))
	}
	where = append(where, "g.kind = 'full'", "g.started_at IS NOT NULL")
	if f.Owner != nil {
		add("g.chat_id IN (SELECT id FROM chats WHERE owner = $%d)", *f.Owner)
	}
	if f.ChatID != "" {
		add("g.chat_id = $%d::uuid", f.ChatID)
	}
	if f.GrantID > 0 {
		add("g.id = $%d", f.GrantID)
	}
	if !f.Since.IsZero() {
		add("g.started_at >= $%d", f.Since)
	}
	if !f.Until.IsZero() {
		add("g.started_at < $%d", f.Until)
	}
	rows, err := s.pool.Query(ctx, `SELECT g.id, g.chat_id::text, g.bytes - COALESCE((SELECT sum(w.bytes_up + w.bytes_down) FROM web_requests w WHERE w.grant_id = g.id), 0)
		FROM internet_grants g WHERE `+strings.Join(where, " AND ")+` ORDER BY g.id DESC LIMIT 200`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UnlistedBytes{}
	for rows.Next() {
		var u UnlistedBytes
		if err := rows.Scan(&u.GrantID, &u.ChatID, &u.Bytes); err != nil {
			return nil, err
		}
		if u.Bytes > 0 {
			out = append(out, u)
		}
	}
	return out, rows.Err()
}

// RootStrings lists the packages the agent asked for in the canonical form ("pip:torch==2.5.1"); empty for other
// kinds. (The stored column holds the roots; the API replaces it by the package states.)
func (g Grant) RootStrings() []string {
	out := []string{}
	var roots []netgrant.PkgRoot
	if json.Unmarshal(g.Packages, &roots) == nil {
		for _, r := range roots {
			if r.Name != "" {
				out = append(out, r.String())
			}
		}
	}
	return out
}

// ActivityGrant describes the grant an internet entry of the activity belongs to.
type ActivityGrant struct {
	ID          int64      `json:"id"`
	Kind        string     `json:"kind"`
	Domains     []string   `json:"domains"`
	Packages    []string   `json:"packages"`
	Bytes       int64      `json:"bytes"`
	MaxBytes    int64      `json:"max_bytes"`
	State       string     `json:"state"`
	Origin      string     `json:"origin"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	EndedAt     *time.Time `json:"ended_at,omitempty"`
	EndedReason string     `json:"ended_reason,omitempty"`
}

func activityGrantOf(g Grant) *ActivityGrant {
	d := g.Domains
	if d == nil {
		d = []string{}
	}
	return &ActivityGrant{ID: g.ID, Kind: g.Kind, Domains: d, Packages: g.RootStrings(), Bytes: g.Bytes, MaxBytes: g.MaxBytes, State: g.State,
		Origin: g.Origin, StartedAt: g.StartedAt, ExpiresAt: g.ExpiresAt, EndedAt: g.EndedAt, EndedReason: g.EndedReason}
}

// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Outcomes of a platform call, derived from the result text of its socket_calls entry.
const (
	OutcomeOK       = "ok"
	OutcomeError    = "error"
	OutcomeBlocked  = "blocked"  // violation blocked by the delegation
	OutcomeLogged   = "logged"   // violation let through and logged (delegation without enforce)
	OutcomeRejected = "rejected" // rejected by the user (or no decision in time)
	OutcomeRefused  = "refused"  // refused by the gateway before it went out
)

// Outcomes lists the valid outcomes in display order.
var Outcomes = []string{OutcomeOK, OutcomeError, OutcomeBlocked, OutcomeLogged, OutcomeRejected, OutcomeRefused}

// outcomeSQL classifies sc.result like the result texts are written in sock.runPlatform. The order matters:
// "ok 200 · violation, logged only: …" is logged, not ok.
const outcomeSQL = `CASE
	WHEN sc.result LIKE 'violation blocked%' THEN 'blocked'
	WHEN sc.result LIKE '%violation, logged only%' THEN 'logged'
	WHEN sc.result LIKE 'rejected%' THEN 'rejected'
	WHEN sc.result LIKE 'refused%' OR sc.result LIKE 'not configured%' THEN 'refused'
	WHEN sc.result LIKE 'ok%' THEN 'ok'
	ELSE 'error' END`

// Kinds of activity entries (parameter kind of GET /api/activity, field kind of an entry).
const (
	KindPlatform = "platform" // platform calls (op platform); the default
	KindInternet = "internet" // internet switches: requests and "off" by the agent, switches by the user
	KindAll      = "all"      // both, in one list
)

// ActivityKinds lists the valid values of the kind parameter.
var ActivityKinds = []string{KindPlatform, KindInternet, KindAll}

// OpInternetUser is the user's internet switch in the socket log (via "user"); the agent's request and
// switch-off are the ops internet and internet_off.
const OpInternetUser = "internet_set"

// internetSQL describes an internet entry: action, origin and result. Request results: approved, already_on
// (the access was already granted, no approval), expired (no decision in time), rejected, refused (the gateway
// refused the request, issue #175), error; the end of a grant (op internet_end, action end): the result is the
// reason, time, bytes, installed, user or agent, and the origin user, agent or system; switch-off by the agent: off,
// already_off, error; the user's switch: on, off, already_on, already_off, error. Older entries logged an expired
// request as "rejected"; with a tool call its approval tells (ap.state).
const internetSQL = `CASE sc.op WHEN 'internet' THEN 'request' WHEN 'internet_off' THEN 'off' WHEN 'internet_end' THEN 'end' ELSE 'switch' END,
	CASE WHEN sc.op = 'internet_set' OR (sc.op = 'internet_end' AND sc.result = 'user') THEN 'user'
		WHEN sc.op = 'internet_end' AND sc.result <> 'agent' THEN 'system' ELSE 'agent' END,
	CASE sc.op
	WHEN 'internet' THEN CASE
		WHEN sc.result LIKE 'approved%' THEN 'approved'
		WHEN sc.result LIKE 'already on%' THEN 'already_on'
		WHEN sc.result LIKE 'expired%' OR (sc.result LIKE 'rejected%' AND ap.state = 'expired') THEN 'expired'
		WHEN sc.result LIKE 'rejected%' THEN 'rejected'
		WHEN sc.result LIKE 'refused%' THEN 'refused'
		ELSE 'error' END
	WHEN 'internet_end' THEN sc.result
	ELSE CASE
		WHEN sc.result IN ('on', 'off') THEN sc.result
		WHEN sc.result IN ('already on', 'already off') THEN replace(sc.result, ' ', '_')
		ELSE 'error' END
	END`

// ActivityFilter selects platform calls (and, with Kind, internet switches) across chats. Zero values do not
// filter.
type ActivityFilter struct {
	// Kind: KindPlatform (also ""), KindInternet or KindAll.
	Kind string
	// Owner limits to the chats of this user; nil: all chats (token mode).
	Owner  *string
	ChatID string
	// ToolCall limits to the calls of one tool call (the step details link to them, issue L6/#123).
	ToolCall string
	Since    time.Time // inclusive
	Until    time.Time // exclusive
	Outcome  string
	// Before: only calls with a smaller id (cursor of the previous page).
	Before int64
	Limit  int
}

// ActivityApproval is the approval a writing platform call waited for.
type ActivityApproval struct {
	ID        string     `json:"id"`
	State     string     `json:"state"`
	CreatedAt time.Time  `json:"created_at"`
	DecidedAt *time.Time `json:"decided_at,omitempty"`
}

// ActivityInternet describes an internet entry (kind internet).
type ActivityInternet struct {
	// Action: request (the agent asked, with approval), off (the agent switched off), switch (the user's switch),
	// end (a grant ended, issue #175).
	Action string `json:"action"`
	// Origin: agent (main agent or subagent, see session), user, or system (a grant ended by itself).
	Origin string `json:"origin"`
	// Result: approved, already_on, rejected, expired, on, off, already_off or error (see internetSQL).
	Result string `json:"result"`
}

// ActivityCall is a platform call with its outcome and approval, or an internet entry.
type ActivityCall struct {
	SocketCall
	Kind string `json:"kind"` // KindPlatform or KindInternet
	// Outcome of a platform call; empty for internet entries.
	Outcome  string            `json:"outcome,omitempty"`
	Internet *ActivityInternet `json:"internet,omitempty"`
	// Approval: the platform_write approval of a platform call, the internet_access approval of a request.
	Approval *ActivityApproval `json:"approval,omitempty"`
}

// ActivityChat is what the activity view needs of a chat.
type ActivityChat struct {
	ID         string          `json:"id"`
	Title      string          `json:"title"`
	Model      string          `json:"model"`
	Variant    string          `json:"variant"`
	Delegation json.RawMessage `json:"delegation,omitempty"`
	// Deleted: the chat was deleted for good; its calls are kept anonymised (issue #76). Title is empty then.
	Deleted bool `json:"deleted,omitempty"`
}

// ActivityDuration summarizes the measured durations (nil fields: nothing measured).
type ActivityDuration struct {
	Count int      `json:"count"`
	AvgMs *float64 `json:"avg_ms,omitempty"`
	P95Ms *float64 `json:"p95_ms,omitempty"`
	MaxMs *float64 `json:"max_ms,omitempty"`
}

// ActivitySummary covers all platform calls of the filter except outcome, cursor and limit; internet entries
// never count (whatever the kind).
type ActivitySummary struct {
	Total    int              `json:"total"`
	Outcomes map[string]int   `json:"outcomes"`
	Chats    int              `json:"chats"` // chats with at least one platform call
	Runs     int              `json:"runs"`  // requests to the agent (chat_turns) in the same chats and period
	Duration ActivityDuration `json:"duration"`
}

// ActivityPage is one page of platform calls, newest first.
type ActivityPage struct {
	Calls []ActivityCall          `json:"calls"`
	Chats map[string]ActivityChat `json:"chats"`
	// NextBefore: pass as before for the next page (0: no further page).
	NextBefore int64           `json:"next_before,omitempty"`
	Summary    ActivitySummary `json:"summary"`
}

// activityWhere builds the conditions shared by the calls, the summary and the runs (t: table alias with
// chat_id and created_at). The outcome is added by the caller where it applies.
// activityOwner is the owner of a call's chat: the live chat, or the tombstone of a deleted one (issue #76).
const activityOwner = "COALESCE(c.owner, d.owner)"

// activityWhere builds the conditions for the table alias t; owner is the expression the owner filter compares
// with (activityOwner where the query joins deleted_chats, else c.owner).
func activityWhere(f ActivityFilter, t, owner string, args *[]any) []string {
	add := func(v any) string {
		*args = append(*args, v)
		return fmt.Sprintf("$%d", len(*args))
	}
	var w []string
	if f.Owner != nil {
		w = append(w, owner+" = "+add(*f.Owner))
	}
	if f.ChatID != "" {
		if t == "sc" {
			w = append(w, "COALESCE(sc.chat_id, sc.deleted_chat)::text = "+add(f.ChatID))
		} else {
			w = append(w, t+".chat_id::text = "+add(f.ChatID))
		}
	}
	if f.ToolCall != "" && t == "sc" {
		// Only the socket calls carry a tool call id; the runs (t = "t", chat_turns) cannot be limited to one, so
		// they keep counting, while the owner and the period apply to them as always.
		w = append(w, "sc.tool_call_id = "+add(f.ToolCall))
	}
	if !f.Since.IsZero() {
		w = append(w, t+".created_at >= "+add(f.Since))
	}
	if !f.Until.IsZero() {
		w = append(w, t+".created_at < "+add(f.Until))
	}
	return w
}

// ListActivity returns platform calls across chats, newest first, with a summary of the period.
func (s *Store) ListActivity(ctx context.Context, f ActivityFilter) (ActivityPage, error) {
	page := ActivityPage{Calls: []ActivityCall{}, Chats: map[string]ActivityChat{}}
	if f.Limit <= 0 {
		f.Limit = 100
	}

	args := []any{}
	// The gateway's own reads for approval summaries (op summary_read, issue L2/#119) belong to the platform kind
	// (they are platform calls the gateway made), but never to the figures: activitySummary counts op = 'platform'
	// only, so the key numbers of the evaluation stay about the agent.
	ops := "sc.op IN ('platform', 'summary_read')"
	switch f.Kind {
	case KindInternet:
		ops = "sc.op IN ('internet', 'internet_off', 'internet_set', 'internet_end')"
	case KindAll:
		ops = "sc.op IN ('platform', 'summary_read', 'internet', 'internet_off', 'internet_set', 'internet_end')"
	}
	where := append([]string{ops, "(sc.chat_id IS NOT NULL OR sc.deleted_chat IS NOT NULL)"}, activityWhere(f, "sc", activityOwner, &args)...)
	if f.Outcome != "" {
		// Outcomes belong to platform calls: the filter leaves out internet entries.
		args = append(args, f.Outcome)
		where = append(where, fmt.Sprintf("sc.op = 'platform' AND (%s) = $%d", outcomeSQL, len(args)))
	}
	if f.Before > 0 {
		args = append(args, f.Before)
		where = append(where, fmt.Sprintf("sc.id < $%d", len(args)))
	}
	args = append(args, f.Limit+1)
	// The approval of a writing call: same chat, same tool call and the same call text (one tool call may make
	// several platform calls).
	// An internet request's approval: same chat and tool call, internet_access; a request answered "already on"
	// asked nobody.
	q := `SELECT sc.id, COALESCE(sc.chat_id, sc.deleted_chat)::text, sc.slot_id, sc.via, sc.op, sc.detail, sc.result, sc.created_at, sc.session,
		sc.tool_call_id, sc.duration_ms, COALESCE(sc.effect, ''), ` + outcomeSQL + `, ` + internetSQL + `,
		ap.id::text, ap.state, ap.created_at, ap.decided_at
	FROM socket_calls sc LEFT JOIN chats c ON c.id = sc.chat_id LEFT JOIN deleted_chats d ON d.id = sc.deleted_chat
	LEFT JOIN LATERAL (
		SELECT a.id, a.state, a.created_at, a.decided_at FROM approvals a
		WHERE a.chat_id = sc.chat_id AND sc.tool_call_id <> '' AND a.tool_call_id = sc.tool_call_id
		  AND a.created_at <= sc.created_at
		  AND ((sc.op = 'platform' AND a.kind = 'platform_write' AND a.name = sc.detail)
		    OR (sc.op = 'internet' AND a.kind = 'internet_access' AND sc.result NOT LIKE 'already on%'))
		ORDER BY a.created_at DESC LIMIT 1) ap ON true
	WHERE ` + strings.Join(where, " AND ") + fmt.Sprintf(` ORDER BY sc.id DESC LIMIT $%d`, len(args))
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	chatIDs := []string{}
	seen := map[string]bool{}
	for rows.Next() {
		var c ActivityCall
		var in ActivityInternet
		var apID, apState *string
		var apCreated, apDecided *time.Time
		if err := rows.Scan(&c.ID, &c.ChatID, &c.SlotID, &c.Via, &c.Op, &c.Detail, &c.Result, &c.CreatedAt, &c.Session,
			&c.ToolCallID, &c.DurationMs, &c.Effect, &c.Outcome, &in.Action, &in.Origin, &in.Result,
			&apID, &apState, &apCreated, &apDecided); err != nil {
			return page, err
		}
		switch c.Op {
		case "platform", "summary_read":
			// summary_read: the gateway's own reads for a summary (issue L2/#119) are platform calls of the system,
			// not of the agent; the frontend tells them apart by the op.
			c.Kind = KindPlatform
		default:
			c.Kind, c.Outcome, c.Internet = KindInternet, "", &in
		}
		if apID != nil {
			c.Approval = &ActivityApproval{ID: *apID, State: *apState, CreatedAt: *apCreated, DecidedAt: apDecided}
		}
		page.Calls = append(page.Calls, c)
		if !seen[c.ChatID] {
			seen[c.ChatID] = true
			chatIDs = append(chatIDs, c.ChatID)
		}
	}
	if err := rows.Err(); err != nil {
		return page, err
	}
	if len(page.Calls) > f.Limit {
		page.Calls = page.Calls[:f.Limit]
		page.NextBefore = page.Calls[f.Limit-1].ID
	}

	if len(chatIDs) > 0 {
		crow, err := s.pool.Query(ctx, `SELECT id::text, title, model, variant, delegation FROM chats WHERE id::text = ANY($1)`, chatIDs)
		if err != nil {
			return page, err
		}
		defer crow.Close()
		for crow.Next() {
			var c ActivityChat
			var del []byte
			if err := crow.Scan(&c.ID, &c.Title, &c.Model, &c.Variant, &del); err != nil {
				return page, err
			}
			if len(del) > 0 {
				c.Delegation = del
			}
			page.Chats[c.ID] = c
		}
		if err := crow.Err(); err != nil {
			return page, err
		}
		// Calls of deleted chats: the tombstone gives model and binding; the title is gone (issue #76).
		drows, err := s.pool.Query(ctx, `SELECT id::text, model, variant FROM deleted_chats WHERE id::text = ANY($1)`, chatIDs)
		if err != nil {
			return page, err
		}
		defer drows.Close()
		for drows.Next() {
			var c ActivityChat
			if err := drows.Scan(&c.ID, &c.Model, &c.Variant); err != nil {
				return page, err
			}
			c.Deleted = true
			if _, ok := page.Chats[c.ID]; !ok {
				page.Chats[c.ID] = c
			}
		}
		if err := drows.Err(); err != nil {
			return page, err
		}
	}

	sum, err := s.activitySummary(ctx, f)
	if err != nil {
		return page, err
	}
	page.Summary = sum
	return page, nil
}

func (s *Store) activitySummary(ctx context.Context, f ActivityFilter) (ActivitySummary, error) {
	sum := ActivitySummary{Outcomes: map[string]int{}}
	for _, o := range Outcomes {
		sum.Outcomes[o] = 0
	}
	args := []any{}
	where := append([]string{"sc.op = 'platform'", "(sc.chat_id IS NOT NULL OR sc.deleted_chat IS NOT NULL)"}, activityWhere(f, "sc", activityOwner, &args)...)
	q := `SELECT o, count(*), count(DISTINCT chat_id), count(duration_ms), avg(duration_ms),
		percentile_cont(0.95) WITHIN GROUP (ORDER BY duration_ms), max(duration_ms)
	FROM (SELECT COALESCE(sc.chat_id, sc.deleted_chat) AS chat_id, sc.duration_ms, ` + outcomeSQL + ` AS o
		FROM socket_calls sc LEFT JOIN chats c ON c.id = sc.chat_id LEFT JOIN deleted_chats d ON d.id = sc.deleted_chat
		WHERE ` + strings.Join(where, " AND ") + `) x
	GROUP BY ROLLUP (o)`
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return sum, err
	}
	defer rows.Close()
	for rows.Next() {
		var o *string
		var n, chats, measured int
		var avg, p95, max *float64
		if err := rows.Scan(&o, &n, &chats, &measured, &avg, &p95, &max); err != nil {
			return sum, err
		}
		if o != nil {
			sum.Outcomes[*o] = n
			continue
		}
		// Total row of the rollup.
		sum.Total, sum.Chats = n, chats
		sum.Duration = ActivityDuration{Count: measured, AvgMs: round1(avg), P95Ms: round1(p95), MaxMs: round1(max)}
	}
	if err := rows.Err(); err != nil {
		return sum, err
	}

	args = []any{}
	where = activityWhere(f, "t", "c.owner", &args)
	q = `SELECT count(*) FROM chat_turns t JOIN chats c ON c.id = t.chat_id`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	if err := s.pool.QueryRow(ctx, q, args...).Scan(&sum.Runs); err != nil {
		return sum, err
	}
	return sum, nil
}

func round1(v *float64) *float64 {
	if v == nil {
		return nil
	}
	r := float64(int64(*v*10+0.5)) / 10
	return &r
}

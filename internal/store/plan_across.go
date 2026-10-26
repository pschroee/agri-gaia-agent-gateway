// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"fmt"
	"strings"
)

// PlanStates lists every state a plan can be in.
var PlanStates = []string{"proposed", "approved", "paused", "completed", "failed", "cancelled", "rejected", "expired", "superseded"}

// PlanListFilter selects plans across chats (GET /api/plans, issue #223). Owner nil means every chat.
type PlanListFilter struct {
	Owner         *string
	States        []string
	Mode, ChatID  string
	Limit, Offset int
}

// PlanListItem is a plan together with the title of its chat.
type PlanListItem struct {
	Plan
	ChatTitle string `json:"chat_title"`
}

// planActiveStates are the states of a plan that is still going on; they sort first.
const planActiveStates = `('proposed','approved','paused')`

func (f PlanListFilter) where() (string, []any) {
	w := []string{"c.trashed_at IS NULL"}
	var args []any
	add := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	if f.Owner != nil {
		w = append(w, "c.owner = "+add(*f.Owner))
	}
	if len(f.States) > 0 {
		w = append(w, "p.state = ANY("+add(f.States)+")")
	}
	if f.Mode != "" {
		w = append(w, "p.mode = "+add(f.Mode))
	}
	if f.ChatID != "" {
		w = append(w, "p.chat_id = "+add(f.ChatID)+"::uuid")
	}
	return strings.Join(w, " AND "), args
}

// ListPlansAcross returns one page of the plans of not-trashed chats, running ones first, then newest first, each
// with its steps and the chat title, and the number of all plans matching the filter.
func (s *Store) ListPlansAcross(ctx context.Context, f PlanListFilter) ([]PlanListItem, int, error) {
	if f.ChatID != "" && !isUUID(f.ChatID) {
		return []PlanListItem{}, 0, nil
	}
	if err := s.expireLapsedPlans(ctx, f.Owner); err != nil {
		return nil, 0, err
	}
	where, args := f.where()
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM plans p JOIN chats c ON c.id = p.chat_id WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	limit := f.Limit
	if limit < 1 {
		limit = 25
	}
	args = append(args, limit, max(f.Offset, 0))
	q := fmt.Sprintf(`SELECT %s, chat_title FROM (SELECT p.*, c.title AS chat_title FROM plans p JOIN chats c ON c.id = p.chat_id WHERE %s) x
ORDER BY (state IN %s) DESC, created_at DESC, id DESC LIMIT $%d OFFSET $%d`, planCols, where, planActiveStates, len(args)-1, len(args))
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, 0, err
	}
	out := []PlanListItem{}
	for rows.Next() {
		var it PlanListItem
		var grant string
		err := rows.Scan(&it.ID, &it.ChatID, &it.ApprovalID, &it.Title, &it.Session, &it.ToolCallID, &it.Via, &it.State,
			&it.Mode, &it.Budget.MaxDurationS, &it.Budget.MaxLLMCost, &it.Budget.MaxAutoTurns, &it.AutoTurns, &grant,
			&it.PauseReason, &it.Replaces, &it.CreatedAt, &it.DecidedAt, &it.ExpiresAt, &it.EndedAt, &it.ChatTitle)
		if err != nil {
			rows.Close()
			return nil, 0, err
		}
		it.Grant = rawOrNil(grant)
		out = append(out, it)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	ids := make([]string, len(out))
	for i := range out {
		ids[i] = out[i].ID
	}
	steps, err := s.planStepsOf(ctx, ids)
	if err != nil {
		return nil, 0, err
	}
	for i := range out {
		out[i].Steps = steps[out[i].ID]
		if out[i].Steps == nil {
			out[i].Steps = []PlanStep{}
		}
	}
	return out, total, nil
}

// planStepsOf reads the steps of several plans in one query, by plan id, in step order.
func (s *Store) planStepsOf(ctx context.Context, ids []string) (map[string][]PlanStep, error) {
	out := map[string][]PlanStep{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT s.plan_id::text, `+planStepCols+` FROM `+planStepFrom+` WHERE s.plan_id = ANY($1::uuid[]) ORDER BY s.plan_id, s.idx`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var pid string
		st, err := scanPlanStep(prefixedRow{rows, &pid})
		if err != nil {
			return nil, err
		}
		st.PlanID = pid
		out[pid] = append(out[pid], st)
	}
	return out, rows.Err()
}

// RunningPlanCount counts the approved and paused plans of not-trashed chats (owner nil: every chat).
func (s *Store) RunningPlanCount(ctx context.Context, owner *string) (int, error) {
	if err := s.expireLapsedPlans(ctx, owner); err != nil {
		return 0, err
	}
	q := `SELECT count(*) FROM plans p JOIN chats c ON c.id = p.chat_id WHERE c.trashed_at IS NULL AND p.state IN ('approved','paused')`
	args := []any{}
	if owner != nil {
		q += ` AND c.owner = $1`
		args = append(args, *owner)
	}
	var n int
	err := s.pool.QueryRow(ctx, q, args...).Scan(&n)
	return n, err
}

// prefixedRow lets scanPlanStep read a row that starts with one extra column.
type prefixedRow struct {
	row    interface{ Scan(...any) error }
	prefix *string
}

func (r prefixedRow) Scan(dest ...any) error {
	return r.row.Scan(append([]any{r.prefix}, dest...)...)
}

// expireLapsedPlans moves approved plans whose time is over to expired (owner nil: every chat). The chat does so
// only when the next call of the plan comes (planNext), so a plan nobody touched again stayed "running" in the
// overview for days (render check of #223, 26.10.2026).
func (s *Store) expireLapsedPlans(ctx context.Context, owner *string) error {
	q := `UPDATE plans p SET state='expired' FROM chats c WHERE c.id = p.chat_id AND p.state='approved'
AND p.expires_at IS NOT NULL AND p.expires_at <= now()`
	args := []any{}
	if owner != nil {
		q += ` AND c.owner = $1`
		args = append(args, *owner)
	}
	_, err := s.pool.Exec(ctx, q, args...)
	return err
}

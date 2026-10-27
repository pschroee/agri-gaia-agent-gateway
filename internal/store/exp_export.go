// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package store

// Export of experiment data per plan (X5.7). One read path per table with fixed columns; the API streams the rows as
// JSONL and the CLI turns them into JSONL and CSV. Only chats that belong to the plan are read, never the whole
// database.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ExportTable is one table of the export: fixed column names and rows in the same order.
type ExportTable struct {
	Name    string
	Columns []string
	Rows    [][]any
}

// PlanRun is one attempt of one run. ID is the attempt (run key plus "#<attempt>" after a retry), RunID the logical
// run key (<plan>/<cell>/<task>/<rep>) that the evaluation counts; superseded attempts stay visible.
type PlanRun struct {
	ID                 string
	RunID, Cell, Task  string
	Rep                int
	ChatID, Model      string
	Pass               *bool
	EndReason          string
	SupersededBy       string
	Status             string
	GatewayVersion     string
	PlanSHA256         string
	ImagePi            string
	ImageExec          string
	CatalogSHA256      string
	SkillsSHA256       string
	InferenceRuntime   string
	StartedAt, EndedAt *time.Time
	DurationMs         *int64
}

// PlanRuns reads the attempts of a plan from exp_runs (X1).
func (s *Store) PlanRuns(ctx context.Context, plan string) ([]PlanRun, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, run_key, cell, task, rep, COALESCE(chat_id::text,''), COALESCE(model,''),
 pass, COALESCE(end_reason,''), COALESCE(superseded_by,''), COALESCE(status,''), started_at, ended_at,
 CASE WHEN ended_at IS NOT NULL THEN (extract(epoch FROM ended_at-started_at)*1000)::bigint END,
 COALESCE(gateway_version,''), COALESCE(plan_sha256,''),
 COALESCE(image_pi,''), COALESCE(image_exec,''), COALESCE(catalog_sha256,''), COALESCE(skills_sha256,''), COALESCE(inference_runtime,'')
FROM exp_runs WHERE plan=$1 ORDER BY position, run_key, id`, plan)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PlanRun{}
	for rows.Next() {
		var r PlanRun
		var pass *bool
		if err := rows.Scan(&r.ID, &r.RunID, &r.Cell, &r.Task, &r.Rep, &r.ChatID, &r.Model, &pass, &r.EndReason,
			&r.SupersededBy, &r.Status, &r.StartedAt, &r.EndedAt, &r.DurationMs, &r.GatewayVersion, &r.PlanSHA256,
			&r.ImagePi, &r.ImageExec, &r.CatalogSHA256, &r.SkillsSHA256, &r.InferenceRuntime); err != nil {
			return nil, err
		}
		r.Pass = pass
		out = append(out, r)
	}
	return out, rows.Err()
}

// planChatsCTE maps a run's chat to the logical run (run_key) and its cell, task and repetition.
func (s *Store) planChatsCTE(ctx context.Context) (string, error) {
	return `plan_chats AS (
  SELECT chat_id, run_key AS run_id, COALESCE(cell,'') AS cell, COALESCE(task,'') AS task, COALESCE(rep,0) AS rep
  FROM exp_runs WHERE plan=$1 AND chat_id IS NOT NULL
)`, nil
}

var exportTables = []string{"runs", "platform_calls", "tool_executions", "llm_calls", "reconcile", "fixtures", "syscalls", "turns"}

// ExportNames lists the tables of the export in the order of the manifest.
func ExportNames() []string { return append([]string(nil), exportTables...) }

// ExportTable reads one table for a plan. "reconcile" is computed by the API because it needs the chat package.
func (s *Store) ExportTable(ctx context.Context, plan, table string) (*ExportTable, error) {
	switch table {
	case "runs":
		return s.exportRuns(ctx, plan)
	case "platform_calls":
		return s.exportQuery(ctx, plan, table,
			[]string{"run_id", "cell", "task", "rep", "id", "chat_id", "slot_id", "via", "op", "detail", "result", "created_at",
				"session", "tool_call_id", "duration_ms", "effect", "decision", "reason", "rule", "body", "body_truncated",
				"body_in_sha256", "call_ref", "call_ref_source", "call_ref_candidates", "call_ref_claimed", "within_delegation",
				"approval_id", "approval_decision", "plan_step"},
			`SELECT r.run_id, r.cell, r.task, r.rep, s.id, COALESCE(s.chat_id::text,''), s.slot_id, s.via, s.op, s.detail, s.result,
  s.created_at, s.session, s.tool_call_id, s.duration_ms, COALESCE(s.effect,''), COALESCE(s.decision,''), COALESCE(s.reason,''),
  s.rule, CASE WHEN s.body IS NULL THEN NULL ELSE convert_from(s.body,'UTF8') END, s.body_truncated,
  COALESCE(s.body_in_sha256,''), COALESCE(s.call_ref,''), COALESCE(s.call_ref_source,''), s.call_ref_candidates,
  COALESCE(s.call_ref_claimed,''), s.within_delegation, COALESCE(s.approval_id::text,''), COALESCE(s.approval_decision,''),
  s.plan_step
FROM socket_calls s JOIN plan_chats r ON r.chat_id = s.chat_id
WHERE s.op = 'platform' ORDER BY s.id`)
	case "tool_executions":
		return s.exportQuery(ctx, plan, table,
			[]string{"run_id", "cell", "task", "rep", "id", "chat_id", "slot_id", "session", "tool_call_id", "tool", "op", "args",
				"exit_code", "error", "output_excerpt", "output_sha256", "output_bytes", "started_at", "duration_ms",
				"args_truncated", "command_bytes", "command_sha256"},
			`SELECT r.run_id, r.cell, r.task, r.rep, e.id, COALESCE(e.chat_id::text,''), e.slot_id, e.session, e.tool_call_id,
  e.tool, e.op, e.args, e.exit_code, e.error, e.output_excerpt, e.output_sha256, e.output_bytes, e.started_at,
  e.duration_ms, e.args_truncated, e.command_bytes, COALESCE(e.command_sha256,'')
FROM tool_executions e JOIN plan_chats r ON r.chat_id = e.chat_id ORDER BY e.id`)
	case "llm_calls":
		return s.exportQuery(ctx, plan, table,
			[]string{"run_id", "cell", "task", "rep", "id", "chat_id", "slot_id", "model", "response_id", "status",
				"tokens_in", "tokens_out", "input", "output", "cache_read", "cache_write", "cost", "peak", "tool_calls",
				"started_at", "duration_ms", "finish_reason", "complete", "request_bytes", "tools_sha256"},
			`SELECT r.run_id, r.cell, r.task, r.rep, x.id, COALESCE(x.chat_id::text,''), x.slot_id, x.model, x.response_id,
  x.status, x.input AS tokens_in, x.output AS tokens_out, x.input, x.output, x.cache_read, x.cache_write, x.cost, x.peak,
  x.tool_calls, x.started_at, x.duration_ms, x.finish_reason, x.complete, x.request_bytes, COALESCE(x.tools_sha256,'')
FROM llm_calls x JOIN plan_chats r ON r.chat_id = x.chat_id ORDER BY x.id`)
	case "turns":
		return s.exportQuery(ctx, plan, table,
			[]string{"run_id", "cell", "task", "rep", "id", "chat_id", "trigger", "origin", "sources", "queue_ids",
				"created_at", "abort_reason"},
			`SELECT r.run_id, r.cell, r.task, r.rep, t.id, t.chat_id::text, t.trigger, t.origin, t.sources, t.queue_ids,
  t.created_at, COALESCE(t.abort_reason,'')
FROM chat_turns t JOIN plan_chats r ON r.chat_id = t.chat_id ORDER BY t.id`)
	case "fixtures":
		// X4 records the seeded platform objects. A fixture seeded by the runner carries its run_key (#207) and
		// belongs to that run only; an older row without run_key belongs to the whole cell, so the export repeats
		// it once per run of the cell.
		return s.exportQuery(ctx, plan, table,
			[]string{"run_id", "cell", "task", "rep", "fixture_id", "manifest_id", "kind", "owner", "object_id",
				"field_sha256", "created_at", "removed_at", "remove_result", "run_key"},
			`SELECT r.run_id, r.cell, r.task, r.rep, f.id, f.manifest_id, f.kind, f.owner, f.object_id, f.field_sha256,
  f.created_at, f.removed_at, f.remove_result, f.run_key
FROM exp_fixtures f JOIN plan_chats r ON (f.run_key <> '' AND f.run_key = r.run_id) OR (f.run_key = '' AND r.cell = f.cell) WHERE f.plan = $1 ORDER BY f.created_at, f.id, r.run_id`)
	case "syscalls":
		// X6: failed syscalls per execution; the chat assigns them to the run.
		return s.exportQuery(ctx, plan, table,
			[]string{"run_id", "cell", "task", "rep", "id", "chat_id", "slot_id", "execution_id", "pid", "syscall",
				"args", "errno", "ts", "trace_truncated"},
			`SELECT r.run_id, r.cell, r.task, r.rep, y.id, COALESCE(y.chat_id::text,''), y.slot_id, y.execution_id,
  y.pid, y.syscall, y.args, y.errno, y.ts, y.trace_truncated
FROM exec_syscalls y JOIN plan_chats r ON r.chat_id = y.chat_id ORDER BY y.id`)
	}
	return nil, fmt.Errorf("unknown export table %q", table)
}

// exportQuery runs one table query with the plan CTE and scans the values generically.
func (s *Store) exportQuery(ctx context.Context, plan, name string, cols []string, selectSQL string) (*ExportTable, error) {
	cte, err := s.planChatsCTE(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, "WITH "+cte+"\n"+selectSQL, plan)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := &ExportTable{Name: name, Columns: cols}
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			return nil, err
		}
		for i, v := range vals {
			if b, ok := v.([]byte); ok {
				if json.Valid(b) {
					vals[i] = json.RawMessage(b)
				}
			}
		}
		out.Rows = append(out.Rows, vals)
	}
	return out, rows.Err()
}

// exportRuns writes the runs table. binding, protection and attack are derived from the cell identifier
// <binding>-<protection>-<attack>-<model>; model_role stays empty unless the cell names the role, and the CLI fills
// it from plan.yaml when it has the material.
func (s *Store) exportRuns(ctx context.Context, plan string) (*ExportTable, error) {
	runs, err := s.PlanRuns(ctx, plan)
	if err != nil {
		return nil, err
	}
	cols := []string{"run_id", "cell", "task", "rep", "id", "chat_id", "model", "model_role", "pass", "end_reason", "status",
		"superseded_by", "binding", "protection", "attack", "started_at", "ended_at", "duration_ms", "gateway_version", "plan_sha256",
		"image_pi", "image_exec", "catalog_sha256", "skills_sha256", "inference_runtime"}
	out := &ExportTable{Name: "runs", Columns: cols}
	for _, r := range runs {
		binding, protection, attack, role := cellParts(r.Cell)
		var pass any
		if r.Pass != nil {
			pass = *r.Pass
		}
		var dur any
		if r.DurationMs != nil {
			dur = *r.DurationMs
		}
		out.Rows = append(out.Rows, []any{r.RunID, r.Cell, r.Task, r.Rep, r.ID, r.ChatID, r.Model, role, pass,
			r.EndReason, r.Status, r.SupersededBy, binding, protection, attack, r.StartedAt, r.EndedAt, dur, r.GatewayVersion, r.PlanSHA256,
			r.ImagePi, r.ImageExec, r.CatalogSHA256, r.SkillsSHA256, r.InferenceRuntime})
	}
	return out, nil
}

// cellParts splits <binding>-<protection>-<attack>-<model>. A cell that does not follow the scheme leaves the
// factors empty; the evaluation then reports the unreadable cell.
func cellParts(cell string) (binding, protection, attack, role string) {
	// A combined binding itself carries a hyphen (mcp-cli, cli-mcp, mcp,cli); it must not be split as binding and
	// protection.
	for _, b := range []string{"mcp-cli", "cli-mcp", "mcp,cli"} {
		if strings.HasPrefix(cell, b+"-") {
			parts := strings.SplitN(strings.TrimPrefix(cell, b+"-"), "-", 3)
			if len(parts) < 2 {
				return b, "", "", ""
			}
			return b, parts[0], parts[1], roleAt(parts, 2)
		}
	}
	parts := strings.SplitN(cell, "-", 4)
	if len(parts) < 3 {
		return "", "", "", ""
	}
	return parts[0], parts[1], parts[2], roleAt(parts, 3)
}

func roleAt(parts []string, i int) string {
	if i < len(parts) && (parts[i] == "main" || parts[i] == "second") {
		return parts[i]
	}
	return ""
}

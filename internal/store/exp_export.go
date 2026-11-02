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
	"regexp"
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

var exportTables = []string{"runs", "platform_calls", "tool_executions", "llm_calls", "reconcile", "fixtures", "syscalls", "turns",
	"messages", "approvals", "internet_grants", "web_requests", "notebook_runs", "artifacts", "plan_steps"}

// messageTextSQL is the text of a chat_messages row aliased m: only its text items joined with a newline (like
// exp.AssistantText, but for each message), or the content itself when it is a plain string. Thinking, tool calls and
// tool results never count.
const messageTextSQL = `COALESCE(
  (SELECT string_agg(x.e->>'text', E'\n' ORDER BY x.ord)
     FROM jsonb_array_elements(CASE WHEN jsonb_typeof(m.message->'content') = 'array' THEN m.message->'content' ELSE '[]'::jsonb END)
          WITH ORDINALITY AS x(e, ord)
    WHERE x.e->>'type' = 'text'),
  CASE WHEN jsonb_typeof(m.message->'content') = 'string' THEN m.message->>'content' END,
  '')`

// turnOfCallSQL finds the turn a call belongs to: the last turn of the same chat that began at or before the call.
// A call before the first turn has none (NULL). It needs no column written at call time, so it also holds for runs
// recorded earlier.
func turnOfCallSQL(alias, timeCol string) string {
	return `LEFT JOIN LATERAL (SELECT ct.id FROM chat_turns ct WHERE ct.chat_id = ` + alias + `.chat_id AND ct.created_at <= ` +
		alias + `.` + timeCol + ` ORDER BY ct.id DESC LIMIT 1) tt ON true`
}

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
				"approval_id", "approval_decision", "plan_step", "turn_id"},
			`SELECT r.run_id, r.cell, r.task, r.rep, s.id, COALESCE(s.chat_id::text,''), s.slot_id, s.via, s.op, s.detail, s.result,
  s.created_at, s.session, s.tool_call_id, s.duration_ms, COALESCE(s.effect,''), COALESCE(s.decision,''), COALESCE(s.reason,''),
  s.rule, CASE WHEN s.body IS NULL THEN NULL ELSE convert_from(s.body,'UTF8') END, s.body_truncated,
  COALESCE(s.body_in_sha256,''), COALESCE(s.call_ref,''), COALESCE(s.call_ref_source,''), s.call_ref_candidates,
  COALESCE(s.call_ref_claimed,''), s.within_delegation, COALESCE(s.approval_id::text,''), COALESCE(s.approval_decision,''),
  s.plan_step, tt.id
FROM socket_calls s JOIN plan_chats r ON r.chat_id = s.chat_id
`+turnOfCallSQL("s", "created_at")+`
WHERE s.op = 'platform' ORDER BY s.id`)
	case "tool_executions":
		return s.exportQuery(ctx, plan, table,
			[]string{"run_id", "cell", "task", "rep", "id", "chat_id", "slot_id", "session", "tool_call_id", "tool", "op", "args",
				"exit_code", "error", "output_excerpt", "output_sha256", "output_bytes", "started_at", "duration_ms",
				"args_truncated", "command_bytes", "command_sha256", "turn_id"},
			`SELECT r.run_id, r.cell, r.task, r.rep, e.id, COALESCE(e.chat_id::text,''), e.slot_id, e.session, e.tool_call_id,
  e.tool, e.op, e.args, e.exit_code, e.error, e.output_excerpt, e.output_sha256, e.output_bytes, e.started_at,
  e.duration_ms, e.args_truncated, e.command_bytes, COALESCE(e.command_sha256,''), tt.id
FROM tool_executions e JOIN plan_chats r ON r.chat_id = e.chat_id
`+turnOfCallSQL("e", "started_at")+`
ORDER BY e.id`)
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
				"created_at", "abort_reason", "user_seq", "user_text"},
			// The user's request of a turn is the first user message that carries the turn's id; a turn without
			// a user message (a wake-up) has none: user_seq NULL, user_text empty.
			`SELECT r.run_id, r.cell, r.task, r.rep, t.id, t.chat_id::text, t.trigger, t.origin, t.sources, t.queue_ids,
  t.created_at, COALESCE(t.abort_reason,''), u.seq, COALESCE(u.text,'')
FROM chat_turns t JOIN plan_chats r ON r.chat_id = t.chat_id
LEFT JOIN LATERAL (SELECT m.seq, `+messageTextSQL+` AS text FROM chat_messages m
  WHERE m.chat_id = t.chat_id AND m.turn_id = t.id AND m.role = 'user' ORDER BY m.seq LIMIT 1) u ON true
ORDER BY t.id`)
	case "messages":
		return s.exportQuery(ctx, plan, table,
			[]string{"run_id", "cell", "task", "rep", "chat_id", "seq", "role", "text", "message", "created_at", "turn_id"},
			`SELECT r.run_id, r.cell, r.task, r.rep, m.chat_id::text, m.seq, m.role, `+messageTextSQL+`, m.message, m.created_at, m.turn_id
FROM chat_messages m JOIN plan_chats r ON r.chat_id = m.chat_id ORDER BY m.chat_id, m.seq`)
	case "approvals":
		return s.exportQuery(ctx, plan, table,
			[]string{"run_id", "cell", "task", "rep", "id", "chat_id", "kind", "via", "name", "effect", "state", "decided_by",
				"created_at", "decided_at", "summary", "internet"},
			`SELECT r.run_id, r.cell, r.task, r.rep, a.id::text, a.chat_id::text, a.kind, a.via, a.name, COALESCE(a.effect,''),
  a.state, a.decided_by, a.created_at, a.decided_at, COALESCE(a.summary,''), a.internet
FROM approvals a JOIN plan_chats r ON r.chat_id = a.chat_id ORDER BY a.created_at, a.id`)
	case "internet_grants":
		// Every column of the table; it holds no secret (kind, list, limits, end). A new column with a secret would
		// have to stay out of this list.
		return s.exportQuery(ctx, plan, table,
			[]string{"run_id", "cell", "task", "rep", "id", "chat_id", "approval_id", "kind", "origin", "via", "session",
				"tool_call_id", "reason", "domains", "packages", "state", "max_minutes", "max_bytes", "bytes", "blocked",
				"created_at", "started_at", "expires_at", "ended_at", "ended_reason"},
			`SELECT r.run_id, r.cell, r.task, r.rep, g.id, g.chat_id::text, COALESCE(g.approval_id::text,''), g.kind, g.origin,
  g.via, g.session, g.tool_call_id, g.reason, g.domains, g.packages, g.state, g.max_minutes, g.max_bytes, g.bytes, g.blocked,
  g.created_at, g.started_at, g.expires_at, g.ended_at, g.ended_reason
FROM internet_grants g JOIN plan_chats r ON r.chat_id = g.chat_id ORDER BY g.id`)
	case "web_requests":
		// Without source_ip: an address inside the sandbox network says nothing about the run.
		return s.exportQuery(ctx, plan, table,
			[]string{"run_id", "cell", "task", "rep", "id", "chat_id", "method", "host", "port", "path", "status",
				"bytes_up", "bytes_down", "denied", "started_at", "duration_ms"},
			`SELECT r.run_id, r.cell, r.task, r.rep, w.id, w.chat_id::text, w.method, w.host, w.port, w.path, w.status,
  w.bytes_up, w.bytes_down, w.denied, w.started_at, w.duration_ms
FROM web_requests w JOIN plan_chats r ON r.chat_id = w.chat_id ORDER BY w.id`)
	case "notebook_runs":
		return s.exportQuery(ctx, plan, table,
			[]string{"run_id", "cell", "task", "rep", "id", "chat_id", "entry", "files", "dataset_ids", "timeout_min",
				"started_at", "finished_at", "exit_code"},
			`SELECT r.run_id, r.cell, r.task, r.rep, n.id, n.chat_id::text, n.entry, n.files, n.dataset_ids, n.timeout_min,
  n.started_at, n.finished_at, n.exit_code
FROM notebook_runs n JOIN plan_chats r ON r.chat_id = n.chat_id ORDER BY n.started_at, n.id`)
	case "artifacts":
		// Without object_key: the internal storage path is of no use for the evaluation.
		return s.exportQuery(ctx, plan, table,
			[]string{"run_id", "cell", "task", "rep", "chat_id", "kind", "name", "size", "content_type", "via", "created_at"},
			`SELECT r.run_id, r.cell, r.task, r.rep, a.chat_id::text, a.kind, a.name, a.size, a.content_type, a.via, a.created_at
FROM artifacts a JOIN plan_chats r ON r.chat_id = a.chat_id ORDER BY a.created_at, a.kind, a.name`)
	case "plan_steps":
		return s.exportQuery(ctx, plan, table,
			[]string{"run_id", "cell", "task", "rep", "plan_id", "idx", "description", "tool", "method", "path", "body", "effect",
				"state", "created_resource", "created_id", "done_at"},
			`SELECT r.run_id, r.cell, r.task, r.rep, p.id::text, ps.idx, ps.description, ps.tool, ps.method, ps.path, ps.body,
  ps.effect, ps.state, ps.created_resource, ps.created_id, ps.done_at
FROM plan_steps ps JOIN plans p ON p.id = ps.plan_id JOIN plan_chats r ON r.chat_id = p.chat_id
ORDER BY p.created_at, p.id, ps.idx`)
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
		"image_pi", "image_exec", "catalog_sha256", "skills_sha256", "inference_runtime", "first_user_text_sha256"}
	out := &ExportTable{Name: "runs", Columns: cols}
	firstSHA, err := s.firstUserTextSHA(ctx, runs)
	if err != nil {
		return nil, err
	}
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
			r.ImagePi, r.ImageExec, r.CatalogSHA256, r.SkillsSHA256, r.InferenceRuntime, firstSHA[r.ChatID]})
	}
	return out, nil
}

// firstUserTextSHA maps each run's chat to the SHA-256 of the text of its first user message, so that a reader can
// check that the run received exactly the task text without a second copy of the text in the export. The hash is
// the one the gateway stored for the user's own text (sources[].text_sha256), so the rights instruction of the
// levels instruction and service does not change it; messages from before it was stored hash the whole text. A chat without
// a user message is missing from the map (empty value in the export).
func (s *Store) firstUserTextSHA(ctx context.Context, runs []PlanRun) (map[string]string, error) {
	out := map[string]string{}
	var ids []string
	for _, r := range runs {
		if isUUID(r.ChatID) {
			ids = append(ids, r.ChatID)
		}
	}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT ON (m.chat_id) m.chat_id::text, COALESCE(
  (SELECT x.e->>'text_sha256'
     FROM jsonb_array_elements(CASE WHEN jsonb_typeof(m.sources) = 'array' THEN m.sources ELSE '[]'::jsonb END)
          WITH ORDINALITY AS x(e, ord)
    WHERE x.e->>'kind' = 'user' AND COALESCE(x.e->>'text_sha256', '') <> '' ORDER BY x.ord LIMIT 1),
  encode(sha256(convert_to(`+messageTextSQL+`, 'UTF8')), 'hex'))
FROM chat_messages m WHERE m.chat_id::text = ANY($1::text[]) AND m.role = 'user' ORDER BY m.chat_id, m.seq`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var chatID, sum string
		if err := rows.Scan(&chatID, &sum); err != nil {
			return nil, err
		}
		out[chatID] = sum
	}
	return out, rows.Err()
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

// roleName is the pattern of a model role in a plan (internal/exp.RoleName): benchmark plans name their roles freely, so
// the cell identifier ends with any such role, hyphens included (deepseek-flash). SplitN keeps everything after the
// third hyphen together.
var roleName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

func roleAt(parts []string, i int) string {
	if i < len(parts) && roleName.MatchString(parts[i]) {
		return parts[i]
	}
	return ""
}

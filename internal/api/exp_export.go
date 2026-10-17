// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package api

// GET /api/exp/export?plan=&table= (X5.7): one table of experiment data as a JSONL stream. Only the separate
// experiment key reaches it (auth middleware). The CLI writes the files, the CSV forms and the manifest.

import (
	"encoding/json"
	"net/http"
	"slices"

	"agw/internal/store"
	"agw/internal/version"
)

// expExport streams one export table.
func (s *Server) expExport(w http.ResponseWriter, r *http.Request) {
	plan := r.URL.Query().Get("plan")
	table := r.URL.Query().Get("table")
	if plan == "" || table == "" {
		writeErr(w, http.StatusBadRequest, "plan and table are required")
		return
	}
	if !slices.Contains(store.ExportNames(), table) {
		writeErr(w, http.StatusBadRequest, "unknown table "+table)
		return
	}
	tbl, err := s.exportTable(r, plan, table)
	if err != nil {
		fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("X-Agw-Gateway-Version", GatewayVersion())
	cols, _ := json.Marshal(tbl.Columns)
	w.Header().Set("X-Agw-Columns", string(cols))
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	for _, row := range tbl.Rows {
		obj := make(map[string]any, len(tbl.Columns))
		for i, c := range tbl.Columns {
			if i < len(row) {
				obj[c] = row[i]
			}
		}
		if err := enc.Encode(obj); err != nil {
			return
		}
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// exportTable reads one table; reconcile is computed from the gateway's own execution records.
func (s *Server) exportTable(r *http.Request, plan, table string) (*store.ExportTable, error) {
	if table != "reconcile" {
		return s.M.Store().ExportTable(r.Context(), plan, table)
	}
	cols := []string{"run_id", "cell", "task", "rep", "tool_call_id", "state", "tool", "executed_tool", "requested",
		"executed", "main", "session", "llm_call_id", "response_id", "arguments", "requested_at", "started_at", "ops",
		"reason", "exit_code", "error", "duration_ms", "output_sha256", "execution_ids"}
	out := &store.ExportTable{Name: table, Columns: cols}
	runs, err := s.M.Store().PlanRuns(r.Context(), plan)
	if err != nil {
		return nil, err
	}
	for _, run := range runs {
		if run.ChatID == "" {
			continue
		}
		rec, err := s.M.ToolExecutions(r.Context(), run.ChatID)
		if err != nil {
			return nil, err
		}
		for _, c := range rec.Calls {
			out.Rows = append(out.Rows, []any{run.RunID, run.Cell, run.Task, run.Rep, c.ToolCallID, c.State, c.Tool,
				c.ExecutedTool, c.Requested, c.Executed, c.Main, c.Session, c.LLMCallID, c.ResponseID, c.Arguments,
				c.RequestedAt, c.StartedAt, c.Ops, c.Reason, c.ExitCode, c.Error, c.DurationMs, c.OutputSHA256, c.ExecutionIDs})
		}
	}
	return out, nil
}

// GatewayVersion names the deployed gateway in the manifest (X5.7/X8.1). It is the value embedded at build
// time; a local build falls back to AGW_VERSION or the module version.
func GatewayVersion() string { return version.String() }

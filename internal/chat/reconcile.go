package chat

// Abgleich angeforderter und ausgeführter Werkzeugaufrufe (E9). Angefordert
// heißt: Der LLM-Proxy hat den Aufruf mit seiner ID in der Antwort des
// Anbieters gesehen (llm_calls.tool_calls). Ausgeführt heißt: Der
// Orchestrator hat für diese ID eine Operation in der Ausführungs-Sandbox
// ausgeführt (tool_executions). Beide Quellen liegen außerhalb der
// Reichweite des Agenten.

import (
	"context"
	"encoding/json"
	"log/slog"
	"sort"
	"strings"
	"time"

	"agw/internal/sock"
	"agw/internal/store"
)

const (
	RecConfirmed   = "confirmed"   // angefordert und ausgeführt: belegt
	RecUnrequested = "unrequested" // ausgeführt, am Proxy aber nie angefordert
	RecUnexecuted  = "unexecuted"  // angefordert, Werkzeug mit Ausführung, aber keine Ausführung
	RecMismatch    = "mismatch"    // ausgeführt unter einem anderen Werkzeug als angefordert
	RecInternal    = "internal"    // angefordert; das Werkzeug läuft nicht in der Sandbox (todo, subagent, mcp_ping …)
	// Harmlose Ursachen für „angefordert, nicht ausgeführt“ (M1), getrennt von einer Umgehung:
	RecAborted  = "aborted"  // die Antwort des Modells brach ab (am Proxy ohne finish_reason); pi führt nichts davon aus
	RecRejected = "rejected" // laut Sitzung von pi abgewiesen (ungültige Argumente, Werkzeug ausgeblendet, Wächter); nicht fälschungssicher
)

type ReconciledCall struct {
	ToolCallID   string     `json:"tool_call_id"`
	State        string     `json:"state"`
	Tool         string     `json:"tool"` // angefordert, sonst ausgeführt
	ExecutedTool string     `json:"executed_tool,omitempty"`
	Requested    bool       `json:"requested"`
	Executed     bool       `json:"executed"`
	Main         bool       `json:"main"`              // Anforderung in einer Antwort der Hauptsitzung
	Session      string     `json:"session,omitempty"` // aus der Ausführung: "main" oder Lauf des Subagenten
	LLMCallID    int64      `json:"llm_call_id,omitempty"`
	ResponseID   string     `json:"response_id,omitempty"`
	Arguments    string     `json:"arguments,omitempty"` // angefordert (gekürzt)
	RequestedAt  *time.Time `json:"requested_at,omitempty"`
	StartedAt    *time.Time `json:"started_at,omitempty"`
	Ops          []string   `json:"ops"`
	// Reason: bei RecRejected die Fehlermeldung aus der Sitzung (nicht fälschungssicher).
	Reason       string  `json:"reason,omitempty"`
	ExitCode     *int    `json:"exit_code,omitempty"`
	Error        string  `json:"error,omitempty"`
	DurationMs   int64   `json:"duration_ms"`
	OutputSHA256 string  `json:"output_sha256,omitempty"` // der letzten Operation
	ExecutionIDs []int64 `json:"execution_ids"`
}

type Reconciliation struct {
	Calls      []ReconciledCall      `json:"calls"`
	Summary    map[string]int        `json:"summary"`
	Executions []store.ToolExecution `json:"executions"`
	// ExecutedTools: Werkzeuge, deren Ausführung am Socket belegt wird (für die UI, L6).
	ExecutedTools []string `json:"executed_tools"`
}

var executedTools = func() map[string]bool {
	m := map[string]bool{}
	for _, t := range sock.ExecutedTools() {
		m[t] = true
	}
	return m
}()

// Reconcile gleicht ab. Reihenfolge: nach Zeitpunkt der Anforderung, sonst der
// ersten Ausführung. rejected: Fehlermeldungen aus den Sitzungen je toolCallId
// (store.ToolRejections), nur als Hinweis für nicht ausgeführte Aufrufe.
// web/src/lib/evidence.ts bildet dasselbe in der UI nach.
func Reconcile(calls []store.LLMCall, execs []store.ToolExecution, rejected map[string]string) Reconciliation {
	byID := map[string]*ReconciledCall{}
	incomplete := map[string]bool{}
	var order []*ReconciledCall
	for _, c := range calls {
		var tcs []struct {
			ID        string `json:"id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		}
		_ = json.Unmarshal(c.ToolCalls, &tcs)
		for _, tc := range tcs {
			if tc.ID == "" || byID[tc.ID] != nil {
				continue
			}
			at := c.StartedAt
			args := tc.Arguments
			if len(args) > 500 {
				args = args[:500] + " …"
			}
			rc := &ReconciledCall{ToolCallID: tc.ID, Tool: tc.Name, Requested: true, Main: c.Main, LLMCallID: c.ID,
				ResponseID: c.ResponseID, Arguments: args, RequestedAt: &at, Ops: []string{}, ExecutionIDs: []int64{}}
			byID[tc.ID] = rc
			incomplete[tc.ID] = !c.Complete
			order = append(order, rc)
		}
	}
	for _, e := range execs {
		rc := byID[e.ToolCallID]
		if rc == nil {
			rc = &ReconciledCall{ToolCallID: e.ToolCallID, Tool: e.Tool, Ops: []string{}, ExecutionIDs: []int64{}}
			byID[e.ToolCallID] = rc
			order = append(order, rc)
		}
		if !rc.Executed {
			rc.Executed, rc.ExecutedTool, rc.Session = true, e.Tool, e.Session
			at := e.StartedAt
			rc.StartedAt = &at
		}
		if !contains(strings.Split(rc.ExecutedTool, ","), e.Tool) {
			rc.ExecutedTool = rc.ExecutedTool + "," + e.Tool
		}
		rc.Ops = append(rc.Ops, e.Op)
		rc.ExecutionIDs = append(rc.ExecutionIDs, e.ID)
		rc.DurationMs += e.DurationMs
		rc.ExitCode, rc.Error, rc.OutputSHA256 = e.ExitCode, e.Error, e.OutputSHA256
	}
	sum := map[string]int{RecConfirmed: 0, RecUnrequested: 0, RecUnexecuted: 0, RecMismatch: 0, RecInternal: 0, RecAborted: 0, RecRejected: 0}
	out := make([]ReconciledCall, 0, len(order))
	for _, rc := range order {
		switch {
		case rc.Requested && rc.Executed && rc.ExecutedTool != rc.Tool:
			rc.State = RecMismatch
		case rc.Requested && rc.Executed:
			rc.State = RecConfirmed
		case rc.Executed:
			rc.State = RecUnrequested
		case !executedTools[rc.Tool]:
			rc.State = RecInternal
		case incomplete[rc.ToolCallID]:
			rc.State = RecAborted
		case rejected[rc.ToolCallID] != "":
			rc.State, rc.Reason = RecRejected, rejected[rc.ToolCallID]
		default:
			rc.State = RecUnexecuted
		}
		sum[rc.State]++
		out = append(out, *rc)
	}
	sort.SliceStable(out, func(i, j int) bool { return firstTime(out[i]).Before(firstTime(out[j])) })
	if execs == nil {
		execs = []store.ToolExecution{}
	}
	return Reconciliation{Calls: out, Summary: sum, Executions: execs, ExecutedTools: ExecutedToolNames()}
}

// ExecutedToolNames: Werkzeuge, deren Ausführung am Socket belegt wird, sortiert.
func ExecutedToolNames() []string {
	out := sock.ExecutedTools()
	sort.Strings(out)
	return out
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func firstTime(c ReconciledCall) time.Time {
	if c.RequestedAt != nil {
		return *c.RequestedAt
	}
	if c.StartedAt != nil {
		return *c.StartedAt
	}
	return time.Time{}
}

// RecordToolExecution speichert eine am Socket ausgeführte Operation und
// meldet sie der UI (sock.ToolRecorder).
func (m *Manager) RecordToolExecution(e store.ToolExecution) {
	rec, err := m.st.AddToolExecution(context.Background(), e)
	if err != nil {
		// Ein fehlender Eintrag fiele im Abgleich als „nicht ausgeführt“ auf.
		slog.Error("Werkzeugausführung nicht gespeichert", "chat", e.ChatID, "id", e.ToolCallID, "fehler", err)
		return
	}
	if e.ChatID != "" {
		m.publish(e.ChatID, Event{Kind: "tool_execution", Data: rec})
	}
}

// ToolExecutions liefert den Abgleich eines Chats.
func (m *Manager) ToolExecutions(ctx context.Context, chatID string) (Reconciliation, error) {
	if _, err := m.st.GetChat(ctx, chatID); err != nil {
		return Reconciliation{}, err
	}
	calls, err := m.st.ListLLMCalls(ctx, chatID)
	if err != nil {
		return Reconciliation{}, err
	}
	execs, err := m.st.ListToolExecutions(ctx, chatID)
	if err != nil {
		return Reconciliation{}, err
	}
	rejected, err := m.st.ToolRejections(ctx, chatID)
	if err != nil {
		return Reconciliation{}, err
	}
	return Reconcile(calls, execs, rejected), nil
}

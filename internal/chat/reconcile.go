package chat

// Reconciliation of requested and executed tool calls (E9). Requested means:
// the LLM proxy has seen the call with its ID in the provider's response
// (llm_calls.tool_calls). Executed means: the orchestrator has executed an
// operation for this ID in the execution sandbox (tool_executions). Both
// sources lie outside the agent's reach.

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
	RecConfirmed   = "confirmed"   // requested and executed: proven
	RecUnrequested = "unrequested" // executed, but never requested at the proxy
	RecUnexecuted  = "unexecuted"  // requested, a tool with execution, but no execution
	RecMismatch    = "mismatch"    // executed under a different tool than requested
	RecInternal    = "internal"    // requested; the tool does not run in the sandbox (todo, subagent, mcp_ping …)
	// Harmless causes of "requested, not executed" (M1), kept apart from a bypass:
	RecAborted  = "aborted"  // the model's response broke off (no finish_reason at the proxy); pi executes none of it
	RecRejected = "rejected" // refused by pi according to the session (invalid arguments, tool hidden, guard); not tamper-proof
)

type ReconciledCall struct {
	ToolCallID   string     `json:"tool_call_id"`
	State        string     `json:"state"`
	Tool         string     `json:"tool"` // requested, otherwise executed
	ExecutedTool string     `json:"executed_tool,omitempty"`
	Requested    bool       `json:"requested"`
	Executed     bool       `json:"executed"`
	Main         bool       `json:"main"`              // requested in a response of the main session
	Session      string     `json:"session,omitempty"` // from the execution: "main" or the subagent's run
	LLMCallID    int64      `json:"llm_call_id,omitempty"`
	ResponseID   string     `json:"response_id,omitempty"`
	Arguments    string     `json:"arguments,omitempty"` // requested (truncated)
	RequestedAt  *time.Time `json:"requested_at,omitempty"`
	StartedAt    *time.Time `json:"started_at,omitempty"`
	Ops          []string   `json:"ops"`
	// Reason: for RecRejected the error message from the session (not tamper-proof).
	Reason       string  `json:"reason,omitempty"`
	ExitCode     *int    `json:"exit_code,omitempty"`
	Error        string  `json:"error,omitempty"`
	DurationMs   int64   `json:"duration_ms"`
	OutputSHA256 string  `json:"output_sha256,omitempty"` // of the last operation
	ExecutionIDs []int64 `json:"execution_ids"`
}

type Reconciliation struct {
	Calls      []ReconciledCall      `json:"calls"`
	Summary    map[string]int        `json:"summary"`
	Executions []store.ToolExecution `json:"executions"`
	// ExecutedTools: tools whose execution is proven at the socket (for the UI, L6).
	ExecutedTools []string `json:"executed_tools"`
}

var executedTools = func() map[string]bool {
	m := map[string]bool{}
	for _, t := range sock.ExecutedTools() {
		m[t] = true
	}
	return m
}()

// Reconcile reconciles. Order: by time of the request, otherwise of the first
// execution. rejected: error messages from the sessions per toolCallId
// (store.ToolRejections), only as a hint for calls that were not executed.
// web/src/lib/evidence.ts mirrors the same in the UI.
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

// ExecutedToolNames: tools whose execution is proven at the socket, sorted.
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

// RecordToolExecution stores an operation executed at the socket and reports
// it to the UI (sock.ToolRecorder).
func (m *Manager) RecordToolExecution(e store.ToolExecution) {
	rec, err := m.st.AddToolExecution(context.Background(), e)
	if err != nil {
		// A missing entry would show up in the reconciliation as "not executed".
		slog.Error("tool execution not stored", "chat", e.ChatID, "id", e.ToolCallID, "err", err)
		return
	}
	if e.ChatID != "" {
		m.publish(e.ChatID, Event{Kind: "tool_execution", Data: rec})
	}
}

// ToolExecutions returns a chat's reconciliation.
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

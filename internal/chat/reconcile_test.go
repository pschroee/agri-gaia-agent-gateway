package chat

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"agw/internal/llmproxy"
	"agw/internal/store"
)

func TestReconcile(t *testing.T) {
	t0 := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	code := 0
	calls := []store.LLMCall{
		{ID: 1, ResponseID: "r1", Main: true, Complete: true, StartedAt: t0, ToolCalls: json.RawMessage(`[
			{"id":"call_00_a","name":"bash","arguments":"{\"command\":\"ls\"}"},
			{"id":"call_01_b","name":"read","arguments":"{}"},
			{"id":"call_02_c","name":"todo","arguments":"{}"},
			{"id":"call_03_d","name":"edit","arguments":"{}"}]`)},
		{ID: 2, ResponseID: "r2", Complete: true, StartedAt: t0.Add(time.Second), ToolCalls: json.RawMessage(`[{"id":"call_10_e","name":"write","arguments":"{}"},{"name":"alt_ohne_id"}]`)},
		{ID: 3, Complete: true, StartedAt: t0.Add(2 * time.Second), ToolCalls: json.RawMessage(`[]`)},
		// M1: abgebrochene Antwort (kein finish_reason) und von pi abgewiesene Aufrufe
		{ID: 4, Complete: false, StartedAt: t0.Add(3 * time.Second), ToolCalls: json.RawMessage(`[{"id":"call_20_f","name":"bash","arguments":"{\"comm"}]`)},
		{ID: 5, Complete: true, FinishReason: "tool_calls", StartedAt: t0.Add(4 * time.Second), ToolCalls: json.RawMessage(`[{"id":"call_30_g","name":"grep","arguments":"{}"},{"id":"call_31_h","name":"read","arguments":"{}"}]`)},
		// L6: ein Aufruf mit Operationen unter zwei Werkzeugen, eines davon doppelt
		{ID: 6, Complete: true, StartedAt: t0.Add(5 * time.Second), ToolCalls: json.RawMessage(`[{"id":"call_40_i","name":"read","arguments":"{}"}]`)},
	}
	execs := []store.ToolExecution{
		{ID: 10, ToolCallID: "call_00_a", Tool: "bash", Op: "bash", ExitCode: &code, Session: "main", StartedAt: t0.Add(100 * time.Millisecond), DurationMs: 5, OutputSHA256: "aa"},
		{ID: 11, ToolCallID: "call_03_d", Tool: "edit", Op: "access", Session: "main", StartedAt: t0.Add(200 * time.Millisecond), DurationMs: 1},
		{ID: 12, ToolCallID: "call_03_d", Tool: "edit", Op: "read", Session: "main", StartedAt: t0.Add(210 * time.Millisecond), DurationMs: 1},
		{ID: 13, ToolCallID: "call_03_d", Tool: "edit", Op: "write", Session: "main", StartedAt: t0.Add(220 * time.Millisecond), DurationMs: 2},
		{ID: 14, ToolCallID: "call_10_e", Tool: "bash", Op: "bash", Session: "run-x", StartedAt: t0.Add(1100 * time.Millisecond)},
		{ID: 15, ToolCallID: "call_99_x", Tool: "bash", Op: "bash", Session: "main", StartedAt: t0.Add(-time.Second), Error: "aborted"},
		{ID: 16, ToolCallID: "call_40_i", Tool: "read", Op: "read", StartedAt: t0.Add(5100 * time.Millisecond)},
		{ID: 17, ToolCallID: "call_40_i", Tool: "bash", Op: "bash", StartedAt: t0.Add(5200 * time.Millisecond)},
		{ID: 18, ToolCallID: "call_40_i", Tool: "read", Op: "stat", StartedAt: t0.Add(5300 * time.Millisecond)},
	}
	rejected := map[string]string{"call_30_g": "Tool grep not found", "call_00_a": "egal, wurde ausgeführt"}
	r := Reconcile(calls, execs, rejected)
	want := map[string]string{"call_00_a": RecConfirmed, "call_01_b": RecUnexecuted, "call_02_c": RecInternal,
		"call_03_d": RecConfirmed, "call_10_e": RecMismatch, "call_99_x": RecUnrequested,
		"call_20_f": RecAborted, "call_30_g": RecRejected, "call_31_h": RecUnexecuted, "call_40_i": RecMismatch}
	if len(r.Calls) != len(want) {
		t.Fatalf("Aufrufe: %+v", r.Calls)
	}
	got := map[string]ReconciledCall{}
	for _, c := range r.Calls {
		got[c.ToolCallID] = c
		if c.State != want[c.ToolCallID] {
			t.Errorf("%s: %s, erwartet %s", c.ToolCallID, c.State, want[c.ToolCallID])
		}
	}
	if r.Calls[0].ToolCallID != "call_99_x" {
		t.Fatalf("Reihenfolge nach Zeit: %s zuerst", r.Calls[0].ToolCallID)
	}
	d := got["call_03_d"]
	if len(d.Ops) != 3 || d.Ops[2] != "write" || d.DurationMs != 4 || len(d.ExecutionIDs) != 3 || !d.Main || d.ResponseID != "r1" {
		t.Fatalf("edit: %+v", d)
	}
	if a := got["call_00_a"]; a.ExitCode == nil || a.Session != "main" || a.OutputSHA256 != "aa" || a.Arguments != `{"command":"ls"}` {
		t.Fatalf("bash: %+v", a)
	}
	if e := got["call_10_e"]; e.Tool != "write" || e.ExecutedTool != "bash" || e.Session != "run-x" || e.Main {
		t.Fatalf("abweichend: %+v", e)
	}
	if r.Summary[RecConfirmed] != 2 || r.Summary[RecUnexecuted] != 2 || r.Summary[RecUnrequested] != 1 || r.Summary[RecInternal] != 1 ||
		r.Summary[RecMismatch] != 2 || r.Summary[RecAborted] != 1 || r.Summary[RecRejected] != 1 {
		t.Fatalf("Zusammenfassung: %v", r.Summary)
	}
	if g := got["call_30_g"]; g.Reason != "Tool grep not found" {
		t.Fatalf("Hinweis aus der Sitzung: %+v", g)
	}
	if got["call_00_a"].Reason != "" {
		t.Fatal("Hinweis an einem ausgeführten Aufruf")
	}
	if i := got["call_40_i"]; i.ExecutedTool != "read,bash" {
		t.Fatalf("doppelt angehängt: %q", i.ExecutedTool)
	}
	if empty := Reconcile(nil, nil, nil); len(empty.Calls) != 0 || empty.Executions == nil || empty.Summary[RecConfirmed] != 0 {
		t.Fatalf("leer: %+v", empty)
	}
}

// Über den Manager gegen Postgres: Proxy meldet die angeforderten IDs, der
// Socket die Ausführungen; der Abgleich verbindet beide, SSE meldet jede Ausführung.
func TestToolExecutionsReconciledInManager(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, _ := e.m.Create(ctx, NewChat{})
	a := e.agent(0)
	att := e.m.Attribute(a.IP())
	events, cancel := e.m.Subscribe(c.ID)
	defer cancel()
	e.m.Record(llmproxy.Call{ChatID: c.ID, SlotID: att.SlotID, SourceIP: a.IP(), Model: "deepseek/deepseek-flash", ResponseID: "r1", Status: 200,
		StartedAt: time.Now(), Complete: true, FinishReason: "tool_calls", ToolCalls: []llmproxy.ToolCall{{ID: "call_00_a", Name: "bash", Arguments: "{}"}, {ID: "call_01_b", Name: "read", Arguments: "{}"}}})
	code := 0
	e.m.RecordToolExecution(store.ToolExecution{ChatID: c.ID, SlotID: att.SlotID, ToolCallID: "call_00_a", Tool: "bash", Op: "bash", ExitCode: &code, StartedAt: time.Now()})
	ev := waitEvent(t, events, "tool_execution", "")
	if te, ok := ev.Data.(store.ToolExecution); !ok || te.ID == 0 || te.ToolCallID != "call_00_a" {
		t.Fatalf("SSE: %#v", ev.Data)
	}
	e.m.RecordToolExecution(store.ToolExecution{ChatID: c.ID, SlotID: att.SlotID, ToolCallID: "call_zz", Tool: "bash", Op: "bash", StartedAt: time.Now()})
	r, err := e.m.ToolExecutions(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if r.Summary[RecConfirmed] != 1 || r.Summary[RecUnexecuted] != 1 || r.Summary[RecUnrequested] != 1 || len(r.Executions) != 2 {
		t.Fatalf("Abgleich: %+v", r.Summary)
	}
	if _, err := e.m.ToolExecutions(ctx, "00000000-0000-0000-0000-000000000000"); err == nil {
		t.Fatal("unbekannter Chat ohne Fehler")
	}
}

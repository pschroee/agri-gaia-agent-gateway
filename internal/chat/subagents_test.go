package chat

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

const childSession = `{"type":"session","id":"s1"}
{"type":"model_change","id":"a1"}
{"type":"message","id":"u1","message":{"role":"user","content":[{"type":"text","text":"Task: Find out the Python version."}]}}
{"type":"message","id":"m1","message":{"role":"assistant","responseId":"resp-9","content":[{"type":"thinking","thinking":"ok"},{"type":"toolCall","id":"call_1","name":"bash","arguments":{"command":"python3 --version"}}]}}
{"type":"message","id":"r1","message":{"role":"toolResult","toolCallId":"call_1","toolName":"bash","content":[{"type":"text","text":"Python 3.11.2\n"}],"isError":false}}
{"type":"message","id":"m2","message":{"role":"assistant","responseId":"resp-10","content":[{"type":"text","text":"Python 3.11.2"}]}}
broken line
`

func TestParseChildSession(t *testing.T) {
	es := parseChildSession("chat", "run1", "scout", childSession)
	var kinds []string
	for _, e := range es {
		kinds = append(kinds, e.Kind)
	}
	if strings.Join(kinds, ",") != "task,tool_call,tool_result,text" {
		t.Fatalf("kinds: %v", kinds)
	}
	call := es[1]
	var p struct{ Name, Arguments string }
	_ = json.Unmarshal(call.Payload, &p)
	var ids struct {
		ID         string `json:"id"`
		ToolCallID string `json:"tool_call_id"`
	}
	_ = json.Unmarshal(call.Payload, &ids)
	var rid struct {
		ToolCallID string `json:"tool_call_id"`
	}
	_ = json.Unmarshal(es[2].Payload, &rid)
	if ids.ID != "call_1" || rid.ToolCallID != "call_1" {
		t.Fatalf("call ID missing: call %q, result %q", ids.ID, rid.ToolCallID)
	}
	if call.ResponseID != "resp-9" || p.Name != "bash" || !strings.Contains(p.Arguments, "python3 --version") || call.Agent != "scout" || call.RunID != "run1" {
		t.Fatalf("tool call: %+v %+v", call, p)
	}
	var r struct {
		Name    string `json:"name"`
		Text    string `json:"text"`
		IsError bool   `json:"is_error"`
	}
	_ = json.Unmarshal(es[2].Payload, &r)
	if r.Name != "bash" || !strings.Contains(r.Text, "3.11.2") {
		t.Fatalf("result: %+v", r)
	}
	// Unique IDs per entry and content part
	seen := map[string]bool{}
	for _, e := range es {
		if seen[e.EntryID] {
			t.Fatalf("duplicate ID %s", e.EntryID)
		}
		seen[e.EntryID] = true
	}
}

func TestParseChildSessionTruncates(t *testing.T) {
	long := strings.Repeat("x", 20000)
	line := `{"type":"message","id":"r","message":{"role":"toolResult","toolName":"bash","content":[{"type":"text","text":"` + long + `"}]}}` + "\n"
	es := parseChildSession("c", "r", "", line)
	if len(es) != 1 || len(es[0].Payload) > 6000 {
		t.Fatalf("not truncated: %d bytes", len(es[0].Payload))
	}
}

func TestRunKeyFromPath(t *testing.T) {
	cases := map[string]string{
		"/agent/sessions/2026_x/4fe6edef-159e-41a3-aedc-47b85916de47/run-0/session.jsonl": "4fe6edef-159e-41a3-aedc-47b85916de47",
		"/agent/sessions/2026_x/4fe6edef-159e-41a3-aedc-47b85916de47/run-2/session.jsonl": "4fe6edef-159e-41a3-aedc-47b85916de47#2",
		"/etc/passwd": "",
	}
	for in, want := range cases {
		if got := runKey(in); got != want {
			t.Errorf("runKey(%q) = %q, want %q", in, got, want)
		}
	}
}

// Which runs count towards the limit of subagents at the same time.
func TestRunningSubagents(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	turn := now.Add(-time.Minute)
	runs := map[string]*subTrack{
		"bg-running":  {State: "running", Last: now.Add(-10 * time.Second)},
		"bg-Running":  {State: "Running", Last: now},
		"bg-starting": {State: "starting", Last: now},
		"bg-queued":   {State: "queued", Last: now},
		"bg-paused":   {State: "paused", Last: now},
		"bg-done":     {State: "complete", Last: now},
		"bg-failed":   {State: "failed", Last: now},
		"bg-stale":    {State: "running", Last: now.Add(-subagentQuiet - time.Second)},
		"bg-killed":   {State: "running", Last: now, Killed: true},
		"fg-working":  {LastKind: "tool_call", Last: now.Add(-5 * time.Second)},
		"fg-answered": {LastKind: "text", Last: now},
		"fg-old-turn": {LastKind: "tool_call", Last: turn.Add(-time.Second)},
	}
	got := strings.Join(runningSubagents(runs, now, turn), ",")
	if want := "bg-Running,bg-running,bg-starting,fg-working"; got != want {
		t.Fatalf("during a turn: %s, want %s", got, want)
	}
	// While the main agent is idle no foreground run can work; background runs go on.
	got = strings.Join(runningSubagents(runs, now, time.Time{}), ",")
	if want := "bg-Running,bg-running,bg-starting"; got != want {
		t.Fatalf("idle: %s, want %s", got, want)
	}
}

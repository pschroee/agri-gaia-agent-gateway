// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"encoding/json"
	"testing"
)

// X5.1–X5.5: every structured field of a platform call survives the round trip.
func TestCallRecordRoundTrip(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c, err := s.CreateChat(ctx, NewChat{Title: "x", Model: "m", Variant: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	within := true
	step := 2
	rule := json.RawMessage(`{"access":{"action":"create","resource":"dataset"}}`)
	rec := CallRecord{ChatID: c.ID, SlotID: "slot", Via: "cli", Op: "platform", Detail: "POST /datasets",
		Result: "ok 201", Session: "main", ToolCallID: "call_1", DurationMs: ptrFloat(12.5), Effect: "write",
		Decision: "allowed", Reason: "rule", Rule: rule, Body: []byte(`{"name":"x"}`), BodyTruncated: true,
		BodyInSHA256: "in", CallRef: "call_1", CallRefSource: "exec_window", CallRefCandidates: []string{"a", "b"},
		CallRefClaimed: "claimed", WithinDelegation: &within, ApprovalDecision: "user", PlanStep: &step}
	stored, err := s.AddSocketCallRecord(ctx, rec)
	if err != nil {
		t.Fatal(err)
	}
	list, err := s.ListSocketCalls(ctx, c.ID)
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v %v", list, err)
	}
	got := list[0]
	if got.ID != stored.ID || got.Decision != "allowed" || got.Reason != "rule" || string(got.Rule) != string(rule) ||
		string(got.Body) != `{"name":"x"}` || !got.BodyTruncated || got.BodyInSHA256 != "in" ||
		got.CallRef != "call_1" || got.CallRefSource != "exec_window" || len(got.CallRefCandidates) != 2 ||
		got.CallRefClaimed != "claimed" || got.WithinDelegation == nil || !*got.WithinDelegation ||
		got.ApprovalDecision != "user" || got.PlanStep == nil || *got.PlanStep != 2 {
		t.Fatalf("round trip: %+v", got)
	}
}

// X5.4: truncation of a command is marked; length and checksum belong to the untruncated command.
func TestToolExecutionTruncationRoundTrip(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c, err := s.CreateChat(ctx, NewChat{Title: "x", Model: "m", Variant: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	n := int64(100000)
	e, err := s.AddToolExecution(ctx, ToolExecution{ChatID: c.ID, SlotID: "slot", ToolCallID: "call_1", Tool: "bash",
		Op: "bash", Args: json.RawMessage(`{"command":"x"}`), ArgsTruncated: true, CommandBytes: &n, CommandSHA256: "sum"})
	if err != nil {
		t.Fatal(err)
	}
	list, err := s.ListToolExecutions(ctx, c.ID)
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v %v", list, err)
	}
	got := list[0]
	if got.ID != e.ID || !got.ArgsTruncated || got.CommandBytes == nil || *got.CommandBytes != n || got.CommandSHA256 != "sum" {
		t.Fatalf("truncation: %+v", got)
	}
}

// X5.6: the tool list is stored once per checksum.
func TestToolsetRoundTrip(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	tools := json.RawMessage(`[{"name":"bash"}]`)
	if err := s.PutToolset(ctx, "abc", tools); err != nil {
		t.Fatal(err)
	}
	if err := s.PutToolset(ctx, "abc", tools); err != nil { // idempotent
		t.Fatal(err)
	}
	got, err := s.Toolset(ctx, "abc")
	if err != nil || string(got) != string(tools) {
		t.Fatalf("toolset: %s %v", got, err)
	}
}

func ptrFloat(v float64) *float64 { return &v }

// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
)

// sameJSON compares two JSON documents semantically: jsonb normalises spacing and key order, so a byte
// comparison would fail on a value that round-tripped correctly.
func sameJSON(a, b []byte) bool {
	var va, vb any
	if json.Unmarshal(a, &va) != nil || json.Unmarshal(b, &vb) != nil {
		return false
	}
	return reflect.DeepEqual(va, vb)
}

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
		CallRefClaimed: "claimed", WithinDelegation: &within, ApprovalID: "00000000-0000-0000-0000-000000000001",
		ApprovalDecision: "user", PlanStep: &step}
	stored, err := s.AddSocketCallRecord(ctx, rec)
	if err != nil {
		t.Fatal(err)
	}
	list, err := s.ListSocketCalls(ctx, c.ID)
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v %v", list, err)
	}
	got := list[0]
	if got.ID != stored.ID {
		t.Fatalf("id: %d, stored %d", got.ID, stored.ID)
	}
	if got.Decision != "allowed" || got.Reason != "rule" {
		t.Errorf("decision/reason: %q/%q", got.Decision, got.Reason)
	}
	if !sameJSON(got.Rule, rule) {
		t.Errorf("rule: %s, want %s", got.Rule, rule)
	}
	// Body is bytea, not jsonb: it must come back byte for byte.
	if string(got.Body) != `{"name":"x"}` {
		t.Errorf("body: %q", got.Body)
	}
	if !got.BodyTruncated {
		t.Error("body_truncated lost")
	}
	if got.BodyInSHA256 != "in" {
		t.Errorf("body_in_sha256: %q", got.BodyInSHA256)
	}
	if got.CallRef != "call_1" || got.CallRefSource != "exec_window" || got.CallRefClaimed != "claimed" {
		t.Errorf("call ref: %q/%q/%q", got.CallRef, got.CallRefSource, got.CallRefClaimed)
	}
	if !reflect.DeepEqual(got.CallRefCandidates, []string{"a", "b"}) {
		t.Errorf("candidates: %v", got.CallRefCandidates)
	}
	if got.WithinDelegation == nil || !*got.WithinDelegation {
		t.Errorf("within_delegation: %v", got.WithinDelegation)
	}
	if got.ApprovalID != "00000000-0000-0000-0000-000000000001" || got.ApprovalDecision != "user" {
		t.Errorf("approval: %q/%q", got.ApprovalID, got.ApprovalDecision)
	}
	if got.PlanStep == nil || *got.PlanStep != 2 {
		t.Errorf("plan_step: %v", got.PlanStep)
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
	if err != nil {
		t.Fatal(err)
	}
	if !sameJSON(got, tools) {
		t.Fatalf("toolset: %s, want %s", got, tools)
	}
}

func ptrFloat(v float64) *float64 { return &v }

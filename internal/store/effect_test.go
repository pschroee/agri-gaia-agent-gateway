// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestEffectContext(t *testing.T) {
	if got := EffectFrom(context.Background()); got != "" {
		t.Fatalf("without effect: %q", got)
	}
	if got := EffectFrom(WithEffect(context.Background(), "irreversible")); got != "irreversible" {
		t.Fatalf("with effect: %q", got)
	}
}

func TestEffectStored(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c, _ := s.CreateChat(ctx, NewChat{Title: "x", Model: "m/m", Variant: "cli"})
	ap, err := s.CreateApproval(ctx, Approval{ChatID: c.ID, Kind: "platform_write", Via: "cli", Name: "DELETE /datasets/5", PendingKey: "p", Effect: "irreversible", ToolCallID: "t1"})
	if err != nil || ap.Effect != "irreversible" {
		t.Fatalf("CreateApproval: %+v %v", ap, err)
	}
	net, err := s.CreateApproval(ctx, Approval{ChatID: c.ID, Kind: "internet_access", Via: "cli", Name: "internet", PendingKey: "q"})
	if err != nil || net.Effect != "" {
		t.Fatalf("internet approval: %+v %v", net, err)
	}
	raw, _ := json.Marshal(net)
	if strings.Contains(string(raw), `"effect"`) {
		t.Fatalf("empty effect must not be in JSON: %s", raw)
	}
	list, err := s.ListApprovals(ctx, "", c.ID)
	if err != nil || len(list) != 2 || list[0].Effect != "irreversible" || list[1].Effect != "" {
		t.Fatalf("ListApprovals: %+v %v", list, err)
	}

	platform, err := s.AddSocketCall(ctx, SocketCall{ChatID: c.ID, SlotID: "p", Via: "cli", Op: "platform", Detail: "DELETE /datasets/5", Result: "ok", ToolCallID: "t1", Effect: "irreversible"})
	if err != nil || platform.Effect != "irreversible" {
		t.Fatalf("AddSocketCall platform: %+v %v", platform, err)
	}
	if _, err := s.AddSocketCall(ctx, SocketCall{ChatID: c.ID, SlotID: "p", Via: "cli", Op: "upload", Detail: "a.png", Result: "ok"}); err != nil {
		t.Fatal(err)
	}
	socket, err := s.ListSocketCalls(ctx, c.ID)
	if err != nil || len(socket) != 2 || socket[0].Effect != "irreversible" || socket[1].Effect != "" {
		t.Fatalf("ListSocketCalls: %+v %v", socket, err)
	}

	act, err := s.ListActivity(ctx, ActivityFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range act.Calls {
		if a.Op == "platform" {
			found = true
			if a.Effect != "irreversible" {
				t.Fatalf("activity effect: %q", a.Effect)
			}
			if a.Approval == nil || a.Approval.ID != ap.ID {
				t.Fatalf("activity approval: %+v, want %s", a.Approval, ap.ID)
			}
		}
	}
	if !found {
		t.Fatalf("platform call not in activity: %+v", act.Calls)
	}
}

func TestComputeReservation(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c, _ := s.CreateChat(ctx, NewChat{Title: "x", Model: "m/m", Variant: "cli"})
	if n, err := s.ComputeCalls(ctx, c.ID); err != nil || n != 0 {
		t.Fatalf("new chat: %d %v", n, err)
	}
	for i, want := range []bool{true, true, false} {
		ok, err := s.ReserveCompute(ctx, c.ID, 2)
		if err != nil || ok != want {
			t.Fatalf("reserve %d: %v %v, want %v", i, ok, err, want)
		}
	}
	if n, _ := s.ComputeCalls(ctx, c.ID); n != 2 {
		t.Fatalf("count after limit: %d", n)
	}
	if err := s.ReleaseCompute(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.ComputeCalls(ctx, c.ID); n != 1 {
		t.Fatalf("count after release: %d", n)
	}
	if err := s.ReleaseCompute(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.ReleaseCompute(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.ComputeCalls(ctx, c.ID); n != 0 {
		t.Fatalf("count never below zero: %d", n)
	}
	other, _ := s.CreateChat(ctx, NewChat{Title: "y", Model: "m/m", Variant: "cli"})
	if n, _ := s.ComputeCalls(ctx, other.ID); n != 0 {
		t.Fatalf("new chat has its own count: %d", n)
	}
}

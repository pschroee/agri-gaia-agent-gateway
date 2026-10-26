// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agw/internal/config"
	"agw/internal/platform"
	"agw/internal/store"
)

// The capability "plans" names the limits of an autonomous plan from the options of the manager and from the
// environment of the setup; without limits an autonomous plan is not offered (issue #218).
func TestCapabilityPlans(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, err := e.m.Create(ctx, NewChat{})
	if err != nil {
		t.Fatal(err)
	}
	if knownCapabilities[len(knownCapabilities)-1] != "plans" {
		t.Fatalf("plans belongs at the end of the list: %v", knownCapabilities)
	}
	e.m.opt.PlanMaxDuration, e.m.opt.PlanMaxLLMCost = 2*time.Hour, 3
	p := byName(e.m.Capabilities(ctx, c.ID))["plans"]
	want := "up to 10 steps; autonomous budget at most 120 minutes, 3.00 USD and 50 turns without the user; max_attempts 1-3"
	if !p.Available || p.Detail != want || p.Without == "" {
		t.Fatalf("plans: %+v", p)
	}
	e.m.opt.PlanMaxDuration = 0
	p = byName(e.m.Capabilities(ctx, c.ID))["plans"]
	if p.Available || p.Detail != "autonomous plans are not configured on this instance; interactive plans up to 10 steps" {
		t.Fatalf("plans without limits: %+v", p)
	}
	// The system note carries the same line from the environment.
	lines := strings.Join(SetupLines(Setup{Catalog: e.cat, Variant: "cli", Env: config.Env{PlanMaxDuration: time.Hour, PlanMaxLLMCost: 2}}), "\n")
	if !strings.Contains(lines, "plans: up to 10 steps; autonomous budget at most 60 minutes, 2.00 USD and 50 turns") {
		t.Fatalf("setup lines: %s", lines)
	}
	if !strings.Contains(strings.Join(SetupLines(Setup{Catalog: e.cat, Variant: "cli"}), "\n"), "plans: autonomous plans are not configured") {
		t.Fatal("no limits in the environment: the note says so")
	}
}

// A proposal with a problem of the shape (ParseProposal) and one of a step (Build) gets both in one answer.
func TestProposePlanJoinsParseAndBuild(t *testing.T) {
	e := setup(t)
	withPlanPlatform(t, e)
	ctx := store.WithSession(context.Background(), "main")
	c, _ := e.m.Create(ctx, NewChat{})
	slot := e.m.live[c.ID].slot.ID
	body := `{"title":" ","steps":[{"tool":"nonsense_tool"},{"method":"POST","path":"/datasets","bogus":1}]}`
	req, err := platform.Normalize(platform.Request{Method: "POST", Path: platform.PlanPath, Body: json.RawMessage(body)})
	if err != nil {
		t.Fatal(err)
	}
	r, err := e.m.PlatformCall(ctx, c.ID, slot, "cli", req)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "error" {
		t.Fatalf("result: %+v", r)
	}
	for _, want := range []string{"plan refused, 3 problems", "title must have 1 to 120 characters", "step 1: unknown tool \"nonsense_tool\"", "step 2: json: unknown field \"bogus\""} {
		if !strings.Contains(r.Message, want) {
			t.Errorf("answer misses %q:\n%s", want, r.Message)
		}
	}
	// One problem reads as before.
	req, _ = platform.Normalize(platform.Request{Method: "POST", Path: platform.PlanPath, Body: json.RawMessage(`{"title":"t","steps":[{"method":"GET","path":"/datasets"}]}`)})
	r, _ = e.m.PlatformCall(ctx, c.ID, slot, "cli", req)
	if r.Message != "step 1: reading calls do not belong in a plan" {
		t.Fatalf("single problem: %q", r.Message)
	}
}

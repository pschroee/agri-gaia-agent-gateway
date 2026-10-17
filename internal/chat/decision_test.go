// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"encoding/json"
	"testing"

	"agw/internal/platform"
)

// X5.1: every exit of PlatformCall records its decision as a field, with a stable reason code.
func TestDecisionPerExit(t *testing.T) {
	t.Run("not configured", func(t *testing.T) {
		e := setup(t)
		ctx := context.Background()
		c, _ := e.m.Create(ctx, NewChat{})
		res, err := e.m.PlatformCall(ctx, c.ID, e.m.live[c.ID].slot.ID, "cli", platform.Request{Method: "GET", Path: "/datasets"})
		if err != nil || res.Decision != platform.DecisionError || res.Reason != "not_configured" {
			t.Fatalf("res: %+v %v", res, err)
		}
	})

	t.Run("blocked path", func(t *testing.T) {
		e := setup(t)
		withPlatform(t, e)
		ctx := context.Background()
		c, _ := e.m.Create(ctx, NewChat{})
		res, _ := e.m.PlatformCall(ctx, c.ID, e.m.live[c.ID].slot.ID, "cli", platform.Request{Method: "GET", Path: "/network"})
		if res.Decision != platform.DecisionDeniedFixed || res.Reason != "blocked_path" {
			t.Fatalf("res: %+v", res)
		}
	})

	t.Run("field rule", func(t *testing.T) {
		e := setup(t)
		withPlatform(t, e)
		ctx := context.Background()
		c, _ := e.m.Create(ctx, NewChat{})
		res, _ := e.m.PlatformCall(ctx, c.ID, e.m.live[c.ID].slot.ID, "api", platform.Request{Method: "PATCH", Path: "/datasets/5",
			Body: json.RawMessage(`{"bucket_name":"x"}`)})
		if res.Decision != platform.DecisionDeniedFixed || res.Reason != "field_rule" || len(res.Rule) == 0 {
			t.Fatalf("res: %+v", res)
		}
	})

	t.Run("denied delegation", func(t *testing.T) {
		e := setup(t)
		withPlatform(t, e)
		allowExperiment(e)
		c := createExperiment(t, e, "service", NewChat{Delegation: json.RawMessage(`{"rules":[{"action":"read","resource":"dataset","ids":["*"]}]}`)})
		res, _ := e.m.PlatformCall(context.Background(), c.ID, e.m.live[c.ID].slot.ID, "api",
			platform.Request{Method: "POST", Path: "/datasets", Body: json.RawMessage(`{"name":"x"}`)})
		if res.Decision != platform.DecisionDeniedDelegation || res.WithinDelegation == nil || *res.WithinDelegation {
			t.Fatalf("res: %+v", res)
		}
	})

	t.Run("allowed read", func(t *testing.T) {
		e := setup(t)
		withPlatform(t, e)
		allowExperiment(e)
		c := createExperiment(t, e, "service", NewChat{})
		res, _ := e.m.PlatformCall(context.Background(), c.ID, e.m.live[c.ID].slot.ID, "api",
			platform.Request{Method: "GET", Path: "/datasets"})
		if res.Decision != platform.DecisionAllowed || res.WithinDelegation == nil || !*res.WithinDelegation {
			t.Fatalf("res: %+v", res)
		}
	})

	t.Run("approved automatically", func(t *testing.T) {
		e := setup(t)
		withPlatform(t, e)
		allowExperiment(e)
		c := createExperiment(t, e, "service", NewChat{Delegation: json.RawMessage(`{"rules":[{"action":"create","resource":"dataset","ids":[]}]}`)})
		res, _ := e.m.PlatformCall(context.Background(), c.ID, e.m.live[c.ID].slot.ID, "api",
			platform.Request{Method: "POST", Path: "/datasets", Body: json.RawMessage(`{"name":"x","metadata":"{}"}`)})
		if res.Decision != platform.DecisionApprovedAuto || res.ApprovalDecision != "experiment" || res.ApprovalID == "" {
			t.Fatalf("res: %+v", res)
		}
	})
}

// X5.5: within_delegation is computed in the stage without protective measure too, where the call is only logged.
func TestWithinDelegationInProtectionNone(t *testing.T) {
	e := setup(t)
	withPlatform(t, e)
	allowExperiment(e)
	c := createExperiment(t, e, "none", NewChat{Delegation: json.RawMessage(`{"rules":[{"action":"read","resource":"dataset","ids":["*"]}]}`)})
	res, _ := e.m.PlatformCall(context.Background(), c.ID, e.m.live[c.ID].slot.ID, "api",
		platform.Request{Method: "POST", Path: "/datasets", Body: json.RawMessage(`{"name":"x","metadata":"{}"}`)})
	if res.Decision != platform.DecisionAllowedViolation || res.WithinDelegation == nil || *res.WithinDelegation {
		t.Fatalf("res: %+v", res)
	}
}

// X5.2: the recorded body is the one after the rebuild; the checksum still describes what the agent sent.
func TestBodyRecordedAfterRebuild(t *testing.T) {
	e := setup(t)
	withPlatform(t, e)
	allowExperiment(e)
	c := createExperiment(t, e, "service", NewChat{Delegation: json.RawMessage(`{"rules":[{"action":"create","resource":"dataset","ids":[]}]}`)})
	raw := json.RawMessage("{ \"name\" : \"x\", \"metadata\" : \"{}\" }")
	res, _ := e.m.PlatformCall(context.Background(), c.ID, e.m.live[c.ID].slot.ID, "api",
		platform.Request{Method: "POST", Path: "/datasets", Body: raw})
	if string(res.SentBody) != `{"name":"x","metadata":"{}"}` || res.BodyInSHA256 == "" || res.BodyTruncated {
		t.Fatalf("body: %q in=%q truncated=%v", res.SentBody, res.BodyInSHA256, res.BodyTruncated)
	}
}

// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"testing"
)

// #263: rows the checks wrote as "HTTP 2xx" before the format was aligned count as ok; "HTTP 4xx/5xx" and
// the new "ok 200" are classified as they should be.
func TestOutcomeLegacyHTTPResult(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	owner := "sub-legacy"
	c, err := s.CreateChat(ctx, NewChat{Title: "legacy", Model: "p/m", Variant: "cli", Owner: owner})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"GET /models/58":                "ok",
		"GET /container-deployments/30": "ok",
		"GET /edge-devices":             "error",
		"GET /new-format":               "ok",
		"GET /error-text":               "error",
	}
	for detail, result := range map[string]string{
		"GET /models/58":                "HTTP 200",
		"GET /container-deployments/30": "HTTP 204",
		"GET /edge-devices":             "HTTP 404",
		"GET /new-format":               "ok 200",
		"GET /error-text":               "error: boom",
	} {
		if _, err := s.AddSocketCall(ctx, SocketCall{ChatID: c.ID, SlotID: "p", Via: "mcp", Op: "platform", Detail: detail, Result: result}); err != nil {
			t.Fatal(err)
		}
	}
	p, err := s.ListActivity(ctx, ActivityFilter{Owner: &owner})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Calls) != len(want) {
		t.Fatalf("%d calls, want %d", len(p.Calls), len(want))
	}
	for _, call := range p.Calls {
		if want[call.Detail] != call.Outcome {
			t.Errorf("%s: outcome %q, want %q", call.Detail, call.Outcome, want[call.Detail])
		}
	}
}

// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// Request sizes are stored per call; calls without a size (older rows) read back as nil (issue #69).
func TestLLMCallRequestBytes(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c, _ := s.CreateChat(ctx, NewChat{Title: "x", Model: "m/m", Variant: "cli", MaxSubagents: 2})
	n := int64(1234)
	now := time.Now()
	_, err := s.AddLLMCall(ctx, LLMCall{ChatID: c.ID, SlotID: "p", SourceIP: "10.0.0.2", Model: "m/m", ResponseID: "r-a", Status: 200, StartedAt: now, RequestBytes: &n})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.AddLLMCall(ctx, LLMCall{ChatID: c.ID, SlotID: "p", SourceIP: "10.0.0.2", Model: "m/m", ResponseID: "r-b", Status: 200, StartedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	calls, err := s.ListLLMCalls(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || calls[0].RequestBytes == nil || *calls[0].RequestBytes != 1234 || calls[1].RequestBytes != nil {
		t.Fatalf("request bytes: %+v", calls)
	}
	raw, _ := json.Marshal(second)
	if strings.Contains(string(raw), `"request_bytes"`) {
		t.Fatalf("NULL must be left out of the JSON: %s", raw)
	}
}

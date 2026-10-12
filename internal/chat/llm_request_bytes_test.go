// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"testing"
	"time"

	"agw/internal/llmproxy"
)

func TestRequestBytesHelper(t *testing.T) {
	if got := requestBytes(0); got != nil {
		t.Fatalf("0 must be stored as NULL, got %d", *got)
	}
	if got := requestBytes(5); got == nil || *got != 5 {
		t.Fatalf("5 must be kept, got %v", got)
	}
}

// The measured request size reaches the store with the model call (issue #69).
func TestRecordStoresRequestBytes(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, _ := e.m.Create(ctx, NewChat{})
	e.m.Record(llmproxy.Call{ChatID: c.ID, SlotID: "p", SourceIP: "10.0.0.2", Model: "p/m", ResponseID: "r-69", Status: 200,
		StartedAt: time.Now(), RequestBytes: 4321})
	calls, err := e.st.ListLLMCalls(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0].RequestBytes == nil || *calls[0].RequestBytes != 4321 {
		t.Fatalf("stored calls: %+v", calls)
	}
}

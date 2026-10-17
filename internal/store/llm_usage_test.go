// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"testing"
	"time"
)

// X3.2/X3.3: the usage of a run is the sum of the cost column and of input + output without the cache reads,
// over the chat's calls only (subagents included, other chats not).
func TestChatLLMUsageSums(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c, err := s.CreateChat(ctx, NewChat{Title: "Run", Model: "deepseek/deepseek-flash", Variant: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.CreateChat(ctx, NewChat{Title: "Other", Model: "deepseek/deepseek-flash", Variant: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for _, call := range []LLMCall{
		{ChatID: c.ID, SlotID: "s", SourceIP: "10.0.0.2", Model: "p/m", Status: 200, Input: 100, Output: 50, CacheRead: 900, Cost: 0.10, StartedAt: now},
		{ChatID: c.ID, SlotID: "s", SourceIP: "10.0.0.2", Model: "p/m", Status: 200, Input: 10, Output: 5, Cost: 0.25, StartedAt: now},
		{ChatID: other.ID, SlotID: "s", SourceIP: "10.0.0.2", Model: "p/m", Status: 200, Input: 1000, Output: 1000, Cost: 9, StartedAt: now},
	} {
		if _, err := s.AddLLMCall(ctx, call); err != nil {
			t.Fatal(err)
		}
	}
	cost, tokens, err := s.ChatLLMUsage(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cost < 0.3499 || cost > 0.3501 || tokens != 165 {
		t.Fatalf("usage: cost=%v tokens=%d", cost, tokens)
	}
}

// X3.6/X3.7: the values the proxy sent and the model the provider reported are stored and read back; a call
// without them reads back as NULL / empty.
func TestLLMCallSamplingAndReportedModel(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c, err := s.CreateChat(ctx, NewChat{Title: "Run", Model: "deepseek/deepseek-flash", Variant: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	temp, seed := 0.0, int64(7)
	if _, err := s.AddLLMCall(ctx, LLMCall{ChatID: c.ID, SlotID: "s", SourceIP: "1", Model: "p/m", Status: 200, StartedAt: time.Now(),
		Temperature: &temp, Seed: &seed, ModelReported: "deepseek-flash fp_1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddLLMCall(ctx, LLMCall{ChatID: c.ID, SlotID: "s", SourceIP: "1", Model: "p/m", Status: 200, StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	calls, err := s.ListLLMCalls(ctx, c.ID)
	if err != nil || len(calls) != 2 {
		t.Fatalf("list: %+v %v", calls, err)
	}
	first := calls[0]
	if first.Temperature == nil || *first.Temperature != 0 || first.Seed == nil || *first.Seed != 7 || first.ModelReported != "deepseek-flash fp_1" {
		t.Fatalf("sampling and reported model: %+v", first)
	}
	second := calls[1]
	if second.Temperature != nil || second.Seed != nil || second.ModelReported != "" {
		t.Fatalf("a call without sampling must read back NULL: %+v", second)
	}
}

// X3.5: the limit that ends a run is stored once in the chat's experiment profile; a second write keeps the
// first reason, and a product chat is left alone.
func TestSetExperimentLimitOnce(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	run, err := s.CreateChat(ctx, NewChat{Title: "Run", Model: "deepseek/deepseek-flash", Variant: "exp:cli",
		Experiment: &Experiment{Protection: "none", Cell: "mcp", Run: "r1", Seed: 7}})
	if err != nil {
		t.Fatal(err)
	}
	product, err := s.CreateChat(ctx, NewChat{Title: "Product", Model: "deepseek/deepseek-flash", Variant: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.SetExperimentLimit(ctx, run.ID, "cost"); err != nil || !ok {
		t.Fatalf("first limit: ok=%v err=%v", ok, err)
	}
	if ok, err := s.SetExperimentLimit(ctx, run.ID, "tokens"); err != nil || ok {
		t.Fatalf("second limit must not replace the first: ok=%v err=%v", ok, err)
	}
	got, err := s.GetChat(ctx, run.ID)
	if err != nil || got.Experiment == nil || got.Experiment.Limit != "cost" {
		t.Fatalf("stored limit: %+v %v", got.Experiment, err)
	}
	if ok, err := s.SetExperimentLimit(ctx, product.ID, "duration"); err != nil || ok {
		t.Fatalf("product chat: ok=%v err=%v", ok, err)
	}
}

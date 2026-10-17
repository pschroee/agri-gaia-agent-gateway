// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"strings"
	"testing"
	"time"

	"agw/internal/store"
)

// addRunCall stores a model call of an experiment run, as the proxy would record it.
func addRunCall(t *testing.T, e *env, chatID string, cost float64, tokens int64) {
	t.Helper()
	_, err := e.st.AddLLMCall(context.Background(), store.LLMCall{ChatID: chatID, SlotID: "s", SourceIP: "10.0.0.2",
		Model: "deepseek/deepseek-flash", Status: 200, Input: tokens, Output: 0, Cost: cost, StartedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
}

// X3.2: the cost of the run's calls ends the run at the limit, stores the reason at the chat and refuses the
// next call with the reason in the message.
func TestLimitCost(t *testing.T) {
	e := setup(t)
	allowExperiment(e)
	e.m.opt.ExpMaxCostUSD, e.m.opt.ExpMaxTokens, e.m.opt.ExpMaxDuration = 0.50, 0, 0
	c := createExperiment(t, e, "none", NewChat{})

	addRunCall(t, e, c.ID, 0.20, 1000)
	if err := e.m.chatBudget(c.ID); err != nil {
		t.Fatalf("under the cost limit: %v", err)
	}
	addRunCall(t, e, c.ID, 0.30, 1000)
	err := e.m.chatBudget(c.ID)
	if err == nil || !strings.Contains(err.Error(), "experiment run limit reached: cost") {
		t.Fatalf("over the cost limit: %v", err)
	}
	got, _ := e.st.GetChat(context.Background(), c.ID)
	if got.Experiment == nil || got.Experiment.Limit != ExpLimitCost {
		t.Fatalf("stored limit: %+v", got.Experiment)
	}
}

// X3.3: the token limit counts input + output (without cache reads) and ends the run just like the cost limit.
func TestLimitTokens(t *testing.T) {
	e := setup(t)
	allowExperiment(e)
	e.m.opt.ExpMaxCostUSD, e.m.opt.ExpMaxTokens, e.m.opt.ExpMaxDuration = 0, 1000, 0
	c := createExperiment(t, e, "none", NewChat{})

	addRunCall(t, e, c.ID, 0, 900)
	if err := e.m.chatBudget(c.ID); err != nil {
		t.Fatalf("under the token limit: %v", err)
	}
	addRunCall(t, e, c.ID, 0, 100)
	if err := e.m.chatBudget(c.ID); err == nil || !strings.Contains(err.Error(), "experiment run limit reached: tokens") {
		t.Fatalf("at the token limit: %v", err)
	}
}

// X3.4: the run's duration counts from the chat's creation.
func TestLimitDuration(t *testing.T) {
	e := setup(t)
	allowExperiment(e)
	e.m.opt.ExpMaxCostUSD, e.m.opt.ExpMaxTokens = 0, 0
	e.m.opt.ExpMaxDuration = time.Microsecond
	c := createExperiment(t, e, "none", NewChat{})
	time.Sleep(2 * time.Millisecond)

	if err := e.m.chatBudget(c.ID); err == nil || !strings.Contains(err.Error(), "experiment run limit reached: duration") {
		t.Fatalf("over the duration limit: %v", err)
	}
	got, _ := e.st.GetChat(context.Background(), c.ID)
	if got.Experiment == nil || got.Experiment.Limit != ExpLimitDuration {
		t.Fatalf("stored limit: %+v", got.Experiment)
	}
}

// The limits apply to experiment chats only; a product chat is still held to the plan budget and nothing else.
func TestLimitNotForNormalChats(t *testing.T) {
	e := setup(t)
	e.m.opt.ExpMaxCostUSD, e.m.opt.ExpMaxTokens = 0.01, 10
	e.m.opt.ExpMaxDuration = time.Microsecond
	c, err := e.m.Create(context.Background(), NewChat{})
	if err != nil {
		t.Fatal(err)
	}
	addRunCall(t, e, c.ID, 5, 10000)
	if err := e.m.chatBudget(c.ID); err != nil {
		t.Fatalf("a product chat must not be held to the run limits: %v", err)
	}
	if err := e.m.experimentLimit(c.ID); err != nil {
		t.Fatalf("experimentLimit on a product chat: %v", err)
	}
}

// X3.5: the first reason that ends a run stays; a later call reports the stored one even after the limit that
// produced it was turned off.
func TestLimitSetsEndReason(t *testing.T) {
	e := setup(t)
	allowExperiment(e)
	e.m.opt.ExpMaxCostUSD, e.m.opt.ExpMaxDuration = 0, 0
	e.m.opt.ExpMaxTokens = 100
	c := createExperiment(t, e, "none", NewChat{})
	addRunCall(t, e, c.ID, 0, 200)

	if err := e.m.chatBudget(c.ID); err == nil || !strings.Contains(err.Error(), ": tokens") {
		t.Fatalf("first hit: %v", err)
	}
	got, _ := e.st.GetChat(context.Background(), c.ID)
	if got.Experiment == nil || got.Experiment.Limit != ExpLimitTokens {
		t.Fatalf("stored limit: %+v", got.Experiment)
	}
	e.m.opt.ExpMaxTokens = 0
	if err := e.m.chatBudget(c.ID); err == nil || !strings.Contains(err.Error(), ": tokens") {
		t.Fatalf("the stored reason must stay: %v", err)
	}
}

// X3.1/X3.6: at the proxy an experiment chat is attributed with its model and seed; a product chat is not.
func TestAttributeExperimentRun(t *testing.T) {
	e := setup(t)
	allowExperiment(e)
	e.m.opt.ExpTemperature = 0.3
	c := createExperiment(t, e, "none", NewChat{})
	product, err := e.m.Create(context.Background(), NewChat{})
	if err != nil {
		t.Fatal(err)
	}
	ipOf := func(chatID string) string {
		e.m.mu.Lock()
		defer e.m.mu.Unlock()
		if l := e.m.live[chatID]; l != nil {
			return l.ip
		}
		return ""
	}
	a := e.m.Attribute(ipOf(c.ID))
	if a.ChatID != c.ID || a.Model != c.Model || !a.Experiment || a.Seed != 7 || a.Temperature != 0.3 {
		t.Fatalf("experiment attribution: %+v", a)
	}
	p := e.m.Attribute(ipOf(product.ID))
	if p.ChatID != product.ID || p.Model != "" || p.Experiment {
		t.Fatalf("product attribution: %+v", p)
	}
}

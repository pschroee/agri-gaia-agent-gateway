// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"agw/internal/agwclient"
	"agw/internal/exp"
)

func endOf(msg string) (string, string) {
	var det agwclient.ChatDetail
	det.Messages = append(det.Messages, agwclient.StoredMessage{Role: "assistant", Message: []byte(msg)})
	return expEndReason(det)
}

// BM9: a failure of the model provider is marked in the reason, so the evaluation does not book it as the agent's.
func TestEndReasonMarksProviderError(t *testing.T) {
	st, r := endOf(`{"stopReason":"error","errorMessage":"503: {\"message\":\"Service is too busy\"}"}`)
	if st != "error" || !strings.HasPrefix(r, "provider_error: 503") {
		t.Fatalf("%s %q", st, r)
	}
	st, r = endOf(`{"stopReason":"error","errorMessage":"tool failed"}`)
	if st != "error" || strings.HasPrefix(r, "provider_error") {
		t.Fatalf("%s %q", st, r)
	}
}

func TestEndReasonEmptyAnswerIsProviderError(t *testing.T) {
	st, r := endOf(`{"stopReason":"stop","content":[],"usage":{"output":1}}`)
	if st != "error" || !strings.HasPrefix(r, "provider_error: empty") {
		t.Fatalf("%s %q", st, r)
	}
	if st, _ := endOf(`{"stopReason":"stop","content":[{"type":"text","text":"done"}],"usage":{"output":5}}`); st != "done" {
		t.Fatalf("a real answer: %s", st)
	}
}

// Issue #279: a 408 (request timeout of a proxy in front of a slow model) is a provider error, not the model's.
func TestEndReason408(t *testing.T) {
	st, r := endOf(`{"stopReason":"error","errorMessage":"408 Request Timeout"}`)
	if st != "error" || !strings.HasPrefix(r, "provider_error: 408") {
		t.Fatalf("%s %q", st, r)
	}
	// Text that merely contains 408 is not one.
	st, r = endOf(`{"stopReason":"error","errorMessage":"read 408 bytes failed"}`)
	if st != "error" || strings.HasPrefix(r, "provider_error") {
		t.Fatalf("%s %q", st, r)
	}
}

// Issue #279: an error end after a response that was answered with 200 but never came to its end is a cut
// response stream, which is the provider side's and not the model's, unless a run limit caused the end.
func TestEndReasonStreamCut(t *testing.T) {
	cut := []agwclient.LLMCall{{Status: 200, Complete: true}, {Status: 200, Complete: false}}
	st, r := streamCutReason("error", "terminated", cut, false)
	if st != "error" || r != "provider_error: response stream ended early" {
		t.Fatalf("cut stream: %s %q", st, r)
	}
	for name, c := range map[string]struct {
		status, reason string
		calls          []agwclient.LLMCall
		limited        bool
	}{
		"complete last call": {"error", "tool failed", []agwclient.LLMCall{{Status: 200, Complete: true}}, false},
		"last call not 200":  {"error", "tool failed", []agwclient.LLMCall{{Status: 502, Complete: false}}, false},
		"a limit ended it":   {"error", "aborted by limit", cut, true},
		"no calls":           {"error", "tool failed", nil, false},
		"already provider":   {"error", "provider_error: 503", cut, false},
		"not an error":       {"done", "", cut, false},
	} {
		if st, r := streamCutReason(c.status, c.reason, c.calls, c.limited); st != c.status || r != c.reason {
			t.Errorf("%s: %s %q", name, st, r)
		}
	}
}

// The runner asks the gateway for the recorded calls when an error end matches no provider error.
func TestRunnerEndReasonUsesRecordedCalls(t *testing.T) {
	api := &fakeExpAPI{llmCalls: map[string][]agwclient.LLMCall{"c1": {{Status: 200, Complete: false}}}}
	det := agwclient.ChatDetail{Messages: []agwclient.StoredMessage{{Role: "assistant", Message: []byte(`{"stopReason":"error","errorMessage":"terminated"}`)}}}
	r := &expRunner{api: api}
	st, reason := r.endReason(context.Background(), "c1", det)
	if st != "error" || reason != "provider_error: response stream ended early" {
		t.Fatalf("%s %q", st, reason)
	}
}

// Under token_basis "output" the runner's own watch counts output tokens, like the gateway.
func TestRunnerLimitCountsByBasis(t *testing.T) {
	c := agwclient.Chat{Tokens: agwclient.Tokens{Input: 900000, Output: 700}}
	lim := exp.Limits{MaxTokens: 1000, TokenBasis: "output"}
	if kind, _, _ := expOverLimit(lim, c, time.Now(), time.Now(), time.Now()); kind != "" {
		t.Fatalf("output 700 of 1000: %q", kind)
	}
	lim.TokenBasis = ""
	if kind, _, _ := expOverLimit(lim, c, time.Now(), time.Now(), time.Now()); kind != "tokens" {
		t.Fatalf("input + output over 1000 on the default basis: %q", kind)
	}
	if got := planTokensUsed(exp.Limits{TokenBasis: "output"}, agwclient.ExpUsage{TokensTotal: 9, OutputTokensTotal: 4}); got != 4 {
		t.Fatalf("plan basis output: %d", got)
	}
	if got := planTokensUsed(exp.Limits{}, agwclient.ExpUsage{TokensTotal: 9, OutputTokensTotal: 4}); got != 9 {
		t.Fatalf("plan basis default: %d", got)
	}
	if costLimitText(0) != "no cost limit" || costLimitText(0.5) != "0.5 USD (at list price)" {
		t.Fatal("costLimitText")
	}
}

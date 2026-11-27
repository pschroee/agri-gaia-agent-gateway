// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package main

import (
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

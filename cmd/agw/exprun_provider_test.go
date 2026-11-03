// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package main

import (
	"strings"
	"testing"

	"agw/internal/agwclient"
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

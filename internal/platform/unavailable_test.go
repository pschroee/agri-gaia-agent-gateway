// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

import (
	"strings"
	"testing"
)

// The status unavailable has its own rendering: the agent should work around a missing capability
// instead of repeating the call (G1.13).
func TestUnavailableStatusText(t *testing.T) {
	if StatusUnavailable != "unavailable" {
		t.Fatalf("status %q", StatusUnavailable)
	}
	msg := UnavailableText("notebook", "the notebook server is not configured on this instance", "run the code in your own sandbox")
	want := "notebook is not available in this setup: the notebook server is not configured on this instance. " +
		"run the code in your own sandbox. capabilities shows what this setup offers."
	if msg != want {
		t.Fatalf("message:\n got %q\nwant %q", msg, want)
	}
	res := Result{Status: StatusUnavailable, Message: msg}
	text := res.Text()
	if !strings.HasPrefix(text, "unavailable: notebook is not available in this setup:") {
		t.Errorf("text: %q", text)
	}
	if strings.Contains(text, "HTTP ") {
		t.Errorf("an unavailable answer names no HTTP status: %q", text)
	}
	// It is not confused with the other statuses.
	for _, other := range []string{"ok", "error", "denied", "rejected"} {
		if strings.HasPrefix(Result{Status: other, Message: msg}.Text(), "unavailable:") {
			t.Errorf("%s is rendered as unavailable", other)
		}
	}
}

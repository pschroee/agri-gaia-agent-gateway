// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

import (
	"strings"
	"testing"
)

// A-E2: what the gateway adds to an answer for the agent is part of the text of every binding (the CLI
// and MCP render Text); the live run of 16.10.2026 showed the agent polling the platform itself because
// nothing told it that the gateway observes the job.
func TestResultTextShowsTheGatewayNote(t *testing.T) {
	r := Result{Status: "ok", HTTPStatus: 202, Location: "/tasks/38", Body: `{"id":38}`,
		Note: "The gateway observes this platform job (job w1) for step 1 and wakes you when it ends; do not poll the job yourself — end your turn."}
	text := r.Text()
	for _, want := range []string{"HTTP 202", "Location: /tasks/38", `{"id":38}`, "wakes you when it ends", "end your turn"} {
		if !strings.Contains(text, want) {
			t.Errorf("the text misses %q:\n%s", want, text)
		}
	}
	// Without a note the text stays what it was.
	plain := Result{Status: "ok", HTTPStatus: 202, Location: "/tasks/38", Body: `{}`}
	if got := plain.Text(); got != "HTTP 202 · Location: /tasks/38\n{}" {
		t.Errorf("a result without a note: %q", got)
	}
}

// A-E2 (D2.8): a call that waits for the user's decision reports the sentence the gateway wrote, instead
// of rendering as "HTTP 0" — otherwise the agent never learns that it may end its turn.
func TestResultTextShowsAParkedApproval(t *testing.T) {
	r := Result{Status: StatusPendingUser, Message: "waiting for the user's approval of step 1; end your turn, you will be woken when the user decides"}
	text := r.Text()
	if !strings.Contains(text, "waiting for the user") || !strings.Contains(text, "end your turn") {
		t.Fatalf("the message does not reach the agent: %q", text)
	}
	if strings.Contains(text, "HTTP 0") {
		t.Fatalf("the answer renders as an empty HTTP status: %q", text)
	}
}

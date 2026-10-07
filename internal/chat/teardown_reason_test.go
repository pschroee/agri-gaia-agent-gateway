// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"testing"
	"time"

	"agw/internal/pool"
)

// Issue #55: a chat that goes idle returns its slot with the reason chat_suspended, which the
// worker logs with "slot torn down".
func TestSuspendTeardownReason(t *testing.T) {
	e := setup(t)
	idleChat(t, e)
	a := e.agent(0)
	deadline := time.Now().Add(5 * time.Second)
	for {
		a.mu.Lock()
		r := a.reason
		a.mu.Unlock()
		if r != "" {
			if r != pool.ReasonSuspended {
				t.Fatalf("reason %q, want %q", r, pool.ReasonSuspended)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("slot not torn down")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

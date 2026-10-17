// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package sock

import (
	"net/http/httptest"
	"testing"

	"agw/internal/store"
)

// X5.3: only pi's socket trusts the extension's own header; the execution sandbox can forge it, and the forged value
// stays the claimed one.
func TestRefFromPiSocketTrusted(t *testing.T) {
	h := newHandler("p-test", &fakeBackend{chat: "chat-1"}, 1<<20)
	h.trustedRef = true
	r := httptest.NewRequest("POST", "http://agw/platform-api/datasets", nil)
	r.Header.Set("X-Agw-Pi-Tool-Call", "call_1")
	r.Header.Set("X-Agw-Tool-Call", "call_claim")
	ref, ok := store.CallRefFrom(h.callerCtx(r))
	if !ok || ref.Ref != "call_1" || ref.Source != "pi" || ref.Claimed != "call_claim" {
		t.Fatalf("ref: %+v %v", ref, ok)
	}
}

func TestRefHeaderOnExecSocketIgnored(t *testing.T) {
	h := newHandler("p-test", &fakeBackend{chat: "chat-1"}, 1<<20)
	r := httptest.NewRequest("POST", "http://agw/platform-api/datasets", nil)
	r.Header.Set("X-Agw-Pi-Tool-Call", "forged")
	r.Header.Set("X-Agw-Tool-Call", "claimed")
	ref, _ := store.CallRefFrom(h.callerCtx(r))
	if ref.Ref != "" || ref.Source != "" || ref.Claimed != "claimed" {
		t.Fatalf("ref: %+v", ref)
	}
}

// X5.3: the execution window binds a CLI call to the one running foreground command.
func TestRefExecWindowSingle(t *testing.T) {
	h := newHandler("p-test", &fakeBackend{chat: "chat-1"}, 1<<20)
	h.fg = NewForeground()
	h.fg.add("call_1", "chat-1", func() {})
	ref, _ := store.CallRefFrom(h.withExecRef(h.callerCtx(httptest.NewRequest("POST", "/", nil)), "chat-1"))
	if ref.Ref != "call_1" || ref.Source != "exec_window" {
		t.Fatalf("ref: %+v", ref)
	}
}

func TestRefExecWindowAmbiguous(t *testing.T) {
	h := newHandler("p-test", &fakeBackend{chat: "chat-1"}, 1<<20)
	h.fg = NewForeground()
	h.fg.add("call_1", "chat-1", func() {})
	h.fg.add("call_2", "chat-1", func() {})
	ref, _ := store.CallRefFrom(h.withExecRef(h.callerCtx(httptest.NewRequest("POST", "/", nil)), "chat-1"))
	if ref.Ref != "" || ref.Source != "exec_ambiguous" || len(ref.Candidates) != 2 {
		t.Fatalf("ref: %+v", ref)
	}
}

// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package sock

import (
	"net/url"
	"testing"
)

// #198: the gateway's note reaches the REST binding as a header, URL-encoded (umlauts, line breaks).
func TestRESTNoteHeader(t *testing.T) {
	note := "The gateway observes this job.\nEnd your turn — Bräuche gelten."
	b := &platformBackend{fakeBackend: fakeBackend{chat: "chat-n", decide: "approved"}, note: note}
	c := start(t, b)
	code, _, hdr := apiCall(t, c, "GET", "/platform-api/datasets", "", "", nil)
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	raw := hdr.Get("X-Agw-Note")
	if raw == "" {
		t.Fatal("no X-Agw-Note header")
	}
	if got, err := url.PathUnescape(raw); err != nil || got != note {
		t.Fatalf("decoded %q (%v), want %q", got, err, note)
	}
	b.note = ""
	if _, _, hdr = apiCall(t, c, "GET", "/platform-api/datasets", "", "", nil); hdr.Get("X-Agw-Note") != "" {
		t.Error("an empty note must send no header")
	}
}

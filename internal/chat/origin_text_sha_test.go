// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"agw/internal/store"
)

// The user part of a message carries the SHA-256 of the user's own text, without the notes in the same message (BM3).
func TestComposeMessageUserTextSHA(t *testing.T) {
	note := systemNote{Type: store.NoteRightsInstruction, Summary: "rights"}
	c := composeMessage([]store.QueueEntry{{Kind: store.QueueUser, ID: "q1", Text: "  count the cars  "}}, &note)
	sum := sha256.Sum256([]byte("count the cars"))
	var got string
	for _, s := range c.Sources {
		if s.Kind == store.QueueUser {
			got = s.TextSHA256
		} else if s.TextSHA256 != "" {
			t.Errorf("note part with a hash: %+v", s)
		}
	}
	if got != hex.EncodeToString(sum[:]) {
		t.Fatalf("hash %q, sources %+v", got, c.Sources)
	}
}

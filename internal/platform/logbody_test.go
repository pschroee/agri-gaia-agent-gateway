// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// X5.2: multipart files appear only as name, size and SHA-256, never with their content.
func TestMultipartFilesOnlyAsDigest(t *testing.T) {
	body, truncated := LogBody(Request{Path: "/datasets", Form: map[string][]string{"name": {"x"}},
		Files:   []File{{Field: "file", Path: "/workspace/a.csv"}},
		Uploads: []Upload{{Field: "file", Name: "a.csv", Data: []byte("secret"), SHA256: "abc"}}})
	if truncated {
		t.Fatal("unexpected truncation")
	}
	if strings.Contains(string(body), "secret") || !strings.Contains(string(body), `"size":6`) || !strings.Contains(string(body), `"sha256":"abc"`) {
		t.Fatalf("body: %s", body)
	}
}

// X5.2: bodies beyond 64 KiB are cut and marked; the body checksum describes the input.
func TestBodyTruncatedMarked(t *testing.T) {
	raw := json.RawMessage(`{"x":"` + strings.Repeat("a", MaxLoggedBody) + `"}`)
	body, truncated := LogBody(Request{Body: raw})
	if !truncated || len(body) != MaxLoggedBody {
		t.Fatalf("truncation: %v %d", truncated, len(body))
	}
	n, err := Normalize(Request{Method: "POST", Path: "/datasets", Body: raw})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	if n.BodyInSHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("body checksum: %q", n.BodyInSHA256)
	}
	// A second normalize keeps the checksum of the agent's body, not of the compacted one.
	if n2, _ := Normalize(n); n2.BodyInSHA256 != n.BodyInSHA256 {
		t.Fatalf("checksum overwritten: %q", n2.BodyInSHA256)
	}
}

// X5.1: a blocked path is distinguishable from other invalid calls.
func TestBlockedPathSentinel(t *testing.T) {
	_, err := Normalize(Request{Method: "GET", Path: "/network"})
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("blocked path: %v", err)
	}
}

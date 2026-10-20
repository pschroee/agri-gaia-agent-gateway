// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// The summary of an upload approval comes back with the approval; the list of every file only through
// GetApprovalUploadEntries, and only for the chat of the approval. Other approvals carry nothing.
func TestStoreApprovalUploadsRoundTrip(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c, err := s.CreateChat(ctx, NewChat{Title: "uploads", Model: "p/m", Variant: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	ap, err := s.CreateApproval(ctx, Approval{ChatID: c.ID, Kind: "platform_write", Via: "cli", Name: "POST /datasets", PendingKey: "-",
		Uploads:       json.RawMessage(`{"files":2,"manifest_sha256":"aa"}`),
		UploadEntries: json.RawMessage(`[{"field":"files","path":"/a/1.png","bytes":3,"sha256":"bb"},{"field":"files","path":"/a/2.png","bytes":4,"sha256":"cc"}]`)})
	if err != nil {
		t.Fatal(err)
	}
	stored, err := s.GetApproval(ctx, ap.ID)
	if err != nil {
		t.Fatal(err)
	}
	var sum struct {
		Files  int    `json:"files"`
		SHA256 string `json:"manifest_sha256"`
	}
	if err := json.Unmarshal(stored.Uploads, &sum); err != nil || sum.Files != 2 || sum.SHA256 != "aa" {
		t.Fatalf("summary: %s %v", stored.Uploads, err)
	}
	if stored.UploadEntries != nil {
		t.Error("the list of every file is not read with the approval")
	}
	raw, err := s.GetApprovalUploadEntries(ctx, c.ID, ap.ID)
	var entries []map[string]any
	if err != nil || json.Unmarshal(raw, &entries) != nil || len(entries) != 2 {
		t.Fatalf("entries: %s %v", raw, err)
	}
	if _, err := s.GetApprovalUploadEntries(ctx, "00000000-0000-0000-0000-000000000000", ap.ID); err != ErrNotFound {
		t.Errorf("another chat: %v", err)
	}
	plain, err := s.CreateApproval(ctx, Approval{ChatID: c.ID, Kind: "platform_write", Via: "cli", Name: "POST /x", PendingKey: "-"})
	if err != nil {
		t.Fatal(err)
	}
	if plain.Uploads != nil {
		t.Errorf("an approval without uploads carries none: %s", plain.Uploads)
	}
	if raw, err := s.GetApprovalUploadEntries(ctx, c.ID, plain.ID); err != nil || raw != nil {
		t.Errorf("no entries expected: %s %v", raw, err)
	}
	if b, _ := json.Marshal(plain); strings.Contains(string(b), `"uploads"`) {
		t.Errorf("omitempty: %s", b)
	}
}

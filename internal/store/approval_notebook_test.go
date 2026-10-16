// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"encoding/json"
	"testing"
)

// The structured data of the approval of a run (own code C-E4) survives the round trip through the store;
// every other approval leaves the field empty. jsonb brings the keys back in its own order, so the test
// compares the fields and not the bytes.
func TestApprovalNotebookRoundTrip(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c, err := s.CreateChat(ctx, NewChat{Title: "notebook", Model: "p/m", Variant: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	const data = `{"entry":"train.py","files":[{"name":"train.py","size":9,"sha256":"aa11","text":"print(1)\n"}],` +
		`"datasets":[{"id":17,"name":"mnist-mini"}],"limits":{"cpus":2,"mem_gb":8,"timeout_min":30,"internet":false}}`
	ap, err := s.CreateApproval(ctx, Approval{ChatID: c.ID, Kind: "platform_write", Via: "cli",
		Name: "POST /_agw/notebook/runs", PendingKey: "-", Notebook: json.RawMessage(data)})
	if err != nil {
		t.Fatal(err)
	}
	assertNotebookData(t, ap.Notebook)
	stored, err := s.GetApproval(ctx, ap.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertNotebookData(t, stored.Notebook)
	// A re-read at the moment of the approval replaces the summary, not the code: the card keeps showing what
	// the user was asked about (issue L2/#119).
	replaced, ok, err := s.ReplaceApprovalSummary(ctx, ap.ID, "Run train.py (at most 120 min)", "")
	if err != nil || !ok {
		t.Fatalf("replace: ok=%v err=%v", ok, err)
	}
	assertNotebookData(t, replaced.Notebook)
	// A decided approval keeps the data: the card shows the code after the decision too.
	dec, ok, err := s.DecideApproval(ctx, ap.ID, ApprovalApproved)
	if err != nil || !ok {
		t.Fatalf("decide: ok=%v err=%v", ok, err)
	}
	assertNotebookData(t, dec.Notebook)
	// An approval of another kind carries nothing.
	plain, err := s.CreateApproval(ctx, Approval{ChatID: c.ID, Kind: "platform_write", Via: "cli",
		Name: "PATCH /datasets/16", PendingKey: "-"})
	if err != nil {
		t.Fatal(err)
	}
	if len(plain.Notebook) != 0 {
		t.Errorf("an approval without data carries %s", plain.Notebook)
	}
}

// assertNotebookData checks the fields of the structured data of one run.
func assertNotebookData(t *testing.T, raw json.RawMessage) {
	t.Helper()
	var d struct {
		Entry string `json:"entry"`
		Files []struct {
			Name   string `json:"name"`
			Size   int64  `json:"size"`
			SHA256 string `json:"sha256"`
			Text   string `json:"text"`
		} `json:"files"`
		Datasets []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"datasets"`
		Limits struct {
			CPUs       int  `json:"cpus"`
			MemGB      int  `json:"mem_gb"`
			TimeoutMin int  `json:"timeout_min"`
			Internet   bool `json:"internet"`
		} `json:"limits"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatalf("notebook data %s: %v", raw, err)
	}
	if d.Entry != "train.py" || len(d.Files) != 1 || d.Files[0].Name != "train.py" || d.Files[0].SHA256 != "aa11" ||
		d.Files[0].Size != 9 || d.Files[0].Text != "print(1)\n" {
		t.Fatalf("data: %+v", d)
	}
	if len(d.Datasets) != 1 || d.Datasets[0].ID != 17 || d.Datasets[0].Name != "mnist-mini" {
		t.Errorf("datasets: %+v", d.Datasets)
	}
	if d.Limits.CPUs != 2 || d.Limits.MemGB != 8 || d.Limits.TimeoutMin != 30 || d.Limits.Internet {
		t.Errorf("limits: %+v", d.Limits)
	}
}

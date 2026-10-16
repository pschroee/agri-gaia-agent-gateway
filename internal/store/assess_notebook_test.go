// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

// A fetched file is remembered with the SHA-256 of the bytes the gateway read; the newest row of a SHA
// wins, the run is part of the key, and another chat sees nothing (R2.1).
func TestNotebookFetchRecorded(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c := testChatRow(t, s, "fetches")
	other := testChatRow(t, s, "elsewhere")
	sha := "aa" + strings.Repeat("0", 62)
	if _, found, err := s.NotebookFetchOf(ctx, c.ID, sha); err != nil || found {
		t.Fatalf("without a row: %v %v", found, err)
	}
	if err := s.RecordNotebookFetch(ctx, NotebookFetch{ChatID: c.ID, RunID: "r20260101120000-aaaa",
		Path: "out/model.onnx", SHA256: sha, Size: 4096}); err != nil {
		t.Fatal(err)
	}
	got, found, err := s.NotebookFetchOf(ctx, c.ID, sha)
	if err != nil || !found {
		t.Fatalf("the row: %v %v", found, err)
	}
	if got.RunID != "r20260101120000-aaaa" || got.Path != "out/model.onnx" || got.Size != 4096 ||
		got.FetchedAt.IsZero() {
		t.Fatalf("the fetch: %+v", got)
	}
	if _, found, err := s.NotebookFetchOf(ctx, other.ID, sha); err != nil || found {
		t.Fatalf("another chat sees the file: %v %v", found, err)
	}
	// The same file from another run is a row of its own; the newest row of the SHA answers.
	if err := s.RecordNotebookFetch(ctx, NotebookFetch{ChatID: c.ID, RunID: "r20260101130000-bbbb",
		Path: "model.onnx", SHA256: sha, Size: 4096}); err != nil {
		t.Fatal(err)
	}
	if got, _, err := s.NotebookFetchOf(ctx, c.ID, sha); err != nil || got.RunID != "r20260101130000-bbbb" {
		t.Fatalf("the newest row: %+v %v", got, err)
	}
	// The same file from the same run keeps one row; the newest size wins.
	if err := s.RecordNotebookFetch(ctx, NotebookFetch{ChatID: c.ID, RunID: "r20260101120000-aaaa",
		Path: "out/model.onnx", SHA256: sha, Size: 8192, FetchedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, s, `SELECT count(*) FROM notebook_fetches WHERE chat_id=$1 AND sha256=$2`, c.ID, sha); n != 2 {
		t.Fatalf("%d rows, want 2 (one per run)", n)
	}
	// An empty SHA or a chat that is no uuid asks nothing.
	if _, found, err := s.NotebookFetchOf(ctx, c.ID, ""); err != nil || found {
		t.Errorf("an empty SHA: %v %v", found, err)
	}
	if _, found, err := s.NotebookFetchOf(ctx, "not-a-uuid", sha); err != nil || found {
		t.Errorf("a chat that is no uuid: %v %v", found, err)
	}
}

// Seen are the datasets of every run of the chat, whatever run it was (R2.3); a repeated dataset counts
// once, and another chat's runs do not count.
func TestNotebookSeenDatasets(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c := testChatRow(t, s, "seen")
	other := testChatRow(t, s, "elsewhere")
	if ids, err := s.NotebookSeenDatasetIDs(ctx, c.ID); err != nil || len(ids) != 0 {
		t.Fatalf("without runs: %v %v", ids, err)
	}
	run := func(id, chat string, datasets []int64) NotebookRun {
		return NotebookRun{ID: id, ChatID: chat, HubUser: "u", Server: "s", Entry: "train.py",
			DatasetIDs: datasets, TimeoutMin: 60, StartedAt: time.Now()}
	}
	if err := s.CreateNotebookRun(ctx, run("r20260101120000-aaaa", c.ID, []int64{7})); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateNotebookRun(ctx, run("r20260101130000-bbbb", c.ID, []int64{5, 7})); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateNotebookRun(ctx, run("r20260101140000-cccc", c.ID, nil)); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateNotebookRun(ctx, run("r20260101150000-dddd", other.ID, []int64{9})); err != nil {
		t.Fatal(err)
	}
	ids, err := s.NotebookSeenDatasetIDs(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[0] != 5 || ids[1] != 7 {
		t.Fatalf("seen datasets: %v", ids)
	}
	if ids, err := s.NotebookSeenDatasetIDs(ctx, other.ID); err != nil || len(ids) != 1 || ids[0] != 9 {
		t.Fatalf("the other chat: %v %v", ids, err)
	}
}

// The origin of an uploaded model names the file it was uploaded from (R2.6): the model finds its check
// of that file through it, and the check card finds the model behind a file.
func TestModelOriginWithSourceSHA(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c := testChatRow(t, s, "uploaded")
	sha := "bb" + strings.Repeat("1", 62)
	if _, found, err := s.ModelOriginOfSHA(ctx, c.ID, sha); err != nil || found {
		t.Fatalf("without a row: %v %v", found, err)
	}
	if err := s.RecordModelOrigin(ctx, ModelOrigin{ModelID: 31, ChatID: c.ID, Source: OriginNotebook,
		SourceSHA: sha, SeenDatasetIDs: []int64{5}}); err != nil {
		t.Fatal(err)
	}
	got, err := s.ModelOriginOf(ctx, c.ID, 31)
	if err != nil || got.SourceSHA != sha || got.Source != OriginNotebook || got.TrainContainerID != 0 {
		t.Fatalf("the origin: %+v %v", got, err)
	}
	bySHA, found, err := s.ModelOriginOfSHA(ctx, c.ID, sha)
	if err != nil || !found || bySHA.ModelID != 31 {
		t.Fatalf("by SHA: %+v %v %v", bySHA, found, err)
	}
	// A model from a training carries no SHA, and the lookup of a SHA that no model names stays empty.
	if err := s.RecordModelOrigin(ctx, ModelOrigin{ModelID: 32, ChatID: c.ID, Source: OriginTraining,
		TrainContainerID: 9, SeenDatasetIDs: []int64{5}}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ModelOriginOf(ctx, c.ID, 32); err != nil || got.SourceSHA != "" {
		t.Fatalf("a training model carries a SHA: %+v %v", got, err)
	}
	if _, found, err := s.ModelOriginOfSHA(ctx, c.ID, "cc"+strings.Repeat("2", 62)); err != nil || found {
		t.Fatalf("an unknown SHA: %v %v", found, err)
	}
}

// countRows answers the count of one query (a helper of these tests).
func countRows(t *testing.T, s *Store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

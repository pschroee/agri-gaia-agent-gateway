// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"testing"
	"time"
)

// A recorded stop on the chat's last run hides the chat from the reaper; a stop recorded on an older
// run does not, because a newer run made the server a candidate again.
func TestIdleNotebookServersSkipsStopped(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c, err := s.CreateChat(ctx, NewChat{Title: "notebook", Model: "p/m", Variant: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	add := func(id string, end time.Time) {
		if err := s.CreateNotebookRun(ctx, NotebookRun{ID: id, ChatID: c.ID, HubUser: "u", Server: "agw-x",
			Entry: "a.py", TimeoutMin: 10, StartedAt: end.Add(-time.Minute)}); err != nil {
			t.Fatal(err)
		}
		if err := s.FinishNotebookRun(ctx, id, 0, end); err != nil {
			t.Fatal(err)
		}
	}
	idle := func() []NotebookServer {
		out, err := s.IdleNotebookServers(ctx, now.Add(-30*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	add("r1", now.Add(-3*time.Hour))
	if got := idle(); len(got) != 1 || got[0].RunID != "r1" {
		t.Fatalf("idle servers: %+v", got)
	}
	if err := s.MarkNotebookServerStopped(ctx, "r1", now); err != nil {
		t.Fatal(err)
	}
	if got := idle(); len(got) != 0 {
		t.Fatalf("a stopped server is still idle: %+v", got)
	}
	// A newer run: the mark on the older row does not hide the chat.
	add("r2", now.Add(-2*time.Hour))
	if got := idle(); len(got) != 1 || got[0].RunID != "r2" {
		t.Fatalf("after a new run: %+v", got)
	}
}

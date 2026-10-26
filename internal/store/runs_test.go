// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func ownedChat(t *testing.T, s *Store, owner, title string) Chat {
	t.Helper()
	c, err := s.CreateChat(context.Background(), NewChat{Title: title, Model: "p/m", Variant: "cli", Owner: owner})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func singleTrial(t *testing.T, s *Store, chatID string) Trial {
	t.Helper()
	tr, err := s.DeclareTrial(context.Background(), Trial{ChatID: chatID, Path: SearchPathTemplate})
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

// Issue #222: the run queries cover the chats they are given; the owner's chats come from OwnerChatTitles, which
// leaves out the trash and other owners.
func TestScopeQueriesSkipTrashAndForeign(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	anna, bert := "anna", "bert"
	a1, a2 := ownedChat(t, s, anna, "A one"), ownedChat(t, s, anna, "A two")
	b1 := ownedChat(t, s, bert, "B one")
	for _, c := range []Chat{a1, a2, b1} {
		singleTrial(t, s, c.ID)
		if _, err := s.CreateSearch(ctx, Search{ChatID: c.ID, Title: "s", Path: SearchPathTemplate, Objective: json.RawMessage(`{}`),
			Space: json.RawMessage(`{}`), Budget: json.RawMessage(`{}`)}); err != nil {
			t.Fatal(err)
		}
	}
	if ok, err := s.MoveToTrash(ctx, a2.ID, time.Now()); err != nil || !ok {
		t.Fatalf("trash: %v %v", ok, err)
	}
	titles, err := s.OwnerChatTitles(ctx, &anna)
	if err != nil || len(titles) != 1 || titles[a1.ID] != "A one" {
		t.Fatalf("anna: %v %v", titles, err)
	}
	if all, _ := s.OwnerChatTitles(ctx, nil); len(all) != 2 {
		t.Fatalf("token mode sees every chat that is not in the trash: %v", all)
	}
	sc := RunScope{ChatIDs: []string{a1.ID}}
	if got, err := s.ScopeSearches(ctx, sc); err != nil || len(got) != 1 || got[0].ChatID != a1.ID {
		t.Fatalf("searches: %v %v", got, err)
	}
	if got, err := s.ScopeSingleTrials(ctx, sc); err != nil || len(got) != 1 || got[0].Trial.ChatID != a1.ID {
		t.Fatalf("trials: %v %v", got, err)
	}
	if got, _ := s.ScopeSearches(ctx, RunScope{}); len(got) != 0 {
		t.Errorf("no chats, no rows: %v", got)
	}
}

// A notebook run that became a trial is the trial's row; one without figures is a row of its own, with its entry.
func TestScopeNotebookJobsWithoutTrial(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c := ownedChat(t, s, "anna", "Notebook")
	for _, id := range []string{"r1", "r2"} {
		if err := s.CreateNotebookRun(ctx, NotebookRun{ID: id, ChatID: c.ID, HubUser: "u", Server: "s", Entry: id + ".py", TimeoutMin: 5, StartedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	withTrial, _ := s.AddWatchJob(ctx, WatchJob{ChatID: c.ID, Kind: "notebook_run", Ref: "r1"})
	without, _ := s.AddWatchJob(ctx, WatchJob{ChatID: c.ID, Kind: "notebook_run", Ref: "r2"})
	other, _ := s.AddWatchJob(ctx, WatchJob{ChatID: c.ID, Kind: "training_run", Ref: "9"})
	tr, err := s.DeclareTrial(ctx, Trial{ChatID: c.ID, Path: SearchPathNotebook, Ref: "r1", JobID: withTrial})
	if err != nil || tr.JobID != withTrial {
		t.Fatalf("trial: %+v %v", tr, err)
	}
	got, err := s.ScopeNotebookJobs(ctx, RunScope{ChatIDs: []string{c.ID}})
	if err != nil || len(got) != 1 || got[0].Job.ID != without || got[0].Entry != "r2.py" {
		t.Fatalf("jobs: %+v %v (training job %s must not be in)", got, err, other)
	}
}

// The period is an overlap: a run that began yesterday and still runs is in "today", one that ended before it is not.
func TestScopePeriodOverlap(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c := ownedChat(t, s, "anna", "Period")
	now := time.Now()
	mk := func(start, end time.Time) string {
		tr := singleTrial(t, s, c.ID)
		if _, err := s.pool.Exec(ctx, `UPDATE experiment_trials SET state='done', started_at=$2, ended_at=$3 WHERE id=$1`, tr.ID, start, end); err != nil {
			t.Fatal(err)
		}
		return tr.ID
	}
	running := singleTrial(t, s, c.ID)
	if _, err := s.pool.Exec(ctx, `UPDATE experiment_trials SET state='running', started_at=$2 WHERE id=$1`, running.ID, now.Add(-30*time.Hour)); err != nil {
		t.Fatal(err)
	}
	old := mk(now.Add(-60*time.Hour), now.Add(-50*time.Hour))
	today := mk(now.Add(-2*time.Hour), now.Add(-1*time.Hour))
	got, err := s.ScopeSingleTrials(ctx, RunScope{ChatIDs: []string{c.ID}, Since: now.Add(-12 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, g := range got {
		ids[g.Trial.ID] = true
	}
	if !ids[running.ID] || !ids[today] || ids[old] {
		t.Fatalf("since: running=%v today=%v old=%v", ids[running.ID], ids[today], ids[old])
	}
	got, _ = s.ScopeSingleTrials(ctx, RunScope{ChatIDs: []string{c.ID}, Until: now.Add(-55 * time.Hour)})
	if len(got) != 1 || got[0].Trial.ID != old {
		t.Fatalf("until: %d rows, want only the old run", len(got))
	}
}

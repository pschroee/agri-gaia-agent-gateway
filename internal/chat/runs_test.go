// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agw/internal/store"
)

func runIDs(rows []runSource) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.id
	}
	return out
}

func sameIDs(t *testing.T, got []runSource, want ...string) {
	t.Helper()
	g := strings.Join(runIDs(got), ",")
	if w := strings.Join(want, ","); g != w {
		t.Errorf("order %s, want %s", g, w)
	}
}

// The states of the three sources map to one vocabulary (issue #222).
func TestRunStateMapping(t *testing.T) {
	cases := []struct{ kind, state, reason, by, want string }{
		{RunKindSearch, store.SearchProposed, "", "", RunWaiting},
		{RunKindSearch, store.SearchRunning, "", "", RunRunning},
		{RunKindSearch, store.SearchEnded, endStartFailed, "", RunFailed},
		{RunKindSearch, store.SearchEnded, endStopped, "", RunStopped},
		{RunKindSearch, store.SearchEnded, "budget_runs", "", RunDone},
		{RunKindTraining, store.TrialDeclared, "", "", RunRunning},
		{RunKindTraining, store.TrialRunning, "", "", RunRunning},
		{RunKindTraining, store.TrialDone, "", "", RunDone},
		{RunKindTraining, store.TrialFailed, "", "", RunFailed},
		{RunKindTraining, store.TrialStopped, "", "", RunStopped},
		{RunKindNotebook, store.JobRunning, "", "", RunRunning},
		{RunKindNotebook, store.JobFailed, "", "", RunFailed},
		{RunKindNotebook, store.JobDone, "", store.StoppedByUser, RunStopped},
		{RunKindNotebook, store.JobDone, "", "", RunDone},
	}
	for _, c := range cases {
		if got := runState(c.kind, c.state, c.reason, c.by); got != c.want {
			t.Errorf("%s/%s/%s/%s: %s, want %s", c.kind, c.state, c.reason, c.by, got, c.want)
		}
	}
}

func at(h int) *time.Time { t := time.Date(2026, 10, 26, h, 0, 0, 0, time.UTC); return &t }

func sampleRuns() []runSource {
	return []runSource{
		{id: "a", state: RunDone, start: at(9), duration: 30, title: "beta", chatTitle: "Zoo"},
		{id: "b", state: RunRunning, start: at(8), duration: 10, title: "Alpha", chatTitle: "Yak"},
		{id: "c", state: RunWaiting, start: at(12), duration: 0, title: "gamma", chatTitle: "Yak"},
		{id: "d", state: RunRunning, start: at(10), duration: 20, title: "delta", chatTitle: "Ant"},
		{id: "e", state: RunFailed, start: at(11), duration: 30, title: "epsilon", chatTitle: "Ant"},
	}
}

func TestSortRunsDefaultRunningFirst(t *testing.T) {
	rows := sampleRuns()
	sortRuns(rows, "", "")
	sameIDs(t, rows, "d", "b", "c", "e", "a")
}

func TestSortRunsByColumn(t *testing.T) {
	cases := []struct {
		by, order string
		want      []string
	}{
		{"started", "desc", []string{"c", "e", "d", "a", "b"}},
		{"started", "asc", []string{"b", "a", "d", "e", "c"}},
		{"duration", "desc", []string{"e", "a", "d", "b", "c"}}, // tie e/a: newest first
		{"duration", "asc", []string{"c", "b", "d", "e", "a"}},
		{"title", "asc", []string{"b", "a", "d", "e", "c"}},
		{"title", "desc", []string{"c", "e", "d", "a", "b"}},
		{"state", "asc", []string{"d", "b", "c", "e", "a"}},
		{"chat", "asc", []string{"e", "d", "c", "b", "a"}}, // Ant: e newer than d; Yak: c newer than b
		{"chat", "desc", []string{"a", "c", "b", "e", "d"}},
	}
	for _, c := range cases {
		rows := sampleRuns()
		sortRuns(rows, c.by, c.order)
		t.Run(c.by+"/"+c.order, func(t *testing.T) { sameIDs(t, rows, c.want...) })
	}
	// equal start: the id decides
	rows := []runSource{{id: "z", start: at(1)}, {id: "y", start: at(1)}}
	sortRuns(rows, "started", "desc")
	sameIDs(t, rows, "y", "z")
}

type runsEnv struct {
	e       *env
	a, b    store.Chat
	anna    string
	bertSub string
}

func newRunsEnv(t *testing.T) *runsEnv {
	t.Helper()
	e := setup(t)
	ctx := context.Background()
	mk := func(owner, title string) store.Chat {
		c, err := e.st.CreateChat(ctx, store.NewChat{Title: title, Model: "p/m", Variant: "cli", Owner: owner})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	return &runsEnv{e: e, anna: "anna", bertSub: "bert", a: mk("anna", "Anna chat"), b: mk("anna", "Second chat")}
}

func (r *runsEnv) single(t *testing.T, chatID string) store.Trial {
	t.Helper()
	tr, err := r.e.st.DeclareTrial(context.Background(), store.Trial{ChatID: chatID, Path: store.SearchPathTemplate})
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

func (r *runsEnv) search(t *testing.T, chatID, state string, budget string) store.Search {
	t.Helper()
	ctx := context.Background()
	s, err := r.e.st.CreateSearch(ctx, store.Search{ChatID: chatID, Title: "Search " + state, Path: store.SearchPathTemplate,
		Objective: json.RawMessage(`{"metric":"val_acc","mode":"max"}`), Space: json.RawMessage(`{}`), Budget: json.RawMessage(budget)})
	if err != nil {
		t.Fatal(err)
	}
	if state == store.SearchProposed {
		return s
	}
	ends := time.Now().Add(time.Hour)
	if _, ok, err := r.e.st.DecideSearch(ctx, s.ID, store.SearchRunning, &ends); err != nil || !ok {
		t.Fatalf("decide: %v %v", ok, err)
	}
	if state == store.SearchEnded {
		if ok, err := r.e.st.EndSearch(ctx, s.ID, "budget_runs", nil); err != nil || !ok {
			t.Fatalf("end: %v %v", ok, err)
		}
	}
	return s
}

// Counts ignore the state filter; the chat facet ignores the chat filter.
func TestExperimentRunsCountsAndFacet(t *testing.T) {
	r := newRunsEnv(t)
	ctx := context.Background()
	running := r.single(t, r.a.ID)
	if ok, err := r.e.st.StartTrial(ctx, running.ID, "9", ""); err != nil || !ok {
		t.Fatal(ok, err)
	}
	done := r.single(t, r.a.ID)
	_, _ = r.e.st.StartTrial(ctx, done.ID, "10", "")
	_, _ = r.e.st.FinishTrial(ctx, done.ID, store.TrialDone, "")
	r.search(t, r.b.ID, store.SearchProposed, `{"max_runs":3}`)
	owner := r.anna
	page, err := r.e.m.ExperimentRuns(ctx, RunFilter{Owner: &owner, States: []string{RunDone}, ChatID: r.b.ID})
	if err != nil {
		t.Fatal(err)
	}
	if page.Counts[RunRunning] != 0 || page.Counts[RunWaiting] != 1 || page.Counts[RunDone] != 0 {
		t.Errorf("counts for chat b ignore the state: %v", page.Counts)
	}
	if page.Total != 0 || len(page.Runs) != 0 {
		t.Errorf("state done in chat b: %d rows", page.Total)
	}
	if len(page.Chats) != 1 || page.Chats[0].ID != r.a.ID || page.Chats[0].Runs != 1 {
		t.Errorf("facet ignores the chat filter but not the state: %+v", page.Chats)
	}
	all, _ := r.e.m.ExperimentRuns(ctx, RunFilter{Owner: &owner})
	if all.Total != 3 || all.Counts[RunRunning] != 1 || len(all.Chats) != 2 || all.Chats[0].ID != r.a.ID {
		t.Errorf("all: total %d counts %v chats %+v", all.Total, all.Counts, all.Chats)
	}
	if all.Runs[0].State != RunRunning || all.Runs[0].ChatTitle != "Anna chat" {
		t.Errorf("running first: %+v", all.Runs[0])
	}
	stranger := "bert"
	if none, _ := r.e.m.ExperimentRuns(ctx, RunFilter{Owner: &stranger}); none.Total != 0 {
		t.Errorf("another owner sees %d runs", none.Total)
	}
}

// Only the rows of the page cost the views of search, trial and job.
func TestExperimentRunsEnrichesOnlyPage(t *testing.T) {
	r := newRunsEnv(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		c, _ := r.e.st.CreateChat(ctx, store.NewChat{Title: "c", Model: "p/m", Variant: "cli", Owner: r.anna})
		r.search(t, c.ID, store.SearchEnded, `{}`)
	}
	owner := r.anna
	before := enrichedRuns.Load()
	page, err := r.e.m.ExperimentRuns(ctx, RunFilter{Owner: &owner, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if n := enrichedRuns.Load() - before; n != 2 || len(page.Runs) != 2 || page.Total != 3 {
		t.Errorf("enriched %d, rows %d, total %d", n, len(page.Runs), page.Total)
	}
	if page.Runs[0].Search == nil {
		t.Error("a search row carries its view")
	}
	if p2, _ := r.e.m.ExperimentRuns(ctx, RunFilter{Owner: &owner, Limit: 2, Offset: 2}); len(p2.Runs) != 1 {
		t.Errorf("second page: %d rows", len(p2.Runs))
	}
}

// Trial 3 of 10 at epoch 4 of 20, two trials done.
func TestExperimentRunsSearchProgress(t *testing.T) {
	r := newRunsEnv(t)
	ctx := context.Background()
	s := r.search(t, r.a.ID, store.SearchRunning, `{"max_runs":10}`)
	for i := 1; i <= 3; i++ {
		tr, err := r.e.st.DeclareTrial(ctx, store.Trial{ChatID: r.a.ID, SearchID: s.ID, Path: store.SearchPathTemplate})
		if err != nil {
			t.Fatal(err)
		}
		_, _ = r.e.st.StartTrial(ctx, tr.ID, "c"+string(rune('0'+i)), "")
		if i < 3 {
			score := 0.5 + float64(i)/10
			_ = r.e.st.SetTrialProgress(ctx, tr.ID, nil, nil, &score, nil, nil)
			_, _ = r.e.st.FinishTrial(ctx, tr.ID, store.TrialDone, "")
			continue
		}
		epoch, epochs := 4, 20
		_ = r.e.st.SetTrialProgress(ctx, tr.ID, &epoch, &epochs, nil, nil, nil)
	}
	owner := r.anna
	page, err := r.e.m.ExperimentRuns(ctx, RunFilter{Owner: &owner})
	if err != nil || len(page.Runs) != 1 {
		t.Fatalf("%v %d", err, len(page.Runs))
	}
	p := page.Runs[0].Progress
	if p.Trial != 3 || p.Trials != 10 || p.TrialsDone != 2 || p.Epoch != 4 || p.Epochs != 20 {
		t.Errorf("progress: %+v", p)
	}
	if sc := page.Runs[0].Score; sc == nil || sc.Trial != 2 || sc.Metric != "val_acc" || sc.Mode != "max" {
		t.Errorf("best value: %+v", sc)
	}
}

// A training from a plan step carries the plan and the step of its job.
func TestExperimentRunsPlanStep(t *testing.T) {
	r := newRunsEnv(t)
	ctx := context.Background()
	jobID, err := r.e.st.AddWatchJob(ctx, store.WatchJob{ChatID: r.a.ID, Kind: "training_run", Ref: "9", PlanID: "plan-1", StepIdx: 4, ToolCallID: "call-7"})
	if err != nil {
		t.Fatal(err)
	}
	tr := r.single(t, r.a.ID)
	if ok, err := r.e.st.StartTrial(ctx, tr.ID, "9", jobID); err != nil || !ok {
		t.Fatal(ok, err)
	}
	owner := r.anna
	page, _ := r.e.m.ExperimentRuns(ctx, RunFilter{Owner: &owner, Kinds: []string{RunKindTraining}})
	if len(page.Runs) != 1 || page.Runs[0].PlanID != "plan-1" || page.Runs[0].PlanStep != 4 || page.Runs[0].ToolCallID != "call-7" {
		t.Fatalf("%+v", page.Runs)
	}
}

// The error line is the first line, without tokens, at most 200 runes.
func TestExperimentRunsRedactsError(t *testing.T) {
	r := newRunsEnv(t)
	ctx := context.Background()
	tr := r.single(t, r.a.ID)
	_, _ = r.e.st.StartTrial(ctx, tr.ID, "9", "")
	jwt := "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ4In0.c2lnbmF0dXJl"
	_, _ = r.e.st.FinishTrial(ctx, tr.ID, store.TrialFailed, "token "+jwt+" rejected "+strings.Repeat("x", 400)+"\nsecond line")
	owner := r.anna
	page, _ := r.e.m.ExperimentRuns(ctx, RunFilter{Owner: &owner})
	if len(page.Runs) != 1 {
		t.Fatalf("%d rows", len(page.Runs))
	}
	e := page.Runs[0].Error
	if strings.Contains(e, "eyJ") || strings.Contains(e, "second") || len([]rune(e)) > 202 || e == "" {
		t.Errorf("error line: %q", e)
	}
}

// A scope above the limit is cut and says so.
func TestExperimentRunsTruncated(t *testing.T) {
	r := newRunsEnv(t)
	old := store.RunScopeLimit
	store.RunScopeLimit = 2
	t.Cleanup(func() { store.RunScopeLimit = old })
	for i := 0; i < 3; i++ {
		r.single(t, r.a.ID)
	}
	owner := r.anna
	page, err := r.e.m.ExperimentRuns(context.Background(), RunFilter{Owner: &owner})
	if err != nil || !page.Truncated || page.Total != 2 {
		t.Errorf("truncated=%v total=%d err=%v", page.Truncated, page.Total, err)
	}
}

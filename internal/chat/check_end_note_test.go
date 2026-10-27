// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"agw/internal/assess"
	"agw/internal/platform"
	"agw/internal/store"
	"agw/internal/watch"
)

// #252: the end of a check the agent started outside a plan reaches the agent; the final test of a search also
// wakes a resting chat; the trials of a search get no second note.

// checkJob registers the kind of a check, creates a check of the chat and the job that observes it.
func checkJob(t *testing.T, e *env, chatID string, final bool) (store.Assessment, string) {
	t.Helper()
	ctx := context.Background()
	watch.Register((&assess.Service{St: e.st}).WatchKind())
	a, err := e.st.CreateAssessment(ctx, store.Assessment{ChatID: chatID, Kind: "training", Subject: "model:5", EvalDatasetID: 9, Final: final})
	if err != nil {
		t.Fatal(err)
	}
	jobID, err := e.st.AddWatchJob(ctx, store.WatchJob{ChatID: chatID, Kind: assess.KindName, Ref: a.ID})
	if err != nil {
		t.Fatal(err)
	}
	return a, jobID
}

// endedSearchWithFinal makes an ended search of the chat whose final test is the check.
func endedSearchWithFinal(t *testing.T, e *env, c ChatView, checkID string) store.Search {
	t.Helper()
	ctx := context.Background()
	s, err := runningSearchIn(t, e, c)
	if err != nil {
		t.Fatal(err)
	}
	ho, _ := json.Marshal(SearchHoldout{Available: true, FinalState: "started", CheckID: checkID})
	if err := e.st.SetSearchHoldout(ctx, s.ID, ho); err != nil {
		t.Fatal(err)
	}
	if ok, err := e.st.EndSearch(ctx, s.ID, "budget_runs", nil); err != nil || !ok {
		t.Fatalf("end: %v %v", ok, err)
	}
	return s
}

func finishCheck(t *testing.T, e *env, chatID, id, state, report, errText string) {
	t.Helper()
	var raw json.RawMessage
	if report != "" {
		raw = json.RawMessage(report)
	}
	if err := e.st.FinishAssessment(context.Background(), chatID, id, state, raw, errText); err != nil {
		t.Fatal(err)
	}
}

func promptsContain(e *env, want string) bool {
	for i := 0; i < 4; i++ {
		if a := e.agent(i); a != nil && strings.Contains(strings.Join(a.prompts(), "\n"), want) {
			return true
		}
	}
	return false
}

// The end of a check outside a plan is a note with the header of the report and no log.
func TestCheckEndOutsidePlanQueuesNote(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, err := e.m.Create(ctx, NewChat{})
	if err != nil {
		t.Fatal(err)
	}
	a, jobID := checkJob(t, e, c.ID, false)
	finishCheck(t, e, c.ID, a.ID, store.AssessFinished, `{"header":"Result: usable, accuracy 0.91"}`, "")
	runWatchersOnce(t, e)
	waitFor(t, func() bool { return promptsContain(e, "Check "+a.ID+" finished: Result: usable, accuracy 0.91") })
	list, err := e.st.ListQueue(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range list {
		if q.Note == store.NoteJob && (len(q.Refs) != 2 || q.Refs[0] != jobID || q.Refs[1] != a.ID) {
			t.Errorf("refs of the note: %v", q.Refs)
		}
	}
	for _, p := range e.agent(0).prompts() {
		if strings.Contains(p, "Platform job") || strings.Contains(p, "Log (last") {
			t.Fatalf("the note of a check is a job note with a log: %q", p)
		}
	}
}

// The final test of an ended search wakes a resting chat, as the end of the search did.
func TestFinalCheckOfSearchWakesRestingChat(t *testing.T) {
	e := setup(t)
	ctx := store.WithSession(context.Background(), "main")
	c, err := e.m.Create(ctx, NewChat{})
	if err != nil {
		t.Fatal(err)
	}
	a, _ := checkJob(t, e, c.ID, true)
	endedSearchWithFinal(t, e, c, a.ID)
	if _, err := e.m.Suspend(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if v, _ := e.m.View(ctx, c.ID); v.State != store.StateDormant {
		t.Fatalf("the chat does not rest: %+v", v)
	}
	finishCheck(t, e, c.ID, a.ID, store.AssessFinished, `{"header":"Result: usable"}`, "")
	runWatchersOnce(t, e)
	waitWoken(t, e, c.ID)
	waitFor(t, func() bool { return promptsContain(e, "Check "+a.ID+" finished") })
}

// Any other check outside a plan reaches a live chat but never wakes a resting one (D2.6).
func TestOtherCheckOutsidePlanWakesOnlyLiveChat(t *testing.T) {
	e := setup(t)
	ctx := store.WithSession(context.Background(), "main")
	c, err := e.m.Create(ctx, NewChat{})
	if err != nil {
		t.Fatal(err)
	}
	a, _ := checkJob(t, e, c.ID, false)
	if _, err := e.m.Suspend(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	finishCheck(t, e, c.ID, a.ID, store.AssessFinished, `{"header":"Result: usable"}`, "")
	runWatchersOnce(t, e)
	waitFor(t, func() bool { return len(queueNotes(t, e, c.ID, store.NoteJob)) == 1 })
	if v, _ := e.m.View(ctx, c.ID); v.State != store.StateDormant {
		t.Fatalf("a check without approval woke the chat: %+v", v)
	}
}

// A failed check says so and names the error.
func TestFailedCheckOutsidePlanNote(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, err := e.m.Create(ctx, NewChat{})
	if err != nil {
		t.Fatal(err)
	}
	a, _ := checkJob(t, e, c.ID, false)
	finishCheck(t, e, c.ID, a.ID, store.AssessFailed, "", "no images to measure")
	runWatchersOnce(t, e)
	waitFor(t, func() bool { return promptsContain(e, "Check "+a.ID+" failed: no images to measure") })
}

// The trial of a search gets no note of the type job even when the mark of the hook is already gone (the hook order
// of the running gateway); the row of the trial decides.
func TestSearchTrialJobGetsNoJobNote(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, err := e.m.Create(ctx, NewChat{})
	if err != nil {
		t.Fatal(err)
	}
	s, err := runningSearchIn(t, e, c)
	if err != nil {
		t.Fatal(err)
	}
	jobID := addRunningJob(t, e, c.ID, "training_run", "63")
	tr, err := e.st.DeclareTrial(ctx, store.Trial{ChatID: c.ID, SearchID: s.ID, Path: s.Path, Params: json.RawMessage(`{"lr":0.1}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.st.SetTrialJob(ctx, tr.ID, "63", jobID); err != nil {
		t.Fatal(err)
	}
	trialJobs.Delete(jobID)
	e.m.noteJobEnd(ctx, store.WatchJob{ID: jobID, ChatID: c.ID, Kind: "training_run", Ref: "63"}, nil, watch.Progress{State: watch.StateDone}, 0, false)
	if notes := queueNotes(t, e, c.ID, store.NoteJob); len(notes) != 0 {
		t.Fatalf("a job note for the trial of a search: %+v", notes)
	}
}

// A single run (no search) keeps its note.
func TestSingleTrialJobKeepsNote(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, err := e.m.Create(ctx, NewChat{})
	if err != nil {
		t.Fatal(err)
	}
	jobID := addRunningJob(t, e, c.ID, "training_run", "63")
	tr, err := e.st.DeclareTrial(ctx, store.Trial{ChatID: c.ID, Path: store.SearchPathTemplate})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.st.SetTrialJob(ctx, tr.ID, "63", jobID); err != nil {
		t.Fatal(err)
	}
	e.m.noteJobEnd(ctx, store.WatchJob{ID: jobID, ChatID: c.ID, Kind: "training_run", Ref: "63"}, nil, watch.Progress{State: watch.StateDone}, 0, false)
	if notes := queueNotes(t, e, c.ID, store.NoteJob); len(notes) != 1 {
		t.Fatalf("notes of the single run: %+v", notes)
	}
}

// The tools promise only what the gateway does.
func TestCheckToolNoteWording(t *testing.T) {
	for _, tool := range platform.Tools {
		if strings.Contains(tool.Desc, "also wakes you") {
			t.Errorf("tool %s promises a wake-up for every check", tool.Name)
		}
	}
	if !watchKindEndNote(assess.KindName) {
		t.Error("the kind of a check does not ask for its end note")
	}
}

func watchKindEndNote(name string) bool {
	k, ok := watch.Lookup(name)
	return ok && k.EndNote && watch.TellsEndOutsidePlan(name) && !watch.AnnouncedOutsidePlan(name)
}

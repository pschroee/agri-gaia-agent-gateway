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

	"agw/internal/platform"
	"agw/internal/search"
	"agw/internal/store"
	"agw/internal/watch"
)

func up(name, data string) platform.Upload {
	return platform.Upload{Field: "file", Name: name, Data: []byte(data), SHA256: sha256Hex([]byte(data))}
}

func testCode() store.SearchCode {
	return store.SearchCode{Version: 1, Entry: "train.py", TimeoutMinutes: 60,
		Files: []store.SearchFile{searchFileOf(up("train.py", "print(1)\n")), searchFileOf(up("util.py", "x = 1\n"))}}
}

func TestCodeMatches(t *testing.T) {
	code := testCode()
	run := platform.RunBody{Entry: "train.py", TimeoutMinutes: 60}
	params := json.RawMessage(`{"lr":0.01}`)
	files := []platform.Upload{up("train.py", "print(1)\n"), up("util.py", "x = 1\n"), up("params.json", `{ "lr": 1e-2 }`)}

	changed, ok, err := codeMatches(code, run, files, params)
	if err != nil || !ok || len(changed) != 0 {
		t.Fatalf("same code: %v %v %v", changed, ok, err)
	}
	// params.json other than declared
	_, ok, _ = codeMatches(code, run, []platform.Upload{files[0], files[1], up("params.json", `{"lr":0.02}`)}, params)
	if ok {
		t.Error("other params accepted")
	}
	// no params.json at all is not the declared params
	if _, ok, _ = codeMatches(code, run, files[:2], params); ok {
		t.Error("a run without params.json accepted")
	}
	// a changed file, an added file and a missing file are changes
	changed, _, _ = codeMatches(code, run, []platform.Upload{up("train.py", "print(2)\n"), up("util.py", "x = 1\n"), files[2]}, params)
	if len(changed) != 1 || changed[0] != "train.py" {
		t.Errorf("changed file: %v", changed)
	}
	changed, _, _ = codeMatches(code, run, []platform.Upload{files[0], files[1], up("extra.py", "y"), files[2]}, params)
	if len(changed) != 1 || changed[0] != "extra.py" {
		t.Errorf("added file: %v", changed)
	}
	changed, _, _ = codeMatches(code, run, []platform.Upload{files[0], files[2]}, params)
	if len(changed) != 1 || changed[0] != "util.py" {
		t.Errorf("missing file: %v", changed)
	}
	// entry, args, datasets and a longer timeout are not parameters
	for name, r := range map[string]platform.RunBody{
		"entry":   {Entry: "other.py", TimeoutMinutes: 60},
		"args":    {Entry: "train.py", Args: []string{"--x"}, TimeoutMinutes: 60},
		"dataset": {Entry: "train.py", DatasetIDs: []int64{3}, TimeoutMinutes: 60},
		"timeout": {Entry: "train.py", TimeoutMinutes: 61},
	} {
		if _, _, err := codeMatches(code, r, files, params); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// a shorter timeout is fine
	if _, ok, err := codeMatches(code, platform.RunBody{Entry: "train.py", TimeoutMinutes: 30}, files, params); err != nil || !ok {
		t.Errorf("shorter timeout: %v %v", ok, err)
	}
}

func TestFileChangesDiff(t *testing.T) {
	old := testCode()
	next := testCode()
	next.Version = 2
	next.Files[0] = searchFileOf(up("train.py", "print(2)\n"))
	got := fileChanges(old, next, []string{"train.py"})
	// The unchanged file of the new version follows the changed one, without a diff (#182).
	if len(got) != 2 || got[1].Name != "util.py" || got[1].Diff != "" || got[1].OldSHA256 != got[1].NewSHA256 ||
		got[0].OldSHA256 == got[0].NewSHA256 || !strings.Contains(got[0].Diff, "+print(2)") || !strings.Contains(got[0].Diff, "-print(1)") {
		t.Fatalf("changes: %+v", got)
	}
	var long strings.Builder
	for i := 0; i < 600; i++ {
		long.WriteString("a\n")
	}
	var long2 strings.Builder
	for i := 0; i < 600; i++ {
		long2.WriteString("b\n")
	}
	next.Files[0] = searchFileOf(up("train.py", long2.String()))
	old.Files[0] = searchFileOf(up("train.py", long.String()))
	got = fileChanges(old, next, []string{"train.py"})
	if !got[0].DiffTooLong || got[0].NewText == "" || got[0].Diff != "" {
		t.Errorf("a diff of over 400 lines: %+v", got[0])
	}
}

func TestFileChangesIncludesUnchanged(t *testing.T) {
	old := testCode()
	next := testCode()
	next.Version = 2
	next.Files[0] = searchFileOf(up("train.py", "print(2)\n"))
	got := fileChanges(old, next, []string{"train.py"})
	if len(got) != 2 || got[0].Name != "train.py" || got[1].Name != "util.py" {
		t.Fatalf("order: %+v", got)
	}
	u := got[1]
	if u.Diff != "" || u.DiffTooLong || u.Added || u.Removed || u.OldSHA256 == "" || u.OldSHA256 != u.NewSHA256 {
		t.Errorf("unchanged file: %+v", u)
	}
}

func TestStartFailedTwice(t *testing.T) {
	one := 1
	failed := func(epoch *int) store.Trial { return store.Trial{State: store.TrialFailed, Epoch: epoch} }
	done := store.Trial{State: store.TrialDone, Epoch: &one}
	if !startFailedTwice([]store.Trial{done, failed(nil), failed(nil)}) {
		t.Error("two trials that never started")
	}
	if startFailedTwice([]store.Trial{failed(nil), done, failed(nil)}) {
		t.Error("not in a row")
	}
	if startFailedTwice([]store.Trial{failed(nil), failed(&one)}) {
		t.Error("a failure after an epoch is no failed start")
	}
	if startFailedTwice([]store.Trial{failed(nil)}) {
		t.Error("one failure")
	}
}

func TestSearchWakeNotes(t *testing.T) {
	n := func(id string) store.QueueEntry {
		return store.QueueEntry{Kind: store.QueueSystem, Note: store.NoteSearch, Refs: []string{id}}
	}
	if searchWakeNotes([]store.QueueEntry{n("s1"), n("s1")}) != "s1" {
		t.Error("two notes of one search")
	}
	if searchWakeNotes([]store.QueueEntry{n("s1"), n("s2")}) != "" {
		t.Error("two searches")
	}
	if searchWakeNotes([]store.QueueEntry{n("s1"), {Kind: store.QueueUser}}) != "" {
		t.Error("a user message among them")
	}
	if maxAutoTurns(5) != 18 || maxAutoTurns(40) != 50 {
		t.Errorf("limit: %d %d", maxAutoTurns(5), maxAutoTurns(40))
	}
}

func TestEndTextWinnerAndFinalCalls(t *testing.T) {
	m := &Manager{}
	score := 0.91
	w := &store.Trial{Idx: 2, Score: &score, Ref: "63"}
	obj := search.Objective{Metric: "val_acc", Mode: "max"}
	s := store.Search{Path: store.SearchPathTemplate}
	got := m.endText(s, obj, "budget_runs", w, SearchHoldout{Available: true, FinalState: "pending", EvalDataset: &SearchNamed{ID: 12}})
	for _, want := range []string{"The training search ended", "Winner: trial 2", "val_acc 0.91", "create_model_from_training", "final=true", "eval_dataset_id=12"} {
		if !strings.Contains(got, want) {
			t.Errorf("end text lacks %q: %s", want, got)
		}
	}
	if got := m.endText(s, obj, "budget_runs", w, SearchHoldout{FinalState: "none"}); !strings.Contains(got, "no held-back test set") {
		t.Errorf("without a test set: %s", got)
	}
	if got := m.endText(s, obj, "start_failed", nil, SearchHoldout{}); !strings.Contains(got, "No trial finished") {
		t.Errorf("without a winner: %s", got)
	}
}

func TestSearchCSV(t *testing.T) {
	score := 0.5
	e := SearchExport{Search: SearchView{Search: store.Search{WinnerIdx: new(int)}}, Trials: []SearchExportTrial{
		{TrialView: TrialView{Trial: store.Trial{Idx: 0, State: "done", Params: json.RawMessage(`{"lr":0.1}`), Note: "=cmd", Score: &score}}}}}
	b, err := e.CSV()
	if err != nil {
		t.Fatal(err)
	}
	out := string(b)
	if !strings.HasPrefix(out, "trial,state,params") || !strings.Contains(out, "'=cmd") || !strings.Contains(out, "yes") {
		t.Errorf("csv: %s", out)
	}
}

// --- with a database ---

const testBase = `{"container_id":7,"dataset_id":16,"provider":"Torchvision","architecture":"EfficientNet","config":{"lr":0.1,"epochs":5}}`
const testSpaceJSON = `{"lr":{"type":"float","min":0.0001,"max":0.5,"scale":"log"}}`

// runningSearch makes a chat with a running template search over container 7.
func runningSearch(t *testing.T, e *env, budget string) (ChatView, store.Search) {
	t.Helper()
	ctx := context.Background()
	c, err := e.m.Create(ctx, NewChat{})
	if err != nil {
		t.Fatal(err)
	}
	s, err := e.st.CreateSearch(ctx, store.Search{ChatID: c.ID, Title: "t", Path: store.SearchPathTemplate,
		Objective: json.RawMessage(`{"metric":"val_acc","mode":"max"}`), Space: json.RawMessage(testSpaceJSON),
		Budget: json.RawMessage(budget), Base: json.RawMessage(testBase), Holdout: json.RawMessage(`{"available":true,"eval_dataset":{"id":9}}`)})
	if err != nil {
		t.Fatal(err)
	}
	ends := time.Now().Add(time.Hour)
	s, ok, err := e.st.DecideSearch(ctx, s.ID, store.SearchRunning, &ends)
	if err != nil || !ok {
		t.Fatalf("decide: %v %v", ok, err)
	}
	return c, s
}

func TestSearchNextOutsideSpaceAndRepeat(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, s := runningSearch(t, e, `{"max_runs":3}`)
	res, _ := e.m.searchNext(ctx, c.ID, s.ID, []byte(`{"params":{"lr":2},"note":"too big"}`))
	if res.Status != "error" || !strings.Contains(res.Message, "outside the search space") {
		t.Fatalf("outside: %+v", res)
	}
	events, _ := e.st.SearchEvents(ctx, s.ID)
	if len(events) != 1 || events[0].Kind != "trial_refused" {
		t.Fatalf("events: %+v", events)
	}
	res, _ = e.m.searchNext(ctx, c.ID, s.ID, []byte(`{"params":{"lr":0.01},"note":"first"}`))
	if res.Status != "ok" || !strings.Contains(res.Body, "clone_training with container_id=7") || !strings.Contains(res.Body, `"lr":0.01`) ||
		!strings.Contains(res.Body, `"epochs":5`) {
		t.Fatalf("declared: %+v", res)
	}
	// The same declaration again is the same answer, no second trial.
	res2, _ := e.m.searchNext(ctx, c.ID, s.ID, []byte(`{"params":{"lr":1e-2}}`))
	if res2.Body != res.Body {
		t.Errorf("redeclaration: %+v", res2)
	}
	// Missing parameter.
	res, _ = e.m.searchNext(ctx, c.ID, s.ID, []byte(`{"params":{}}`))
	if res.Status != "error" || !strings.Contains(res.Message, "missing: lr") {
		t.Errorf("missing: %+v", res)
	}
	// A trial that ran is not repeated.
	trials, _ := e.st.ListTrials(ctx, c.ID)
	var open store.Trial
	for _, tr := range trials {
		if tr.State == store.TrialDeclared {
			open = tr
		}
	}
	if ok, err := e.st.ClaimTrialStart(ctx, open.ID, ""); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	if ok, _ := e.st.FinishTrial(ctx, open.ID, store.TrialDone, ""); !ok {
		t.Fatal("finish")
	}
	res, _ = e.m.searchNext(ctx, c.ID, s.ID, []byte(`{"params":{"lr":0.01}}`))
	if res.Status != "error" || !strings.Contains(res.Message, "already ran these parameters") {
		t.Errorf("repeat: %+v", res)
	}
}

func TestSearchNextBudget(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, s := runningSearch(t, e, `{"max_runs":1}`)
	tr, err := e.st.DeclareTrial(ctx, store.Trial{ChatID: c.ID, SearchID: s.ID, Path: s.Path, Params: json.RawMessage(`{"lr":0.1}`)})
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := e.st.ClaimTrialStart(ctx, tr.ID, ""); !ok {
		t.Fatal("claim")
	}
	_, _ = e.st.FinishTrial(ctx, tr.ID, store.TrialDone, "")
	res, _ := e.m.searchNext(ctx, c.ID, s.ID, []byte(`{"params":{"lr":0.2}}`))
	if res.Status != "error" || !strings.Contains(res.Message, "budget_runs") {
		t.Fatalf("budget: %+v", res)
	}
}

func clonedRequest(id string, cfg string) platform.Request {
	return platform.Request{Method: "POST", Path: "/train/containers/" + id + "/clone", Body: json.RawMessage(`{"train_config":` + cfg + `}`)}
}

func TestCloneCoveredOnlyExact(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, s := runningSearch(t, e, `{"max_runs":3}`)
	if res, _ := e.m.searchNext(ctx, c.ID, s.ID, []byte(`{"params":{"lr":0.01}}`)); res.Status != "ok" {
		t.Fatalf("declare: %+v", res)
	}
	for name, req := range map[string]platform.Request{
		"other lr":      clonedRequest("7", `{"lr":0.02,"epochs":5}`),
		"extra key":     clonedRequest("7", `{"lr":0.01,"epochs":5,"momentum":0.9}`),
		"other epochs":  clonedRequest("7", `{"lr":0.01,"epochs":6}`),
		"other base":    clonedRequest("8", `{"lr":0.01,"epochs":5}`),
		"missing field": clonedRequest("7", `{"lr":0.01}`),
	} {
		if claim := e.m.searchMatch(ctx, c.ID, req); claim != nil {
			t.Errorf("%s: covered", name)
		}
	}
	claim := e.m.searchMatch(ctx, c.ID, clonedRequest("7", `{"epochs":5,"lr":1e-2}`))
	if claim == nil || !claim.Covered || claim.Kind != claimClone {
		t.Fatalf("exact clone: %+v", claim)
	}
	// Reserving takes the trial once.
	if !e.m.reserveClaim(ctx, claim) || e.m.reserveClaim(ctx, claim) {
		t.Error("a trial can be reserved once")
	}
}

func TestStartCoveredOnlyDeclaredRef(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, s := runningSearch(t, e, `{"max_runs":3}`)
	if res, _ := e.m.searchNext(ctx, c.ID, s.ID, []byte(`{"params":{"lr":0.01}}`)); res.Status != "ok" {
		t.Fatalf("declare: %+v", res)
	}
	start := func(id string) platform.Request {
		return platform.Request{Method: "POST", Path: "/train/containers/" + id + "/run"}
	}
	if claim := e.m.searchMatch(ctx, c.ID, start("63")); claim != nil {
		t.Error("a start before the copy was made is covered")
	}
	trials, _ := e.st.ListTrials(ctx, c.ID)
	if ok, _ := e.st.ClaimTrialClone(ctx, trials[0].ID); !ok {
		t.Fatal("claim clone")
	}
	if ok, _ := e.st.SetTrialRef(ctx, trials[0].ID, "63"); !ok {
		t.Fatal("ref")
	}
	if claim := e.m.searchMatch(ctx, c.ID, start("64")); claim != nil {
		t.Error("the start of another container is covered")
	}
	claim := e.m.searchMatch(ctx, c.ID, start("63"))
	if claim == nil || !claim.Covered || claim.Kind != claimStart {
		t.Fatalf("start: %+v", claim)
	}
}

func TestFinalTestGuard(t *testing.T) {
	e := setup(t)
	countPlatform(t, e)
	e.m.RegisterVirtual(platform.AssessPrefix, &assessStub{})
	ctx := context.Background()
	c, _ := runningSearch(t, e, `{"max_runs":3}`)
	slot := e.m.live[c.ID].slot.ID
	res, err := e.m.PlatformCall(ctx, c.ID, slot, "cli", platform.Request{Method: "POST", Path: platform.AssessCheckPath,
		Body: json.RawMessage(`{"model_id":5,"final":true}`)})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "denied" || !strings.Contains(res.Message, "the final test runs once, for the winner, after the training search has ended") {
		t.Fatalf("final during a search: %+v", res)
	}
}

func TestTrialEndedFinishesSearchAtBudget(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, s := runningSearch(t, e, `{"max_runs":1}`)
	tr, _ := e.st.DeclareTrial(ctx, store.Trial{ChatID: c.ID, SearchID: s.ID, Path: s.Path, Params: json.RawMessage(`{"lr":0.1}`)})
	if ok, _ := e.st.ClaimTrialStart(ctx, tr.ID, ""); !ok {
		t.Fatal("claim")
	}
	jobID, err := e.st.AddWatchJob(ctx, store.WatchJob{ChatID: c.ID, Kind: "training_run", Ref: "63"})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.st.SetTrialJob(ctx, tr.ID, "63", jobID); err != nil {
		t.Fatal(err)
	}
	score, epoch, epochs := 0.93, 5, 5
	if err := e.st.SetTrialProgress(ctx, tr.ID, &epoch, &epochs, &score, &epoch, nil); err != nil {
		t.Fatal(err)
	}
	searchTrialEnded(ctx, e.m, store.WatchJob{ID: jobID, ChatID: c.ID, Kind: "training_run", Ref: "63"}, watch.Progress{State: watch.StateDone})
	got, err := e.st.GetSearch(ctx, c.ID, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.SearchEnded || got.EndReason != "budget_runs" || got.WinnerIdx == nil || *got.WinnerIdx != 1 {
		t.Fatalf("search: %+v", got)
	}
	var ho SearchHoldout
	_ = json.Unmarshal(got.Holdout, &ho)
	if ho.FinalState != "pending" {
		t.Errorf("final state: %q", ho.FinalState)
	}
	events, _ := e.st.SearchEvents(ctx, s.ID)
	if len(events) == 0 || events[len(events)-1].Kind != "ended" {
		t.Errorf("events: %+v", events)
	}
}

func TestBudgetCapAndControls(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, s := runningSearch(t, e, `{"max_runs":2}`)
	// compute left is 3 (default), no trials yet: at most 3 runs
	if _, err := e.m.SetSearchBudget(ctx, c.ID, s.ID, []byte(`{"max_runs":4}`)); err == nil {
		t.Error("a budget above used runs + compute left accepted")
	} else if _, ok := err.(*SearchInputError); !ok {
		t.Errorf("error type: %T", err)
	}
	v, err := e.m.SetSearchBudget(ctx, c.ID, s.ID, []byte(`{"max_runs":3,"patience":2}`))
	if err != nil || v.State != store.SearchRunning {
		t.Fatalf("budget: %+v %v", v, err)
	}
	if _, err := e.m.HintSearch(ctx, c.ID, s.ID, "  "); err == nil {
		t.Error("an empty hint accepted")
	}
	if _, err := e.m.AdoptTrial(ctx, c.ID, s.ID, 1); err == nil {
		t.Error("adopting a trial that does not exist")
	}
	v, err = e.m.StopSearch(ctx, c.ID, s.ID, "search")
	if err != nil || v.State != store.SearchEnded || v.EndReason != endStopped {
		t.Fatalf("stop: %+v %v", v, err)
	}
	if _, err := e.m.SetSearchBudget(ctx, c.ID, s.ID, []byte(`{"max_runs":1}`)); err != ErrSearchNotRunning {
		t.Errorf("after the end: %v", err)
	}
	events, _ := e.st.SearchEvents(ctx, s.ID)
	var kinds []string
	for _, ev := range events {
		kinds = append(kinds, ev.Kind)
	}
	if strings.Join(kinds, ",") != "budget_changed,stopped,ended" {
		t.Errorf("events: %v", kinds)
	}
}

func TestSearchWakeLimitCounts(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, s := runningSearch(t, e, `{"max_runs":1}`) // limit 6
	open := []store.QueueEntry{{Kind: store.QueueSystem, Note: store.NoteSearch, Refs: []string{s.ID}}}
	for i := 1; i <= 6; i++ {
		if _, limited, handled := e.m.searchWakeLimit(ctx, c.ID, open); limited || !handled {
			t.Fatalf("turn %d held", i)
		}
	}
	if ev, limited, _ := e.m.searchWakeLimit(ctx, c.ID, open); !limited || ev.Limit != 6 {
		t.Errorf("turn 7: %+v %v", ev, limited)
	}
	if _, _, handled := e.m.searchWakeLimit(ctx, c.ID, []store.QueueEntry{{Kind: store.QueueUser}}); handled {
		t.Error("a user message is a search wake")
	}
}

func TestProposeSearchRefusals(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	got := countPlatform(t, e)
	e.m.RegisterVirtual(platform.SearchPrefix, e.m.SearchDoer())
	c, err := e.m.Create(ctx, NewChat{})
	if err != nil {
		t.Fatal(err)
	}
	slot := e.m.live[c.ID].slot.ID
	form := func(extra map[string][]string) map[string][]string {
		f := map[string][]string{"title": {"t"}, "path": {"template"}, "base_container_id": {"7"},
			"space": {testSpaceJSON}, "budget": {`{"max_runs":9}`}}
		for k, v := range extra {
			f[k] = v
		}
		return f
	}
	call := func(f map[string][]string) platform.Result {
		r, err := e.m.PlatformCall(ctx, c.ID, slot, "cli", platform.Request{Method: "POST", Path: platform.SearchPrefix, Form: f})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	if r := call(form(nil)); r.Status != "error" || !strings.Contains(r.Message, "not enough compute calls left: 9 runs need 9, this chat has 3 left") {
		t.Errorf("compute: %+v", r)
	}
	if r := call(form(map[string][]string{"budget": {`{"max_runs":2}`}})); r.Status != "error" || !strings.Contains(r.Message, "not one this chat made") {
		t.Errorf("foreign base: %+v", r)
	}
	if aps, _ := e.st.ListApprovals(ctx, store.ApprovalPending, c.ID); len(aps) != 0 {
		t.Errorf("a refused proposal asked the user: %+v", aps)
	}
	if n := got(); len(n) != 0 {
		t.Errorf("platform reached: %v", n)
	}
	// A running search blocks the next proposal.
	_, _ = runningSearchIn(t, e, c)
	if r := call(form(map[string][]string{"budget": {`{"max_runs":2}`}})); r.Status != "error" || !strings.Contains(r.Message, "still running") {
		t.Errorf("running search: %+v", r)
	}
}

func runningSearchIn(t *testing.T, e *env, c ChatView) (store.Search, error) {
	t.Helper()
	ctx := context.Background()
	s, err := e.st.CreateSearch(ctx, store.Search{ChatID: c.ID, Title: "t", Path: store.SearchPathTemplate,
		Objective: json.RawMessage(`{"metric":"val_acc","mode":"max"}`), Space: json.RawMessage(testSpaceJSON), Budget: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	ends := time.Now().Add(time.Hour)
	s, _, err = e.st.DecideSearch(ctx, s.ID, store.SearchRunning, &ends)
	return s, err
}

// The final test's result is copied from its check into holdout.result, and the agent's closing note of a search
// it ended itself is part of the view (#168).
func TestSearchViewCarriesFinalResultAndEndNote(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, s := runningSearch(t, e, `{"max_runs":3}`)
	check, err := e.st.CreateAssessment(ctx, store.Assessment{ChatID: c.ID, Kind: "training", Subject: "model:5", EvalDatasetID: 9, Final: true})
	if err != nil {
		t.Fatal(err)
	}
	ho, _ := json.Marshal(SearchHoldout{Available: true, FinalState: "started", CheckID: check.ID})
	if err := e.st.SetSearchHoldout(ctx, s.ID, ho); err != nil {
		t.Fatal(err)
	}
	view := func() SearchView {
		list, err := e.m.Searches(ctx, c.ID)
		if err != nil || len(list) != 1 {
			t.Fatalf("searches: %v %v", list, err)
		}
		return list[0]
	}
	resultOf := func(v SearchView) []byte {
		var h SearchHoldout
		_ = json.Unmarshal(v.Holdout, &h)
		return h.Result
	}
	if r := resultOf(view()); len(r) != 0 {
		t.Fatalf("a result before the check ended: %s", r)
	}
	report := `{"figures":{"overall":{"accuracy":0.87}},"measured_images":92,"verdict":"usable"}`
	if err := e.st.FinishAssessment(ctx, c.ID, check.ID, store.AssessFinished, json.RawMessage(report), ""); err != nil {
		t.Fatal(err)
	}
	var got FinalResult
	if err := json.Unmarshal(resultOf(view()), &got); err != nil || got.State != "finished" || got.Accuracy == nil || *got.Accuracy != 0.87 ||
		got.Images != 92 || got.Verdict != "usable" {
		t.Fatalf("result: %+v %v", got, err)
	}
	// stored, so that the export and later reads carry it as well
	stored, _ := e.st.GetSearch(ctx, c.ID, s.ID)
	var h SearchHoldout
	_ = json.Unmarshal(stored.Holdout, &h)
	if len(h.Result) == 0 {
		t.Error("the result was not stored")
	}
	// the note of the agent that ended the search
	if ended, _ := e.m.finishSearch(ctx, s, endByAgent, nil, "good enough"); !ended {
		t.Fatal("search not ended")
	}
	if n := view().EndNote; n != "good enough" {
		t.Errorf("end note: %q", n)
	}
}

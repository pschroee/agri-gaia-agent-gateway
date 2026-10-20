// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func newSearch(t *testing.T, s *Store, chatID string) Search {
	t.Helper()
	x, err := s.CreateSearch(context.Background(), Search{ChatID: chatID, Title: "lr and batch size", Path: SearchPathTemplate,
		Objective: json.RawMessage(`{"metric":"val_acc","mode":"max"}`),
		Space:     json.RawMessage(`{"lr":{"type":"float","min":0.0001,"max":0.1,"scale":"log"}}`),
		Budget:    json.RawMessage(`{"max_runs":3}`), Base: json.RawMessage(`{"container_id":7}`)})
	if err != nil {
		t.Fatal(err)
	}
	return x
}

// A search is proposed, decided once, ends once; a second running search in the chat is refused by the index.
func TestSearchLifecycle(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c := newPlanChat(t, s)
	x := newSearch(t, s, c.ID)
	if x.State != SearchProposed || x.ID == "" || x.Code.Version != 0 {
		t.Fatalf("proposed: %+v", x)
	}
	endsBy := time.Now().Add(time.Hour)
	got, ok, err := s.DecideSearch(ctx, x.ID, SearchRunning, &endsBy)
	if err != nil || !ok || got.State != SearchRunning || got.StartedAt == nil || got.EndsBy == nil {
		t.Fatalf("decide: %+v ok=%v err=%v", got, ok, err)
	}
	if _, ok, _ := s.DecideSearch(ctx, x.ID, SearchRunning, &endsBy); ok {
		t.Error("a search that already runs must not be decided again")
	}
	other := newSearch(t, s, c.ID)
	if _, _, err := s.DecideSearch(ctx, other.ID, SearchRunning, &endsBy); !errors.Is(err, ErrSearchRunning) {
		t.Fatalf("second running search: %v", err)
	}
	if r, err := s.RunningSearch(ctx, c.ID); err != nil || r.ID != x.ID {
		t.Fatalf("running search: %+v %v", r, err)
	}
	if ok, err := s.UpdateSearchBudget(ctx, x.ID, json.RawMessage(`{"max_runs":5}`)); err != nil || !ok {
		t.Fatalf("budget: ok=%v err=%v", ok, err)
	}
	w := 2
	if ok, err := s.EndSearch(ctx, x.ID, "budget_runs", &w); err != nil || !ok {
		t.Fatalf("end: ok=%v err=%v", ok, err)
	}
	if ok, _ := s.EndSearch(ctx, x.ID, "again", nil); ok {
		t.Error("an ended search must not end again")
	}
	if ok, _ := s.UpdateSearchBudget(ctx, x.ID, json.RawMessage(`{}`)); ok {
		t.Error("the budget of an ended search must not change")
	}
	if _, err := s.RunningSearch(ctx, c.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("no search runs any more: %v", err)
	}
	end, _ := s.GetSearch(ctx, c.ID, x.ID)
	if end.State != SearchEnded || end.EndReason != "budget_runs" || end.WinnerIdx == nil || *end.WinnerIdx != 2 || end.EndedAt == nil {
		t.Fatalf("ended: %+v", end)
	}
	if _, ok, err := s.DecideSearch(ctx, other.ID, SearchRejected, nil); err != nil || !ok {
		t.Fatalf("reject: ok=%v err=%v", ok, err)
	}
	list, err := s.ListSearches(ctx, c.ID)
	if err != nil || len(list) != 2 {
		t.Fatalf("list: %d %v", len(list), err)
	}
	// Another chat's search is not found.
	c2 := newPlanChat(t, s)
	if _, err := s.GetSearch(ctx, c2.ID, x.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("foreign search: %v", err)
	}
}

func TestSearchCodeAndHoldout(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c := newPlanChat(t, s)
	x := newSearch(t, s, c.ID)
	code := SearchCode{Version: 1, Entry: "train.py", Files: []SearchFile{{Name: "train.py", SHA256: "ab", Size: 12}},
		DatasetIDs: []int64{3}, Args: []string{"--x=1"}, TimeoutMinutes: 30}
	if err := s.SetSearchCode(ctx, x.ID, code); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSearchHoldout(ctx, x.ID, json.RawMessage(`{"available":true,"eval_dataset_id":9}`)); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetSearch(ctx, c.ID, x.ID)
	if got.Code.Version != 1 || got.Code.Entry != "train.py" || len(got.Code.Files) != 1 || got.Code.Files[0].SHA256 != "ab" || got.Code.TimeoutMinutes != 30 {
		t.Fatalf("code: %+v", got.Code)
	}
	var h struct {
		Available bool `json:"available"`
		Eval      int  `json:"eval_dataset_id"`
	}
	if err := json.Unmarshal(got.Holdout, &h); err != nil || !h.Available || h.Eval != 9 {
		t.Fatalf("holdout: %s %v", got.Holdout, err)
	}
}

// Trials are numbered within the chat (seq) and within the search (idx); single runs have idx 0; jobs and runs
// find their trial; a trial ends once.
func TestTrialsLifecycle(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c := newPlanChat(t, s)
	x := newSearch(t, s, c.ID)
	single, err := s.DeclareTrial(ctx, Trial{ChatID: c.ID, Path: SearchPathTemplate, Ref: "5", Params: json.RawMessage(`{}`)})
	if err != nil || single.Seq != 1 || single.Idx != 0 || single.SearchID != "" || single.State != TrialDeclared {
		t.Fatalf("single run: %+v %v", single, err)
	}
	t1, err := s.DeclareTrial(ctx, Trial{ChatID: c.ID, SearchID: x.ID, Path: SearchPathTemplate, Params: json.RawMessage(`{"lr":0.01}`), Note: "start"})
	if err != nil || t1.Seq != 2 || t1.Idx != 1 || t1.SearchID != x.ID {
		t.Fatalf("trial 1: %+v %v", t1, err)
	}
	t2, _ := s.DeclareTrial(ctx, Trial{ChatID: c.ID, SearchID: x.ID, Path: SearchPathTemplate, Params: json.RawMessage(`{"lr":0.001}`)})
	if t2.Seq != 3 || t2.Idx != 2 {
		t.Fatalf("trial 2: %+v", t2)
	}
	if ok, err := s.SetTrialRef(ctx, t1.ID, "63"); err != nil || !ok {
		t.Fatalf("ref: ok=%v err=%v", ok, err)
	}
	if got, err := s.TrialByRef(ctx, c.ID, SearchPathTemplate, "63"); err != nil || got.ID != t1.ID {
		t.Fatalf("by ref: %+v %v", got, err)
	}
	if ok, err := s.StartTrial(ctx, t1.ID, "63", "w1"); err != nil || !ok {
		t.Fatalf("start: ok=%v err=%v", ok, err)
	}
	if ok, _ := s.StartTrial(ctx, t1.ID, "63", "w1"); ok {
		t.Error("a running trial must not start again")
	}
	if got, err := s.TrialByJob(ctx, "w1"); err != nil || got.ID != t1.ID || got.State != TrialRunning || got.StartedAt == nil {
		t.Fatalf("by job: %+v %v", got, err)
	}
	if _, err := s.TrialByJob(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown job: %v", err)
	}
	e, es, sc, se := 3, 10, 0.8, 3
	if err := s.SetTrialProgress(ctx, t1.ID, &e, &es, &sc, &se, json.RawMessage(`{"util":50}`)); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetTrial(ctx, c.ID, t1.ID)
	if got.Epoch == nil || *got.Epoch != 3 || *got.Epochs != 10 || *got.Score != 0.8 || *got.ScoreEpoch != 3 || string(got.GPU) != `{"util": 50}` {
		t.Fatalf("progress: %+v gpu=%s", got, got.GPU)
	}
	if ok, err := s.FinishTrial(ctx, t1.ID, TrialDone, ""); err != nil || !ok {
		t.Fatalf("finish: ok=%v err=%v", ok, err)
	}
	if ok, _ := s.FinishTrial(ctx, t1.ID, TrialFailed, "late"); ok {
		t.Error("a finished trial must not finish again")
	}
	if _, err := s.FinishTrial(ctx, t2.ID, "running", ""); err == nil {
		t.Error("running is no final state")
	}
	if ok, err := s.FinishTrial(ctx, t2.ID, TrialRefused, "outside the space"); err != nil || !ok {
		t.Fatalf("refuse a declared trial: ok=%v err=%v", ok, err)
	}
	list, _ := s.ListTrials(ctx, c.ID)
	if len(list) != 3 || list[0].ID != single.ID || list[2].State != TrialRefused || list[2].Error != "outside the space" {
		t.Fatalf("list: %+v", list)
	}
	if _, err := s.GetTrial(ctx, newPlanChat(t, s).ID, t1.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("foreign trial: %v", err)
	}
}

func TestTrialSeries(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c := newPlanChat(t, s)
	x := newSearch(t, s, c.ID)
	tr, _ := s.DeclareTrial(ctx, Trial{ChatID: c.ID, SearchID: x.ID, Path: SearchPathTemplate})
	t0 := time.Now().UTC().Truncate(time.Second)
	lines := []TrialEpoch{{Epoch: 1, At: t0, Data: json.RawMessage(`{"val_acc":0.5}`)}, {Epoch: 2, At: t0.Add(time.Minute), Data: json.RawMessage(`{"val_acc":0.6}`)}}
	if err := s.AddTrialEpochs(ctx, tr.ID, lines); err != nil {
		t.Fatal(err)
	}
	// The same epoch again replaces the line.
	if err := s.AddTrialEpochs(ctx, tr.ID, []TrialEpoch{{Epoch: 2, At: t0.Add(2 * time.Minute), Data: json.RawMessage(`{"val_acc":0.7}`)}}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddTrialGPU(ctx, tr.ID, []TrialGPU{{At: t0, Util: 90, MemUsedMB: 1000, MemTotalMB: 16000}, {At: t0.Add(5 * time.Second), Util: 80, MemUsedMB: 1100, MemTotalMB: 16000}}); err != nil {
		t.Fatal(err)
	}
	ep, gpu, err := s.TrialSeries(ctx, tr.ID)
	if err != nil || len(ep) != 2 || len(gpu) != 2 {
		t.Fatalf("series: %d epochs, %d gpu, %v", len(ep), len(gpu), err)
	}
	var d map[string]float64
	if json.Unmarshal(ep[1].Data, &d) != nil || d["val_acc"] != 0.7 {
		t.Errorf("epoch 2 not replaced: %s", ep[1].Data)
	}
	if gpu[0].Util != 90 || gpu[1].MemUsedMB != 1100 || gpu[0].MemTotalMB != 16000 {
		t.Errorf("gpu: %+v", gpu)
	}
}

// Above the limit the older half is thinned out 2:1 and the newest lines stay.
func TestTrialSeriesThinned(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c := newPlanChat(t, s)
	tr, _ := s.DeclareTrial(ctx, Trial{ChatID: c.ID, Path: SearchPathNotebook})
	t0 := time.Now().UTC().Truncate(time.Second)
	var lines []TrialEpoch
	for i := 1; i <= MaxTrialEpochs+100; i++ {
		lines = append(lines, TrialEpoch{Epoch: i, At: t0.Add(time.Duration(i) * time.Second), Data: json.RawMessage(`{}`)})
	}
	if err := s.AddTrialEpochs(ctx, tr.ID, lines); err != nil {
		t.Fatal(err)
	}
	ep, _, _ := s.TrialSeries(ctx, tr.ID)
	if len(ep) > MaxTrialEpochs || len(ep) < MaxTrialEpochs/2 {
		t.Fatalf("after thinning %d lines", len(ep))
	}
	if ep[len(ep)-1].Epoch != MaxTrialEpochs+100 {
		t.Errorf("the newest line must stay, last is %d", ep[len(ep)-1].Epoch)
	}
}

func TestSearchEvents(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c := newPlanChat(t, s)
	x := newSearch(t, s, c.ID)
	if _, err := s.AddSearchEvent(ctx, SearchEvent{SearchID: x.ID, Kind: "approved"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddSearchEvent(ctx, SearchEvent{SearchID: x.ID, Kind: "hint", Data: json.RawMessage(`{"text":"try a lower rate"}`)}); err != nil {
		t.Fatal(err)
	}
	ev, err := s.SearchEvents(ctx, x.ID)
	if err != nil || len(ev) != 2 || ev[0].Kind != "approved" || ev[1].Kind != "hint" || ev[1].ID <= ev[0].ID {
		t.Fatalf("events: %+v %v", ev, err)
	}
}

// The approval carries the data of a search and gives it back.
func TestApprovalSearchData(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c := newPlanChat(t, s)
	a, err := s.CreateApproval(ctx, Approval{ChatID: c.ID, Kind: "search", Via: "cli", Name: "search", ContentType: "application/json",
		PendingKey: "-", Search: json.RawMessage(`{"title":"x"}`)})
	if err != nil || len(a.Search) == 0 {
		t.Fatalf("create: %+v %v", a, err)
	}
	got, err := s.GetApproval(ctx, a.ID)
	if err != nil || string(got.Search) != `{"title": "x"}` {
		t.Fatalf("get: %s %v", got.Search, err)
	}
	plain, _ := s.CreateApproval(ctx, Approval{ChatID: c.ID, Kind: "platform_write", Via: "cli", Name: "n", ContentType: "application/json", PendingKey: "-"})
	if len(plain.Search) != 0 {
		t.Errorf("an ordinary approval carries no search data: %s", plain.Search)
	}
}

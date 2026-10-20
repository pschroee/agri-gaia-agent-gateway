// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"agw/internal/platform"
	"agw/internal/search"
	"agw/internal/store"
	"agw/internal/watch"
)

// SearchView is a search as the API and the event "search" carry it: the row plus what it has used.
type SearchView struct {
	store.Search
	Used        search.Used `json:"used"`
	BestIdx     *int        `json:"best_idx,omitempty"`
	ComputeLeft int         `json:"compute_left"`
	// RunningTrial is the idx of the trial that is declared or running, if any.
	RunningTrial *int `json:"running_trial,omitempty"`
	// EndNote is the agent's own closing note of a search it ended itself (end_search); the UI shows it as not
	// verified.
	EndNote string `json:"end_note,omitempty"`
}

// TrialView is a trial as the API carries it: the row plus its duration and the end the watcher expects.
type TrialView struct {
	store.Trial
	DurationS float64    `json:"duration_s,omitempty"`
	ETA       *time.Time `json:"eta,omitempty"`
}

// SearchEventData is the payload of the event "search".
type searchStats struct {
	trials  []store.Trial // the trials of this search in the order they were declared
	used    search.Used
	best    *float64
	bestIdx *int
	open    *store.Trial // the declared or running trial
	obj     search.Objective
	budget  search.Budget
}

func isRun(t store.Trial) bool {
	switch t.State {
	case store.TrialRunning, store.TrialDone, store.TrialFailed, store.TrialStopped:
		return true
	}
	return false
}

// statsOf measures a search from its trials: the budget used, the best score, the patience counter.
func (m *Manager) statsOf(ctx context.Context, s store.Search) (searchStats, error) {
	var st searchStats
	obj, err := search.ParseObjective(s.Objective)
	if err != nil {
		return st, err
	}
	st.obj = obj
	if st.budget, err = search.ParseBudget(s.Budget); err != nil {
		return st, err
	}
	all, err := m.st.ListTrials(ctx, s.ChatID)
	if err != nil {
		return st, err
	}
	var results []search.Result
	now := time.Now()
	for _, t := range all {
		if t.SearchID != s.ID {
			continue
		}
		st.trials = append(st.trials, t)
	}
	for i := range st.trials {
		t := st.trials[i]
		if t.State == store.TrialDeclared || t.State == store.TrialRunning {
			st.open = &st.trials[i]
		}
		if !isRun(t) {
			continue
		}
		st.used.Runs++
		if t.StartedAt != nil {
			end := now
			if t.EndedAt != nil {
				end = *t.EndedAt
			}
			if end.After(*t.StartedAt) {
				st.used.ComputeMinutes += end.Sub(*t.StartedAt).Minutes()
			}
		}
		results = append(results, search.Result{Idx: t.Idx, Done: t.State == store.TrialDone, Score: t.Score})
	}
	if idx, score, ok := search.Winner(obj, results); ok {
		st.best, st.bestIdx = &score, &idx
	}
	st.used.SinceImprovement = search.SinceImprovement(obj, results)
	return st, nil
}

// admit asks the budget whether another trial may start now.
func (m *Manager) admit(ctx context.Context, s store.Search, st searchStats) (bool, string) {
	endsBy := time.Now().Add(24 * time.Hour)
	if s.EndsBy != nil {
		endsBy = *s.EndsBy
	}
	return search.Admit(st.budget, st.used, st.best, st.obj, time.Now(), endsBy, m.computeLeft(ctx, s.ChatID))
}

func (m *Manager) searchView(ctx context.Context, s store.Search) (SearchView, error) {
	st, err := m.statsOf(ctx, s)
	if err != nil {
		return SearchView{Search: s}, err
	}
	s = m.withFinalResult(ctx, s)
	v := SearchView{Search: s, Used: st.used, BestIdx: st.bestIdx, ComputeLeft: m.computeLeft(ctx, s.ChatID)}
	if s.State == store.SearchEnded && s.EndReason == endByAgent {
		v.EndNote = m.endNoteOf(ctx, s.ID)
	}
	if st.open != nil {
		idx := st.open.Idx
		v.RunningTrial = &idx
	}
	return v, nil
}

// publishSearch sends the whole search to the subscribers of its chat.
func (m *Manager) publishSearch(ctx context.Context, chatID, searchID string) {
	s, err := m.st.GetSearch(ctx, chatID, searchID)
	if err != nil {
		slog.Error("search event not published", "search", searchID, "error", err)
		return
	}
	v, err := m.searchView(ctx, s)
	if err != nil {
		slog.Error("search not measured", "search", searchID, "error", err)
	}
	m.publish(chatID, Event{Kind: "search", Data: v})
}

func (m *Manager) publishTrial(ctx context.Context, chatID, trialID string) {
	t, err := m.st.GetTrial(ctx, chatID, trialID)
	if err != nil {
		return
	}
	m.publish(chatID, Event{Kind: "trial", Data: m.trialView(ctx, t, nil)})
}

// trialView adds the duration and the expected end (from the observed job) to a trial.
func (m *Manager) trialView(ctx context.Context, t store.Trial, jobs map[string]store.WatchJob) TrialView {
	v := TrialView{Trial: t}
	if t.StartedAt != nil {
		end := time.Now()
		if t.EndedAt != nil {
			end = *t.EndedAt
		}
		v.DurationS = end.Sub(*t.StartedAt).Seconds()
	}
	if t.JobID != "" && t.State == store.TrialRunning {
		if j, ok := jobs[t.JobID]; ok {
			v.ETA = j.ETA
		} else if j, err := m.st.GetWatchJob(ctx, t.ChatID, t.JobID); err == nil && j.State == watch.StateRunning {
			v.ETA = j.ETA
		}
	}
	return v
}

// addSearchEvent writes a line of the record of a search; a failure is logged.
func (m *Manager) addSearchEvent(ctx context.Context, searchID, kind string, data any) {
	raw, err := json.Marshal(data)
	if err != nil {
		raw = []byte("{}")
	}
	if _, err := m.st.AddSearchEvent(ctx, store.SearchEvent{SearchID: searchID, Kind: kind, Data: raw}); err != nil {
		slog.Error("search event not recorded", "search", searchID, "kind", kind, "error", err)
	}
}

// Searches and Trials are the reads of the API: the book of trials of a chat.
func (m *Manager) Searches(ctx context.Context, chatID string) ([]SearchView, error) {
	list, err := m.st.ListSearches(ctx, chatID)
	if err != nil {
		return nil, err
	}
	out := make([]SearchView, 0, len(list))
	for _, s := range list {
		v, _ := m.searchView(ctx, s)
		out = append(out, v)
	}
	return out, nil
}

func (m *Manager) Trials(ctx context.Context, chatID string) ([]TrialView, error) {
	list, err := m.st.ListTrials(ctx, chatID)
	if err != nil {
		return nil, err
	}
	jobs := map[string]store.WatchJob{}
	if js, err := m.st.ChatWatchJobs(ctx, chatID); err == nil {
		for _, j := range js {
			jobs[j.ID] = j
		}
	}
	out := make([]TrialView, 0, len(list))
	for _, t := range list {
		out = append(out, m.trialView(ctx, t, jobs))
	}
	return out, nil
}

// endNoteOf reads the note the agent gave when it ended the search (the record's "ended" line).
func (m *Manager) endNoteOf(ctx context.Context, searchID string) string {
	events, err := m.st.SearchEvents(ctx, searchID)
	if err != nil {
		return ""
	}
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind != "ended" {
			continue
		}
		var d struct {
			Note string `json:"note"`
		}
		if json.Unmarshal(events[i].Data, &d) == nil {
			return platform.VisibleControls(d.Note)
		}
		return ""
	}
	return ""
}

// FinalResult is what the final test of a search measured, copied from its check into holdout.result.
type FinalResult struct {
	State    string   `json:"state"` // finished | failed
	Accuracy *float64 `json:"accuracy,omitempty"`
	Images   int      `json:"images,omitempty"`
	Verdict  string   `json:"verdict,omitempty"`
}

// withFinalResult fills holdout.result of a search whose final test has ended, from the check the search names
// (holdout.check_id). The result is stored once, so that later reads and the export carry it too. A search without
// a finished check is returned as it is.
func (m *Manager) withFinalResult(ctx context.Context, s store.Search) store.Search {
	if len(s.Holdout) == 0 {
		return s
	}
	var ho SearchHoldout
	if json.Unmarshal(s.Holdout, &ho) != nil || ho.CheckID == "" || len(ho.Result) > 0 {
		return s
	}
	row, err := m.st.Assessment(ctx, s.ChatID, ho.CheckID)
	if err != nil || (row.State != store.AssessFinished && row.State != store.AssessFailed) {
		return s
	}
	res := FinalResult{State: row.State}
	var rep struct {
		Figures struct {
			Overall struct {
				Accuracy *float64 `json:"accuracy"`
			} `json:"overall"`
			Accuracy *float64 `json:"accuracy"`
		} `json:"figures"`
		MeasuredImages int    `json:"measured_images"`
		Verdict        string `json:"verdict"`
	}
	if len(row.Report) > 0 && json.Unmarshal(row.Report, &rep) == nil {
		res.Accuracy = rep.Figures.Overall.Accuracy
		if res.Accuracy == nil {
			res.Accuracy = rep.Figures.Accuracy
		}
		res.Images, res.Verdict = rep.MeasuredImages, platform.VisibleControls(rep.Verdict)
	}
	raw, err := json.Marshal(res)
	if err != nil {
		return s
	}
	ho.Result = raw
	out, err := json.Marshal(ho)
	if err != nil {
		return s
	}
	if err := m.st.SetSearchHoldout(ctx, s.ID, out); err != nil {
		slog.Warn("final result of a search not stored", "search", s.ID, "error", err)
	}
	s.Holdout = out
	return s
}

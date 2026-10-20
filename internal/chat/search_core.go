// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

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
	v := SearchView{Search: s, Used: st.used, BestIdx: st.bestIdx, ComputeLeft: m.computeLeft(ctx, s.ChatID)}
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

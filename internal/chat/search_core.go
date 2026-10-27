// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"
	"strings"
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
	// Name says what ran (the entry file of a notebook run, the training container of the template); Link is the
	// address of the run, as the job gives it (issue #194).
	Name string `json:"name,omitempty"`
	Link string `json:"link,omitempty"`
	// PlanID and PlanStep name the plan step whose call started the run, as the job says (issue #227).
	PlanID   string `json:"plan_id,omitempty"`
	PlanStep int    `json:"plan_step,omitempty"`
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
		if t.State != store.TrialRunning {
			st.used.Finished++
		}
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
	s = m.withFinalResult(ctx, m.withLinkedFinalCheck(ctx, s))
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

// trialView adds the duration, the expected end (from the observed job), the name and the link to a trial. The
// duration is the run's own time (markers of its starter) when the job has it, else the job's times for a
// single trial, else the trial's own times; the run card counts the same way.
func (m *Manager) trialView(ctx context.Context, t store.Trial, jobs map[string]store.WatchJob) TrialView {
	v := TrialView{Trial: t}
	if t.StartedAt != nil {
		end := time.Now()
		if t.EndedAt != nil {
			end = *t.EndedAt
		}
		v.DurationS = end.Sub(*t.StartedAt).Seconds()
	}
	var job *store.WatchJob
	if t.JobID != "" {
		if j, ok := jobs[t.JobID]; ok {
			job = &j
		} else if j, err := m.st.GetWatchJob(ctx, t.ChatID, t.JobID); err == nil {
			job = &j
		}
	}
	if job != nil {
		if t.State == store.TrialRunning && job.State == watch.StateRunning {
			v.ETA = job.ETA
		}
		if job.RunStartedAt != nil {
			// The run's own clock (issue #203): the same two instants as the job line and the run card.
			end := time.Now()
			if job.RunEndedAt != nil {
				end = *job.RunEndedAt
			} else if job.EndedAt != nil {
				end = *job.EndedAt
			}
			v.DurationS = end.Sub(*job.RunStartedAt).Seconds()
		} else if t.SearchID == "" && !job.StartedAt.IsZero() {
			end := time.Now()
			if job.EndedAt != nil {
				end = *job.EndedAt
			}
			v.DurationS = end.Sub(job.StartedAt).Seconds()
		}
		if t.Path == store.SearchPathNotebook {
			v.Link = job.Link
		}
		v.PlanID, v.PlanStep = job.PlanID, job.StepIdx
	}
	switch t.Path {
	case store.SearchPathNotebook:
		if t.Ref != "" {
			if r, err := m.st.GetNotebookRun(ctx, t.ChatID, t.Ref); err == nil {
				v.Name = r.Entry
			}
		}
	case store.SearchPathTemplate:
		if t.Ref != "" {
			v.Name = "training container " + t.Ref
		}
		var b SearchBase
		if len(t.Subject) > 0 && json.Unmarshal(t.Subject, &b) == nil {
			v.Name = subjectText(b)
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

// withLinkedFinalCheck links the final test of an ended template search that still says "pending" to the check that
// ran it outside the search's own matching (B29): a final check (final=true) of the winner's model on the search's
// held-back set, started after the search ended. Until B29 a final test run as a plan step was not recorded, and the
// search of Demo 12 showed "Final test pending" for good; reading the search records the link once, with a "final"
// event. A check that did not run the held-back images (#181) is not the final test.
func (m *Manager) withLinkedFinalCheck(ctx context.Context, s store.Search) store.Search {
	if s.State != store.SearchEnded || s.WinnerIdx == nil || s.EndedAt == nil || s.Path != store.SearchPathTemplate || len(s.Holdout) == 0 {
		return s
	}
	var ho SearchHoldout
	if json.Unmarshal(s.Holdout, &ho) != nil || !ho.Available || ho.FinalState != "pending" || ho.CheckID != "" || ho.EvalDataset == nil {
		return s
	}
	trials, err := m.st.ListTrials(ctx, s.ChatID)
	if err != nil {
		return s
	}
	winnerRef := ""
	for _, t := range trials {
		if t.SearchID == s.ID && t.Idx == *s.WinnerIdx && t.State == store.TrialDone {
			winnerRef = t.Ref
		}
	}
	if winnerRef == "" {
		return s
	}
	checks, err := m.st.Assessments(ctx, s.ChatID)
	if err != nil {
		return s
	}
	var found *store.Assessment
	for i := range checks {
		a := checks[i]
		if !a.Final || !a.HoldoutMeasured || a.EvalDatasetID != ho.EvalDataset.ID || a.CreatedAt.Before(*s.EndedAt) ||
			!strings.HasPrefix(a.Subject, "model:") {
			continue
		}
		id, err := strconv.ParseInt(strings.TrimPrefix(a.Subject, "model:"), 10, 64)
		if err != nil {
			continue
		}
		o, err := m.st.ModelOriginOf(ctx, s.ChatID, id)
		if err != nil || strconv.FormatInt(o.TrainContainerID, 10) != winnerRef {
			continue
		}
		if found == nil || a.CreatedAt.After(found.CreatedAt) {
			found = &a
		}
	}
	if found == nil {
		return s
	}
	ho.FinalState, ho.CheckID = "started", found.ID
	out, err := json.Marshal(ho)
	if err != nil {
		return s
	}
	if err := m.st.SetSearchHoldout(ctx, s.ID, out); err != nil {
		slog.Warn("final check of a search not linked", "search", s.ID, "error", err)
		return s
	}
	m.addSearchEvent(ctx, s.ID, "final", map[string]any{"check_id": found.ID, "trial": s.WinnerIdx, "linked": "read"})
	s.Holdout = out
	return s
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
		HoldoutNotRun  bool   `json:"holdout_not_run"`
	}
	if len(row.Report) > 0 && json.Unmarshal(row.Report, &rep) == nil && rep.HoldoutNotRun && row.State == store.AssessFinished {
		// #181: the check ended before the hold-out images ran through the model, so the final test is not
		// used up. The search goes back to "pending" and the agent may run the check again.
		ho.FinalState, ho.CheckID = "pending", ""
		ho.FinalRetries++
		out, err := json.Marshal(ho)
		if err != nil {
			return s
		}
		if err := m.st.SetSearchHoldout(ctx, s.ID, out); err != nil {
			slog.Warn("final retry of a search not stored", "search", s.ID, "error", err)
			return s
		}
		s.Holdout = out
		m.publishSearch(ctx, s.ChatID, s.ID)
		return s
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

// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"agw/internal/platform"
	"agw/internal/search"
	"agw/internal/store"
	"agw/internal/watch"
)

func init() { registerJobEnded(searchTrialEnded) }

// Reasons a search ends.
const (
	endStartFailed = "start_failed"
	endStopped     = "stopped_by_user"
	endAdopted     = "adopted"
	endByAgent     = "ended_by_agent"
)

// endReasonText is the reason in words, as the agent and the user read it.
func endReasonText(reason string) string {
	switch reason {
	case "target_reached":
		return "the target was reached"
	case "budget_runs":
		return "the number of trials is used up"
	case "budget_time":
		return "the compute time is used up"
	case "patience":
		return "no improvement within the patience"
	case "time_limit":
		return "the time limit of the search"
	case "compute_limit":
		return "no compute calls left in this chat"
	case endStartFailed:
		return "the platform could not start the trials (the copy or the start failed twice in a row)"
	case endStopped:
		return "the user stopped the search"
	case endAdopted:
		return "the user adopted a trial"
	case endByAgent:
		return "the agent ended the search"
	}
	return reason
}

// searchTrialEnded is the hook of the end of a job: when the job is a trial of a search, the trial ends, and the
// search either goes on (the agent hears the result) or ends with the reason (S5). The figures were stored by the
// progress hook of the same poll.
func searchTrialEnded(ctx context.Context, m *Manager, j store.WatchJob, p watch.Progress) {
	t, err := m.st.TrialByJob(ctx, j.ID)
	if err != nil || t.SearchID == "" {
		return
	}
	state, errText := store.TrialDone, ""
	switch {
	case t.StoppedBy != "":
		state = store.TrialStopped
	case p.State == watch.StateFailed:
		state, errText = store.TrialFailed, platform.RedactText(oneLine(p.Message))
	}
	if ok, err := m.st.FinishTrial(ctx, t.ID, state, errText); err != nil || !ok {
		return // already ended
	}
	m.publishTrial(ctx, t.ChatID, t.ID)
	s, err := m.st.GetSearch(ctx, t.ChatID, t.SearchID)
	if err != nil || s.State != store.SearchRunning {
		m.publishSearch(ctx, t.ChatID, t.SearchID)
		return
	}
	t, _ = m.st.GetTrial(ctx, t.ChatID, t.ID)
	st, err := m.statsOf(ctx, s)
	if err != nil {
		slog.Error("search not measured at the end of a trial", "search", s.ID, "error", err)
		return
	}
	head := fmt.Sprintf("Trial %d of the training search %q (%s) %s", t.Idx, s.Title, s.ID, state)
	if t.Score != nil {
		head += fmt.Sprintf(": %s %s", st.obj.Metric, strconv.FormatFloat(*t.Score, 'g', 6, 64))
		if t.ScoreEpoch != nil {
			head += fmt.Sprintf(" at epoch %d", *t.ScoreEpoch)
		}
	}
	head += "."
	if w := m.cpuRunLine(t); w != "" {
		head += " " + w
	}
	if startFailedTwice(st.trials) {
		m.finishSearch(ctx, s, endStartFailed, nil, "")
		return
	}
	if ok, reason := m.admit(ctx, s, st); !ok {
		m.finishSearch(ctx, s, reason, nil, "")
		return
	}
	if st.bestIdx != nil && st.best != nil {
		head += fmt.Sprintf(" Best so far: trial %d (%s %s).", *st.bestIdx, st.obj.Metric, strconv.FormatFloat(*st.best, 'g', 6, 64))
	}
	head += " " + usedText(st) + " Read {{tool:search_status}}, then declare the next trial with {{tool:search_next}}, or end the search with {{tool:end_search}}."
	body := ""
	if errText != "" {
		body = errText
	}
	m.searchNote(ctx, s, head, body)
	m.publishSearch(ctx, t.ChatID, s.ID)
}

// startFailedTwice reports whether the last two trials that ran both failed before their first epoch: the
// platform cannot start trials, and the gateway does not try again by itself (issue #168).
func startFailedTwice(trials []store.Trial) bool {
	n := 0
	for i := len(trials) - 1; i >= 0 && n < 2; i-- {
		t := trials[i]
		if !isRun(t) {
			continue
		}
		if t.State != store.TrialFailed || t.Epoch != nil {
			return false
		}
		n++
	}
	return n == 2
}

func usedText(st searchStats) string {
	s := fmt.Sprintf("Used: %d", st.used.Runs)
	if st.budget.MaxRuns != nil {
		s += fmt.Sprintf(" of %d", *st.budget.MaxRuns)
	}
	return s + " trials."
}

// searchNote queues a note about a search for the agent and wakes it (within the wake limit of the search).
func (m *Manager) searchNote(ctx context.Context, s store.Search, summary, body string) {
	n := systemNote{Type: store.NoteSearch, Refs: []string{s.ID}, Summary: m.RenderNote(ctx, s.ChatID, summary), Body: m.RenderNote(ctx, s.ChatID, body)}
	if _, err := m.st.EnqueueSystem(ctx, s.ChatID, n.Type, n.Refs, n.Text()); err != nil {
		slog.Error("note about a search not queued", "search", s.ID, "error", err)
		return
	}
	m.publishQueue(ctx, s.ChatID, "queued", nil, "")
	m.wakeForNote(s.ChatID, s.ID)
}

// finishSearch ends a running search with a reason (S5). The winner is the best finished trial unless one is given
// (a trial the user adopted). The final test is prepared for it. byAgentNote is the agent's own note for the record.
// It returns whether this call ended it, and the text for the agent.
func (m *Manager) finishSearch(ctx context.Context, s store.Search, reason string, winner *int, agentNote string) (bool, string) {
	st, err := m.statsOf(ctx, s)
	if err != nil {
		slog.Error("search not measured at its end", "search", s.ID, "error", err)
		return false, ""
	}
	if winner == nil {
		winner = st.bestIdx
	}
	ended, err := m.st.EndSearch(ctx, s.ID, reason, winner)
	if err != nil || !ended {
		return false, ""
	}
	// Whatever is still open ends with the search.
	if st.open != nil {
		if st.open.State == store.TrialDeclared {
			_, _ = m.st.FinishTrial(ctx, st.open.ID, store.TrialStopped, "the search ended")
			m.publishTrial(ctx, s.ChatID, st.open.ID)
		}
	}
	var ho SearchHoldout
	_ = json.Unmarshal(s.Holdout, &ho)
	ho.FinalState = "none"
	var wt *store.Trial
	if winner != nil {
		for i := range st.trials {
			if st.trials[i].Idx == *winner {
				wt = &st.trials[i]
			}
		}
		if ho.Available && wt != nil {
			ho.FinalState = "pending"
		}
	}
	if raw, err := json.Marshal(ho); err == nil {
		_ = m.st.SetSearchHoldout(ctx, s.ID, raw)
	}
	ev := map[string]any{"reason": reason, "winner": winner}
	if agentNote != "" {
		ev["note"] = agentNote
	}
	if wt != nil && wt.Score != nil {
		ev["score"] = *wt.Score
	}
	m.addSearchEvent(ctx, s.ID, "ended", ev)
	m.searchStepEnded(ctx, s, wt)
	text := m.endText(s, st.obj, reason, wt, ho)
	if _, idx, err := m.st.PlanStepOfSearch(ctx, s.ID); err == nil && wt != nil {
		// A search inside a plan (B19): the plan's next steps are the way on, exactly as approved; the free hints
		// above (eval_dataset_id, a new plan) would make the calls differ from the steps.
		text = planSearchEndText(s, st.obj, reason, wt, idx)
	}
	if reason != endByAgent {
		m.searchNote(ctx, s, text, "")
	}
	m.publishSearch(ctx, s.ChatID, s.ID)
	slog.Info("search ended", "chat", s.ChatID, "search", s.ID, "reason", reason, "winner", winner)
	return true, text
}

// endText is the message at the end: the reason, the winner and the exact calls of the final test.
func (m *Manager) endText(s store.Search, obj search.Objective, reason string, wt *store.Trial, ho SearchHoldout) string {
	var b strings.Builder
	fmt.Fprintf(&b, "The training search ended: %s.", endReasonText(reason))
	if wt == nil {
		b.WriteString(" No trial finished, so there is no winner.")
		return b.String()
	}
	b.WriteString(fmt.Sprintf(" Winner: trial %d", wt.Idx))
	if wt.Score != nil {
		b.WriteString(fmt.Sprintf(" (%s %s)", obj.Metric, strconv.FormatFloat(*wt.Score, 'g', 6, 64)))
	}
	b.WriteString(".")
	b.WriteString(" " + curvesLineSearch(s.ID))
	if ho.FinalState != "pending" {
		b.WriteString(" There is no held-back test set (the dataset was not split first); report the validation value only.")
		return b.String()
	}
	eval := ""
	if ho.EvalDataset != nil {
		eval = " eval_dataset_id=" + strconv.FormatInt(ho.EvalDataset.ID, 10)
	}
	if s.Path == store.SearchPathTemplate {
		fmt.Fprintf(&b, " Now run the final test once: {{tool:create_model_from_training}} for training container %s, then {{tool:assess_model}} with that model_id and final=true,%s.", wt.Ref, eval)
	} else {
		fmt.Fprintf(&b, " Now run the final test once: {{tool:notebook_fetch}} the model file of run %s, then {{tool:assess_model}} with its path and final=true,%s.", wt.Ref, eval)
	}
	b.WriteString(" If the user's task goes on after the final test (rename, inference image, deployment, model card), propose the remaining steps as ONE plan with {{tool:propose_plan}}; the model takeover of the winner and its final test are covered by this search.")
	if s.Path != store.SearchPathTemplate {
		b.WriteString(" The check takes class names and preprocessing from outputs/model.json of that run.")
	}
	return b.String()
}

// planSearchEndText is the note at the end of a search that is a plan step (B19, demo 5 of 27.10.2026): no free
// wording of the final test – the agent named eval_dataset_id because the free text suggested it, and the call
// differed from the plan's step.
func planSearchEndText(s store.Search, obj search.Objective, reason string, wt *store.Trial, idx int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "The training search ended: %s.", endReasonText(reason))
	b.WriteString(fmt.Sprintf(" Winner: trial %d", wt.Idx))
	if wt.Score != nil {
		b.WriteString(fmt.Sprintf(" (%s %s)", obj.Metric, strconv.FormatFloat(*wt.Score, 'g', 6, 64)))
	}
	b.WriteString(".")
	b.WriteString(" " + curvesLineSearch(s.ID))
	fmt.Fprintf(&b, " This search was step %d of the approved plan; {{step%d.id}} is now training container %s.", idx, idx, wt.Ref)
	b.WriteString(" Go on with the plan's next steps exactly as approved, with the same arguments and nothing added" +
		" (the final test takes the held-back set by itself; do not add eval_dataset_id). Do not propose a new plan.")
	return b.String()
}

// searchTick is the clock of the searches (S5), called with every round of the watcher: it ends a search whose time
// is over and stops the running trial of a search that has used its compute minutes.
func (m *Manager) searchTick(ctx context.Context) {
	list, err := m.st.OpenSearches(ctx)
	if err != nil {
		slog.Warn("running searches not readable", "error", err)
		return
	}
	for _, s := range list {
		st, err := m.statsOf(ctx, s)
		if err != nil {
			continue
		}
		timeUp := s.EndsBy != nil && !time.Now().Before(*s.EndsBy)
		budgetUp := st.budget.MaxComputeMinutes != nil && st.used.ComputeMinutes >= float64(*st.budget.MaxComputeMinutes)
		if !timeUp && !budgetUp {
			continue
		}
		if st.open != nil && st.open.State == store.TrialRunning && st.open.JobID != "" {
			by := "budget_time"
			if timeUp {
				by = "time_limit"
			}
			m.stopTrial(ctx, *st.open, by)
			if timeUp {
				m.finishSearch(ctx, s, "time_limit", nil, "")
			}
			continue
		}
		if timeUp {
			m.finishSearch(ctx, s, "time_limit", nil, "")
		} else if st.open == nil {
			m.finishSearch(ctx, s, "budget_time", nil, "")
		}
	}
}

// stopTrial stops the job of a running trial and records who did.
func (m *Manager) stopTrial(ctx context.Context, t store.Trial, by string) {
	if err := m.st.SetTrialStoppedBy(ctx, t.ID, by); err != nil {
		slog.Error("who stopped a trial not recorded", "trial", t.ID, "error", err)
	}
	if _, err := m.StopJob(ctx, t.ChatID, t.JobID); err != nil {
		slog.Warn("trial not stopped", "trial", t.ID, "job", t.JobID, "error", err)
	}
}

// searchWakeNotes names the search whose notes are all that is open, or "" (like planStepWake).
func searchWakeNotes(open []store.QueueEntry) string {
	id := ""
	for _, e := range open {
		if e.Kind != store.QueueSystem || e.Note != store.NoteSearch || len(e.Refs) == 0 {
			return ""
		}
		if id == "" {
			id = e.Refs[0]
		} else if e.Refs[0] != id {
			return ""
		}
	}
	return id
}

// searchWakeLimit counts a wake-up that carries the notes of a search against the search's own limit,
// min(50, 3·max_runs+3) turns (issue #168): the search is the user's own budget, and the gateway-wide limit of turns
// without the user would stop a long search early. handled is false when the search is unknown.
func (m *Manager) searchWakeLimit(ctx context.Context, chatID string, open []store.QueueEntry) (ev AutoHeldEvent, limited, handled bool) {
	id := searchWakeNotes(open)
	if id == "" {
		return AutoHeldEvent{}, false, false
	}
	s, err := m.st.GetSearch(ctx, chatID, id)
	if err != nil {
		return AutoHeldEvent{}, false, false
	}
	n, err := m.st.IncSearchAutoTurns(ctx, id)
	if err != nil {
		return AutoHeldEvent{Reason: HoldAutoTurns}, true, true
	}
	runs := m.computeMax()
	if b, err := search.ParseBudget(s.Budget); err == nil && b.MaxRuns != nil {
		runs = *b.MaxRuns
	}
	limit := maxAutoTurns(runs)
	if s.State == store.SearchRunning && n > limit {
		slog.Warn("turn limit of a training search used up, note stays queued", "chat", chatID, "search", id, "turns", n, "limit", limit)
		return AutoHeldEvent{Reason: HoldAutoTurns, Limit: limit, Count: n}, true, true
	}
	return AutoHeldEvent{}, false, true
}

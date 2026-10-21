// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"agw/internal/platform"
	"agw/internal/search"
	"agw/internal/store"
)

// The user's four controls of a running search (S6): stop, change the budget, give a hint, adopt a trial. They are
// routes of the API, never tools of the agent; each is one line in the record of the search, an event, and a note
// to the agent.

func (m *Manager) controlled(ctx context.Context, chatID, searchID string) (store.Search, error) {
	m.userActive(chatID)
	s, err := m.st.GetSearch(ctx, chatID, searchID)
	if err != nil {
		return s, err
	}
	if s.State != store.SearchRunning {
		return s, ErrSearchNotRunning
	}
	return s, nil
}

func (m *Manager) viewOf(ctx context.Context, chatID, searchID string) (SearchView, error) {
	s, err := m.st.GetSearch(ctx, chatID, searchID)
	if err != nil {
		return SearchView{}, err
	}
	return m.searchView(ctx, s)
}

// StopSearch stops the running trial (scope "trial") or the whole search (scope "search").
func (m *Manager) StopSearch(ctx context.Context, chatID, searchID, scope string) (SearchView, error) {
	if scope != "trial" && scope != "search" {
		return SearchView{}, &SearchInputError{`scope must be "trial" or "search"`}
	}
	s, err := m.controlled(ctx, chatID, searchID)
	if err != nil {
		return SearchView{}, err
	}
	st, err := m.statsOf(ctx, s)
	if err != nil {
		return SearchView{}, err
	}
	if scope == "trial" && st.open == nil {
		return SearchView{}, &SearchInputError{"no trial is declared or running"}
	}
	idx := 0
	if st.open != nil {
		idx = st.open.Idx
		switch st.open.State {
		case store.TrialRunning:
			if st.open.JobID != "" {
				m.stopTrial(ctx, *st.open, store.StoppedByUser)
			}
		case store.TrialDeclared:
			_, _ = m.st.FinishTrial(ctx, st.open.ID, store.TrialStopped, "stopped by the user")
			_ = m.st.SetTrialStoppedBy(ctx, st.open.ID, store.StoppedByUser)
			m.publishTrial(ctx, chatID, st.open.ID)
		}
	}
	m.addSearchEvent(ctx, searchID, "stopped", map[string]any{"scope": scope, "trial": idx})
	if scope == "search" {
		m.finishSearch(ctx, s, endStopped, nil, "")
	} else {
		m.searchNote(ctx, s, fmt.Sprintf("The user stopped trial %d of the training search %q (%s). Read {{tool:search_status}}, then declare the next trial or end the search.", idx, s.Title, s.ID), "")
	}
	m.publishSearch(ctx, chatID, searchID)
	return m.viewOf(ctx, chatID, searchID)
}

// SetSearchBudget replaces the budget of a running search (body: the four fields, null = not set). max_runs can
// be raised only as far as the trials so far plus the compute calls the chat has left (issue #168, G10).
func (m *Manager) SetSearchBudget(ctx context.Context, chatID, searchID string, body []byte) (SearchView, error) {
	s, err := m.controlled(ctx, chatID, searchID)
	if err != nil {
		return SearchView{}, err
	}
	b, err := search.ParseBudget(body)
	if err != nil {
		return SearchView{}, &SearchInputError{err.Error()}
	}
	st, err := m.statsOf(ctx, s)
	if err != nil {
		return SearchView{}, err
	}
	if b.MaxRuns != nil {
		if limit := st.used.Runs + m.computeLeft(ctx, chatID); *b.MaxRuns > limit {
			return SearchView{}, &SearchInputError{fmt.Sprintf("max_runs can be at most %d: the %d trials so far plus the compute calls this chat has left", limit, st.used.Runs)}
		}
	}
	if ok, err := m.st.UpdateSearchBudget(ctx, searchID, search.MarshalBudget(b)); err != nil {
		return SearchView{}, err
	} else if !ok {
		return SearchView{}, ErrSearchNotRunning
	}
	m.addSearchEvent(ctx, searchID, "budget_changed", map[string]any{"from": st.budget, "to": b})
	s, _ = m.st.GetSearch(ctx, chatID, searchID)
	st.budget = b
	// A budget lowered below what is used ends the search at once when no trial runs; a running trial ends it.
	if ok, reason := m.admit(ctx, s, st); !ok && st.open == nil {
		m.finishSearch(ctx, s, reason, nil, "")
	} else {
		m.searchNote(ctx, s, fmt.Sprintf("The user changed the budget of the training search %q (%s): now %s.", s.Title, s.ID, budgetText(b)), "")
	}
	m.publishSearch(ctx, chatID, searchID)
	return m.viewOf(ctx, chatID, searchID)
}

// HintSearch gives the agent a hint: a message of the user, marked with the search it is for.
func (m *Manager) HintSearch(ctx context.Context, chatID, searchID, text string) (SearchView, error) {
	s, err := m.controlled(ctx, chatID, searchID)
	if err != nil {
		return SearchView{}, err
	}
	text = strings.TrimSpace(text)
	if text == "" || !utf8.ValidString(text) || utf8.RuneCountInString(text) > search.NoteMaxRunes {
		return SearchView{}, &SearchInputError{fmt.Sprintf("text: between 1 and %d characters", search.NoteMaxRunes)}
	}
	m.addSearchEvent(ctx, searchID, "hint", map[string]any{"text": platform.VisibleControls(text)})
	if _, err := m.SendSearchHint(ctx, chatID, s.ID, text); err != nil {
		return SearchView{}, err
	}
	m.publishSearch(ctx, chatID, searchID)
	return m.viewOf(ctx, chatID, searchID)
}

// AdoptTrial makes a finished trial the winner now: a running trial is stopped, the search ends as "adopted".
func (m *Manager) AdoptTrial(ctx context.Context, chatID, searchID string, idx int) (SearchView, error) {
	s, err := m.controlled(ctx, chatID, searchID)
	if err != nil {
		return SearchView{}, err
	}
	st, err := m.statsOf(ctx, s)
	if err != nil {
		return SearchView{}, err
	}
	var pick *store.Trial
	for i := range st.trials {
		if st.trials[i].Idx == idx {
			pick = &st.trials[i]
		}
	}
	if pick == nil || pick.State != store.TrialDone {
		return SearchView{}, &SearchInputError{fmt.Sprintf("trial %d is not a finished trial of this search", idx)}
	}
	if st.open != nil && st.open.State == store.TrialRunning && st.open.JobID != "" {
		m.stopTrial(ctx, *st.open, store.StoppedByUser)
	}
	m.addSearchEvent(ctx, searchID, "adopted", map[string]any{"trial": idx})
	m.finishSearch(ctx, s, endAdopted, &idx, "")
	return m.viewOf(ctx, chatID, searchID)
}

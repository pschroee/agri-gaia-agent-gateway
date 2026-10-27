// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"agw/internal/plan"
	"agw/internal/platform"
	"agw/internal/search"
	"agw/internal/store"
)

// searchDoer answers the paths under /_agw/search (G1.5): search_next, search_status and end_search. They change
// nothing on the platform; the gateway's own bookkeeping needs no approval, but the delegation has been checked.
type searchDoer struct{ m *Manager }

// SearchDoer is the doer main registers for platform.SearchPrefix.
func (m *Manager) SearchDoer() VirtualDoer { return searchDoer{m} }

var searchPathRe = regexp.MustCompile(`^` + regexp.QuoteMeta(platform.SearchPrefix) + `/(s[0-9a-f]{12})(/trials|/end)?$`)

// isSearchControl reports whether a call is one of the search's own paths below the prefix.
func isSearchControl(req platform.Request) bool {
	return searchPathRe.MatchString(req.Path) || req.Path == platform.CurvesPath
}

func (d searchDoer) Do(ctx context.Context, chatID string, req platform.Request) (platform.Result, error) {
	if req.Path == platform.CurvesPath {
		if req.Method != http.MethodGet {
			return searchErr("method not allowed for " + req.Path)
		}
		return d.m.trainingCurves(ctx, chatID, req.Query)
	}
	mm := searchPathRe.FindStringSubmatch(req.Path)
	if mm == nil {
		return searchErr("not a training search path")
	}
	id := mm[1]
	switch {
	case mm[2] == "" && req.Method == http.MethodGet:
		return d.m.searchStatusText(ctx, chatID, id)
	case mm[2] == "/trials" && req.Method == http.MethodPost:
		return d.m.searchNext(ctx, chatID, id, req.Body)
	case mm[2] == "/end" && req.Method == http.MethodPost:
		return d.m.searchEndByAgent(ctx, chatID, id, req.Body)
	}
	return searchErr("method not allowed for " + req.Path)
}

// runSearch loads the running search of a chat; the refusal is an answer for the agent.
func (m *Manager) runSearch(ctx context.Context, chatID, id string) (store.Search, string, error) {
	s, err := m.st.GetSearch(ctx, chatID, id)
	if err != nil {
		if err == store.ErrNotFound {
			return s, "no such training search in this chat", nil
		}
		return s, "", err
	}
	if s.State != store.SearchRunning {
		msg := fmt.Sprintf("search %s is not running (state %s)", id, s.State)
		if s.EndReason != "" {
			msg += ", it ended: " + endReasonText(s.EndReason)
		}
		return s, msg, nil
	}
	return s, "", nil
}

type nextBody struct {
	Params json.RawMessage `json:"params"`
	Note   string          `json:"note"`
}

// searchNext declares the next trial (S3): the budget, the space, no repetition, one open trial at a time.
func (m *Manager) searchNext(ctx context.Context, chatID, id string, body []byte) (platform.Result, error) {
	s, msg, err := m.runSearch(ctx, chatID, id)
	if err != nil {
		return platform.Result{}, err
	}
	if msg != "" {
		return searchErr(msg)
	}
	canon, err := plan.Canonical(body)
	var nb nextBody
	if err == nil {
		err = strictJSON(canon, &nb)
	}
	if err != nil || len(nb.Params) == 0 {
		return searchErr("{{tool:search_next}} takes params (an object) and note")
	}
	note := strings.TrimSpace(nb.Note)
	if !utf8.ValidString(note) || utf8.RuneCountInString(note) > search.NoteMaxRunes {
		return searchErr(fmt.Sprintf("note: at most %d characters", search.NoteMaxRunes))
	}
	note = platform.VisibleControls(note)
	st, err := m.statsOf(ctx, s)
	if err != nil {
		return platform.Result{}, err
	}
	// Repeating the open declaration is the same answer again (the agent may have lost it).
	if st.open != nil && st.open.State == store.TrialDeclared && search.SameJSON(st.open.Params, nb.Params) {
		return m.nextAnswer(s, *st.open)
	}
	if st.open != nil && st.open.State == store.TrialRunning {
		return searchErr(fmt.Sprintf("trial %d is still running; wait for it to end, the gateway tells you and wakes you", st.open.Idx))
	}
	if ok, reason := m.admit(ctx, s, st); !ok {
		return searchErr(search.ExhaustedText(reason))
	}
	var space search.Space
	if err := json.Unmarshal(s.Space, &space); err != nil {
		return platform.Result{}, err
	}
	params, err := space.Check(nb.Params)
	if err != nil {
		// A trial outside the space is kept in the book as refused, so that the user sees what the agent tried.
		if t, derr := m.st.DeclareTrial(ctx, store.Trial{ChatID: chatID, SearchID: id, Path: s.Path, Params: nb.Params, Note: note,
			State: store.TrialDeclared, CodeVersion: s.Code.Version}); derr == nil {
			_, _ = m.st.FinishTrial(ctx, t.ID, store.TrialRefused, err.Error())
			m.addSearchEvent(ctx, id, "trial_refused", map[string]any{"trial": t.Idx, "reason": err.Error()})
			m.publishTrial(ctx, chatID, t.ID)
		}
		return searchErr("Trial refused: " + err.Error())
	}
	_ = params
	canonParams, _ := plan.Canonical(nb.Params)
	for _, t := range st.trials {
		if (isRun(t) || t.State == store.TrialDeclared) && search.SameJSON(t.Params, canonParams) {
			return searchErr(fmt.Sprintf("trial %d already ran these parameters", t.Idx))
		}
	}
	// A declared trial that never started is replaced by the new declaration.
	if st.open != nil {
		_, _ = m.st.FinishTrial(ctx, st.open.ID, store.TrialRefused, "replaced by the next declaration")
		m.publishTrial(ctx, chatID, st.open.ID)
	}
	t, err := m.st.DeclareTrial(ctx, store.Trial{ChatID: chatID, SearchID: id, Path: s.Path, Params: canonParams, Note: note,
		State: store.TrialDeclared, CodeVersion: s.Code.Version, Source: "search"})
	if err != nil {
		return platform.Result{}, err
	}
	m.addSearchEvent(ctx, id, "trial_declared", map[string]any{"trial": t.Idx, "params": json.RawMessage(canonParams), "note": note})
	m.publishTrial(ctx, chatID, t.ID)
	m.publishSearch(ctx, chatID, id)
	return m.nextAnswer(s, t)
}

// nextAnswer names the exact calls that follow a declared trial (the plan's wording).
func (m *Manager) nextAnswer(s store.Search, t store.Trial) (platform.Result, error) {
	switch s.Path {
	case store.SearchPathTemplate:
		var b storedBase
		if err := json.Unmarshal(s.Base, &b); err != nil {
			return searchErr("the base of this search is unreadable")
		}
		cfg, _ := json.Marshal(b.Config)
		merged, err := search.Merge(cfg, paramsOf(t.Params))
		if err != nil {
			return searchErr(err.Error())
		}
		return platform.Result{Status: "ok", HTTPStatus: 200, Body: fmt.Sprintf(
			"trial %d declared. Now call {{tool:clone_training}} with container_id=%d and train_config=%s, then {{tool:start_training}} with the new container id as train_container_id. Both run without approval.",
			t.Idx, b.ContainerID, merged)}, nil
	default:
		return platform.Result{Status: "ok", HTTPStatus: 200, Body: fmt.Sprintf(
			"trial %d declared. Now write params.json with exactly %s next to %s and call {{tool:notebook_run}} with the same files, entry, dataset_ids and args as approved; it runs without approval. Changed code asks the user again.",
			t.Idx, string(t.Params), s.Code.Entry)}, nil
	}
}

func paramsOf(raw json.RawMessage) map[string]json.RawMessage {
	var m map[string]json.RawMessage
	_ = json.Unmarshal(raw, &m)
	return m
}

func strictJSON(raw []byte, v any) error {
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.DisallowUnknownFields()
	return d.Decode(v)
}

// searchStatusText is search_status: budget used and left, every trial, the best, the user's hints.
func (m *Manager) searchStatusText(ctx context.Context, chatID, id string) (platform.Result, error) {
	s, err := m.st.GetSearch(ctx, chatID, id)
	if err != nil {
		if err == store.ErrNotFound {
			return searchErr("no such training search in this chat")
		}
		return platform.Result{}, err
	}
	st, err := m.statsOf(ctx, s)
	if err != nil {
		return platform.Result{}, err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Search %s %q: %s", s.ID, s.Title, s.State)
	if s.EndReason != "" {
		b.WriteString(" (" + endReasonText(s.EndReason) + ")")
	}
	fmt.Fprintf(&b, "\nObjective: %s (%s). Space: %s\n", st.obj.Metric, st.obj.Mode, string(s.Space))
	fmt.Fprintf(&b, "Budget: %s. Used: %d trials, %.1f minutes, %d trials since the best one. Compute calls left in this chat: %d.\n",
		budgetText(st.budget), st.used.Runs, st.used.ComputeMinutes, st.used.SinceImprovement, m.computeLeft(ctx, chatID))
	b.WriteString("Trials:\n")
	for _, t := range st.trials {
		fmt.Fprintf(&b, "  %d. %s params=%s", t.Idx, t.State, string(t.Params))
		if t.Score != nil {
			fmt.Fprintf(&b, " %s=%s", st.obj.Metric, strconv.FormatFloat(*t.Score, 'g', 6, 64))
		}
		if t.Epoch != nil && t.Epochs != nil && t.State == store.TrialRunning {
			fmt.Fprintf(&b, " epoch %d/%d", *t.Epoch, *t.Epochs)
		}
		if t.Error != "" {
			b.WriteString(" error: " + platform.VisibleControls(oneLine(t.Error)))
		}
		b.WriteString("\n")
	}
	if st.bestIdx != nil && st.best != nil {
		fmt.Fprintf(&b, "Best: trial %d (%s=%s).\n", *st.bestIdx, st.obj.Metric, strconv.FormatFloat(*st.best, 'g', 6, 64))
	}
	if evs, err := m.st.SearchEvents(ctx, id); err == nil {
		for _, e := range evs {
			if e.Kind != "hint" {
				continue
			}
			var d struct {
				Text string `json:"text"`
			}
			if json.Unmarshal(e.Data, &d) == nil && d.Text != "" {
				b.WriteString("User hint: " + platform.VisibleControls(d.Text) + "\n")
			}
		}
	}
	return platform.Result{Status: "ok", HTTPStatus: 200, Body: b.String()}, nil
}

// searchEndByAgent is end_search: the agent ends its own search early.
func (m *Manager) searchEndByAgent(ctx context.Context, chatID, id string, body []byte) (platform.Result, error) {
	s, msg, err := m.runSearch(ctx, chatID, id)
	if err != nil {
		return platform.Result{}, err
	}
	if msg != "" {
		return searchErr(msg)
	}
	var nb struct {
		Note string `json:"note"`
	}
	if len(body) > 0 {
		if canon, cerr := plan.Canonical(body); cerr != nil || strictJSON(canon, &nb) != nil {
			return searchErr("{{tool:end_search}} takes note")
		}
	}
	if utf8.RuneCountInString(nb.Note) > search.NoteMaxRunes {
		return searchErr(fmt.Sprintf("note: at most %d characters", search.NoteMaxRunes))
	}
	if st, err := m.statsOf(ctx, s); err == nil && st.open != nil && st.open.State == store.TrialRunning {
		return searchErr(fmt.Sprintf("trial %d is still running; end the search when it has ended", st.open.Idx))
	}
	done, text := m.finishSearch(ctx, s, "ended_by_agent", nil, platform.VisibleControls(nb.Note))
	if !done {
		return searchErr("the search ended in the meantime")
	}
	return platform.Result{Status: "ok", HTTPStatus: 200, Body: text}, nil
}

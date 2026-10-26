// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"cmp"
	"context"
	"encoding/json"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"agw/internal/platform"
	"agw/internal/search"
	"agw/internal/store"
	"agw/internal/watch"
)

// Kinds of a run in the experiments overview (issue #222).
const (
	RunKindSearch   = "training_search"
	RunKindTraining = "training_run"
	RunKindNotebook = "notebook_run"
)

// States of a run in the overview: one vocabulary for all kinds.
const (
	RunRunning = "running"
	RunWaiting = "waiting"
	RunDone    = "done"
	RunFailed  = "failed"
	RunStopped = "stopped"
)

// RunKinds and RunStates list the valid values of the filters, in display order.
var (
	RunKinds  = []string{RunKindSearch, RunKindTraining, RunKindNotebook}
	RunStates = []string{RunRunning, RunWaiting, RunDone, RunFailed, RunStopped}
	// RunSorts are the valid values of the sort parameter.
	RunSorts = []string{"started", "duration", "title", "state", "chat"}
)

// RunFilter says which runs of a user's chats the overview wants (issue #222). Owner nil: every chat (token mode).
type RunFilter struct {
	Owner        *string
	ChatID       string
	Since, Until time.Time
	States       []string
	Kinds        []string
	Sort, Order  string
	Offset       int
	Limit        int
}

// RunProgress is how far a run is: the trial of a search, the epoch of a training.
type RunProgress struct {
	Trial      int        `json:"trial,omitempty"`
	Trials     int        `json:"trials,omitempty"`
	TrialsDone int        `json:"trials_done,omitempty"`
	Epoch      int        `json:"epoch,omitempty"`
	Epochs     int        `json:"epochs,omitempty"`
	ETA        *time.Time `json:"eta,omitempty"`
	Message    string     `json:"message,omitempty"`
}

// RunScore is the best value of a run and the objective it was measured by.
type RunScore struct {
	Metric string  `json:"metric"`
	Mode   string  `json:"mode"`
	Value  float64 `json:"value"`
	Trial  int     `json:"trial,omitempty"`
}

// RunRow is one line of the overview: a training search, a single training run or a notebook run. Exactly one of
// Search, Trial and Job is set on the rows of a page.
type RunRow struct {
	Kind       string          `json:"kind"`
	ID         string          `json:"id"`
	ChatID     string          `json:"chat_id"`
	ChatTitle  string          `json:"chat_title"`
	Title      string          `json:"title"`
	State      string          `json:"state"`
	Path       string          `json:"path,omitempty"`
	StartedAt  *time.Time      `json:"started_at,omitempty"`
	EndedAt    *time.Time      `json:"ended_at,omitempty"`
	DurationS  float64         `json:"duration_s"`
	Progress   RunProgress     `json:"progress"`
	Score      *RunScore       `json:"score,omitempty"`
	Error      string          `json:"error,omitempty"`
	PlanID     string          `json:"plan_id,omitempty"`
	PlanStep   int             `json:"plan_step,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	Link       string          `json:"link,omitempty"`
	Search     *SearchView     `json:"search,omitempty"`
	Trial      *TrialView      `json:"trial,omitempty"`
	Job        *store.WatchJob `json:"job,omitempty"`
}

// RunChat is a chat with the number of runs it has, for the chat filter.
type RunChat struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Runs  int    `json:"runs"`
}

// RunPage is the answer of GET /api/experiments.
type RunPage struct {
	Runs      []RunRow       `json:"runs"`
	Total     int            `json:"total"`
	Offset    int            `json:"offset"`
	Limit     int            `json:"limit"`
	Counts    map[string]int `json:"counts"`
	Chats     []RunChat      `json:"chats"`
	Truncated bool           `json:"truncated"`
}

// runSource is a row before it is enriched: what the three queries give, with state, times and title worked out.
type runSource struct {
	kind, id, chatID, chatTitle, title, state string
	start, end                                *time.Time
	duration                                  float64
	search                                    *store.Search
	trial                                     *store.Trial
	job                                       *store.WatchJob
	entry                                     string
}

// enrichedRuns counts the rows that were enriched with the views of search, trial or job (a test reads it: only the
// rows of the page may cost the extra queries).
var enrichedRuns atomic.Int64

// runState maps the states of the three sources to the one vocabulary of the overview.
func runState(kind, state, endReason, stoppedBy string) string {
	switch kind {
	case RunKindSearch:
		switch state {
		case store.SearchProposed:
			return RunWaiting
		case store.SearchRunning:
			return RunRunning
		}
		switch endReason {
		case endStartFailed:
			return RunFailed
		case endStopped:
			return RunStopped
		}
		return RunDone
	case RunKindTraining:
		switch state {
		case store.TrialDeclared, store.TrialRunning:
			return RunRunning
		case store.TrialFailed:
			return RunFailed
		case store.TrialStopped:
			return RunStopped
		}
		return RunDone
	}
	switch state {
	case store.JobRunning:
		return RunRunning
	case store.JobFailed:
		return RunFailed
	}
	if stoppedBy != "" {
		return RunStopped
	}
	return RunDone
}

func stateRank(s string) int {
	switch s {
	case RunRunning:
		return 0
	case RunWaiting:
		return 1
	case RunFailed:
		return 2
	case RunStopped:
		return 3
	}
	return 4
}

func defaultRank(s string) int {
	switch s {
	case RunRunning:
		return 0
	case RunWaiting:
		return 1
	}
	return 2
}

func spanOf(start, end *time.Time, now time.Time) float64 {
	if start == nil {
		return 0
	}
	e := now
	if end != nil {
		e = *end
	}
	if e.Before(*start) {
		return 0
	}
	return e.Sub(*start).Seconds()
}

func (r runSource) startUnix() int64 {
	if r.start == nil {
		return 0
	}
	return r.start.UnixNano()
}

// sortRuns orders rows: by default the running first, then the waiting, then the rest, newest first inside each;
// with a sort key purely by that key (order asc or desc), ties newest first and then by id.
func sortRuns(rows []runSource, by, order string) {
	less := func(a, b runSource) (bool, bool) { return false, false } // (less, decided)
	switch by {
	case "started":
		less = func(a, b runSource) (bool, bool) {
			return a.startUnix() < b.startUnix(), a.startUnix() != b.startUnix()
		}
	case "duration":
		less = func(a, b runSource) (bool, bool) { return a.duration < b.duration, a.duration != b.duration }
	case "title":
		less = func(a, b runSource) (bool, bool) {
			x, y := strings.ToLower(a.title), strings.ToLower(b.title)
			return x < y, x != y
		}
	case "chat":
		less = func(a, b runSource) (bool, bool) {
			x, y := strings.ToLower(a.chatTitle), strings.ToLower(b.chatTitle)
			return x < y, x != y
		}
	case "state":
		less = func(a, b runSource) (bool, bool) {
			x, y := stateRank(a.state), stateRank(b.state)
			return x < y, x != y
		}
	default:
		less = func(a, b runSource) (bool, bool) {
			x, y := defaultRank(a.state), defaultRank(b.state)
			return x < y, x != y
		}
	}
	desc := order == "desc"
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if l, decided := less(a, b); decided {
			if by != "" && desc {
				return !l
			}
			return l
		}
		if a.startUnix() != b.startUnix() {
			return a.startUnix() > b.startUnix()
		}
		return a.id < b.id
	})
}

func inList(list []string, v string) bool {
	if len(list) == 0 {
		return true
	}
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// singleTrialTitle names a single run by what it is (issue #227): "<architecture> on <dataset>" for a training run
// whose subject is known, else "Training container <id>" or "Training run"; the entry file of a notebook run, else
// "Notebook run".
func singleTrialTitle(t store.Trial, entry string) string {
	if t.Path == store.SearchPathNotebook {
		return cmp.Or(entry, "Notebook run")
	}
	var b SearchBase
	if len(t.Subject) > 0 && json.Unmarshal(t.Subject, &b) == nil {
		if b.ContainerID == 0 {
			b.ContainerID, _ = strconv.ParseInt(t.Ref, 10, 64)
		}
		return subjectText(b)
	}
	if t.Ref != "" {
		return "Training container " + t.Ref
	}
	return "Training run"
}

// ExperimentRuns lists the runs of a user's chats for the overview of issue #222: training searches, single
// training runs and notebook runs, filtered, sorted and cut to a page. Only the rows of the page are enriched.
func (m *Manager) ExperimentRuns(ctx context.Context, f RunFilter) (RunPage, error) {
	if f.Limit <= 0 {
		f.Limit = 25
	}
	page := RunPage{Runs: []RunRow{}, Offset: f.Offset, Limit: f.Limit, Chats: []RunChat{},
		Counts: map[string]int{RunRunning: 0, RunWaiting: 0, RunDone: 0, RunFailed: 0, RunStopped: 0}}
	titles, err := m.st.OwnerChatTitles(ctx, f.Owner)
	if err != nil {
		return page, err
	}
	ids := make([]string, 0, len(titles))
	for id := range titles {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	sc := store.RunScope{ChatIDs: ids, Since: f.Since, Until: f.Until}
	searches, err := m.st.ScopeSearches(ctx, sc)
	if err != nil {
		return page, err
	}
	singles, err := m.st.ScopeSingleTrials(ctx, sc)
	if err != nil {
		return page, err
	}
	jobs, err := m.st.ScopeNotebookJobs(ctx, sc)
	if err != nil {
		return page, err
	}
	if len(searches) > store.RunScopeLimit {
		searches, page.Truncated = searches[:store.RunScopeLimit], true
	}
	if len(singles) > store.RunScopeLimit {
		singles, page.Truncated = singles[:store.RunScopeLimit], true
	}
	if len(jobs) > store.RunScopeLimit {
		jobs, page.Truncated = jobs[:store.RunScopeLimit], true
	}
	now := time.Now()
	var all []runSource
	for i := range searches {
		s := &searches[i]
		start := s.StartedAt
		if start == nil {
			t := s.CreatedAt
			start = &t
		}
		r := runSource{kind: RunKindSearch, id: s.ID, chatID: s.ChatID, title: s.Title, search: s, start: start, end: s.EndedAt,
			state: runState(RunKindSearch, s.State, s.EndReason, "")}
		if r.state != RunWaiting {
			r.duration = spanOf(start, s.EndedAt, now)
		}
		all = append(all, r)
	}
	for i := range singles {
		t := &singles[i].Trial
		start := t.StartedAt
		if start == nil {
			d := t.DeclaredAt
			start = &d
		}
		r := runSource{kind: RunKindTraining, id: t.ID, chatID: t.ChatID, title: singleTrialTitle(*t, singles[i].Entry), trial: t,
			entry: singles[i].Entry, start: start, end: t.EndedAt, state: runState(RunKindTraining, t.State, "", "")}
		r.duration = spanOf(start, t.EndedAt, now)
		all = append(all, r)
	}
	for i := range jobs {
		j := &jobs[i].Job
		start, end := j.RunStartedAt, j.RunEndedAt
		if start == nil {
			d := j.StartedAt
			start = &d
		}
		if end == nil {
			end = j.EndedAt
		}
		title := jobs[i].Entry
		if title == "" {
			title = "Notebook run"
		}
		r := runSource{kind: RunKindNotebook, id: j.ID, chatID: j.ChatID, title: title, job: j, entry: jobs[i].Entry,
			start: start, end: end, state: runState(RunKindNotebook, j.State, "", j.StoppedBy)}
		r.duration = spanOf(start, end, now)
		all = append(all, r)
	}
	for i := range all {
		all[i].chatTitle = titles[all[i].chatID]
	}

	// Counts ignore the state filter, the chat facet ignores the chat filter.
	perChat := map[string]int{}
	var filtered []runSource
	for _, r := range all {
		if len(f.Kinds) > 0 && !contains(f.Kinds, r.kind) {
			continue
		}
		inChat := f.ChatID == "" || r.chatID == f.ChatID
		if inChat {
			page.Counts[r.state]++
		}
		if len(f.States) > 0 && !contains(f.States, r.state) {
			continue
		}
		perChat[r.chatID]++
		if inChat {
			filtered = append(filtered, r)
		}
	}
	for id, n := range perChat {
		page.Chats = append(page.Chats, RunChat{ID: id, Title: titles[id], Runs: n})
	}
	sort.Slice(page.Chats, func(i, j int) bool {
		if page.Chats[i].Runs != page.Chats[j].Runs {
			return page.Chats[i].Runs > page.Chats[j].Runs
		}
		return page.Chats[i].ID < page.Chats[j].ID
	})
	page.Total = len(filtered)
	sortRuns(filtered, f.Sort, f.Order)
	if f.Offset < len(filtered) {
		end := min(f.Offset+f.Limit, len(filtered))
		for _, r := range filtered[f.Offset:end] {
			page.Runs = append(page.Runs, m.enrichRun(ctx, r))
		}
	}
	return page, nil
}

// firstLine is the first non-empty line of a text, redacted and cut to 200 runes.
func firstLine(s string) string {
	for _, l := range strings.Split(platform.RedactText(s), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return watch.ClipRunes(l, 200)
		}
	}
	return ""
}

// httpsOnly returns a link only if it is an https address.
func httpsOnly(link string) string {
	u, err := url.Parse(link)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return ""
	}
	return link
}

// enrichRun adds the view, progress, best value and plan to a row of the page.
func (m *Manager) enrichRun(ctx context.Context, r runSource) RunRow {
	enrichedRuns.Add(1)
	row := RunRow{Kind: r.kind, ID: r.id, ChatID: r.chatID, ChatTitle: r.chatTitle, Title: r.title, State: r.state,
		StartedAt: r.start, EndedAt: r.end, DurationS: r.duration}
	if r.state == RunWaiting {
		row.StartedAt = nil
	}
	switch r.kind {
	case RunKindSearch:
		m.enrichSearch(ctx, &row, *r.search)
	case RunKindTraining:
		m.enrichTrial(ctx, &row, *r.trial)
	default:
		j := redactWatchText(*r.job)
		row.Job = &j
		row.PlanID, row.PlanStep, row.ToolCallID = j.PlanID, j.StepIdx, j.ToolCallID
		row.Link = httpsOnly(j.Link)
		row.Path = store.SearchPathNotebook
		row.Progress.Message = firstLine(j.Message)
		row.Progress.ETA = j.ETA
		if r.state == RunFailed {
			row.Error = firstLine(j.Message)
		}
	}
	return row
}

func (m *Manager) enrichSearch(ctx context.Context, row *RunRow, s store.Search) {
	v, _ := m.searchView(ctx, s)
	row.Search = &v
	row.Path = s.Path
	row.ToolCallID = s.ToolCallID
	st, err := m.statsOf(ctx, s)
	if err != nil {
		return
	}
	if st.budget.MaxRuns != nil {
		row.Progress.Trials = *st.budget.MaxRuns
	}
	for _, t := range st.trials {
		if t.State == store.TrialDone || t.State == store.TrialFailed || t.State == store.TrialStopped {
			row.Progress.TrialsDone++
		}
	}
	if st.open != nil {
		row.Progress.Trial = st.open.Idx
		tv := m.trialView(ctx, *st.open, nil)
		if tv.Epoch != nil {
			row.Progress.Epoch = *tv.Epoch
		}
		if tv.Epochs != nil {
			row.Progress.Epochs = *tv.Epochs
		}
		row.Progress.ETA = tv.ETA
	}
	if st.best != nil && st.bestIdx != nil {
		row.Score = &RunScore{Metric: st.obj.Metric, Mode: st.obj.Mode, Value: *st.best, Trial: *st.bestIdx}
	}
	if row.State == RunFailed {
		row.Error = "The search could not start"
	}
}

func (m *Manager) enrichTrial(ctx context.Context, row *RunRow, t store.Trial) {
	tv := m.trialView(ctx, t, nil)
	row.Trial = &tv
	row.Path = t.Path
	row.Link = httpsOnly(tv.Link)
	if t.Epoch != nil {
		row.Progress.Epoch = *t.Epoch
	}
	if t.Epochs != nil {
		row.Progress.Epochs = *t.Epochs
	}
	row.Progress.ETA = tv.ETA
	if t.Score != nil {
		obj, _ := search.ParseObjective(nil)
		row.Score = &RunScore{Metric: obj.Metric, Mode: obj.Mode, Value: *t.Score}
	}
	if t.JobID != "" {
		if j, err := m.st.GetWatchJob(ctx, t.ChatID, t.JobID); err == nil {
			row.PlanID, row.PlanStep, row.ToolCallID = j.PlanID, j.StepIdx, j.ToolCallID
		}
	}
	if row.PlanID == "" {
		row.PlanID, row.PlanStep = tv.PlanID, tv.PlanStep
	}
	if row.State == RunFailed {
		row.Error = firstLine(t.Error)
	}
}

// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"agw/internal/delegation"
	"agw/internal/plan"
	"agw/internal/platform"
	"agw/internal/search"
	"agw/internal/store"
	"agw/internal/watch"
)

var (
	cloneCallRe = regexp.MustCompile(`^/train/containers/([0-9]+)/clone$`)
	startCallRe = regexp.MustCompile(`^/train/containers/([0-9]+)/run$`)
)

func init() { registerGuard(searchFinalGuard) }

// finalCheckBody is the part of the body of assess_model the search cares about.
type finalCheckBody struct {
	ModelID       int64  `json:"model_id"`
	Path          string `json:"path"`
	EvalDatasetID int64  `json:"eval_dataset_id"`
	Final         bool   `json:"final"`
}

func finalCheck(req platform.Request) (finalCheckBody, bool) {
	var b finalCheckBody
	if req.Method != http.MethodPost || req.Path != platform.AssessCheckPath || json.Unmarshal(req.Body, &b) != nil || !b.Final {
		return b, false
	}
	return b, true
}

// searchFinalGuard refuses the final test while a search of the chat runs (S4): the test set is looked at once, for
// the winner, after the search has ended.
func searchFinalGuard(ctx context.Context, m *Manager, chatID string, req platform.Request, _ delegation.Access) (string, error) {
	if _, ok := finalCheck(req); !ok {
		return "", nil
	}
	if _, err := m.st.RunningSearch(ctx, chatID); err == nil {
		return "the final test runs once, for the winner, after the training search has ended", nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return "", err
	}
	return "", nil
}

// searchMatch returns the claim of the chat's running search on a call, or nil when no search applies (S4). It is
// asked after the plan match and only for a call no plan step claims. A covered claim reserves the trial it
// belongs to; searchFinish releases or completes it.
func (m *Manager) searchMatch(ctx context.Context, chatID string, req platform.Request) *trialClaim {
	if b, ok := finalCheck(req); ok {
		return m.matchFinal(ctx, chatID, b)
	}
	s, err := m.st.RunningSearch(ctx, chatID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			slog.Error("running search not readable", "chat", chatID, "error", err)
		}
		return nil
	}
	st, err := m.statsOf(ctx, s)
	if err != nil || st.open == nil || st.open.State != store.TrialDeclared {
		return nil
	}
	t := *st.open
	switch {
	case s.Path == store.SearchPathTemplate && req.Method == http.MethodPost && cloneCallRe.MatchString(req.Path):
		return m.matchClone(ctx, s, st, t, req)
	case s.Path == store.SearchPathTemplate && req.Method == http.MethodPost && startCallRe.MatchString(req.Path):
		return m.matchStart(ctx, s, st, t, req)
	case s.Path == store.SearchPathNotebook && notebookRun(req):
		return m.matchNotebook(ctx, s, st, t, req)
	}
	return nil
}

// matchClone covers the copy of the base container only when its configuration is the base's with exactly the
// declared params, by value.
func (m *Manager) matchClone(ctx context.Context, s store.Search, st searchStats, t store.Trial, req platform.Request) *trialClaim {
	var base storedBase
	if json.Unmarshal(s.Base, &base) != nil || cloneCallRe.FindStringSubmatch(req.Path)[1] != strconv.FormatInt(base.ContainerID, 10) {
		return nil
	}
	var body struct {
		TrainConfig json.RawMessage `json:"train_config"`
	}
	if json.Unmarshal(req.Body, &body) != nil || len(body.TrainConfig) == 0 {
		return nil
	}
	baseCfg, _ := json.Marshal(base.Config)
	want, err := search.Merge(baseCfg, paramsOf(t.Params))
	if err != nil || !search.SameJSON(body.TrainConfig, want) {
		return nil
	}
	if ok, _ := m.admit(ctx, s, st); !ok {
		return nil
	}
	return &trialClaim{SearchID: s.ID, TrialID: t.ID, Kind: claimClone, Covered: true}
}

// matchStart covers the start of the copy that was made for the declared trial, and only that one.
func (m *Manager) matchStart(ctx context.Context, s store.Search, st searchStats, t store.Trial, req platform.Request) *trialClaim {
	ref := startCallRe.FindStringSubmatch(req.Path)[1]
	if t.Ref == "" || t.Ref == "-" || t.Ref != ref {
		return nil
	}
	if ok, _ := m.admit(ctx, s, st); !ok {
		return nil
	}
	return &trialClaim{SearchID: s.ID, TrialID: t.ID, Kind: claimStart, Covered: true, Ref: ref}
}

// codeMatches binds a notebook run of a search to the approved code (#168). The SHA-256 is computed here from the
// bytes the socket read, never taken from a field. params.json is the only file that may differ, and it must carry
// exactly the declared params. Entry, args, datasets must equal the approved run; the timeout must not grow.
func codeMatches(code store.SearchCode, body platform.RunBody, files []platform.Upload, declared json.RawMessage) (
	changed []string, paramsOK bool, err error) {
	if body.Entry != code.Entry || !slices.Equal(body.Args, code.Args) || !slices.Equal(body.DatasetIDs, code.DatasetIDs) ||
		body.TimeoutMinutes > code.TimeoutMinutes {
		return nil, false, errors.New("entry, args, dataset_ids or timeout differ from the approved search")
	}
	bound := map[string]string{}
	for _, f := range code.Files {
		bound[f.Name] = f.SHA256
	}
	seen := map[string]bool{}
	for _, f := range files {
		if f.Name == "params.json" {
			want, err1 := plan.Canonical(declared)
			got, err2 := plan.Canonical(f.Data)
			paramsOK = err1 == nil && err2 == nil && search.SameJSON(want, got)
			continue
		}
		sum := sha256.Sum256(f.Data)
		if hex.EncodeToString(sum[:]) != bound[f.Name] {
			changed = append(changed, f.Name)
		}
		seen[f.Name] = true
	}
	for name := range bound {
		if !seen[name] {
			changed = append(changed, name) // a missing file is a change, too
		}
	}
	sort.Strings(changed)
	return changed, paramsOK, nil
}

// matchNotebook covers a run of the approved code with exactly the declared params; other code is a claim that is
// not covered and carries the question for the user (S4).
func (m *Manager) matchNotebook(ctx context.Context, s store.Search, st searchStats, t store.Trial, req platform.Request) *trialClaim {
	body, err := platform.ParseRunForm(req.Form, 0)
	if err != nil {
		return nil
	}
	changed, paramsOK, err := codeMatches(s.Code, body, req.Uploads, t.Params)
	if err != nil || !paramsOK {
		return nil // a different run, or params other than declared: an ordinary approval, never a trial
	}
	if ok, _ := m.admit(ctx, s, st); !ok {
		return nil
	}
	if len(changed) == 0 {
		return &trialClaim{SearchID: s.ID, TrialID: t.ID, Kind: claimNotebook, Covered: true}
	}
	next := s.Code
	next.Version = s.Code.Version + 1
	next.Files = nil
	for _, u := range req.Uploads {
		if u.Name != "params.json" {
			next.Files = append(next.Files, searchFileOf(u))
		}
	}
	data := SearchCodeChange{Kind: searchApprovalCodeChange, SearchID: s.ID, Title: s.Title, Trial: t.Idx,
		CodeVersionFrom: s.Code.Version, CodeVersionTo: next.Version, ChangedFiles: changed}
	data.Files = fileChanges(s.Code, next, changed)
	raw, err := json.Marshal(data)
	if err != nil {
		return nil
	}
	return &trialClaim{SearchID: s.ID, TrialID: t.ID, Kind: claimNotebook, Approval: raw, NewCode: &next}
}

// fileChanges describes the changed files: hashes of both versions and the diff (or the whole new file when the diff
// is too long; hashes only for a file without text).
func fileChanges(old, next store.SearchCode, changed []string) []SearchFileChange {
	byName := func(c store.SearchCode) map[string]store.SearchFile {
		out := map[string]store.SearchFile{}
		for _, f := range c.Files {
			out[f.Name] = f
		}
		return out
	}
	o, n := byName(old), byName(next)
	out := make([]SearchFileChange, 0, len(changed))
	for _, name := range changed {
		of, hadOld := o[name]
		nf, hasNew := n[name]
		c := SearchFileChange{Name: platform.VisibleControls(name), OldSHA256: of.SHA256, NewSHA256: nf.SHA256, Added: !hadOld, Removed: !hasNew}
		if hasNew && (nf.Text != "" || nf.Size == 0) && (!hadOld || of.Text != "" || of.Size == 0) {
			diff, tooLong := search.UnifiedDiff(of.Text, nf.Text)
			if tooLong {
				c.DiffTooLong, c.NewText = true, platform.VisibleControls(nf.Text)
			} else {
				c.Diff = platform.VisibleControls(diff)
			}
		}
		out = append(out, c)
	}
	return out
}

// matchFinal covers assess_model final=true when the chat's last search ended with a winner and the call checks
// exactly that winner on the test set the split held back.
func (m *Manager) matchFinal(ctx context.Context, chatID string, b finalCheckBody) *trialClaim {
	list, err := m.st.ListSearches(ctx, chatID)
	if err != nil {
		return nil
	}
	var s *store.Search
	for i := len(list) - 1; i >= 0; i-- {
		if list[i].State == store.SearchEnded {
			s = &list[i]
			break
		}
	}
	if s == nil || s.WinnerIdx == nil {
		return nil
	}
	var ho SearchHoldout
	if json.Unmarshal(s.Holdout, &ho) != nil || !ho.Available || ho.FinalState != "pending" || ho.EvalDataset == nil {
		return nil
	}
	if b.EvalDatasetID != 0 && b.EvalDatasetID != ho.EvalDataset.ID {
		return nil
	}
	trials, err := m.st.ListTrials(ctx, chatID)
	if err != nil {
		return nil
	}
	var w *store.Trial
	for i := range trials {
		if trials[i].SearchID == s.ID && trials[i].Idx == *s.WinnerIdx {
			w = &trials[i]
		}
	}
	if w == nil || w.State != store.TrialDone || w.Ref == "" {
		return nil
	}
	switch s.Path {
	case store.SearchPathTemplate:
		o, err := m.st.ModelOriginOf(ctx, chatID, b.ModelID)
		if err != nil || strconv.FormatInt(o.TrainContainerID, 10) != w.Ref {
			return nil
		}
	default:
		if b.Path == "" || !strings.Contains(b.Path, "/"+w.Ref+"/") {
			return nil
		}
	}
	return &trialClaim{SearchID: s.ID, TrialID: w.ID, Kind: claimFinal, Covered: true}
}

// reserveClaim reserves the trial of a covered claim right before the call goes out, so that a second call cannot
// use the same declaration; a call that returns before that reserves nothing. false: the trial is not available
// any more, and the call must not go out as the search's.
func (m *Manager) reserveClaim(ctx context.Context, c *trialClaim) bool {
	if c == nil || !c.Covered {
		return true
	}
	var ok bool
	var err error
	switch c.Kind {
	case claimClone:
		ok, err = m.st.ClaimTrialClone(ctx, c.TrialID)
	case claimStart:
		ok, err = m.st.ClaimTrialStart(ctx, c.TrialID, c.Ref)
	case claimNotebook:
		ok, err = m.st.ClaimTrialStart(ctx, c.TrialID, "")
	default:
		return true
	}
	return err == nil && ok
}

// searchFinish records the outcome of a claimed call (the clone made, the trial started, the run begun). It is
// called after the platform answered, with the answer or the error, and after the started job is observed, so that
// the trial can name it before the watcher reads the job for the first time. A nil claim does nothing.
func (m *Manager) searchFinish(ctx context.Context, chatID string, c *trialClaim, access delegation.Access, req platform.Request,
	res platform.Result, callErr error, jobID string) {
	if c == nil {
		return
	}
	bg := context.WithoutCancel(ctx)
	ok := callErr == nil && res.Status == "ok" && res.HTTPStatus >= 200 && res.HTTPStatus < 300
	switch c.Kind {
	case claimClone:
		if !ok {
			_ = m.st.ReleaseTrialClone(bg, c.TrialID)
			return
		}
		_, id, created := delegation.Created(access, res)
		if !created {
			_ = m.st.ReleaseTrialClone(bg, c.TrialID)
			slog.Error("the copy of a training container named no id", "chat", chatID, "trial", c.TrialID)
			return
		}
		m.recordDelegationObject(bg, chatID, delegation.Training, id)
		if _, err := m.st.SetTrialRef(bg, c.TrialID, id); err != nil {
			slog.Error("copy of a trial not recorded", "trial", c.TrialID, "error", err)
		}
		// The ref was '-' while the copy was made; SetTrialRef needs the trial declared, which it still is.
	case claimStart, claimNotebook:
		ref := ""
		if kind, r, watching := watch.For(access, req, res); watching && kind == "notebook_run" {
			ref = r
		}
		switch {
		case ok && c.Covered:
			if err := m.st.SetTrialJob(bg, c.TrialID, ref, jobID); err != nil {
				slog.Error("job of a trial not recorded", "trial", c.TrialID, "error", err)
			}
		case ok:
			// The user approved other code for this trial: it becomes the code of the search.
			started, err := m.st.StartTrial(bg, c.TrialID, ref, jobID)
			if err != nil || !started {
				slog.Error("trial with changed code not started", "trial", c.TrialID, "ok", started, "error", err)
				return
			}
			if c.NewCode != nil {
				if err := m.st.SetSearchCode(bg, c.SearchID, *c.NewCode); err != nil {
					slog.Error("approved code not stored", "search", c.SearchID, "error", err)
				}
				_ = m.st.SetTrialCodeVersion(bg, c.TrialID, c.NewCode.Version)
				var ch SearchCodeChange
				_ = json.Unmarshal(c.Approval, &ch)
				m.addSearchEvent(bg, c.SearchID, "code_approved", map[string]any{"trial": ch.Trial, "from": ch.CodeVersionFrom,
					"to": ch.CodeVersionTo, "changed_files": ch.ChangedFiles})
			}
		case c.Covered:
			_ = m.st.ReleaseTrialStart(bg, c.TrialID)
		}
	case claimFinal:
		if !ok {
			return
		}
		s, err := m.st.GetSearch(bg, chatID, c.SearchID)
		if err != nil {
			return
		}
		var ho SearchHoldout
		_ = json.Unmarshal(s.Holdout, &ho)
		ho.FinalState = "started"
		var answer struct {
			CheckID string `json:"check_id"`
		}
		_ = json.Unmarshal([]byte(res.Body), &answer)
		ho.CheckID = answer.CheckID
		if raw, err := json.Marshal(ho); err == nil {
			_ = m.st.SetSearchHoldout(bg, c.SearchID, raw)
		}
		m.addSearchEvent(bg, c.SearchID, "final", map[string]any{"check_id": answer.CheckID, "trial": s.WinnerIdx})
	}
	m.publishTrial(bg, chatID, c.TrialID)
	m.publishSearch(bg, chatID, c.SearchID)
}

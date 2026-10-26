// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"agw/internal/plan"
	"agw/internal/platform"
	"agw/internal/search"
	"agw/internal/store"
	"agw/internal/watch"
)

// The book of trials, fed by the watcher (issue #168): every training run of a chat is a trial, declared by a search
// or not; a notebook run outside a search is a trial once it writes an epoch to outputs/metrics.jsonl (issue #194). The hooks below carry what the watcher read — the figures of each epoch, the
// GPU samples, the number of epochs, the score of the objective — into the tables of the search, and tell the
// browser about it ("trial_progress", at most every progressThrottle per trial). A run that no search declared
// becomes a single trial (no search_id, idx 0). The end of a trial of a search is not decided here (the search
// does that); a single trial ends with its job.

const (
	trialKindTraining = "training_run"
	trialKindNotebook = "notebook_run"
	// trialPollEvery is the distance at which the job of a trial is read while it runs, whatever its age.
	trialPollEvery = 15 * time.Second
)

// progressThrottle is the shortest distance between two "trial_progress" events of one trial. A variable so that
// the tests do not wait for five seconds.
var progressThrottle = 5 * time.Second

func init() {
	registerJobProgress(recordTrialProgress)
	registerJobEnded(finishSingleTrial)
}

// trialJobs are the jobs the hook has recognised as a trial's; the watcher reads them faster (trialPollEvery).
var trialJobs sync.Map

// isTrialJob reports whether the job is a trial's, as far as the hook has seen it.
func isTrialJob(jobID string) bool {
	_, ok := trialJobs.Load(jobID)
	return ok
}

// trialPath names the kind of a job as the path of a trial: the training template or a notebook run.
func trialPath(kind string) (string, bool) {
	switch kind {
	case trialKindTraining:
		return store.SearchPathTemplate, true
	case trialKindNotebook:
		return store.SearchPathNotebook, true
	}
	return "", false
}

// TrialProgressEvent is the SSE event "trial_progress": what a trial's run wrote since the last event.
type TrialProgressEvent struct {
	TrialID  string `json:"trial_id"`
	SearchID string `json:"search_id,omitempty"`
	Epoch    *int   `json:"epoch,omitempty"`
	Epochs   *int   `json:"epochs,omitempty"`
	// ETA is the end the watcher estimates for the job (from epoch / epochs).
	ETA   *time.Time `json:"eta,omitempty"`
	Score *float64   `json:"score,omitempty"`
	// Points are the new epochs: epoch, at and the figures by name (train_loss, val_acc, ...).
	Points []map[string]any `json:"points,omitempty"`
	// GPU is the last sample.
	GPU *store.TrialGPU `json:"gpu,omitempty"`
}

// recordTrialProgress is the hook of every poll of a training or notebook job.
func recordTrialProgress(ctx context.Context, m *Manager, j store.WatchJob, p watch.Progress) {
	path, ok := trialPath(j.Kind)
	if !ok || m.st == nil {
		return
	}
	t, ok := m.trialForJob(ctx, j, path, p)
	if !ok {
		return
	}
	trialJobs.Store(j.ID, struct{}{})
	ev := TrialProgressEvent{TrialID: t.ID, SearchID: t.SearchID, ETA: j.ETA}
	changed := false

	if p.Metrics != nil {
		// What the run writes at all (issue #168, G1): the empty charts say why they are empty.
		figures := "none"
		if p.Metrics.Supported {
			figures = "ok"
		}
		if t.Figures != figures || t.GPUAvailable == nil || *t.GPUAvailable != p.Metrics.GPUAvailable {
			if err := m.st.SetTrialFigures(ctx, t.ID, figures, p.Metrics.GPUAvailable); err != nil {
				slog.Error("figures of a trial not recorded", "trial", t.ID, "error", err)
			}
		}
		changed = m.storeTrialMetrics(ctx, t, p.Metrics, &ev)
	}
	if !changed {
		return
	}
	m.sendTrialProgress(j.ChatID, ev, j.State != watch.StateRunning)
}

// storeTrialMetrics writes what is new in the figures of a run and fills the event; it reports whether there was
// anything.
func (m *Manager) storeTrialMetrics(ctx context.Context, t store.Trial, mt *watch.Metrics, ev *TrialProgressEvent) bool {
	changed := false
	now := time.Now().UTC()

	// Epochs: the lines after the last stored one.
	var fresh []store.TrialEpoch
	for _, e := range mt.Epochs {
		if t.Epoch != nil && e.Epoch <= *t.Epoch {
			continue
		}
		at := e.At
		if at.IsZero() {
			at = now
		}
		data, err := json.Marshal(e.Values)
		if err != nil {
			continue
		}
		fresh = append(fresh, store.TrialEpoch{Epoch: e.Epoch, At: at, Data: data})
		point := map[string]any{"epoch": e.Epoch, "at": at.UTC().Format(time.RFC3339)}
		for k, v := range e.Values {
			point[k] = v
		}
		ev.Points = append(ev.Points, point)
	}
	if len(fresh) > 0 {
		if err := m.st.AddTrialEpochs(ctx, t.ID, fresh); err != nil {
			slog.Error("epochs of a trial not stored", "trial", t.ID, "error", err)
			ev.Points = nil
		} else {
			changed = true
		}
	}

	// Progress: epoch, epochs and the score of the objective, from the whole series as the run wrote it.
	var epoch, epochs *int
	var score *float64
	var scoreEpoch *int
	if n := len(mt.Epochs); n > 0 {
		last := mt.Epochs[n-1]
		if t.Epoch == nil || *t.Epoch != last.Epoch {
			e := last.Epoch
			epoch = &e
		}
		if last.Epochs > 0 && (t.Epochs == nil || *t.Epochs != last.Epochs) {
			e := last.Epochs
			epochs = &e
		}
		if v, ep, ok := search.Score(m.trialObjective(ctx, t), mt.Epochs); ok && (t.Score == nil || *t.Score != v || t.ScoreEpoch == nil || *t.ScoreEpoch != ep) {
			score, scoreEpoch = &v, &ep
		}
	}

	// GPU: the samples after the last stored one.
	var gpuJSON json.RawMessage
	var freshGPU []store.TrialGPU
	var lastAt time.Time
	if len(t.GPU) > 0 {
		var g store.TrialGPU
		if json.Unmarshal(t.GPU, &g) == nil {
			lastAt = g.At
		}
	}
	for _, g := range mt.GPU {
		if g.At.After(lastAt) {
			freshGPU = append(freshGPU, store.TrialGPU{At: g.At, Util: g.Util, MemUsedMB: g.MemUsedMB, MemTotalMB: g.MemTotalMB})
		}
	}
	if len(freshGPU) > 0 {
		if err := m.st.AddTrialGPU(ctx, t.ID, freshGPU); err != nil {
			slog.Error("gpu samples of a trial not stored", "trial", t.ID, "error", err)
		} else {
			lastGPU := freshGPU[len(freshGPU)-1]
			if raw, err := json.Marshal(lastGPU); err == nil {
				gpuJSON = raw
			}
			ev.GPU = &lastGPU
			changed = true
		}
	}

	if epoch != nil || epochs != nil || score != nil || gpuJSON != nil {
		if err := m.st.SetTrialProgress(ctx, t.ID, epoch, epochs, score, scoreEpoch, gpuJSON); err != nil {
			slog.Error("progress of a trial not stored", "trial", t.ID, "error", err)
			return changed
		}
		changed = true
	}
	// The event names the state of the trial after this poll, whether or not each part changed.
	ev.Epoch, ev.Epochs, ev.Score = firstInt(epoch, t.Epoch), firstInt(epochs, t.Epochs), firstFloat(score, t.Score)
	return changed
}

func firstInt(a, b *int) *int {
	if a != nil {
		return a
	}
	return b
}

func firstFloat(a, b *float64) *float64 {
	if a != nil {
		return a
	}
	return b
}

// trialObjective is the objective a trial is scored by: that of its search, val_acc / max for a single run.
func (m *Manager) trialObjective(ctx context.Context, t store.Trial) search.Objective {
	def, _ := search.ParseObjective(nil)
	if t.SearchID == "" {
		return def
	}
	s, err := m.st.GetSearch(ctx, t.ChatID, t.SearchID)
	if err != nil {
		return def
	}
	if o, err := search.ParseObjective(s.Objective); err == nil {
		return o
	}
	return def
}

// trialForJob finds the trial of a job: by the job, else by the run the trial was declared for, else (a run no
// search declared) a single trial is made. ok is false when the trial cannot be had; the poll goes on without it.
func (m *Manager) trialForJob(ctx context.Context, j store.WatchJob, path string, p watch.Progress) (store.Trial, bool) {
	t, err := m.st.TrialByJob(ctx, j.ID)
	if err == nil {
		return t, true
	}
	if !errors.Is(err, store.ErrNotFound) {
		slog.Error("trial of a job not read", "job", j.ID, "error", err)
		return store.Trial{}, false
	}
	if t, err = m.st.TrialByRef(ctx, j.ChatID, path, j.Ref); err == nil {
		// The trial the search declared for this run: it takes the job, if it has none yet. A trial that has
		// ended, or that belongs to another job, is an earlier run of the same container.
		if t.State == store.TrialDeclared {
			if _, serr := m.st.StartTrial(ctx, t.ID, j.Ref, j.ID); serr != nil {
				slog.Error("trial not linked to its job", "trial", t.ID, "job", j.ID, "error", serr)
				return store.Trial{}, false
			}
			t.State, t.JobID = store.TrialRunning, j.ID
			return t, true
		}
		if t.State == store.TrialRunning && (t.JobID == "" || t.JobID == j.ID) {
			return t, true
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		slog.Error("trial of a run not read", "job", j.ID, "ref", j.Ref, "error", err)
		return store.Trial{}, false
	}
	if !singleRunIsTrial(path, p) {
		return store.Trial{}, false
	}
	return m.declareSingleTrial(ctx, j, path, p)
}

// singleRunIsTrial tells whether a run that no search declared belongs in the book (issue #194): a training run
// always does (the template trains, a failed run is a trial too); a notebook run only once it wrote an epoch.
func singleRunIsTrial(path string, p watch.Progress) bool {
	if path == store.SearchPathNotebook {
		return p.Metrics != nil && len(p.Metrics.Epochs) > 0
	}
	return true
}

// declareSingleTrial makes the trial of a run that no search declared and starts it with the job.
func (m *Manager) declareSingleTrial(ctx context.Context, j store.WatchJob, path string, p watch.Progress) (store.Trial, bool) {
	params, subject := m.singleRunInfo(ctx, j, path, p)
	t, err := m.st.DeclareTrial(ctx, store.Trial{ChatID: j.ChatID, Path: path, Params: params, Subject: subject, Ref: j.Ref})
	if err != nil {
		slog.Error("single trial not declared", "job", j.ID, "ref", j.Ref, "error", err)
		return store.Trial{}, false
	}
	at := j.StartedAt
	if at.IsZero() {
		at = time.Now()
	}
	if _, err := m.st.StartTrialAt(ctx, t.ID, j.Ref, j.ID, at); err != nil {
		slog.Error("single trial not started", "trial", t.ID, "job", j.ID, "error", err)
		return store.Trial{}, false
	}
	t.State, t.JobID = store.TrialRunning, j.ID
	return t, true
}

// trainingNoScoreText is the reason of a failed training run whose log names no error.
const trainingNoScoreText = "The training container exited without a score"

// maxTrialParams is the size of the parameters kept with a trial.
const maxTrialParams = 4 << 10

// singleRunInfo names what a run is for the book: its parameters (the template's train_config values, read from the
// platform, or the params.json of a notebook run, read by the notebook kind) and, for a training run, what it trains
// (issue #227). The parameters are an empty object, the subject nil, when there are none or they cannot be read.
func (m *Manager) singleRunInfo(ctx context.Context, j store.WatchJob, path string, p watch.Progress) (params, subject json.RawMessage) {
	var raw json.RawMessage
	switch path {
	case store.SearchPathNotebook:
		if p.Metrics != nil {
			raw = p.Metrics.Params
		}
	case store.SearchPathTemplate:
		read := m.summaryReader(j.ChatID)
		if read == nil {
			return emptyParams(nil), nil
		}
		doc, msg := readTrainConfig(ctx, read, j.Ref)
		if msg != "" {
			return emptyParams(nil), nil
		}
		if len(doc.Config.Train.Values) > 0 {
			raw, _ = json.Marshal(doc.Config.Train.Values)
		}
		id, _ := strconv.ParseInt(j.Ref, 10, 64)
		if b, err := json.Marshal(subjectOf(ctx, read, doc, id)); err == nil {
			subject = b
		}
	}
	return emptyParams(raw), subject
}

// emptyParams checks the parameters of a run (an object of limited size) and gives them canonically, else `{}`.
func emptyParams(raw json.RawMessage) json.RawMessage {
	empty := json.RawMessage(`{}`)
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || len(raw) > maxTrialParams || raw[0] != '{' {
		return empty
	}
	canon, err := plan.Canonical(raw)
	if err != nil {
		return empty
	}
	return canon
}

// finishSingleTrial ends the trial of a run that no search declared with the job. The trials of a search are
// ended by the search (it needs the score and the budget first).
func finishSingleTrial(ctx context.Context, m *Manager, j store.WatchJob, p watch.Progress) {
	if m.st == nil {
		return
	}
	trialJobs.Delete(j.ID)
	t, err := m.st.TrialByJob(ctx, j.ID)
	if err != nil {
		return
	}
	m.flushTrialProgress(t.ID, true)
	if t.SearchID != "" {
		return
	}
	state, errText := store.TrialDone, ""
	switch {
	case j.StoppedBy != "":
		state = store.TrialStopped
		if err := m.st.SetTrialStoppedBy(ctx, t.ID, j.StoppedBy); err != nil {
			slog.Error("who stopped a trial not stored", "trial", t.ID, "error", err)
		}
	case p.State == watch.StateFailed:
		state, errText = store.TrialFailed, watch.ClipRunes(platform.RedactText(p.Message), watch.MaxMessage)
		if j.Kind == trialKindTraining {
			// The last line of the log that names an error says more than the container's status (issue #227).
			errText = watch.ClipRunes(platform.RedactText(cmp.Or(watch.LastErrorLine(p.LogTail), trainingNoScoreText)), watch.MaxMessage)
		}
	}
	if _, err := m.st.FinishTrial(ctx, t.ID, state, errText); err != nil {
		slog.Error("single trial not ended", "trial", t.ID, "error", err)
	}
}

// --- the throttle of "trial_progress" ---

// A trial's events leave at most every progressThrottle. What arrives in between is gathered (new epochs add up, the
// rest is the latest) and sent when the time is up, so that the end of a series is never lost; the last event of a
// job (and the end of the job) goes out at once.

type throttled struct {
	chatID  string
	last    time.Time
	pending *TrialProgressEvent
	timer   *time.Timer
}

var (
	throttleMu sync.Mutex
	throttles  = map[string]*throttled{}
)

// sendTrialProgress publishes the event, or gathers it when the trial's last event was too recent. final sends at
// once and forgets the trial.
func (m *Manager) sendTrialProgress(chatID string, ev TrialProgressEvent, final bool) {
	throttleMu.Lock()
	s := throttles[ev.TrialID]
	if s == nil {
		s = &throttled{chatID: chatID}
		throttles[ev.TrialID] = s
	}
	merged := mergeProgress(s.pending, ev)
	wait := progressThrottle - time.Since(s.last)
	if final || wait <= 0 {
		if s.timer != nil {
			s.timer.Stop()
		}
		s.pending, s.timer, s.last = nil, nil, time.Now()
		if final {
			delete(throttles, ev.TrialID)
		}
		throttleMu.Unlock()
		m.publish(chatID, Event{Kind: "trial_progress", Data: merged})
		return
	}
	s.pending = &merged
	if s.timer == nil {
		id := ev.TrialID
		s.timer = time.AfterFunc(wait, func() { m.flushTrialProgress(id, false) })
	}
	throttleMu.Unlock()
}

// flushTrialProgress sends what was gathered for a trial; forget drops the trial's state as well (its job ended).
func (m *Manager) flushTrialProgress(trialID string, forget bool) {
	throttleMu.Lock()
	s := throttles[trialID]
	if s == nil {
		throttleMu.Unlock()
		return
	}
	pending := s.pending
	if s.timer != nil {
		s.timer.Stop()
	}
	s.pending, s.timer, s.last = nil, nil, time.Now()
	if forget {
		delete(throttles, trialID)
	}
	throttleMu.Unlock()
	if pending != nil {
		m.publish(s.chatID, Event{Kind: "trial_progress", Data: *pending})
	}
}

// mergeProgress adds a new event to the gathered one: the points add up (at most MaxTrialEpochs), every other field
// is the latest.
func mergeProgress(old *TrialProgressEvent, ev TrialProgressEvent) TrialProgressEvent {
	if old == nil {
		return ev
	}
	ev.Points = append(append([]map[string]any(nil), old.Points...), ev.Points...)
	if n := len(ev.Points); n > store.MaxTrialEpochs {
		ev.Points = ev.Points[n-store.MaxTrialEpochs:]
	}
	if ev.GPU == nil {
		ev.GPU = old.GPU
	}
	return ev
}

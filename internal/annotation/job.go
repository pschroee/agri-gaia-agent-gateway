// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package annotation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"agw/internal/platform"
	"agw/internal/store"
	"agw/internal/watch"
)

// The background jobs of V3: a check and a fill ask a function about many images, one call per image.
// That is minutes of work, which does not fit into a tool call (its result would come too late and the
// chat would be busy); the job runs in the gateway, holds one slot at a time (the CPU of the instance
// is shared) and writes its progress into annotation_jobs. The agent follows it with annotation_job or
// lets the observer kind annotation_job report it.

// The kinds of a job (table annotation_jobs). The mode of annotation_check is the kind of its job.
const (
	JobCalibrate = "calibrate"
	JobUncertain = "uncertain"
	JobFill      = "fill"
)

const (
	// JobTimeout bounds one job from its start. 1000 images at about a second each are about 17 minutes;
	// an hour is generous and still ends a job that hangs (V3).
	JobTimeout = 60 * time.Minute
	// progressEvery is how many images a job works through before it writes its progress down (V3).
	progressEvery = 10
	// The limits of a call: at most 1000 images per job, whatever the kind.
	jobLimitMax           = 1000
	jobLimitCalibrate     = 50
	jobLimitUncertain     = 200
	jobLimitFill          = 1000
	uncertainBelowDefault = 60
	// jobPageSize is how many uncertain images one page of an answer carries (V3).
	jobPageSize = 50
	// maxErrorRunes bounds the error text of a job in an answer and in the job line.
	maxErrorRunes = 300
)

// jobSpec is what a started job carries: the parse of the call, as it stands in the row (params) and
// as the runner works from it. The agent asked for these fields — the CVAT form of the mapping is
// rebuilt by the runner out of the dataset, the task and the function.
type jobSpec struct {
	Kind           string            `json:"kind"`
	DatasetID      int64             `json:"dataset_id"`
	Function       string            `json:"function"`
	Limit          int               `json:"limit"`
	Mapping        map[string]string `json:"mapping,omitempty"`
	UncertainBelow int               `json:"uncertain_below,omitempty"`
	MinConfidence  int               `json:"min_confidence,omitempty"`
}

// JobKindName is the kind of an observed background job of pre-annotation (V3, G1.7).
const JobKindName = "annotation_job"

// JobRoutePath is the pattern of the calls that start a job: a check and a fill. The registration with
// internal/watch lives in cmd/orchestrator/main.go, next to the kind.
func JobRoutePath() *regexp.Regexp {
	return regexp.MustCompile(`^` + regexp.QuoteMeta(platform.AnnotationPrefix) + `/(checks|fills)$`)
}

// JobRef reads the job id out of the answer of a started job (G1.7).
func JobRef(_ platform.Request, res platform.Result) (string, bool) {
	var a jobAnswer
	if res.Status != "ok" || json.Unmarshal([]byte(res.Body), &a) != nil || a.JobID == "" {
		return "", false
	}
	return a.JobID, true
}

// JobWatch is the kind that observes one job: its poll reads the row in annotation_jobs and nothing
// else — no platform call and no CVAT (V3).
func JobWatch(st *store.Store) watch.Kind {
	return watch.Kind{Name: JobKindName, Poll: func(ctx context.Context, j watch.Job, _ watch.Reader) (watch.Progress, error) {
		row, err := st.AnnotationJob(ctx, j.ChatID, j.Ref)
		if errors.Is(err, store.ErrNotFound) {
			return watch.Progress{State: watch.StateFailed, Message: "the job is gone"}, nil
		}
		if err != nil {
			return watch.Progress{}, err
		}
		return ProgressOf(row), nil
	}}
}

// ProgressOf is what the observer reports for one job: the share of the images it got through.
func ProgressOf(row store.AnnotationJob) watch.Progress {
	percent := 0.0
	if row.Total > 0 {
		percent = float64(row.Done) / float64(row.Total) * 100
		if percent > 100 {
			percent = 100
		}
	}
	// A finished job is done as a whole: a check samples fewer images than the task holds, and the line
	// must not stay at that share once the job has ended.
	if store.AnnotationJobFinal(row.State) {
		percent = 100
	}
	p := watch.Progress{State: watch.StateRunning, Percent: &percent, Message: jobLine(row)}
	switch row.State {
	case store.AnnotationJobFinished:
		p.State, p.Message = watch.StateDone, jobLine(row)
	case store.AnnotationJobFailed:
		p.State, p.Message = watch.StateFailed, "failed"
		if row.Error != "" {
			p.Message = "failed: " + oneLine(row.Error, 150)
		}
	}
	return p
}

// jobLine is the one line a job shows: its kind and how far it is.
func jobLine(row store.AnnotationJob) string {
	if row.Total > 0 {
		return fmt.Sprintf("%s: %d of %d images", row.Kind, row.Done, row.Total)
	}
	if row.State == store.AnnotationJobQueued {
		return row.Kind + ": waiting for the running job"
	}
	return row.Kind
}

// oneLine flattens text into one line of at most max runes.
func oneLine(s string, maxRunes int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	return string(r[:maxRunes-1]) + "…"
}

// ---------------------------------------------------------------------------
// The calls of the service
// ---------------------------------------------------------------------------

// jobAnswer is the answer of annotation_job and of the calls that start a job.
type jobAnswer struct {
	stepList
	JobID  string          `json:"job_id"`
	Kind   string          `json:"kind"`
	State  string          `json:"state"`
	Done   int             `json:"done"`
	Total  int             `json:"total"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
	Page   int             `json:"page"`
	Pages  int             `json:"pages"`
	Note   string          `json:"note,omitempty"`
}

// checkFollowNote says how to follow a job: the agent starts a background loop or lets the observer
// kind report it.
const checkFollowNote = "Follow it with annotation-job <job_id> in a background loop (30 s or more between queries); a job runs in the gateway and keeps running while the chat is idle."

// startCheck answers POST /_agw/annotation/checks: it checks the call, lays the job down and starts it
// in the background.
func (s *Service) startCheck(ctx context.Context, chatID string, req platform.Request) (platform.Result, error) {
	r := &recorder{chatID: chatID}
	var body struct {
		DatasetID      int64             `json:"dataset_id"`
		Function       string            `json:"function"`
		Mode           string            `json:"mode"`
		Limit          *int              `json:"limit"`
		Mapping        map[string]string `json:"mapping"`
		UncertainBelow *int              `json:"uncertain_below"`
	}
	if err := json.Unmarshal(req.Body, &body); err != nil {
		return failed(errors.New("the body of the check is not readable"))
	}
	// A raw call over the request tool reaches this path as well: check everything the tool checks.
	if body.DatasetID <= 0 {
		return failed(errors.New("dataset_id missing"))
	}
	if body.Function == "" {
		return failed(errors.New("function missing"))
	}
	if body.Mode != JobCalibrate && body.Mode != JobUncertain {
		return failed(fmt.Errorf("mode must be %s or %s", JobCalibrate, JobUncertain))
	}
	limit := jobLimitCalibrate
	if body.Mode == JobUncertain {
		limit = jobLimitUncertain
	}
	if body.Limit != nil {
		limit = *body.Limit
	}
	if limit < 1 || limit > jobLimitMax {
		return failed(fmt.Errorf("limit must be between 1 and %d images", jobLimitMax))
	}
	below := uncertainBelowDefault
	if body.UncertainBelow != nil {
		below = *body.UncertainBelow
	}
	if below < 1 || below > 99 {
		return failed(errors.New("uncertain_below must be between 1 and 99 percent"))
	}
	spec := jobSpec{Kind: body.Mode, DatasetID: body.DatasetID, Function: body.Function,
		Limit: limit, Mapping: body.Mapping, UncertainBelow: below}
	return s.startJob(ctx, chatID, r, spec, checkFollowNote)
}

// startFill answers POST /_agw/annotation/fills: the images without annotations are what it fills.
func (s *Service) startFill(ctx context.Context, chatID string, req platform.Request) (platform.Result, error) {
	r := &recorder{chatID: chatID}
	var body struct {
		DatasetID     int64             `json:"dataset_id"`
		Function      string            `json:"function"`
		MinConfidence *int              `json:"min_confidence"`
		Mapping       map[string]string `json:"mapping"`
		Limit         *int              `json:"limit"`
	}
	if err := json.Unmarshal(req.Body, &body); err != nil {
		return failed(errors.New("the body of the fill is not readable"))
	}
	if body.DatasetID <= 0 {
		return failed(errors.New("dataset_id missing"))
	}
	if body.Function == "" {
		return failed(errors.New("function missing"))
	}
	conf := defaultConfidence
	if body.MinConfidence != nil {
		conf = *body.MinConfidence
	}
	if conf < 1 || conf > 99 {
		return failed(errors.New("min_confidence must be between 1 and 99 percent"))
	}
	limit := jobLimitFill
	if body.Limit != nil {
		limit = *body.Limit
	}
	if limit < 1 || limit > jobLimitMax {
		return failed(fmt.Errorf("limit must be between 1 and %d images", jobLimitMax))
	}
	spec := jobSpec{Kind: JobFill, DatasetID: body.DatasetID, Function: body.Function,
		Limit: limit, Mapping: body.Mapping, MinConfidence: conf}
	return s.startJob(ctx, chatID, r, spec, checkFollowNote)
}

// startJob checks the dataset, the CVAT session and the function of a call, lays the job down and
// starts it. Everything that can be decided before the first image is decided here, so that a call
// that cannot succeed does not become a job.
func (s *Service) startJob(ctx context.Context, chatID string, r *recorder, spec jobSpec, note string) (platform.Result, error) {
	user, err := s.user(ctx, chatID)
	if err != nil {
		return failed(err)
	}
	d, err := s.ownDataset(ctx, chatID, r, spec.DatasetID)
	if err != nil {
		return failed(err)
	}
	taskID, err := taskIDOf(d)
	if err != nil {
		return failed(err)
	}
	if len(d.AnnotationLabels) == 0 {
		return failed(fmt.Errorf("dataset %d has no classes (annotation_labels); a check and a fill need the classes of the dataset", d.ID))
	}
	ses, err := s.currentSession(ctx, chatID, r)
	if err != nil {
		return failed(err)
	}
	fn, err := s.usableFunction(ctx, r, ses, user, spec.Function)
	if err != nil {
		return failed(err)
	}
	// A mapping the agent gave must hold from the start: it decides which classes a fill writes and
	// which ones a check measures through. A fill also needs a mapping that matches at least one class
	// without one, because it writes what the function answers and nothing else.
	if len(spec.Mapping) > 0 || spec.Kind == JobFill {
		if _, err := runMapping(spec.Mapping, fn.Labels, d.AnnotationLabels); err != nil {
			return failed(err)
		}
	}
	// A check without a mapping needs the classes of the function, so that CVAT drops nothing.
	if spec.Kind != JobFill && len(spec.Mapping) == 0 && len(fn.Labels) == 0 {
		return failed(errors.New("the function names no classes and no mapping was given; give mapping of the classes of the function onto the classes of the dataset"))
	}
	spec.Function = fn.ID
	params, err := json.Marshal(spec)
	if err != nil {
		return platform.Result{}, err
	}
	row, err := s.St.AddAnnotationJob(ctx, store.AnnotationJob{ChatID: chatID, Kind: spec.Kind,
		DatasetID: d.ID, TaskID: taskID, Function: fn.ID, Params: string(params)})
	if err != nil {
		return platform.Result{}, err
	}
	go s.runJob(row.ID, chatID, spec)
	return okAnswer(jobAnswer{stepList: r.steps(), JobID: row.ID, Kind: spec.Kind, State: row.State,
		Page: 1, Pages: 1, Note: note})
}

// jobIDRe is the shape of a job id: the uuid the table gives it.
var jobIDRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// jobIDFromPath reads the id of a path of the form <prefix>/jobs/<uuid>.
func jobIDFromPath(path string) (string, bool) {
	rest, ok := strings.CutPrefix(path, platform.AnnotationPrefix+"/jobs/")
	if !ok || rest == "" || strings.Contains(rest, "/") || !jobIDRe.MatchString(rest) {
		return "", false
	}
	return rest, true
}

// jobStatus answers annotation_job: the state of one job of this chat. The uncertain images of a
// finished job are cut into pages of 50 (V3).
func (s *Service) jobStatus(ctx context.Context, chatID, id string, query map[string]string) (platform.Result, error) {
	row, err := s.St.AnnotationJob(ctx, chatID, id)
	if errors.Is(err, store.ErrNotFound) {
		return failed(fmt.Errorf("unknown job %q in this chat", id))
	}
	if err != nil {
		return platform.Result{}, err
	}
	result, page, pages := pageOf(row, pageWanted(query))
	a := jobAnswer{stepList: stepList{Steps: []string{}}, JobID: row.ID, Kind: row.Kind, State: row.State,
		Done: row.Done, Total: row.Total, Result: result, Error: row.Error, Page: page, Pages: pages}
	if row.State == store.AnnotationJobFinished {
		a.Note = jobFinishedNote(row.Kind)
	}
	return okAnswer(a)
}

// pageWanted reads the page of a call; without one the first page is answered.
func pageWanted(query map[string]string) int {
	n := 0
	if v := query["page"]; v != "" {
		for _, c := range v {
			if c < '0' || c > '9' {
				return 0
			}
			n = n*10 + int(c-'0')
			if n > 10000 {
				return 0
			}
		}
	}
	return n
}

// pageOf cuts the result of a job into the page an answer carries. Only the images of an uncertainty
// are a list that can be long; the result of a calibration and of a fill stands as it is.
func pageOf(row store.AnnotationJob, want int) (json.RawMessage, int, int) {
	if row.Result == "" {
		return nil, 1, 1
	}
	if row.Kind != JobUncertain {
		return json.RawMessage(row.Result), 1, 1
	}
	var all uncertainAnswer
	if json.Unmarshal([]byte(row.Result), &all) != nil {
		return json.RawMessage(row.Result), 1, 1
	}
	pages := (len(all.Uncertain) + jobPageSize - 1) / jobPageSize
	page := want
	if page < 1 {
		page = 1
	}
	if pages < 1 {
		pages = 1
	}
	if page > pages {
		page = pages
	}
	from := (page - 1) * jobPageSize
	to := from + jobPageSize
	if to > len(all.Uncertain) {
		to = len(all.Uncertain)
	}
	items := make([]UncertainImage, 0, jobPageSize)
	items = append(items, all.Uncertain[from:to]...)
	raw, err := json.Marshal(uncertainAnswer{Checked: all.Checked, Uncertain: items})
	if err != nil {
		return json.RawMessage(row.Result), 1, 1
	}
	return raw, page, pages
}

// jobFinishedNote points at the step that follows a finished job.
func jobFinishedNote(kind string) string {
	switch kind {
	case JobFill:
		return "The images without annotations carry the proposals of the function now; annotation-summary counts them, annotation-sync writes them into the dataset. Correct them in CVAT before a training."
	case JobUncertain:
		return "The list names the images with the weakest answers; the user corrects them in CVAT, then annotation-sync and a new training follow."
	}
	return "Check the numbers before the run: a low hit rate means the function does not fit this dataset — annotate more images by hand instead of running it over all of them."
}

// ---------------------------------------------------------------------------
// The runner
// ---------------------------------------------------------------------------

// runJob runs one job in the background: it waits for the one slot of the service, works the job
// through and writes how it ended. A job cannot be stopped (V3): it ends when it is through, when its
// images run out or when the timeout of the job is over.
func (s *Service) runJob(id, chatID string, spec jobSpec) {
	ctx, cancel := context.WithTimeout(context.Background(), JobTimeout)
	defer cancel()
	select {
	case s.sem <- struct{}{}:
	case <-ctx.Done():
		s.finishJob(ctx, chatID, id, store.AnnotationJobFailed, "", "the job did not start within "+JobTimeout.String())
		return
	}
	defer func() { <-s.sem }()
	if err := s.St.SetAnnotationJobState(ctx, chatID, id, store.AnnotationJobRunning); err != nil {
		slog.Error("annotation job not started", "chat", chatID, "job", id, "error", err)
		return
	}
	slog.Info("annotation job running", "chat", chatID, "job", id, "kind", spec.Kind, "dataset", spec.DatasetID)
	var (
		result any
		err    error
	)
	if spec.Kind == JobFill {
		result, err = s.runFill(ctx, id, chatID, spec)
	} else {
		result, err = s.runCheck(ctx, id, chatID, spec)
	}
	raw := ""
	if result != nil {
		if b, merr := json.Marshal(result); merr == nil {
			raw = string(b)
		}
	}
	if err != nil {
		// A job that fails keeps what it got through: without it the agent would have no number at all.
		s.finishJob(ctx, chatID, id, store.AnnotationJobFailed, raw, err.Error())
		slog.Warn("annotation job failed", "chat", chatID, "job", id, "kind", spec.Kind, "images", spec.Limit, "error", err)
		return
	}
	s.finishJob(ctx, chatID, id, store.AnnotationJobFinished, raw, "")
	slog.Info("annotation job finished", "chat", chatID, "job", id, "kind", spec.Kind)
}

// finishJob writes the end of a job. It gets a deadline of its own, because the context of the job may
// be over when it is called (a job that ran into JobTimeout still has to end in its row).
func (s *Service) finishJob(ctx context.Context, chatID, id, state, result, errText string) {
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	if err := s.St.FinishAnnotationJob(wctx, chatID, id, state, result, oneLine(errText, maxErrorRunes)); err != nil {
		slog.Error("annotation job not ended", "chat", chatID, "job", id, "error", err)
	}
}

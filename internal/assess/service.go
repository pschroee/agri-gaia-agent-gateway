// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package assess

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"agw/internal/platform"
	"agw/internal/store"
)

// Service is the check service of the gateway (R0.19): it is the doer of the virtual path
// platform.AssessPrefix, so the calls of the check tools reach it after delegation, effect class and
// log like every other call. It starts a check in the background (one at a time), keeps its state in
// the store and answers the paths the check tools use. R1–R3 register their own tools, paths and
// handlers in their own files.
type Service struct {
	// Run is the container runner; without a runtime or an image the service cannot check anything.
	Run *Runner
	St  *store.Store
	// Max is AGW_ASSESS_MAX_PER_CHAT: how many checks one chat may use. Failed checks do not count.
	Max int

	mu       sync.Mutex
	handlers []handler
	sem      chan struct{}
	now      func() time.Time
}

// handler is one registered path of the check prefix.
type handler struct {
	method string
	re     *regexp.Regexp
	f      Handler
}

// Handler answers one path under platform.AssessPrefix; groups are the captures of its pattern.
type Handler func(ctx context.Context, chatID string, req platform.Request, groups []string) (platform.Result, error)

// CheckTimeout bounds one check from its start until its report stands (R0.20): a check that needs
// longer has gone wrong (a model that does not load, a container that hangs).
const CheckTimeout = 15 * time.Minute

// withoutChecks is what the agent should do when this setup cannot check a result (G1.13, R0.23).
const withoutChecks = "report the platform's own figure from the training log and say that the gateway did not check the result"

// New builds the service. A nil runner (no container runtime or no image): the capability "checks" is
// not available, and Start refuses.
func New(run *Runner, st *store.Store, max int) *Service {
	if max < 1 {
		max = 1
	}
	s := &Service{Run: run, St: st, Max: max, sem: make(chan struct{}, 1), now: time.Now}
	// The id is the first capture group: the handler reads it from there (groups[1]).
	s.Handle(http.MethodGet, regexp.MustCompile(`^`+regexp.QuoteMeta(platform.AssessPrefix)+`/checks/(`+checkIDPattern+`)$`), s.status)
	return s
}

// checkIDPattern is the shape of a check id (a uuid), as in platform.AssessCheckIDRe.
const checkIDPattern = `[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`

// Configured reports whether this setup can run checks at all (R0.23).
func (s *Service) Configured() bool { return s != nil && s.St != nil && s.Run.Configured() }

// Handle registers a handler for one path pattern (the full path including the prefix). The order does
// not matter: the patterns of the features do not overlap.
func (s *Service) Handle(method string, path *regexp.Regexp, h Handler) {
	if s == nil || path == nil || h == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers = append(s.handlers, handler{method: method, re: path, f: h})
}

// Do answers one call under the check prefix (the manager's virtual doer, G1.5).
func (s *Service) Do(ctx context.Context, chatID string, req platform.Request) (platform.Result, error) {
	if s == nil {
		return unavailableResult(), nil
	}
	s.mu.Lock()
	list := append([]handler(nil), s.handlers...)
	s.mu.Unlock()
	for _, h := range list {
		if h.method != req.Method {
			continue
		}
		if m := h.re.FindStringSubmatch(req.Path); m != nil {
			return h.f(ctx, chatID, req, m)
		}
	}
	return platform.Result{Status: "error", Message: "unknown check call " + req.Method + " " + req.Path}, nil
}

// unavailableResult is the answer of a check tool on a setup without the capability (G1.13).
func unavailableResult() platform.Result {
	return platform.Result{Status: platform.StatusUnavailable,
		Message: platform.UnavailableText("checks", "it is not configured on this instance", withoutChecks)}
}

// status is GET /_agw/assess/checks/<id>: the state and the report of one check of this chat (R0.17).
func (s *Service) status(ctx context.Context, chatID string, req platform.Request, groups []string) (platform.Result, error) {
	id := ""
	if len(groups) > 1 {
		id = groups[1]
	}
	if s.St == nil {
		return unavailableResult(), nil
	}
	row, err := s.St.Assessment(ctx, chatID, id)
	if errors.Is(err, store.ErrNotFound) {
		return platform.Result{Status: "error", Message: "check not found in this chat"}, nil
	}
	if err != nil {
		return platform.Result{}, err
	}
	return jsonResult(StatusAnswer{
		CheckID:  row.ID,
		Kind:     row.Kind,
		State:    row.State,
		Report:   row.Report,
		Progress: progressOf(row),
		Error:    row.Error,
	})
}

// StatusAnswer is the answer of assess_status and of the check route (R0.17, L4).
type StatusAnswer struct {
	CheckID string `json:"check_id"`
	Kind    string `json:"kind"`
	State   string `json:"state"`
	// Report: the finished report, null while the check runs (or after it failed without a partial one).
	Report json.RawMessage `json:"report"`
	// Progress: what the check is doing, null unless it runs (L4).
	Progress json.RawMessage `json:"progress"`
	Error    string          `json:"error,omitempty"`
}

// progressOf shows the progress only while the check runs: a finished check's progress is stale.
func progressOf(row store.Assessment) json.RawMessage {
	if row.State != store.AssessRunning {
		return nil
	}
	return row.Progress
}

// Progress is what a running check reports about itself (L4): the set it measures, how many images are
// done and a short note.
type Progress struct {
	Set    string `json:"set,omitempty"`
	Images int    `json:"images,omitempty"`
	Note   string `json:"note,omitempty"`
}

// SetProgress records the progress of a running check (L4). A check that is already over keeps the
// state it has.
func (s *Service) SetProgress(ctx context.Context, checkID string, p Progress) error {
	if s == nil || s.St == nil {
		return nil
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return s.St.SetAssessmentProgress(ctx, checkID, raw)
}

// Start runs one check in the background (R0.20). It lays the row down at once (queued) and answers its
// id, so that the tool call does not wait for minutes; the check itself waits for the one slot, then
// runs and writes its report. run gets the report or an error; with an error it may still answer a
// partial report (Figures), which is stored next to the error (L5).
func (s *Service) Start(ctx context.Context, chatID, kind, subject string, evalDataset int64, final bool,
	run func(ctx context.Context) (Report, error)) (string, error) {
	if !s.Configured() {
		return "", errors.New("checks are not configured on this instance")
	}
	used, err := s.St.CountAssessments(ctx, chatID)
	if err != nil {
		return "", err
	}
	if used >= s.Max {
		return "", fmt.Errorf("check limit reached: this chat has used %d of %d checks (AGW_ASSESS_MAX_PER_CHAT)", used, s.Max)
	}
	if final {
		done, err := s.St.FinalUsed(ctx, chatID, evalDataset)
		if err != nil {
			return "", err
		}
		if done {
			return "", fmt.Errorf("the final check of this dataset was already run in this chat; compare further rounds with final=false")
		}
	}
	row, err := s.St.CreateAssessment(ctx, store.Assessment{
		ChatID: chatID, Kind: kind, Subject: subject, EvalDatasetID: evalDataset, Final: final,
		State: store.AssessQueued, ToolCallID: store.ToolCallFrom(ctx),
	})
	if err != nil {
		return "", err
	}
	go s.work(row.ID, chatID, run)
	return row.ID, nil
}

// work is the background part of a check: wait for the one slot, run, record the end.
func (s *Service) work(id, chatID string, run func(ctx context.Context) (Report, error)) {
	ctx, cancel := context.WithTimeout(context.Background(), CheckTimeout)
	defer cancel()
	select {
	case s.sem <- struct{}{}:
	case <-ctx.Done():
		s.fail(id, chatID, Report{}, "the check did not start within "+CheckTimeout.String())
		return
	}
	defer func() { <-s.sem }()
	if err := s.St.SetAssessmentState(ctx, chatID, id, store.AssessRunning); err != nil {
		return
	}
	rep, err := run(ctx)
	if err != nil {
		s.fail(id, chatID, rep, err.Error())
		return
	}
	rep.Finish()
	raw, merr := json.Marshal(rep)
	if merr != nil {
		s.fail(id, chatID, Report{}, "the report is not writable: "+merr.Error())
		return
	}
	_ = s.St.FinishAssessment(ctx, chatID, id, store.AssessFinished, raw, "")
}

// fail ends a check as failed. A partial report (with figures) is kept, without verdict and header:
// there is no verdict behind a check that broke (L5).
func (s *Service) fail(id, chatID string, rep Report, errText string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var raw json.RawMessage
	if rep.Figures != nil {
		rep.Verdict = ""
		rep.Header = ""
		if b, err := json.Marshal(rep); err == nil {
			raw = b
		}
	}
	_ = s.St.FinishAssessment(ctx, chatID, id, store.AssessFailed, raw, errText)
}

// jsonResult is the answer of a check tool: one JSON value.
func jsonResult(v any) (platform.Result, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return platform.Result{Status: "error", Message: "check answer not writable: " + err.Error()}, nil
	}
	return platform.Result{Status: "ok", HTTPStatus: http.StatusOK, Body: string(b)}, nil
}

// TrimMessage cuts one line to the length a job line takes (200 runes, as in watch.Progress).
func TrimMessage(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len([]rune(s)) <= 200 {
		return s
	}
	return string([]rune(s)[:199]) + "…"
}

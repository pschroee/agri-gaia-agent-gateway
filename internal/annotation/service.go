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
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"agw/internal/platform"
	"agw/internal/store"
	"agw/internal/watch"
)

// Service is the pre-annotation service of the gateway (V1). It is the doer of the virtual path
// platform.AnnotationPrefix: the manager sends the calls of the annotation_ tools here instead of to
// the platform (E4), after delegation, effect class and approval like every other call.
type Service struct {
	P  *platform.Client
	C  *CVAT
	St *store.Store
	// Operator are the functions the operator installed (AGW_ANNOTATION_FUNCTIONS). Everything else the
	// agent may run must be the user's own function (V2), recognised by its name.
	Operator []string
	// PublicURL is the address of CVAT for the user's browser (AGW_CVAT_PUBLIC_URL); the links in the
	// answers point there.
	PublicURL string
	// MaxFunctions is how many functions one user may have built from their own models
	// (AGW_ANNOTATION_MAX_FUNCTIONS, E14); ModelMaxMB bounds the model file such a function is built
	// from (AGW_ANNOTATION_MODEL_MAX_MB). Both are enforced by V2.
	MaxFunctions int
	ModelMaxMB   int64

	mu       sync.Mutex
	sessions map[string]session // the CVAT session per user, in memory only (E2)
	// sem is the one slot of a background job (V3): a check and a fill ask one function about one image
	// at a time, and the CPU of the instance is shared with the platform. Without it two jobs of two
	// chats would run side by side.
	sem chan struct{}
}

// session is one user's CVAT session: what the platform's login answered. It never leaves this
// package, and no answer, message or log entry of the gateway carries it (E2).
type session struct {
	key       string
	csrf      string
	sessionID string
	until     time.Time
}

const (
	// sessionTTL is how long one session is used before the gateway logs in again. There is no logout:
	// it would end the user's own session in the browser (E2).
	sessionTTL = 10 * time.Minute
	// Limits of the calls to the platform.
	maxLoginBytes    = 64 << 10
	maxJSONBytes     = 1 << 20
	maxDownloadBytes = 512 << 20 // the whole dataset as a ZIP: the only way the platform exports a task
	// maxExcInfo bounds the traceback of a failed run: text from CVAT is data and stays short.
	maxExcInfo = 2000
	// maxAnswerText bounds what the answer of a failed platform call contributes to a message.
	maxAnswerText = 300
	// imageDatasetType is the only dataset type CVAT can take.
	imageDatasetType = "AgriImageDataResource"
	// sourceAuto marks the annotations a pre-annotation wrote; everything else is manual (E21).
	sourceAuto = "auto"
	// minConfidence of a run when the agent names none, and the range it must stay in (E13).
	defaultConfidence = 50
)

// runIDRe checks a run id the way the tool of the agent does: the id CVAT gives its queue entry (hex)
// or a plain number.
var runIDRe = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)

// New builds the service. p and c must be wired; the store keeps the runs of a chat and the
// functions a user built (V2). maxFunctions and modelMaxMB bound the function build of V2.
func New(p *platform.Client, st *store.Store, c *CVAT, operator []string, publicURL string, maxFunctions int, modelMaxMB int64) *Service {
	return &Service{P: p, C: c, St: st, Operator: operator, PublicURL: publicURL,
		MaxFunctions: maxFunctions, ModelMaxMB: modelMaxMB, sessions: map[string]session{},
		sem: make(chan struct{}, 1)}
}

// WatchKindName is the kind of an observed pre-annotation run (G1.7, autonomy A-E2).
const WatchKindName = "annotation_run"

// RunRoutePath is the path of a call that starts a run; the kind above is bound to it.
var RunRoutePath = regexp.MustCompile(`^` + regexp.QuoteMeta(platform.AnnotationPrefix) + `/runs$`)

// RegisterWatch registers the kind annotation_run and the route of a started run with internal/watch
// (G1.7). The loop that polls the jobs belongs to the autonomy plan (A-E2); without it nothing polls,
// and this service answers annotation_run from CVAT itself.
//
// The build of a function (V2) is observed with the kind platform_task of that plan: this service
// brings only the route, no kind and no polling of its own.
func (s *Service) RegisterWatch() {
	watch.Register(watch.Kind{Name: WatchKindName, Poll: s.poll})
	watch.RegisterRoute(http.MethodPost, RunRoutePath, WatchKindName, RunRef)
	watch.RegisterRoute(http.MethodPost, FunctionRoutePath, WatchKindPlatformTask, FunctionRef)
}

// RunRef reads the run id out of the answer of a started run.
func RunRef(_ platform.Request, res platform.Result) (string, bool) {
	var a runAnswer
	if json.Unmarshal([]byte(res.Body), &a) != nil || a.RunID == "" {
		return "", false
	}
	return a.RunID, true
}

// Do answers one call of the annotation_ tools (E4). Method and path alone decide what runs; the body
// is checked again here, because a raw call over the request tool reaches the same paths.
func (s *Service) Do(ctx context.Context, chatID string, req platform.Request) (platform.Result, error) {
	path := req.Path
	switch {
	case req.Method == http.MethodGet && path == platform.AnnotationPrefix+"/functions":
		return s.functions(ctx, chatID)
	case req.Method == http.MethodPost && path == platform.AnnotationPrefix+"/functions":
		return s.createFunction(ctx, chatID, req)
	case req.Method == http.MethodPost && path == platform.AnnotationPrefix+"/tasks":
		return s.task(ctx, chatID, req)
	case req.Method == http.MethodPost && path == platform.AnnotationPrefix+"/runs":
		return s.preAnnotate(ctx, chatID, req)
	case req.Method == http.MethodGet && path == platform.AnnotationPrefix+"/runs":
		return platform.Result{Status: "error", Message: "annotation_run needs a run id"}, nil
	// V3: a check (calibrate | uncertain) and a fill are background jobs of the gateway; their state is
	// what annotation_job answers.
	case req.Method == http.MethodPost && path == platform.AnnotationPrefix+"/checks":
		return s.startCheck(ctx, chatID, req)
	case req.Method == http.MethodPost && path == platform.AnnotationPrefix+"/fills":
		return s.startFill(ctx, chatID, req)
	}
	if id, ok := jobIDFromPath(path); ok && req.Method == http.MethodGet {
		return s.jobStatus(ctx, chatID, id, req.Query)
	}
	if id, ok := runIDFromPath(path); ok && req.Method == http.MethodGet {
		return s.runStatus(ctx, chatID, id)
	}
	if id, ok := datasetIDFromPath(path, "summary"); ok && req.Method == http.MethodGet {
		return s.summary(ctx, chatID, id)
	}
	if id, ok := datasetIDFromPath(path, "sync"); ok && req.Method == http.MethodPost {
		return s.syncDataset(ctx, chatID, id)
	}
	return platform.Result{Status: "error", Message: "unknown annotation call " + req.Method + " " + path}, nil
}

// runIDFromPath reads the id of a path of the form <prefix>/runs/<id>.
func runIDFromPath(path string) (string, bool) {
	rest, ok := strings.CutPrefix(path, platform.AnnotationPrefix+"/runs/")
	if !ok || rest == "" || strings.Contains(rest, "/") || !runIDRe.MatchString(rest) {
		return "", false
	}
	return rest, true
}

// datasetIDFromPath reads the identifier of a path of the form <prefix>/datasets/<id>/<op>.
func datasetIDFromPath(path, op string) (int64, bool) {
	rest, ok := strings.CutPrefix(path, platform.AnnotationPrefix+"/datasets/")
	if !ok {
		return 0, false
	}
	rest, ok = strings.CutSuffix(rest, "/"+op)
	if !ok || rest == "" || strings.Contains(rest, "/") {
		return 0, false
	}
	id, err := strconv.ParseInt(rest, 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// ---------------------------------------------------------------------------
// Answers, steps and failures
// ---------------------------------------------------------------------------

// stepList is what every answer of the service carries: the calls the one operation made (E5). The
// lines name method, path and status only, never a value of the CVAT session.
type stepList struct {
	Steps []string `json:"steps"`
}

// recorder collects the calls of one operation. Every line is also logged as "annotation step", so the
// protocol shows what an operation did even when the agent only sees the answer.
type recorder struct {
	chatID string
	lines  []string
}

// add notes one call with its status.
func (r *recorder) add(call string, status int) {
	line := fmt.Sprintf("%s %d", call, status)
	if r.lines == nil {
		r.lines = []string{}
	}
	r.lines = append(r.lines, line)
	slog.Info("annotation step", "chat", r.chatID, "call", line)
}

// steps are the lines for the answer; nil becomes an empty list, so the field is always there.
func (r *recorder) steps() stepList {
	if r.lines == nil {
		return stepList{Steps: []string{}}
	}
	return stepList{Steps: r.lines}
}

// cvat notes a call at the CVAT server: 200 when it answered, otherwise its status (0 for a call that
// got no answer at all).
func (r *recorder) cvat(call string, err error) {
	status := http.StatusOK
	if err != nil {
		status = cvatStatus(err)
	}
	r.add(call, status)
}

// platformCall performs one platform call of an operation (E5) and notes it.
func (s *Service) platformCall(ctx context.Context, r *recorder, req platform.Request, limit int64) (platform.Result, []byte, error) {
	res, raw, err := s.P.DoTrusted(ctx, r.chatID, req, limit)
	if err != nil {
		return res, nil, fmt.Errorf("%s: %w", req.String(), err)
	}
	r.add(req.String(), res.HTTPStatus)
	return res, raw, nil
}

// platformJSON performs a reading platform call and decodes its answer.
func (s *Service) platformJSON(ctx context.Context, r *recorder, req platform.Request, limit int64, v any) error {
	raw, err := s.platformExpect(ctx, r, req, limit)
	if err != nil {
		return err
	}
	if json.Unmarshal(raw, v) != nil {
		return fmt.Errorf("%s: the platform's answer is not readable", req.String())
	}
	return nil
}

// platformExpect performs a platform call and refuses anything but 2xx; the answer of the platform
// stays the caller's.
func (s *Service) platformExpect(ctx context.Context, r *recorder, req platform.Request, limit int64) ([]byte, error) {
	res, raw, err := s.platformCall(ctx, r, req, limit)
	if err != nil {
		return nil, err
	}
	if res.Status != "ok" || res.HTTPStatus < 200 || res.HTTPStatus >= 300 {
		return nil, platformAnswered{req: req.String(), status: res.HTTPStatus, body: answerText(res.Body)}
	}
	return raw, nil
}

// answerText shortens what the platform answered to a failed call (it is redacted and truncated by
// DoTrusted already) into one line, so that the agent and the log show why the call failed.
func answerText(body string) string {
	body = strings.Join(strings.Fields(body), " ")
	if body == "" || body == "null" {
		return ""
	}
	return truncate(body, maxAnswerText)
}

// okAnswer marshals the answer of an operation into a Result with status ok and HTTP 200.
func okAnswer(v any) (platform.Result, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return platform.Result{}, err
	}
	return platform.Result{Status: "ok", HTTPStatus: http.StatusOK, Body: string(b)}, nil
}

// Denied is a refusal the gateway writes itself (E6, E7): it reaches the agent with status denied.
type Denied struct{ Msg string }

func (d Denied) Error() string { return d.Msg }

// deny builds such a refusal.
func deny(format string, args ...any) error { return Denied{Msg: fmt.Sprintf(format, args...)} }

// failed turns an error of an operation into the answer of the agent.
func failed(err error) (platform.Result, error) {
	var d Denied
	if errors.As(err, &d) {
		return platform.Result{Status: "denied", Message: err.Error()}, nil
	}
	return platform.Result{Status: "error", Message: err.Error()}, nil
}

// truncate shortens text that comes from CVAT (class names, file names, a traceback): it is data, not
// an instruction, and stays short.
func truncate(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// platformAnswered is the error of a call the platform answered with a status outside 2xx. It names
// the answer of the platform, shortened, so that a failure like "HTTP 500" is diagnosable at once:
// on 16.10.2026 the body carried the reason (a MinIO error) that cost an investigation.
type platformAnswered struct {
	req    string
	status int
	body   string
}

func (p platformAnswered) Error() string {
	if p.body == "" {
		return fmt.Sprintf("%s: the platform answered HTTP %d", p.req, p.status)
	}
	return fmt.Sprintf("%s: the platform answered HTTP %d: %s", p.req, p.status, p.body)
}

// errCVAT is a failed call at the CVAT server with its status; 0 stands for a call that got no answer.
type errCVAT struct {
	method, path string
	status       int
	err          error
}

func (e errCVAT) Error() string { return fmt.Sprintf("%s %s: %v", e.method, e.path, e.err) }

func (e errCVAT) Unwrap() error { return e.err }

// cvatStatus is the HTTP status of a failed call at the CVAT server, 0 when it never got one.
func cvatStatus(err error) int {
	var e errCVAT
	if errors.As(err, &e) {
		return e.status
	}
	return 0
}

// ---------------------------------------------------------------------------
// User and session
// ---------------------------------------------------------------------------

// user is the name whose rights an operation carries (E6): the preferred_username of the token the
// chat uses, the same source the backend keeps as owner.
func (s *Service) user(ctx context.Context, chatID string) (string, error) {
	name, err := s.P.ChatUser(ctx, chatID)
	if err != nil {
		return "", fmt.Errorf("the platform user of this chat is not readable: %w", err)
	}
	return name, nil
}

// currentSession returns the user's CVAT session, logging in through the platform when none is held
// (E1, E2). The session is cached per user for sessionTTL; there is no logout.
func (s *Service) currentSession(ctx context.Context, chatID string, r *recorder) (session, error) {
	name, err := s.user(ctx, chatID)
	if err != nil {
		return session{}, err
	}
	now := time.Now()
	s.mu.Lock()
	ses, ok := s.sessions[name]
	s.mu.Unlock()
	if ok && now.Before(ses.until) {
		return ses, nil
	}
	raw, err := s.platformExpect(ctx, r, platform.Request{Method: http.MethodPost, Path: "/cvat/auth/login"}, maxLoginBytes)
	if err != nil {
		// The backend logs in to CVAT with a password derived from the user's sub; a user CVAT does not
		// know answers 400 there, which the backend passes on as 500. Both mean the same to the agent.
		var pa platformAnswered
		if errors.As(err, &pa) {
			return session{}, errors.New("you have no CVAT account yet; open the platform once in the browser and try again")
		}
		return session{}, err
	}
	var a struct {
		Key       string `json:"key"`
		CSRF      string `json:"csrftoken"`
		SessionID string `json:"sessionid"`
	}
	if json.Unmarshal(raw, &a) != nil || a.Key == "" || a.SessionID == "" {
		return session{}, errors.New("the platform's CVAT login did not answer with a session")
	}
	ses = session{key: a.Key, csrf: a.CSRF, sessionID: a.SessionID, until: now.Add(sessionTTL)}
	s.mu.Lock()
	if s.sessions == nil {
		s.sessions = map[string]session{}
	}
	for k, old := range s.sessions { // drop the sessions of users who are no longer active
		if !now.Before(old.until) {
			delete(s.sessions, k)
		}
	}
	s.sessions[name] = ses
	s.mu.Unlock()
	return ses, nil
}

// ---------------------------------------------------------------------------
// annotation_functions
// ---------------------------------------------------------------------------

// functionInfo is one function the agent may run.
type functionInfo struct {
	ID     string   `json:"id"`
	Name   string   `json:"name"`
	Kind   string   `json:"kind"`
	Labels []string `json:"labels"`
	Source string   `json:"source"` // yours | operator
	State  string   `json:"state"`
}

// functionsAnswer is the answer of annotation_functions.
type functionsAnswer struct {
	stepList
	Functions []functionInfo `json:"functions"`
	Hidden    int            `json:"hidden"`
}

// functionStateReady is the state of a function CVAT lists: it runs. V2 adds deploying, failed and
// missing from the gateway's own register (store.AnnotationFunction*).
const functionStateReady = store.AnnotationFunctionReady

// functions lists the functions the agent may run (E7): the user's own and the operator's. Everything
// else is counted in hidden. The functions the user built from a model (V2) come from the gateway's
// register, which is compared with CVAT's list right here: only what CVAT lists runs.
func (s *Service) functions(ctx context.Context, chatID string) (platform.Result, error) {
	r := &recorder{chatID: chatID}
	user, err := s.user(ctx, chatID)
	if err != nil {
		return failed(err)
	}
	ses, err := s.currentSession(ctx, chatID, r)
	if err != nil {
		return failed(err)
	}
	all, err := s.C.Functions(ctx, ses)
	r.cvat("GET /api/lambda/functions", err)
	if err != nil {
		return failed(err)
	}
	out := functionsAnswer{Functions: []functionInfo{}}
	prefix := FunctionNamePrefix(user)
	live := map[string]bool{}
	for _, f := range all {
		info := functionInfo{ID: f.ID, Name: f.Name, Kind: f.Kind, Labels: f.Labels, State: functionStateReady}
		if info.Name == "" {
			info.Name = f.ID
		}
		switch {
		case contains(s.Operator, f.ID):
			info.Source = "operator"
		case strings.HasPrefix(f.ID, prefix):
			info.Source = "yours"
			live[f.ID] = true
		default:
			// A function of another user is not usable: it receives every image of the run.
			out.Hidden++
			continue
		}
		if info.Labels == nil {
			info.Labels = []string{}
		}
		out.Functions = append(out.Functions, info)
	}
	// The own functions of the register that CVAT does not list (yet): they are building, failed or
	// gone. Their state is what the agent needs to know.
	rows, err := s.St.AnnotationFunctionsOf(ctx, user)
	if err != nil {
		return platform.Result{}, err
	}
	if err := s.syncFunctionStates(ctx, r, rows, live); err != nil {
		return failed(err)
	}
	for _, row := range rows {
		if live[row.Name] {
			continue
		}
		labels := row.Labels
		if labels == nil {
			labels = []string{}
		}
		out.Functions = append(out.Functions, functionInfo{ID: row.Name, Name: row.Name,
			Kind: "detector", Labels: labels, Source: "yours", State: row.State})
	}
	out.stepList = r.steps()
	return okAnswer(out)
}

// FunctionNamePrefix is the beginning of the name of a function that belongs to a user: V2 names a
// function built from a model agw-<user>-m<model id>-<hash>. The rule of <user> lives here alone.
func FunctionNamePrefix(user string) string { return "agw-" + sanitizeUser(user) + "-" }

// sanitizeUser brings a user name into the form V2 puts into a function name: lower case, everything
// except [a-z0-9] becomes a dash, runs of dashes collapse, at most 20 characters.
func sanitizeUser(name string) string {
	var b strings.Builder
	dash := false
	for _, c := range strings.ToLower(name) {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			b.WriteRune(c)
			dash = false
		case !dash:
			b.WriteByte('-')
			dash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 20 {
		out = strings.Trim(out[:20], "-")
	}
	return out
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// annotation_task
// ---------------------------------------------------------------------------

// platformDataset is the part of a platform dataset the service needs.
type platformDataset struct {
	ID               int64    `json:"id"`
	Name             string   `json:"name"`
	Owner            string   `json:"owner"`
	DatasetType      string   `json:"dataset_type"`
	AnnotationLabels []string `json:"annotation_labels"`
	AnnotationTaskID *int64   `json:"annotation_task_id"`
}

// taskAnswer is the answer of annotation_task.
type taskAnswer struct {
	stepList
	DatasetID int64  `json:"dataset_id"`
	TaskID    int64  `json:"task_id"`
	Created   bool   `json:"created"`
	CVATURL   string `json:"cvat_url"`
}

// ownDataset reads a dataset and checks that it belongs to the chat's user (E6): the platform checks
// the owner only when deleting, and its annotate route mounts the bucket of any dataset.
func (s *Service) ownDataset(ctx context.Context, chatID string, r *recorder, id int64) (platformDataset, error) {
	user, err := s.user(ctx, chatID)
	if err != nil {
		return platformDataset{}, err
	}
	var d platformDataset
	if err := s.platformJSON(ctx, r, platform.Request{Method: http.MethodGet, Path: "/datasets/" + strconv.FormatInt(id, 10)}, maxJSONBytes, &d); err != nil {
		return platformDataset{}, err
	}
	if d.Owner != user {
		return d, deny("dataset %d belongs to %s; only your own datasets can be annotated", id, d.Owner)
	}
	return d, nil
}

// noTaskYet is the refusal of an operation that needs a CVAT task of the dataset.
func noTaskYet(id int64) error {
	return fmt.Errorf("dataset %d has no CVAT task yet; run annotation_task first", id)
}

// taskIDOf is the task recorded on a dataset, with the refusal above when there is none.
func taskIDOf(d platformDataset) (int64, error) {
	if d.AnnotationTaskID == nil || *d.AnnotationTaskID <= 0 {
		return 0, noTaskYet(d.ID)
	}
	return *d.AnnotationTaskID, nil
}

// task creates the CVAT task of a dataset or reuses the one recorded on it, and records it on the
// dataset.
func (s *Service) task(ctx context.Context, chatID string, req platform.Request) (platform.Result, error) {
	r := &recorder{chatID: chatID}
	var body struct {
		DatasetID int64 `json:"dataset_id"`
	}
	if err := json.Unmarshal(req.Body, &body); err != nil || body.DatasetID <= 0 {
		return failed(errors.New("dataset_id missing"))
	}
	user, err := s.user(ctx, chatID)
	if err != nil {
		return failed(err)
	}
	d, err := s.ownDataset(ctx, chatID, r, body.DatasetID)
	if err != nil {
		return failed(err)
	}
	if d.DatasetType != imageDatasetType {
		return failed(fmt.Errorf("dataset %d is of type %s; only AgriImageDataResource can be annotated", d.ID, d.DatasetType))
	}
	if len(d.AnnotationLabels) == 0 {
		return failed(fmt.Errorf(`dataset %d has no classes (annotation_labels); CVAT would create the placeholder class "object" and the import fails. Create the dataset with its classes`, d.ID))
	}
	ses, err := s.currentSession(ctx, chatID, r)
	if err != nil {
		return failed(err)
	}
	taskID := int64(0)
	created := false
	if d.AnnotationTaskID != nil && *d.AnnotationTaskID > 0 {
		t, err := s.C.Task(ctx, ses, *d.AnnotationTaskID)
		r.cvat("GET /api/tasks/"+strconv.FormatInt(*d.AnnotationTaskID, 10), err)
		switch {
		case err == nil && t.ID > 0 && strings.EqualFold(t.Owner.Username, user):
			taskID = t.ID // the task is there and belongs to this user: reuse it
		case err == nil:
			return failed(deny("CVAT task %d belongs to %s; only your own tasks can be used", t.ID, t.Owner.Username))
		case errors.Is(err, ErrNotFound):
			// gone in CVAT: create a new one
		default:
			return failed(err)
		}
	}
	if taskID == 0 {
		// 1. The way the toolbar takes: the backend mounts the dataset's bucket in the CVAT container,
		// and CVAT reads the images from there.
		if _, err := s.platformExpect(ctx, r, platform.Request{Method: http.MethodPost, Path: "/datasets/" + strconv.FormatInt(d.ID, 10) + "/annotate"}, maxJSONBytes); err != nil {
			return failed(err)
		}
		// 2. The task itself, with the session in the body; the backend waits for CVAT to import the
		// images, so the call may take minutes.
		sessionBody, err := json.Marshal(map[string]string{"key": ses.key, "csrftoken": ses.csrf, "sessionid": ses.sessionID})
		if err != nil {
			return platform.Result{}, err
		}
		raw, err := s.platformExpect(ctx, r, platform.Request{Method: http.MethodPost, Path: "/cvat/tasks/" + strconv.FormatInt(d.ID, 10), Body: sessionBody}, maxJSONBytes)
		if err != nil {
			return failed(err)
		}
		var out struct {
			ID int64 `json:"id"`
		}
		if json.Unmarshal(raw, &out) != nil || out.ID <= 0 {
			return failed(errors.New("the platform did not answer the CVAT task with an id"))
		}
		taskID = out.ID
		created = true
		// 3. Record it on the dataset, and only with the id from the answer right before: the backend
		// exports the task with the CVAT superuser, so another id would read foreign images (1.5).
		patch := json.RawMessage(`{"annotation_task_id":` + strconv.FormatInt(taskID, 10) + `}`)
		if _, err := s.platformExpect(ctx, r, platform.Request{Method: http.MethodPatch, Path: "/datasets/" + strconv.FormatInt(d.ID, 10), Body: patch}, maxJSONBytes); err != nil {
			return failed(err)
		}
	}
	return okAnswer(taskAnswer{stepList: r.steps(), DatasetID: d.ID, TaskID: taskID, Created: created, CVATURL: s.cvatURL(taskID)})
}

// cvatURL is the link to a task for the user's browser: the public address of CVAT, not the API URL.
func (s *Service) cvatURL(taskID int64) string {
	return strings.TrimRight(s.PublicURL, "/") + "/tasks/" + strconv.FormatInt(taskID, 10)
}

// ---------------------------------------------------------------------------
// pre_annotate
// ---------------------------------------------------------------------------

// runAnswer is the answer of pre_annotate.
type runAnswer struct {
	stepList
	RunID         string            `json:"run_id"`
	DatasetID     int64             `json:"dataset_id"`
	TaskID        int64             `json:"task_id"`
	Function      string            `json:"function"`
	MinConfidence int               `json:"min_confidence"`
	Mapping       map[string]string `json:"mapping"`
	Existing      string            `json:"existing"`
	State         string            `json:"state"`
	Note          string            `json:"note"`
}

// runFollowNote says how to follow a run; the loop is the agent's (E11).
const runFollowNote = "Follow it with annotation-run <run_id> in a background loop (30 s or more between queries)."

// runFinishedNote points at the two steps that follow a finished run.
const runFinishedNote = "The annotations are in CVAT. annotation-summary counts them; annotation-sync writes them into the dataset."

// preAnnotate starts a run of a function over all images of the dataset's CVAT task.
func (s *Service) preAnnotate(ctx context.Context, chatID string, req platform.Request) (platform.Result, error) {
	r := &recorder{chatID: chatID}
	var body struct {
		DatasetID     int64             `json:"dataset_id"`
		Function      string            `json:"function"`
		MinConfidence *int              `json:"min_confidence"`
		Mapping       map[string]string `json:"mapping"`
		Existing      string            `json:"existing"`
	}
	if err := json.Unmarshal(req.Body, &body); err != nil {
		return failed(errors.New("the body of the run is not readable"))
	}
	// A raw call over the request tool reaches this path as well: check everything the tool checks.
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
	if body.Existing == "" {
		body.Existing = "keep"
	}
	if body.Existing != "keep" && body.Existing != "replace" {
		return failed(errors.New("existing must be keep or replace"))
	}
	user, err := s.user(ctx, chatID)
	if err != nil {
		return failed(err)
	}
	d, err := s.ownDataset(ctx, chatID, r, body.DatasetID)
	if err != nil {
		return failed(err)
	}
	taskID, err := taskIDOf(d)
	if err != nil {
		return failed(err)
	}
	ses, err := s.currentSession(ctx, chatID, r)
	if err != nil {
		return failed(err)
	}
	// The function must be usable (E7) and a detector: only a detector runs over all images on its own.
	// The rule is the one of V3 as well (a check and a fill call the same functions).
	fn, err := s.usableFunction(ctx, r, ses, user, body.Function)
	if err != nil {
		return failed(err)
	}
	mapping, err := runMapping(body.Mapping, fn.Labels, d.AnnotationLabels)
	if err != nil {
		return failed(err)
	}
	if body.Existing == "keep" {
		ann, err := s.C.TaskAnnotations(ctx, ses, taskID)
		r.cvat("GET /api/tasks/"+strconv.FormatInt(taskID, 10)+"/annotations", err)
		if err != nil {
			return failed(err)
		}
		if n := annotationCount(ann); n > 0 {
			return failed(fmt.Errorf("the CVAT task already has %d annotations; existing=keep would add duplicates on every image. Use annotation_fill (only images without annotations) or existing=replace (deletes all, irreversible)", n))
		}
	}
	run, err := s.C.CreateRequest(ctx, ses, RunRequest{
		Function: fn.ID, Task: taskID, Quality: "original", Cleanup: body.Existing == "replace",
		Threshold: float64(conf) / 100, Mapping: mapping,
	})
	r.cvat("POST /api/lambda/requests", err)
	if err != nil {
		if cvatStatus(err) == http.StatusConflict {
			return failed(errors.New("CVAT runs only one pre-annotation per task at a time; wait for the running one"))
		}
		return failed(err)
	}
	stored, err := s.St.AddAnnotationRun(ctx, store.AnnotationRun{
		ID: run.ID, ChatID: chatID, DatasetID: d.ID, TaskID: taskID, Function: fn.ID,
		MinConfidence: conf, Mapping: mappingJSON(body.Mapping), Existing: body.Existing, State: store.AnnotationQueued,
	})
	if err != nil {
		return platform.Result{}, err
	}
	return okAnswer(runAnswer{stepList: r.steps(), RunID: stored.ID, DatasetID: d.ID, TaskID: taskID,
		Function: fn.ID, MinConfidence: conf, Mapping: body.Mapping, Existing: body.Existing,
		State: store.AnnotationQueued, Note: runFollowNote})
}

// runMapping checks the mapping of a run against the labels of the function and the classes of the
// dataset (I7) and brings it into CVAT's form {"<function label>": {"name": "<dataset class>"}}.
// Without a mapping only equal names are used; a label without a partner is dropped (CVAT drops it too).
func runMapping(mapping map[string]string, functionLabels, datasetClasses []string) (map[string]Descr, error) {
	out := map[string]Descr{}
	if len(mapping) == 0 {
		for _, l := range functionLabels {
			if contains(datasetClasses, l) {
				out[l] = Descr{Name: l}
			}
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("no function label matches a dataset class; give mapping (function labels: %s; dataset classes: %s)",
				strings.Join(functionLabels, ", "), strings.Join(datasetClasses, ", "))
		}
		return out, nil
	}
	for from, to := range mapping {
		if !contains(functionLabels, from) {
			return nil, fmt.Errorf("function label %q does not exist; labels: %s", from, strings.Join(functionLabels, ", "))
		}
		if !contains(datasetClasses, to) {
			return nil, fmt.Errorf("dataset class %q does not exist; classes: %s", to, strings.Join(datasetClasses, ", "))
		}
		out[from] = Descr{Name: to}
	}
	return out, nil
}

// mappingJSON stores the mapping of a run as the agent gave it.
func mappingJSON(mapping map[string]string) string {
	if len(mapping) == 0 {
		return "{}"
	}
	b, err := json.Marshal(mapping)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// annotationCount counts the annotations of a task: a tag, a shape and every shape of a track count
// once; a shape that marks the object as absent does not.
func annotationCount(a Annotations) int {
	n := len(a.Tags) + len(a.Shapes)
	for _, t := range a.Tracks {
		for _, s := range t.Shapes {
			if !s.Outside {
				n++
			}
		}
	}
	return n
}

// ---------------------------------------------------------------------------
// annotation_run
// ---------------------------------------------------------------------------

// runStatusAnswer is the answer of annotation_run.
type runStatusAnswer struct {
	stepList
	RunID    string  `json:"run_id"`
	State    string  `json:"state"`
	Progress float64 `json:"progress"`
	Started  string  `json:"started"`
	Ended    string  `json:"ended"`
	Error    string  `json:"error,omitempty"`
	Note     string  `json:"note,omitempty"`
}

// runStatus answers the state of a run of this chat. A run that is over is answered from the gateway's
// own row: CVAT's queue drops its jobs after a while, and asking there again would turn a finished run
// into "expired".
func (s *Service) runStatus(ctx context.Context, chatID, id string) (platform.Result, error) {
	r := &recorder{chatID: chatID}
	row, err := s.St.AnnotationRun(ctx, chatID, id)
	if errors.Is(err, store.ErrNotFound) {
		return failed(fmt.Errorf("unknown run %q in this chat", id))
	}
	if err != nil {
		return platform.Result{}, err
	}
	if store.AnnotationRunFinal(row.State) {
		return okAnswer(rowAnswer(r.steps(), row, runFinishedNote))
	}
	ses, err := s.currentSession(ctx, chatID, r)
	if err != nil {
		return failed(err)
	}
	run, err := s.C.Request(ctx, ses, row.ID)
	r.cvat("GET /api/lambda/requests/"+row.ID, err)
	if errors.Is(err, ErrNotFound) {
		// The queue has dropped the job: the annotations it made stay in the task.
		if uerr := s.St.UpdateAnnotationRun(ctx, chatID, row.ID, store.AnnotationExpired, ""); uerr != nil {
			return platform.Result{}, uerr
		}
		return okAnswer(runStatusAnswer{stepList: r.steps(), RunID: row.ID, State: store.AnnotationExpired, Note: runFinishedNote})
	}
	if err != nil {
		return failed(err)
	}
	state := runState(run.Status)
	errText := ""
	if state == store.AnnotationFailed {
		errText = truncate(run.ExcInfo, maxExcInfo)
	}
	if state != row.State || errText != row.Error {
		if uerr := s.St.UpdateAnnotationRun(ctx, chatID, row.ID, state, errText); uerr != nil {
			return platform.Result{}, uerr
		}
	}
	a := runStatusAnswer{stepList: r.steps(), RunID: row.ID, State: state, Progress: run.Progress,
		Started: run.Started, Ended: run.Ended, Error: errText}
	if state == store.AnnotationFinished {
		a.Note = runFinishedNote
	}
	return okAnswer(a)
}

// rowAnswer is the answer for a run that is over: the gateway's own row, without asking CVAT again.
func rowAnswer(steps stepList, row store.AnnotationRun, note string) runStatusAnswer {
	a := runStatusAnswer{stepList: steps, RunID: row.ID, State: row.State, Error: row.Error, Note: note}
	if row.State == store.AnnotationFinished {
		a.Progress = 100
	}
	if row.EndedAt != nil {
		a.Ended = row.EndedAt.UTC().Format(time.RFC3339)
	}
	return a
}

// runState maps the status of CVAT's queue to the state the gateway reports. The queue knows queued,
// started, finished, failed, deferred and scheduled; the last two are still waiting.
func runState(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "finished":
		return store.AnnotationFinished
	case "failed":
		return store.AnnotationFailed
	case "started":
		return store.AnnotationStarted
	case "queued", "deferred", "scheduled", "":
		return store.AnnotationQueued
	}
	return store.AnnotationFailed
}

// poll asks CVAT for the state of a run, keeps the row up to date and reports the progress to the
// observer (G1.7). The reader of the platform is not used: a run's state is CVAT's.
func (s *Service) poll(ctx context.Context, j watch.Job, _ watch.Reader) (watch.Progress, error) {
	row, err := s.St.AnnotationRun(ctx, j.ChatID, j.Ref)
	if err != nil {
		return watch.Progress{}, err
	}
	if store.AnnotationRunFinal(row.State) {
		return runProgress(row.State, 100, row.Error), nil
	}
	ses, err := s.currentSession(ctx, j.ChatID, &recorder{chatID: j.ChatID})
	if err != nil {
		return watch.Progress{}, err
	}
	run, err := s.C.Request(ctx, ses, row.ID)
	if errors.Is(err, ErrNotFound) {
		if uerr := s.St.UpdateAnnotationRun(ctx, j.ChatID, row.ID, store.AnnotationExpired, ""); uerr != nil {
			return watch.Progress{}, uerr
		}
		return runProgress(store.AnnotationExpired, 100, ""), nil
	}
	if err != nil {
		return watch.Progress{}, err
	}
	state := runState(run.Status)
	errText := ""
	if state == store.AnnotationFailed {
		errText = truncate(run.ExcInfo, maxExcInfo)
	}
	if state != row.State || errText != row.Error {
		if uerr := s.St.UpdateAnnotationRun(ctx, j.ChatID, row.ID, state, errText); uerr != nil {
			return watch.Progress{}, uerr
		}
	}
	return runProgress(state, run.Progress, errText), nil
}

// runProgress is what the observer reports for a run: running until CVAT says otherwise. The message
// is one line (G1.7).
func runProgress(state string, percent float64, errText string) watch.Progress {
	p := watch.Progress{State: "running", Percent: &percent}
	switch state {
	case store.AnnotationFinished:
		p.State, p.Message = "done", "finished"
	case store.AnnotationExpired:
		p.State, p.Message = "done", "expired: CVAT no longer knows the run"
	case store.AnnotationFailed:
		p.State, p.Message = "failed", "failed"
		if errText != "" {
			if i := strings.IndexByte(errText, '\n'); i >= 0 {
				errText = errText[:i]
			}
			p.Message = "failed: " + truncate(errText, 150)
		}
	default:
		p.Message = state
	}
	return p
}

// ---------------------------------------------------------------------------
// annotation_summary
// ---------------------------------------------------------------------------

// classCount says how many annotations of one class are manual and how many a run wrote.
type classCount struct {
	Manual int `json:"manual"`
	Auto   int `json:"auto"`
}

// summaryAnswer is the answer of annotation_summary.
type summaryAnswer struct {
	stepList
	TaskID                   int64                 `json:"task_id"`
	Frames                   int                   `json:"frames"`
	FramesWithAnnotations    int                   `json:"frames_with_annotations"`
	FramesWithoutAnnotations int                   `json:"frames_without_annotations"`
	ByClass                  map[string]classCount `json:"by_class"`
	ShapesByType             map[string]int        `json:"shapes_by_type"`
	Tags                     int                   `json:"tags"`
}

// summary counts the annotations of the dataset's CVAT task.
func (s *Service) summary(ctx context.Context, chatID string, datasetID int64) (platform.Result, error) {
	r := &recorder{chatID: chatID}
	d, err := s.ownDataset(ctx, chatID, r, datasetID)
	if err != nil {
		return failed(err)
	}
	taskID, err := taskIDOf(d)
	if err != nil {
		return failed(err)
	}
	ses, err := s.currentSession(ctx, chatID, r)
	if err != nil {
		return failed(err)
	}
	t, err := s.C.Task(ctx, ses, taskID)
	r.cvat("GET /api/tasks/"+strconv.FormatInt(taskID, 10), err)
	if err != nil {
		return failed(err)
	}
	labels := t.Labels
	if len(labels) == 0 {
		labels, err = s.C.Labels(ctx, ses, taskID)
		r.cvat("GET /api/labels", err)
		if err != nil {
			return failed(err)
		}
	}
	ann, err := s.C.TaskAnnotations(ctx, ses, taskID)
	r.cvat("GET /api/tasks/"+strconv.FormatInt(taskID, 10)+"/annotations", err)
	if err != nil {
		return failed(err)
	}
	out := countAnnotations(t.Size, labels, ann)
	out.TaskID = taskID
	out.stepList = r.steps()
	return okAnswer(out)
}

// countAnnotations counts the annotations of a task: per class manual and automatic, how many frames
// carry one, the shapes by their type and the tags. An annotation counts on the frame it sits on: a tag
// once, a shape once and every shape of a track once (a shape that marks the object as absent does
// not count). A class id CVAT does not name is counted under "#<id>".
func countAnnotations(frames int, labels []Label, a Annotations) summaryAnswer {
	byID := map[int64]string{}
	for _, l := range labels {
		byID[l.ID] = l.Name
	}
	out := summaryAnswer{
		ByClass:      map[string]classCount{},
		ShapesByType: map[string]int{},
		Tags:         len(a.Tags),
		Frames:       frames,
	}
	seen := map[int]bool{}
	add := func(labelID int64, source string, frame int) {
		name, ok := byID[labelID]
		if !ok {
			name = "#" + strconv.FormatInt(labelID, 10)
		}
		c := out.ByClass[name]
		if source == sourceAuto {
			c.Auto++
		} else {
			c.Manual++
		}
		out.ByClass[name] = c
		if frame >= 0 {
			seen[frame] = true
		}
	}
	for _, t := range a.Tags {
		add(t.LabelID, t.Source, t.Frame)
	}
	for _, sh := range a.Shapes {
		if sh.Outside {
			continue
		}
		out.ShapesByType[sh.Type]++
		add(sh.LabelID, sh.Source, sh.Frame)
	}
	for _, tr := range a.Tracks {
		for _, sh := range tr.Shapes {
			if sh.Outside {
				continue
			}
			out.ShapesByType[sh.Type]++
			source := sh.Source
			if source == "" {
				source = tr.Source
			}
			add(tr.LabelID, source, sh.Frame)
		}
	}
	out.FramesWithAnnotations = len(seen)
	out.FramesWithoutAnnotations = frames - len(seen)
	if out.FramesWithoutAnnotations < 0 {
		out.FramesWithoutAnnotations = 0
	}
	return out
}

// ---------------------------------------------------------------------------
// annotation_sync
// ---------------------------------------------------------------------------

// syncAnswer is the answer of annotation_sync.
type syncAnswer struct {
	stepList
	DatasetID int64 `json:"dataset_id"`
	Synced    bool  `json:"synced"`
}

// syncDataset writes the annotations of the task back into the dataset: the platform does that while
// downloading a dataset, so the gateway sends exactly that call and throws the ZIP away (E5).
func (s *Service) syncDataset(ctx context.Context, chatID string, datasetID int64) (platform.Result, error) {
	r := &recorder{chatID: chatID}
	d, err := s.ownDataset(ctx, chatID, r, datasetID)
	if err != nil {
		return failed(err)
	}
	if _, err := taskIDOf(d); err != nil {
		return failed(err)
	}
	if _, err := s.platformExpect(ctx, r, platform.Request{Method: http.MethodGet, Path: "/datasets/" + strconv.FormatInt(d.ID, 10) + "/download"}, maxDownloadBytes); err != nil {
		return failed(err)
	}
	return okAnswer(syncAnswer{stepList: r.steps(), DatasetID: d.ID, Synced: true})
}

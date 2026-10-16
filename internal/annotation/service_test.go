// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package annotation

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"agw/internal/platform"
	"agw/internal/store"
)

// testJWT builds an unsigned token with the given claims; the gateway only reads its payload.
func testJWT(claims map[string]any) string {
	b, _ := json.Marshal(claims)
	enc := base64.RawURLEncoding
	return enc.EncodeToString([]byte(`{"alg":"none"}`)) + "." + enc.EncodeToString(b) + ".sig"
}

// fakePlatform stands in for the platform's API and its Keycloak: ChatUser reads preferred_username out
// of the token, so the fake has to issue one. Every call is recorded in the order in which it arrives;
// the bodies of the writing calls are kept.
type fakePlatform struct {
	mu         sync.Mutex
	calls      []string
	patches    []string
	annotates  int
	taskBodies []string
	dataset    string // the answer of GET /datasets/{id}
	login      string // the answer of POST /cvat/auth/login; empty: HTTP 500
	taskID     int64  // the id POST /cvat/tasks/{id} answers
	operators  []string

	// V2: the models of the platform, the file of one of them, the background tasks of the platform
	// and the builds of a function (POST /datasets/auto-annotation, multipart).
	models     map[int64]string
	modelFile  string
	tasks      map[string]string
	taskLoc    string // the Location of POST /datasets/auto-annotation
	builds     int    // how many function archives were uploaded
	buildBytes []int64
}

// datasetAnswer, loginAnswer and tasksAnswer are set before a call goes out; the mutex is the one the
// handler holds, so the race detector sees the order (the HTTP round trip does not count as one).
func (f *fakePlatform) setDataset(s string) { f.mu.Lock(); defer f.mu.Unlock(); f.dataset = s }
func (f *fakePlatform) setLogin(s string)   { f.mu.Lock(); defer f.mu.Unlock(); f.login = s }
func (f *fakePlatform) setTaskID(n int64)   { f.mu.Lock(); defer f.mu.Unlock(); f.taskID = n }

// V2: the model of the platform, its file, the background task of a build and the Location the
// archive upload answers.
func (f *fakePlatform) setModel(id int64, answer string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.models == nil {
		f.models = map[int64]string{}
	}
	f.models[id] = answer
}
func (f *fakePlatform) setModelFile(s string) { f.mu.Lock(); defer f.mu.Unlock(); f.modelFile = s }
func (f *fakePlatform) setTask(id, answer string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tasks == nil {
		f.tasks = map[string]string{}
	}
	f.tasks[id] = answer
}
func (f *fakePlatform) setTaskLocation(s string) { f.mu.Lock(); defer f.mu.Unlock(); f.taskLoc = s }

// buildsOf reports how often a function archive was uploaded and how large it was.
func (f *fakePlatform) buildsOf() (int, []int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.builds, append([]int64(nil), f.buildBytes...)
}

var (
	datasetRe       = regexp.MustCompile(`^/datasets/\d+$`)
	annotateRe      = regexp.MustCompile(`^/datasets/\d+/annotate$`)
	downloadRe      = regexp.MustCompile(`^/datasets/\d+/download$`)
	taskCreateRe    = regexp.MustCompile(`^/cvat/tasks/\d+$`)
	modelRe         = regexp.MustCompile(`^/models/\d+$`)
	modelDownloadRe = regexp.MustCompile(`^/models/\d+/download$`)
	taskGetRe       = regexp.MustCompile(`^/tasks/\d+$`)
)

func (f *fakePlatform) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.URL.Path == "/token":
		fmt.Fprintf(w, `{"access_token":%q,"expires_in":3600}`,
			testJWT(map[string]any{"sub": "u-1", "preferred_username": "test", "azp": "frontend"}))
		return
	case r.URL.Path == "/cvat/auth/login":
		f.calls = append(f.calls, r.Method+" "+r.URL.Path)
		if f.login == "" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, f.login)
		return
	}
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodPost && annotateRe.MatchString(r.URL.Path):
		f.annotates++
		fmt.Fprint(w, f.dataset)
	case r.Method == http.MethodPost && taskCreateRe.MatchString(r.URL.Path):
		f.taskBodies = append(f.taskBodies, readBody(r))
		fmt.Fprintf(w, `{"id":%d,"next":"/tasks/%d"}`, f.taskID, f.taskID)
	case r.Method == http.MethodPatch && datasetRe.MatchString(r.URL.Path):
		f.patches = append(f.patches, readBody(r))
		fmt.Fprint(w, f.dataset)
	case r.Method == http.MethodGet && downloadRe.MatchString(r.URL.Path):
		w.Header().Set("Content-Type", "application/zip")
		fmt.Fprint(w, "PK\x03\x04not a real zip")
	case r.Method == http.MethodGet && datasetRe.MatchString(r.URL.Path):
		if f.dataset == "" {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"detail":"no dataset"}`)
			return
		}
		fmt.Fprint(w, f.dataset)
	case r.Method == http.MethodGet && modelRe.MatchString(r.URL.Path):
		answer, ok := f.models[parseID(strings.TrimPrefix(r.URL.Path, "/models/"))]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"detail":"no model"}`)
			return
		}
		fmt.Fprint(w, answer)
	case r.Method == http.MethodGet && modelDownloadRe.MatchString(r.URL.Path):
		w.Header().Set("Content-Type", "application/octet-stream")
		fmt.Fprint(w, f.modelFile)
	case r.Method == http.MethodGet && taskGetRe.MatchString(r.URL.Path):
		answer, ok := f.tasks[strings.TrimPrefix(r.URL.Path, "/tasks/")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"detail":"no task"}`)
			return
		}
		fmt.Fprint(w, answer)
	case r.Method == http.MethodPost && r.URL.Path == "/datasets/auto-annotation":
		f.builds++
		f.buildBytes = append(f.buildBytes, int64(len(readBody(r))))
		if f.taskLoc == "" {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"detail":"no task"}`)
			return
		}
		w.Header().Set("Location", f.taskLoc)
		w.WriteHeader(http.StatusAccepted)
	default:
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"detail":"not in the fake"}`)
	}
}

func (f *fakePlatform) saw() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakePlatform) sawCall(method, path string) bool {
	for _, c := range f.saw() {
		if c == method+" "+path {
			return true
		}
	}
	return false
}

func (f *fakePlatform) patchBodies() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.patches...)
}

func (f *fakePlatform) creates() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.taskBodies...)
}

// fakeCVAT stands in for the CVAT server: the function list, the tasks, their annotations and the
// queue of runs.
type fakeCVAT struct {
	mu          sync.Mutex
	calls       []string
	functions   string // JSON; empty: an empty list
	tasks       map[int64]string
	annotations map[int64]string
	runs        map[string]string
	newRun      string // the answer of POST /api/lambda/requests
	runBodies   []string
	runStatus   int // status of POST /api/lambda/requests (0: 201)

	// V3: what one function answers for one frame, the frames it was asked about, the bodies of those
	// calls and the writes of a fill; the jobs of a task and the meta of its data.
	funcAnswers map[int]string
	funcCalls   []int
	funcBodies  []string
	funcStatus  int // status of POST /api/lambda/functions/{id} (0: 200)
	writes      []string
	jobs        string
	meta        string
	// onAnnotationsRead runs on every read of the annotations of a task and may answer with another
	// state; it is how a test lets the user annotate while a job runs (V3).
	onAnnotationsRead func(string) string
}

func (f *fakeCVAT) setFunctions(s string)      { f.mu.Lock(); defer f.mu.Unlock(); f.functions = s }
func (f *fakeCVAT) setTask(id int64, s string) { f.mu.Lock(); defer f.mu.Unlock(); f.tasks[id] = s }
func (f *fakeCVAT) setAnnotations(id int64, s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.annotations[id] = s
}
func (f *fakeCVAT) setRun(id, s string)     { f.mu.Lock(); defer f.mu.Unlock(); f.runs[id] = s }
func (f *fakeCVAT) setRunStatus(status int) { f.mu.Lock(); defer f.mu.Unlock(); f.runStatus = status }

// V3: what the function answers for a frame, the jobs and the meta of a task.
func (f *fakeCVAT) setAnswers(byFrame map[int]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.funcAnswers = byFrame
}
func (f *fakeCVAT) setJobs(s string) { f.mu.Lock(); defer f.mu.Unlock(); f.jobs = s }

// setOnAnnotationsRead installs the hook a test uses to let the user annotate while a job runs.
func (f *fakeCVAT) setOnAnnotationsRead(h func(string) string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onAnnotationsRead = h
}
func (f *fakeCVAT) setMeta(s string) { f.mu.Lock(); defer f.mu.Unlock(); f.meta = s }
func (f *fakeCVAT) setFuncStatus(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.funcStatus = status
}

// askedFrames are the frames the function was called for.
func (f *fakeCVAT) askedFrames() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.funcCalls...)
}

// writeBodies are the bodies of the writes of a fill.
func (f *fakeCVAT) writeBodies() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.writes...)
}

func (f *fakeCVAT) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/lambda/functions":
		if f.functions == "" {
			fmt.Fprint(w, `[]`)
			return
		}
		fmt.Fprint(w, f.functions)
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/lambda/functions/"):
		body := readBody(r)
		f.funcBodies = append(f.funcBodies, body)
		if f.funcStatus != 0 {
			w.WriteHeader(f.funcStatus)
			fmt.Fprint(w, `{"detail":"the function failed"}`)
			return
		}
		var sent struct {
			Frame int `json:"frame"`
		}
		_ = json.Unmarshal([]byte(body), &sent)
		f.funcCalls = append(f.funcCalls, sent.Frame)
		if answer, ok := f.funcAnswers[sent.Frame]; ok {
			fmt.Fprint(w, answer)
			return
		}
		fmt.Fprint(w, `[]`)
	case r.Method == http.MethodGet && r.URL.Path == "/api/jobs":
		if f.jobs == "" {
			fmt.Fprint(w, `{"count":0,"results":[]}`)
			return
		}
		fmt.Fprint(w, f.jobs)
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/data/meta"):
		if f.meta == "" {
			fmt.Fprint(w, `{}`)
			return
		}
		fmt.Fprint(w, f.meta)
	case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/annotations"):
		f.writes = append(f.writes, readBody(r))
		fmt.Fprint(w, `{"version":1}`)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/lambda/requests/"):
		body, ok := f.runs[strings.TrimPrefix(r.URL.Path, "/api/lambda/requests/")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"detail":"no such request"}`)
			return
		}
		fmt.Fprint(w, body)
	case r.Method == http.MethodPost && r.URL.Path == "/api/lambda/requests":
		f.runBodies = append(f.runBodies, readBody(r))
		if f.runStatus != 0 {
			w.WriteHeader(f.runStatus)
			fmt.Fprint(w, `{"detail":"only one running request is allowed for the same task"}`)
			return
		}
		if f.newRun == "" {
			fmt.Fprint(w, `{"id":"run-1","status":"queued","progress":0}`)
			return
		}
		fmt.Fprint(w, f.newRun)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/tasks/"):
		rest := strings.TrimPrefix(r.URL.Path, "/api/tasks/")
		if id, ok := strings.CutSuffix(rest, "/annotations"); ok {
			body, ok := f.annotations[parseID(id)]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprint(w, `{"detail":"no task"}`)
				return
			}
			if f.onAnnotationsRead != nil {
				body = f.onAnnotationsRead(body)
			}
			fmt.Fprint(w, body)
			return
		}
		body, ok := f.tasks[parseID(rest)]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"detail":"no task"}`)
			return
		}
		fmt.Fprint(w, body)
	default:
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"detail":"not in the fake"}`)
	}
}

func (f *fakeCVAT) saw() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeCVAT) bodies() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.runBodies...)
}

func parseID(s string) int64 {
	id, _ := strconv.ParseInt(s, 10, 64)
	return id
}

// stepsOf reads the steps out of an answer of the service.
func stepsOf(t *testing.T, res platform.Result) []string {
	t.Helper()
	var a stepList
	if err := json.Unmarshal([]byte(res.Body), &a); err != nil {
		t.Fatalf("the answer carries no steps: %v", err)
	}
	return a.Steps
}

func readBody(r *http.Request) string {
	if r.Body == nil {
		return ""
	}
	b, _ := io.ReadAll(r.Body)
	return string(b)
}

// testEnv is the service with its two fakes and the test database.
type testEnv struct {
	svc *Service
	st  *store.Store
	pf  *fakePlatform
	fc  *fakeCVAT
}

// newTestEnv builds the service against the two fakes; operator names the function ids the operator
// installed.
func newTestEnv(t *testing.T, operator ...string) *testEnv {
	t.Helper()
	url := os.Getenv("AGW_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("AGW_TEST_DATABASE_URL not set")
	}
	pf := &fakePlatform{
		dataset:   datasetJSON(5, "test", "AgriImageDataResource", []string{"0", "1"}, 0),
		login:     `{"key":"SECRET-KEY","csrftoken":"SECRET-CSRF","sessionid":"SECRET-SESSION"}`,
		taskID:    9,
		operators: operator,
	}
	fc := &fakeCVAT{tasks: map[int64]string{}, annotations: map[int64]string{}, runs: map[string]string{}}
	psrv := httptest.NewServer(pf)
	t.Cleanup(psrv.Close)
	csrv := httptest.NewServer(fc)
	t.Cleanup(csrv.Close)
	st, err := store.OpenSchema(context.Background(), url, "anntest_"+strings.ReplaceAll(time.Now().Format("150405.000000"), ".", "_"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.DropSchema(context.Background()); st.Close() })
	p, err := platform.New(platform.Config{APIURL: psrv.URL, TokenURL: psrv.URL + "/token", User: "test", Password: "pw"})
	if err != nil {
		t.Fatal(err)
	}
	c, err := NewCVAT(csrv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The two bounds of V2 stay unset here: a test that needs one sets it on the service
	// (MaxFunctions, ModelMaxMB) before its call.
	return &testEnv{svc: New(p, st, c, operator, "https://cvat.example", 0, 0), st: st, pf: pf, fc: fc}
}

// chat creates the chat the calls belong to and returns its id.
func (e *testEnv) chat(t *testing.T) string {
	t.Helper()
	c, err := e.st.CreateChat(context.Background(), store.NewChat{Title: "t", Model: "m", Variant: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	return c.ID
}

// do sends a call of the annotation tools to the service.
func (e *testEnv) do(t *testing.T, chatID, method, path, body string) platform.Result {
	t.Helper()
	req := platform.Request{Method: method, Path: path}
	if body != "" {
		req.Body = json.RawMessage(body)
	}
	res, err := e.svc.Do(context.Background(), chatID, req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return res
}

// datasetJSON builds the answer of GET /datasets/{id}.
func datasetJSON(id int64, owner, dtype string, labels []string, taskID int64) string {
	var task any
	if taskID > 0 {
		task = taskID
	}
	m := map[string]any{"id": id, "name": "ds", "owner": owner, "dataset_type": dtype,
		"annotation_labels": labels, "bucket_name": "b", "annotation_task_id": task}
	b, _ := json.Marshal(m)
	return string(b)
}

// taskJSON builds the answer of GET /api/tasks/{id} including its owner.
func taskJSON(id int64, owner string, size int, labels []map[string]any) string {
	b, _ := json.Marshal(map[string]any{"id": id, "name": "task", "size": size,
		"owner": map[string]any{"username": owner}, "labels": labels})
	return string(b)
}

func labelsJSON(names ...string) []map[string]any {
	out := make([]map[string]any, 0, len(names))
	for i, n := range names {
		out = append(out, map[string]any{"id": i + 1, "name": n})
	}
	return out
}

// ---------------------------------------------------------------------------
// annotation_task
// ---------------------------------------------------------------------------

// The gateway checks the owner itself (E6): the platform checks it only when deleting, and its
// annotate route mounts the bucket of any dataset. Nothing of the foreign dataset is touched.
func TestTaskOwnerCheck(t *testing.T) {
	e := newTestEnv(t)
	chatID := e.chat(t)
	e.pf.setDataset(datasetJSON(5, "other", "AgriImageDataResource", []string{"0", "1"}, 0))

	res := e.do(t, chatID, http.MethodPost, platform.AnnotationPrefix+"/tasks", `{"dataset_id":5}`)
	if res.Status != "denied" || res.Message != "dataset 5 belongs to other; only your own datasets can be annotated" {
		t.Fatalf("foreign dataset: %+v", res)
	}
	for _, c := range e.pf.saw() {
		if strings.HasPrefix(c, "POST") && !strings.Contains(c, "auth/login") {
			t.Errorf("a foreign dataset was written: %v", e.pf.saw())
		}
	}
	if got := e.fc.saw(); len(got) != 0 {
		t.Errorf("CVAT was called for a foreign dataset: %v", got)
	}
}

// A task CVAT still knows and that belongs to the same user is reused: no new task, no second
// annotate call.
func TestTaskReusesExisting(t *testing.T) {
	e := newTestEnv(t)
	chatID := e.chat(t)
	e.pf.setDataset(datasetJSON(5, "test", "AgriImageDataResource", []string{"0", "1"}, 7))
	e.fc.setTask(7, taskJSON(7, "test", 30, labelsJSON("0", "1")))

	res := e.do(t, chatID, http.MethodPost, platform.AnnotationPrefix+"/tasks", `{"dataset_id":5}`)
	var answer taskAnswer
	if err := json.Unmarshal([]byte(res.Body), &answer); err != nil {
		t.Fatal(err)
	}
	if res.Status != "ok" || answer.TaskID != 7 || answer.Created || answer.CVATURL != "https://cvat.example/tasks/7" {
		t.Fatalf("reuse answer: %+v", answer)
	}
	if e.pf.sawCall("POST", "/datasets/5/annotate") || e.pf.sawCall("POST", "/cvat/tasks/5") || e.pf.sawCall("PATCH", "/datasets/5") {
		t.Errorf("an existing task was created again: %v", e.pf.saw())
	}
	if len(answer.Steps) == 0 {
		t.Error("the answer carries no steps")
	}
}

// A task that is in CVAT but belongs to somebody else is refused, even when the dataset points at it.
func TestTaskForeignTask(t *testing.T) {
	e := newTestEnv(t)
	chatID := e.chat(t)
	e.pf.setDataset(datasetJSON(5, "test", "AgriImageDataResource", []string{"0", "1"}, 7))
	e.fc.setTask(7, taskJSON(7, "other", 30, labelsJSON("0", "1")))

	res := e.do(t, chatID, http.MethodPost, platform.AnnotationPrefix+"/tasks", `{"dataset_id":5}`)
	if res.Status != "denied" || !strings.Contains(res.Message, "CVAT task 7 belongs to other") {
		t.Fatalf("foreign task: %+v", res)
	}
}

// A task CVAT no longer knows (404) is created anew.
func TestTaskRecreatesWhenGone(t *testing.T) {
	e := newTestEnv(t)
	chatID := e.chat(t)
	e.pf.setDataset(datasetJSON(5, "test", "AgriImageDataResource", []string{"0", "1"}, 7))

	res := e.do(t, chatID, http.MethodPost, platform.AnnotationPrefix+"/tasks", `{"dataset_id":5}`)
	if res.Status != "ok" || !e.pf.sawCall("POST", "/datasets/5/annotate") {
		t.Fatalf("recreate: %+v %v", res, e.pf.saw())
	}
}

// The order is annotate → cvat/tasks → PATCH, and the PATCH carries exactly the id of the answer right
// before (the backend exports the task with the CVAT superuser).
func TestTaskCreateOrder(t *testing.T) {
	e := newTestEnv(t)
	chatID := e.chat(t)

	res := e.do(t, chatID, http.MethodPost, platform.AnnotationPrefix+"/tasks", `{"dataset_id":5}`)
	var answer taskAnswer
	if err := json.Unmarshal([]byte(res.Body), &answer); err != nil {
		t.Fatal(err)
	}
	if res.Status != "ok" || answer.TaskID != 9 || !answer.Created {
		t.Fatalf("create answer: %+v", answer)
	}
	want := []string{"GET /datasets/5", "POST /datasets/5/annotate", "POST /cvat/tasks/5", "PATCH /datasets/5"}
	var created []string
	for _, c := range e.pf.saw() {
		if c == "POST /cvat/auth/login" {
			continue
		}
		created = append(created, c)
	}
	if strings.Join(created, ", ") != strings.Join(want, ", ") {
		t.Errorf("order: %v, want %v", created, want)
	}
	if bodies := e.pf.patchBodies(); len(bodies) != 1 || bodies[0] != `{"annotation_task_id":9}` {
		t.Errorf("PATCH body: %v", bodies)
	}
	if bodies := e.pf.creates(); len(bodies) != 1 || !strings.Contains(bodies[0], `"key":"SECRET-KEY"`) {
		t.Errorf("the session did not go into the body of the task: %v", bodies)
	}
}

// Without classes CVAT would create the placeholder class "object" and the import fails.
func TestTaskNoClasses(t *testing.T) {
	e := newTestEnv(t)
	chatID := e.chat(t)
	e.pf.setDataset(datasetJSON(5, "test", "AgriImageDataResource", nil, 0))

	res := e.do(t, chatID, http.MethodPost, platform.AnnotationPrefix+"/tasks", `{"dataset_id":5}`)
	if res.Status != "error" || !strings.Contains(res.Message, "has no classes (annotation_labels)") {
		t.Fatalf("no classes: %+v", res)
	}
	if e.pf.sawCall("POST", "/datasets/5/annotate") {
		t.Error("the annotate call went out although the dataset has no classes")
	}
}

// Only an image dataset can be annotated.
func TestTaskWrongType(t *testing.T) {
	e := newTestEnv(t)
	chatID := e.chat(t)
	e.pf.setDataset(datasetJSON(5, "test", "AgriTabularDataResource", []string{"0"}, 0))

	res := e.do(t, chatID, http.MethodPost, platform.AnnotationPrefix+"/tasks", `{"dataset_id":5}`)
	if res.Status != "error" || !strings.Contains(res.Message, "is of type AgriTabularDataResource") {
		t.Fatalf("wrong type: %+v", res)
	}
}

// Without a CVAT account the login fails (the backend turns CVAT's 400 into a 500).
func TestTaskNoCVATAccount(t *testing.T) {
	e := newTestEnv(t)
	chatID := e.chat(t)
	e.pf.setLogin("")

	res := e.do(t, chatID, http.MethodPost, platform.AnnotationPrefix+"/tasks", `{"dataset_id":5}`)
	if res.Status != "error" || !strings.Contains(res.Message, "you have no CVAT account yet") {
		t.Fatalf("no account: %+v", res)
	}
}

// ---------------------------------------------------------------------------
// pre_annotate
// ---------------------------------------------------------------------------

// readyDataset prepares a dataset with a task and three functions: one of the user, one of the
// operator (not in the list by default) and one of another user.
func readyDataset(t *testing.T, e *testEnv) string {
	t.Helper()
	chatID := e.chat(t)
	e.pf.setDataset(datasetJSON(5, "test", "AgriImageDataResource", []string{"0", "1"}, 7))
	e.fc.setTask(7, taskJSON(7, "test", 30, labelsJSON("0", "1")))
	e.fc.setAnnotations(7, `{"version":0,"tags":[],"shapes":[],"tracks":[]}`)
	e.fc.setFunctions(`[{"id":"agw-test-m1-abcd1234","name":"mine","kind":"detector","labels":["class_0","class_1"]},
		{"id":"omz.public.yolo","name":"yolo","kind":"detector","labels":["cow"]},
		{"id":"agw-other-m1-abcd1234","name":"foreign","kind":"detector","labels":["pig"]}]`)
	return chatID
}

// A function of another user is refused: it would receive every image of the run (E7).
func TestRunForeignFunction(t *testing.T) {
	e := newTestEnv(t)
	chatID := readyDataset(t, e)
	res := e.do(t, chatID, http.MethodPost, platform.AnnotationPrefix+"/runs",
		`{"dataset_id":5,"function":"agw-other-m1-abcd1234","min_confidence":50,"mapping":{"pig":"0"},"existing":"keep"}`)
	if res.Status != "denied" || !strings.Contains(res.Message, `function "agw-other-m1-abcd1234" is not usable`) {
		t.Fatalf("foreign function: %+v", res)
	}
	if len(e.fc.bodies()) != 0 {
		t.Errorf("a run was started with a foreign function: %v", e.fc.bodies())
	}
}

// A function the operator installed may be run, even though its name is not the user's.
func TestRunOperatorFunction(t *testing.T) {
	e := newTestEnv(t, "omz.public.yolo")
	chatID := e.chat(t)
	e.pf.setDataset(datasetJSON(5, "test", "AgriImageDataResource", []string{"Rind"}, 7))
	e.fc.setTask(7, taskJSON(7, "test", 30, labelsJSON("Rind")))
	e.fc.setAnnotations(7, `{"version":0,"tags":[],"shapes":[],"tracks":[]}`)
	e.fc.setFunctions(`[{"id":"omz.public.yolo","name":"yolo","kind":"detector","labels":["cow"]}]`)

	res := e.do(t, chatID, http.MethodPost, platform.AnnotationPrefix+"/runs",
		`{"dataset_id":5,"function":"omz.public.yolo","min_confidence":70,"mapping":{"cow":"Rind"},"existing":"keep"}`)
	var answer runAnswer
	if err := json.Unmarshal([]byte(res.Body), &answer); err != nil {
		t.Fatal(err)
	}
	if res.Status != "ok" || answer.RunID != "run-1" || answer.State != store.AnnotationQueued || answer.MinConfidence != 70 {
		t.Fatalf("run answer: %+v", answer)
	}
	bodies := e.fc.bodies()
	if len(bodies) != 1 {
		t.Fatalf("create bodies: %v", bodies)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(bodies[0]), &sent); err != nil {
		t.Fatal(err)
	}
	if sent["quality"] != "original" || sent["threshold"].(float64) != 0.7 || sent["cleanup"] != false {
		t.Errorf("run body: %v", sent)
	}
	if m, ok := sent["mapping"].(map[string]any); !ok || m["cow"].(map[string]any)["name"] != "Rind" {
		t.Errorf("mapping in CVAT's form: %v", sent["mapping"])
	}
	if len(answer.Steps) == 0 {
		t.Error("no steps in the answer")
	}
}

// A run with existing=replace deletes every annotation of the task first.
func TestRunReplaceSetsCleanup(t *testing.T) {
	e := newTestEnv(t)
	chatID := readyDataset(t, e)
	res := e.do(t, chatID, http.MethodPost, platform.AnnotationPrefix+"/runs",
		`{"dataset_id":5,"function":"agw-test-m1-abcd1234","min_confidence":50,"mapping":{"class_0":"0"},"existing":"replace"}`)
	if res.Status != "ok" {
		t.Fatalf("replace: %+v", res)
	}
	if bodies := e.fc.bodies(); len(bodies) != 1 || !strings.Contains(bodies[0], `"cleanup":true`) {
		t.Errorf("cleanup not set: %v", bodies)
	}
}

// keep on a task that already has annotations would double every manual object (E12).
func TestRunKeepWithAnnotations(t *testing.T) {
	e := newTestEnv(t)
	chatID := readyDataset(t, e)
	e.fc.setAnnotations(7, `{"version":0,"tags":[{"frame":0,"label_id":1,"source":"manual"}],
		"shapes":[{"frame":1,"label_id":2,"type":"rectangle","source":"manual"}],"tracks":[]}`)

	res := e.do(t, chatID, http.MethodPost, platform.AnnotationPrefix+"/runs",
		`{"dataset_id":5,"function":"agw-test-m1-abcd1234","min_confidence":50,"mapping":{"class_0":"0"},"existing":"keep"}`)
	if res.Status != "error" || !strings.Contains(res.Message, "already has 2 annotations") {
		t.Fatalf("keep with annotations: %+v", res)
	}
	if len(e.fc.bodies()) != 0 {
		t.Errorf("the run went out anyway: %v", e.fc.bodies())
	}
}

// CVAT allows one run per task; its 409 becomes a plain message.
func TestRunConflict(t *testing.T) {
	e := newTestEnv(t)
	chatID := readyDataset(t, e)
	e.fc.setRunStatus(http.StatusConflict)

	res := e.do(t, chatID, http.MethodPost, platform.AnnotationPrefix+"/runs",
		`{"dataset_id":5,"function":"agw-test-m1-abcd1234","min_confidence":50,"mapping":{"class_0":"0"},"existing":"keep"}`)
	if res.Status != "error" || !strings.Contains(res.Message, "only one pre-annotation per task") {
		t.Fatalf("conflict: %+v", res)
	}
}

// Without a CVAT task of the dataset there is nothing to run over.
func TestRunWithoutTask(t *testing.T) {
	e := newTestEnv(t)
	chatID := e.chat(t)
	res := e.do(t, chatID, http.MethodPost, platform.AnnotationPrefix+"/runs",
		`{"dataset_id":5,"function":"agw-test-m1-abcd1234","min_confidence":50,"mapping":{"class_0":"0"},"existing":"keep"}`)
	if res.Status != "error" || !strings.Contains(res.Message, "has no CVAT task yet") {
		t.Fatalf("without a task: %+v", res)
	}
}

// The body of a run is checked again, because a raw call over the request tool reaches the same path.
func TestRunBodyChecks(t *testing.T) {
	e := newTestEnv(t)
	chatID := readyDataset(t, e)
	for _, tc := range []struct{ body, want string }{
		{`{"dataset_id":5,"function":"agw-test-m1-abcd1234","min_confidence":100,"existing":"keep"}`, "min_confidence must be between 1 and 99"},
		{`{"dataset_id":5,"function":"agw-test-m1-abcd1234","existing":"drop"}`, "existing must be keep or replace"},
		{`{"function":"agw-test-m1-abcd1234"}`, "dataset_id missing"},
		{`{"dataset_id":5}`, "function missing"},
	} {
		res := e.do(t, chatID, http.MethodPost, platform.AnnotationPrefix+"/runs", tc.body)
		if res.Status != "error" || !strings.Contains(res.Message, tc.want) {
			t.Errorf("%s: %+v", tc.body, res)
		}
	}
}

// The mapping is checked against the labels of the function and the classes of the dataset.
func TestRunMapping(t *testing.T) {
	functionLabels := []string{"class_0", "class_1"}
	classes := []string{"0", "1"}
	// Without a mapping equal names are used; nothing matches here.
	if _, err := runMapping(nil, functionLabels, classes); err == nil || !strings.Contains(err.Error(), "no function label matches a dataset class") {
		t.Errorf("without a mapping: %v", err)
	}
	// Equal names are taken over.
	got, err := runMapping(nil, []string{"0", "1"}, classes)
	if err != nil || got["0"].Name != "0" || len(got) != 2 {
		t.Errorf("equal names: %v %v", got, err)
	}
	// A function label that does not exist.
	if _, err := runMapping(map[string]string{"cow": "0"}, functionLabels, classes); err == nil || !strings.Contains(err.Error(), `function label "cow" does not exist`) {
		t.Errorf("unknown function label: %v", err)
	}
	// A dataset class that does not exist.
	if _, err := runMapping(map[string]string{"class_0": "Rind"}, functionLabels, classes); err == nil || !strings.Contains(err.Error(), `dataset class "Rind" does not exist`) {
		t.Errorf("unknown class: %v", err)
	}
	// The given mapping in CVAT's form.
	got, err = runMapping(map[string]string{"class_0": "0", "class_1": "1"}, functionLabels, classes)
	if err != nil || got["class_0"].Name != "0" || got["class_1"].Name != "1" {
		t.Errorf("mapping: %v %v", got, err)
	}
}

// A run over a non-detector function is refused: only a detector runs over all images on its own.
func TestRunInteractorRefused(t *testing.T) {
	e := newTestEnv(t)
	chatID := readyDataset(t, e)
	e.fc.setFunctions(`[{"id":"agw-test-sam","name":"sam","kind":"interactor","labels":[]}]`)
	res := e.do(t, chatID, http.MethodPost, platform.AnnotationPrefix+"/runs",
		`{"dataset_id":5,"function":"agw-test-sam","min_confidence":50,"mapping":{},"existing":"keep"}`)
	if res.Status != "error" || !strings.Contains(res.Message, `has kind "interactor"`) {
		t.Fatalf("interactor: %+v", res)
	}
}

// ---------------------------------------------------------------------------
// annotation_functions
// ---------------------------------------------------------------------------

// Own functions and the operator's are usable; everything else is counted in hidden (E7).
func TestFunctionsList(t *testing.T) {
	e := newTestEnv(t, "omz.public.yolo")
	chatID := e.chat(t)
	e.fc.setFunctions(`[{"id":"agw-test-m1-abcd1234","name":"mine","kind":"detector","labels":["a","b"]},
		{"id":"omz.public.yolo","name":"yolo","kind":"detector","labels":["cow"]},
		{"id":"agw-other-m1-abcd1234","name":"foreign","kind":"detector","labels":["pig"]}]`)

	res := e.do(t, chatID, http.MethodGet, platform.AnnotationPrefix+"/functions", "")
	var answer functionsAnswer
	if err := json.Unmarshal([]byte(res.Body), &answer); err != nil {
		t.Fatal(err)
	}
	if res.Status != "ok" || len(answer.Functions) != 2 || answer.Hidden != 1 {
		t.Fatalf("functions: %+v", answer)
	}
	if answer.Functions[0].Source != "yours" || answer.Functions[1].Source != "operator" {
		t.Errorf("sources: %+v", answer.Functions)
	}
	if answer.Functions[0].State != functionStateReady {
		t.Errorf("state: %+v", answer.Functions[0])
	}
}

// The name of a function of a user follows the rule V2 uses.
func TestFunctionNamePrefix(t *testing.T) {
	for user, want := range map[string]string{
		"agent-test":      "agw-agent-test-",
		"Philipp.Schröer": "agw-philipp-schr-er-",
		"a":               "agw-a-",
	} {
		if got := FunctionNamePrefix(user); got != want {
			t.Errorf("%s: %q, want %q", user, got, want)
		}
	}
}

// The sanitising of a user name keeps at most 20 characters and collapses dashes.
func TestSanitizeUser(t *testing.T) {
	if got := sanitizeUser("Ein.SEHR_langer--Name!!"); got != "ein-sehr-langer-name" {
		t.Errorf("long name: %q", got)
	}
	if got := sanitizeUser("--"); got != "" {
		t.Errorf("only separators: %q", got)
	}
}

// ---------------------------------------------------------------------------
// annotation_run
// ---------------------------------------------------------------------------

// A run of another chat is not found (E8).
func TestRunStatusOtherChat(t *testing.T) {
	e := newTestEnv(t)
	chatID := e.chat(t)
	other := e.chat(t)
	if _, err := e.st.AddAnnotationRun(context.Background(), store.AnnotationRun{
		ID: "run-1", ChatID: other, DatasetID: 5, TaskID: 7, Function: "f", MinConfidence: 50, Existing: "keep"}); err != nil {
		t.Fatal(err)
	}
	res := e.do(t, chatID, http.MethodGet, platform.AnnotationPrefix+"/runs/run-1", "")
	if res.Status != "error" || res.Message != `unknown run "run-1" in this chat` {
		t.Fatalf("other chat: %+v", res)
	}
	if len(e.fc.saw()) != 0 {
		t.Errorf("CVAT was asked about a foreign run: %v", e.fc.saw())
	}
}

// A run CVAT no longer knows becomes expired; what it wrote stays in the task.
func TestRunStatusExpired(t *testing.T) {
	e := newTestEnv(t)
	chatID := e.chat(t)
	if _, err := e.st.AddAnnotationRun(context.Background(), store.AnnotationRun{
		ID: "run-1", ChatID: chatID, DatasetID: 5, TaskID: 7, Function: "f", MinConfidence: 50, Existing: "keep"}); err != nil {
		t.Fatal(err)
	}
	res := e.do(t, chatID, http.MethodGet, platform.AnnotationPrefix+"/runs/run-1", "")
	if res.Status != "ok" || !strings.Contains(res.Body, `"state":"expired"`) {
		t.Fatalf("expired: %+v", res)
	}
	row, err := e.st.AnnotationRun(context.Background(), chatID, "run-1")
	if err != nil || row.State != store.AnnotationExpired || row.EndedAt == nil {
		t.Fatalf("row: %+v %v", row, err)
	}
}

// A run that is over is answered from the gateway's own row: CVAT's queue drops its jobs, and asking
// again would turn a finished run into expired.
func TestRunStatusFinishedFromRow(t *testing.T) {
	e := newTestEnv(t)
	chatID := e.chat(t)
	row, err := e.st.AddAnnotationRun(context.Background(), store.AnnotationRun{
		ID: "run-1", ChatID: chatID, DatasetID: 5, TaskID: 7, Function: "f", MinConfidence: 50, Existing: "keep"})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.st.UpdateAnnotationRun(context.Background(), chatID, row.ID, store.AnnotationFinished, ""); err != nil {
		t.Fatal(err)
	}
	res := e.do(t, chatID, http.MethodGet, platform.AnnotationPrefix+"/runs/run-1", "")
	var answer runStatusAnswer
	if err := json.Unmarshal([]byte(res.Body), &answer); err != nil {
		t.Fatal(err)
	}
	if answer.State != store.AnnotationFinished || answer.Progress != 100 || answer.Note == "" {
		t.Fatalf("finished run: %+v", answer)
	}
	for _, c := range e.fc.saw() {
		if strings.HasPrefix(c, "GET /api/lambda") {
			t.Errorf("CVAT was asked about a run that is over: %v", e.fc.saw())
		}
	}
}

// A running run is followed at CVAT; its state is written into the row.
func TestRunStatusRunning(t *testing.T) {
	e := newTestEnv(t)
	chatID := e.chat(t)
	if _, err := e.st.AddAnnotationRun(context.Background(), store.AnnotationRun{
		ID: "run-1", ChatID: chatID, DatasetID: 5, TaskID: 7, Function: "f", MinConfidence: 50, Existing: "keep"}); err != nil {
		t.Fatal(err)
	}
	e.fc.setRun("run-1", `{"id":"run-1","status":"started","progress":42,"started":"2026-10-16T10:00:00","ended":null}`)
	res := e.do(t, chatID, http.MethodGet, platform.AnnotationPrefix+"/runs/run-1", "")
	var answer runStatusAnswer
	if err := json.Unmarshal([]byte(res.Body), &answer); err != nil {
		t.Fatal(err)
	}
	if answer.State != store.AnnotationStarted || answer.Progress != 42 || answer.Started == "" {
		t.Fatalf("running: %+v", answer)
	}
	row, err := e.st.AnnotationRun(context.Background(), chatID, "run-1")
	if err != nil || row.State != store.AnnotationStarted {
		t.Fatalf("row not updated: %+v %v", row, err)
	}
}

// ---------------------------------------------------------------------------
// annotation_summary
// ---------------------------------------------------------------------------

// The counting separates manual from automatic annotations and counts the frames.
func TestSummaryCounts(t *testing.T) {
	e := newTestEnv(t)
	chatID := e.chat(t)
	e.pf.setDataset(datasetJSON(5, "test", "AgriImageDataResource", []string{"0", "1"}, 7))
	e.fc.setTask(7, taskJSON(7, "test", 10, labelsJSON("0", "1")))
	e.fc.setAnnotations(7, `{"version":0,
		"tags":[{"frame":0,"label_id":1,"source":"auto"},{"frame":1,"label_id":1,"source":"manual"}],
		"shapes":[{"frame":2,"label_id":2,"type":"rectangle","source":"auto"},
		          {"frame":3,"label_id":2,"type":"rectangle","source":"manual"}],
		"tracks":[{"label_id":2,"source":"auto","shapes":[
			{"frame":4,"label_id":2,"type":"rectangle","source":"auto","outside":false},
			{"frame":5,"label_id":2,"type":"rectangle","source":"auto","outside":true}]}]}`)

	res := e.do(t, chatID, http.MethodGet, platform.AnnotationPrefix+"/datasets/5/summary", "")
	var answer summaryAnswer
	if err := json.Unmarshal([]byte(res.Body), &answer); err != nil {
		t.Fatal(err)
	}
	if res.Status != "ok" || answer.TaskID != 7 || answer.Frames != 10 || answer.Tags != 2 {
		t.Fatalf("summary: %+v", answer)
	}
	if answer.ByClass["0"] != (classCount{Manual: 1, Auto: 1}) || answer.ByClass["1"] != (classCount{Manual: 1, Auto: 2}) {
		t.Errorf("by class: %+v", answer.ByClass)
	}
	// The frames 0 to 4 carry an annotation, 5 does not (that shape marks the object as absent).
	if answer.FramesWithAnnotations != 5 || answer.FramesWithoutAnnotations != 5 {
		t.Errorf("frames: %+v", answer)
	}
	if answer.ShapesByType["rectangle"] != 3 {
		t.Errorf("shapes by type: %+v", answer.ShapesByType)
	}
}

// ---------------------------------------------------------------------------
// annotation_sync
// ---------------------------------------------------------------------------

// sync uses the download of the platform (the only call that exports the task) and throws the ZIP away.
func TestSyncDataset(t *testing.T) {
	e := newTestEnv(t)
	chatID := e.chat(t)
	e.pf.setDataset(datasetJSON(5, "test", "AgriImageDataResource", []string{"0"}, 7))

	res := e.do(t, chatID, http.MethodPost, platform.AnnotationPrefix+"/datasets/5/sync", "")
	var answer syncAnswer
	if err := json.Unmarshal([]byte(res.Body), &answer); err != nil {
		t.Fatal(err)
	}
	if res.Status != "ok" || !answer.Synced || answer.DatasetID != 5 {
		t.Fatalf("sync: %+v", res)
	}
	if !e.pf.sawCall("GET", "/datasets/5/download") {
		t.Errorf("the download did not go out: %v", e.pf.saw())
	}
	if strings.Contains(res.Body, "PK") {
		t.Error("the ZIP reached the answer")
	}
}

// Without a task there is nothing to write back.
func TestSyncWithoutTask(t *testing.T) {
	e := newTestEnv(t)
	chatID := e.chat(t)
	res := e.do(t, chatID, http.MethodPost, platform.AnnotationPrefix+"/datasets/5/sync", "")
	if res.Status != "error" || !strings.Contains(res.Message, "has no CVAT task yet") {
		t.Fatalf("sync without a task: %+v", res)
	}
}

// ---------------------------------------------------------------------------
// The session (E1, E2)
// ---------------------------------------------------------------------------

// No answer of the gateway carries a value of the CVAT session, and the session stays in the gateway.
func TestNoCVATSecretsInResults(t *testing.T) {
	e := newTestEnv(t)
	chatID := readyDataset(t, e)
	calls := [][3]string{
		{http.MethodGet, platform.AnnotationPrefix + "/functions", ""},
		{http.MethodPost, platform.AnnotationPrefix + "/tasks", `{"dataset_id":5}`},
		{http.MethodPost, platform.AnnotationPrefix + "/runs",
			`{"dataset_id":5,"function":"agw-test-m1-abcd1234","min_confidence":50,"mapping":{"class_0":"0"},"existing":"keep"}`},
		{http.MethodGet, platform.AnnotationPrefix + "/datasets/5/summary", ""},
		{http.MethodPost, platform.AnnotationPrefix + "/datasets/5/sync", ""},
	}
	for _, c := range calls {
		res := e.do(t, chatID, c[0], c[1], c[2])
		if res.Status != "ok" {
			t.Fatalf("%s %s: %+v", c[0], c[1], res)
		}
		for _, secret := range []string{"SECRET-KEY", "SECRET-CSRF", "SECRET-SESSION"} {
			if strings.Contains(res.Body, secret) || strings.Contains(res.Message, secret) || strings.Contains(strings.Join(stepsOf(t, res), " "), secret) {
				t.Errorf("%s %s: an answer carries %s", c[0], c[1], secret)
			}
		}
	}
}

// Two operations of the same user share one login, and the gateway never logs out (E2).
func TestNoLogout(t *testing.T) {
	e := newTestEnv(t)
	chatID := readyDataset(t, e)
	for i := 0; i < 3; i++ {
		e.do(t, chatID, http.MethodGet, platform.AnnotationPrefix+"/functions", "")
	}
	logins := 0
	for _, c := range e.pf.saw() {
		if c == "POST /cvat/auth/login" {
			logins++
		}
		if strings.Contains(c, "logout") {
			t.Errorf("the gateway logged out: %v", e.pf.saw())
		}
	}
	if logins != 1 {
		t.Errorf("logins: %d, want 1 (the session is kept per user)", logins)
	}
}

// The session never reaches a log line either: only the call and its status are noted.
func TestStepsCarryNoSecrets(t *testing.T) {
	e := newTestEnv(t)
	chatID := e.chat(t)
	res := e.do(t, chatID, http.MethodPost, platform.AnnotationPrefix+"/tasks", `{"dataset_id":5}`)
	for _, step := range stepsOf(t, res) {
		if !regexp.MustCompile(`^(GET|POST|PATCH|DELETE) /[^ ]* \d+$`).MatchString(step) {
			t.Errorf("a step does not follow the form: %q", step)
		}
	}
}

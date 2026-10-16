// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"agw/internal/artifacts"
	"agw/internal/chat"
	"agw/internal/config"
	"agw/internal/oidc"
	"agw/internal/oidc/oidctest"
	"agw/internal/pool"
	"agw/internal/store"
)

// testCVATBase is AGW_CVAT_PUBLIC_URL of the test setup; the links of the cards begin with it.
const testCVATBase = "https://cvat.example.org"

// annotationServer builds the API of a test setup with a database and the given public CVAT address
// ("" for an instance without pre-annotation: no links, and the route still answers).
func annotationServer(t *testing.T, cvatBase string) (*store.Store, *httptest.Server, *oidctest.Issuer) {
	t.Helper()
	url := os.Getenv("AGW_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("AGW_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	st, err := store.OpenSchema(ctx, url, "apitest_annotation_"+strings.ReplaceAll(time.Now().Format("150405.000000"), ".", "_"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.DropSchema(context.Background()); st.Close() })
	cat, err := config.ParseCatalog([]byte(`{"default":"p/m","providers":[{"id":"p","upstream":"http://x","api":"openai-completions","models":[{"id":"m"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	p := pool.New[chat.Agent](func(context.Context, string, string) (chat.Agent, error) {
		return nil, errors.New("no slot in the test")
	}, func(context.Context, chat.Agent) {}, map[string]int{"cli": 0})
	m := chat.NewManager(st, p, cat, nil, artifacts.NewBroker(), chat.Options{AcquireTimeout: 100 * time.Millisecond, MaxSubagents: 5})
	is := oidctest.New(t)
	var h http.Handler
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.ServeHTTP(w, r) }))
	t.Cleanup(srv.Close)
	auth, err := oidc.New(oidc.Config{Issuer: is.IssuerURL(), ClientID: is.ClientID, ClientSecret: is.ClientSecret, PublicURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	h = (&Server{M: m, Pool: p, Cat: cat, OIDC: auth, Env: config.Env{CVATPublicURL: cvatBase}}).Handler()
	return st, srv, is
}

// dormantChat creates a chat of an owner in the store, as the route's tests need one.
func dormantChat(t *testing.T, st *store.Store, owner, title string) store.Chat {
	t.Helper()
	c, err := st.CreateChat(context.Background(), store.NewChat{Title: title, Model: "p/m", Variant: "cli", Owner: owner})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetState(context.Background(), c.ID, store.StateDormant); err != nil {
		t.Fatal(err)
	}
	return c
}

// annotationAnswerOf reads the route's answer.
func annotationAnswerOf(t *testing.T, body string) (string, []map[string]any, []map[string]any) {
	t.Helper()
	var answer struct {
		CVATURL string           `json:"cvat_url"`
		Runs    []map[string]any `json:"runs"`
		Jobs    []map[string]any `json:"jobs"`
	}
	if err := json.Unmarshal([]byte(body), &answer); err != nil {
		t.Fatalf("answer: %s (%v)", body, err)
	}
	return answer.CVATURL, answer.Runs, answer.Jobs
}

// The annotation route (V4, issue #112) belongs to the owner of the chat: a stranger sees nothing, and
// the runs of another chat never appear in the answer of one's own.
func TestAnnotationRouteOwner(t *testing.T) {
	st, srv, is := annotationServer(t, testCVATBase)
	ctx := context.Background()
	a := loggedIn(t, srv, is, anna)
	browserBert := loggedIn(t, srv, is, bert)
	mine := dormantChat(t, st, anna.Sub, "mine")
	theirs := dormantChat(t, st, bert.Sub, "theirs")

	if _, err := st.AddAnnotationRun(ctx, store.AnnotationRun{ID: "run-bert", ChatID: theirs.ID, DatasetID: 5,
		TaskID: 7, Function: "f", MinConfidence: 50, Existing: "keep"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddAnnotationJob(ctx, store.AnnotationJob{ChatID: theirs.ID, Kind: "fill", DatasetID: 5,
		TaskID: 7, Function: "f"}); err != nil {
		t.Fatal(err)
	}
	route := srv.URL + "/api/chats/" + mine.ID + "/annotation"
	code, body := call(t, a, "GET", route, "")
	if code != 200 {
		t.Fatalf("own chat: %d %s", code, body)
	}
	base, runs, jobs := annotationAnswerOf(t, body)
	if base != testCVATBase || len(runs) != 0 || len(jobs) != 0 {
		t.Fatalf("own chat without runs: %q %v %v", base, runs, jobs)
	}
	if strings.Contains(body, "run-bert") {
		t.Errorf("the runs of another chat appear: %s", body)
	}
	// Bert does not see the chat of Anna at all, and an unknown chat is not found either.
	if code, body := call(t, browserBert, "GET", srv.URL+"/api/chats/"+mine.ID+"/annotation", ""); code != 404 {
		t.Errorf("Bert on Anna's chat: %d %s", code, body)
	}
	if code, _ := call(t, a, "GET", srv.URL+"/api/chats/00000000-0000-0000-0000-000000000000/annotation", ""); code != 404 {
		t.Errorf("unknown chat: %d", code)
	}
	// Bert's own chat carries its run.
	if code, body := call(t, browserBert, "GET", srv.URL+"/api/chats/"+theirs.ID+"/annotation", ""); code != 200 ||
		!strings.Contains(body, `"run_id":"run-bert"`) {
		t.Errorf("Bert's own chat: %d %s", code, body)
	}
}

// A run of the route: the stored row (V1), the state its observed job reported and the tool call the
// card belongs to. The link is built from AGW_CVAT_PUBLIC_URL of this setup.
func TestAnnotationRouteRuns(t *testing.T) {
	st, srv, is := annotationServer(t, testCVATBase)
	ctx := context.Background()
	a := loggedIn(t, srv, is, anna)
	c := dormantChat(t, st, anna.Sub, "run")

	if _, err := st.AddAnnotationRun(ctx, store.AnnotationRun{ID: "run-7", ChatID: c.ID, DatasetID: 5, TaskID: 318,
		Function: "agw-anna-yolov8-1a2b", MinConfidence: 50, Mapping: `{"cow":"Rind"}`, Existing: "replace",
		State: store.AnnotationStarted}); err != nil {
		t.Fatal(err)
	}
	watch, err := st.AddWatchJob(ctx, store.WatchJob{ChatID: c.ID, Kind: "annotation_run", Ref: "run-7", ToolCallID: "call-7"})
	if err != nil {
		t.Fatal(err)
	}
	percent := 41.0
	if err := st.SetJobProgress(ctx, watch, store.WatchProgress{State: store.JobRunning, Percent: &percent, Message: "started"}); err != nil {
		t.Fatal(err)
	}
	route := srv.URL + "/api/chats/" + c.ID + "/annotation"
	code, body := call(t, a, "GET", route, "")
	if code != 200 {
		t.Fatalf("route: %d %s", code, body)
	}
	_, runs, _ := annotationAnswerOf(t, body)
	if len(runs) != 1 {
		t.Fatalf("runs: %s", body)
	}
	line := runs[0]
	if line["run_id"] != "run-7" || line["dataset_id"] != float64(5) || line["task_id"] != float64(318) ||
		line["function"] != "agw-anna-yolov8-1a2b" || line["min_confidence"] != float64(50) ||
		line["existing"] != "replace" || line["state"] != "started" || line["tool_call_id"] != "call-7" {
		t.Errorf("line: %v", line)
	}
	if line["progress"] != float64(41) || line["message"] != "started" {
		t.Errorf("the state of the observed job is missing: %v", line)
	}
	if mapping, _ := line["mapping"].(map[string]any); mapping["cow"] != "Rind" {
		t.Errorf("mapping: %v", line["mapping"])
	}
	if line["cvat_url"] != testCVATBase+"/tasks/318" {
		t.Errorf("link: %v", line["cvat_url"])
	}
	// A run that is over keeps its error and its end; the progress of the watcher stays as it was.
	if err := st.UpdateAnnotationRun(ctx, c.ID, "run-7", store.AnnotationFailed, "CVAT said no"); err != nil {
		t.Fatal(err)
	}
	_, body = call(t, a, "GET", route, "")
	_, runs, _ = annotationAnswerOf(t, body)
	if runs[0]["state"] != "failed" || runs[0]["error"] != "CVAT said no" || runs[0]["ended_at"] == nil {
		t.Errorf("finished run: %v", runs[0])
	}
	// The line carries no field besides the documented ones.
	for key := range runs[0] {
		switch key {
		case "run_id", "dataset_id", "task_id", "function", "min_confidence", "mapping", "existing", "state",
			"error", "progress", "message", "tool_call_id", "created_at", "ended_at", "cvat_url":
		default:
			t.Errorf("the route adds the field %q: %s", key, body)
		}
	}
}

// The jobs of the route: a finished check with its whole result, a running one with its progress and
// its params, and a value the store cannot read is left out instead of breaking the answer.
func TestAnnotationRouteJobs(t *testing.T) {
	st, srv, is := annotationServer(t, testCVATBase)
	ctx := context.Background()
	a := loggedIn(t, srv, is, anna)
	c := dormantChat(t, st, anna.Sub, "jobs")
	route := srv.URL + "/api/chats/" + c.ID + "/annotation"

	cal, err := st.AddAnnotationJob(ctx, store.AnnotationJob{ChatID: c.ID, Kind: "calibrate", DatasetID: 5, TaskID: 7,
		Function: "f", Params: `{"kind":"calibrate","dataset_id":5,"limit":50}`})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddWatchJob(ctx, store.WatchJob{ChatID: c.ID, Kind: "annotation_job", Ref: cal.ID, ToolCallID: "call-9"}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetAnnotationJobProgress(ctx, c.ID, cal.ID, 10, 10); err != nil {
		t.Fatal(err)
	}
	result := `{"images":10,"mapping":{"class_0":"0"},"accuracy":0.9,"proposed_min_confidence":55,"coverage":0.9}`
	if err := st.FinishAnnotationJob(ctx, c.ID, cal.ID, store.AnnotationJobFinished, result, ""); err != nil {
		t.Fatal(err)
	}
	unc, err := st.AddAnnotationJob(ctx, store.AnnotationJob{ChatID: c.ID, Kind: "uncertain", DatasetID: 5, TaskID: 7,
		Function: "f", Params: `{"kind":"uncertain","dataset_id":5,"uncertain_below":60}`})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetAnnotationJobState(ctx, c.ID, unc.ID, store.AnnotationJobRunning); err != nil {
		t.Fatal(err)
	}
	if err := st.SetAnnotationJobProgress(ctx, c.ID, unc.ID, 30, 200); err != nil {
		t.Fatal(err)
	}
	broken, err := st.AddAnnotationJob(ctx, store.AnnotationJob{ChatID: c.ID, Kind: "fill", DatasetID: 5, TaskID: 7, Function: "f"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishAnnotationJob(ctx, c.ID, broken.ID, store.AnnotationJobFailed, "not json", "the function answered nothing"); err != nil {
		t.Fatal(err)
	}

	code, body := call(t, a, "GET", route, "")
	if code != 200 {
		t.Fatalf("route: %d %s", code, body)
	}
	_, _, jobs := annotationAnswerOf(t, body)
	if len(jobs) != 3 {
		t.Fatalf("jobs: %s", body)
	}
	byID := map[string]map[string]any{}
	for _, line := range jobs {
		byID[line["job_id"].(string)] = line
	}
	check := byID[cal.ID]
	if check["kind"] != "calibrate" || check["state"] != "finished" || check["done"] != float64(10) ||
		check["total"] != float64(10) || check["tool_call_id"] != "call-9" || check["error"] != nil {
		t.Errorf("calibrate: %v", check)
	}
	got, _ := check["result"].(map[string]any)
	if got["accuracy"] != 0.9 || got["proposed_min_confidence"] != float64(55) {
		t.Errorf("result: %v", check["result"])
	}
	if params, _ := check["params"].(map[string]any); params["limit"] != float64(50) {
		t.Errorf("params: %v", check["params"])
	}
	if check["cvat_url"] != testCVATBase+"/tasks/7" {
		t.Errorf("link: %v", check["cvat_url"])
	}
	running := byID[unc.ID]
	if running["state"] != "running" || running["done"] != float64(30) || running["total"] != float64(200) ||
		running["result"] != nil || running["ended_at"] != nil {
		t.Errorf("running job: %v", running)
	}
	if params, _ := running["params"].(map[string]any); params["uncertain_below"] != float64(60) {
		t.Errorf("threshold: %v", running["params"])
	}
	failed := byID[broken.ID]
	if failed["state"] != "failed" || failed["error"] != "the function answered nothing" || failed["result"] != nil {
		t.Errorf("failed job: %v", failed)
	}
	// The result goes through whole: the card expands the list, it does not page it.
	const images = 60
	list := make([]string, 0, images)
	for i := 0; i < images; i++ {
		list = append(list, fmt.Sprintf(`{"frame":%d,"file":"img%d.png","cvat_url":"%s/tasks/7/jobs/1?frame=%d"}`,
			i, i, testCVATBase, i))
	}
	big, err := st.AddAnnotationJob(ctx, store.AnnotationJob{ChatID: c.ID, Kind: "uncertain", DatasetID: 5, TaskID: 7, Function: "f"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishAnnotationJob(ctx, c.ID, big.ID, store.AnnotationJobFinished,
		`{"checked":200,"uncertain":[`+strings.Join(list, ",")+`]}`, ""); err != nil {
		t.Fatal(err)
	}
	_, body = call(t, a, "GET", route, "")
	_, _, jobs = annotationAnswerOf(t, body)
	for _, line := range jobs {
		if line["job_id"] != big.ID {
			continue
		}
		got, _ := line["result"].(map[string]any)
		if items, _ := got["uncertain"].([]any); len(items) != images {
			t.Errorf("the route cuts the result: %d of %d", len(items), images)
		}
	}
}

// An instance without pre-annotation answers empty lists and no link at all (AGW_CVAT_PUBLIC_URL empty).
func TestAnnotationRouteWithoutCVAT(t *testing.T) {
	st, srv, is := annotationServer(t, "")
	ctx := context.Background()
	a := loggedIn(t, srv, is, anna)
	c := dormantChat(t, st, anna.Sub, "off")
	if _, err := st.AddAnnotationRun(ctx, store.AnnotationRun{ID: "run-1", ChatID: c.ID, DatasetID: 5, TaskID: 7,
		Function: "f", MinConfidence: 50, Existing: "keep"}); err != nil {
		t.Fatal(err)
	}
	code, body := call(t, a, "GET", srv.URL+"/api/chats/"+c.ID+"/annotation", "")
	if code != 200 {
		t.Fatalf("route: %d %s", code, body)
	}
	base, runs, _ := annotationAnswerOf(t, body)
	if base != "" || len(runs) != 1 || runs[0]["cvat_url"] != nil {
		t.Fatalf("without AGW_CVAT_PUBLIC_URL: %q %v", base, runs)
	}
	if strings.Contains(body, "/tasks/") {
		t.Errorf("the route builds links without a base: %s", body)
	}
}

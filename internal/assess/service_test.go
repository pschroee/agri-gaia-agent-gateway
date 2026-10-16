// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package assess

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"agw/internal/platform"
	"agw/internal/store"
	"agw/internal/watch"
)

// testStore opens its own schema for one test (as the store tests do).
func testStore(t *testing.T) *store.Store {
	t.Helper()
	url := os.Getenv("AGW_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("AGW_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	st, err := store.OpenSchema(ctx, url, "assess_"+strings.ReplaceAll(time.Now().Format("150405.000000"), ".", "_"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.DropSchema(context.Background()); st.Close() })
	return st
}

// testChat creates a chat for a test.
func testChat(t *testing.T, st *store.Store, title string) store.Chat {
	t.Helper()
	c, err := st.CreateChat(context.Background(), store.NewChat{Title: title, Model: "m", Variant: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// waitFor waits for a condition (at most five seconds), so that a test does not sleep a fixed time while
// a background check runs.
func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// A check is started in the background: Start answers at once with the id and the row is queued until
// the runner gets to it.
func TestStartAndReport(t *testing.T) {
	st := testStore(t)
	svc := New(NewRunner(&fakeRuntime{answer: goodAnswer}, "img"), st, 12)
	ctx := context.Background()
	c := testChat(t, st, "checks")
	done := make(chan struct{})
	id, err := svc.Start(ctx, c.ID, "training_model", "model:7", 0, false, func(context.Context) (Report, error) {
		<-done
		rep := NewReport("training_model", SetValidation, 12)
		rep.Verdict = Questionable
		rep.Reasons = []string{"the model is better than the baseline by chance"}
		return rep, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if id == "" {
		t.Fatal("no check id")
	}
	row, err := st.Assessment(ctx, c.ID, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != store.AssessQueued && row.State != store.AssessRunning {
		t.Fatalf("state while running: %q", row.State)
	}
	close(done)
	waitFor(t, "the report", func() bool {
		row, err := st.Assessment(ctx, c.ID, id)
		return err == nil && row.State == store.AssessFinished
	})
	row, _ = st.Assessment(ctx, c.ID, id)
	if !strings.Contains(string(row.Report), "Result: questionable (validation images, interim, no image review)") {
		t.Fatalf("report: %s", row.Report)
	}
	if row.StartedAt == nil || row.EndedAt == nil {
		t.Errorf("times: started %v, ended %v", row.StartedAt, row.EndedAt)
	}
	// The status tool answers the same row (R0.17).
	res, err := svc.Do(ctx, c.ID, platform.Request{Method: http.MethodGet, Path: platform.AssessPrefix + "/checks/" + id})
	if err != nil || res.Status != "ok" || !strings.Contains(res.Body, `"state":"finished"`) {
		t.Fatalf("status: %+v %v", res, err)
	}
	// A check of another chat is not found, and neither is an unknown one.
	if res, _ := svc.Do(ctx, "11111111-1111-1111-1111-111111111111",
		platform.Request{Method: http.MethodGet, Path: platform.AssessPrefix + "/checks/" + id}); res.Status != "error" ||
		!strings.Contains(res.Message, "not found in this chat") {
		t.Errorf("another chat: %+v", res)
	}
	if res, _ := svc.Do(ctx, c.ID, platform.Request{Method: http.MethodGet,
		Path: platform.AssessPrefix + "/checks/11111111-2222-3333-4444-555555555555"}); res.Status != "error" ||
		!strings.Contains(res.Message, "check not found in this chat") {
		t.Errorf("unknown check: %+v", res)
	}
}

// The limit of a chat counts the checks it has used; failed checks do not count (R0.20).
func TestStartLimit(t *testing.T) {
	st := testStore(t)
	svc := New(NewRunner(&fakeRuntime{answer: goodAnswer}, "img"), st, 2)
	ctx := context.Background()
	c := testChat(t, st, "limit")
	release := make(chan struct{})
	run := func(context.Context) (Report, error) {
		<-release
		return NewReport("training_model", SetValidation, 5), nil
	}
	if _, err := svc.Start(ctx, c.ID, "training_model", "model:1", 0, false, run); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Start(ctx, c.ID, "training_model", "model:1", 0, false, run); err != nil {
		t.Fatal(err)
	}
	_, err := svc.Start(ctx, c.ID, "training_model", "model:1", 0, false, run)
	if err == nil || !strings.Contains(err.Error(), "check limit reached: this chat has used 2 of 2 checks (AGW_ASSESS_MAX_PER_CHAT)") {
		t.Fatalf("third check: %v", err)
	}
	close(release)
	waitFor(t, "both checks", func() bool {
		n, err := st.CountAssessments(ctx, c.ID)
		return err == nil && n == 2
	})
	// A failed check does not eat a try: it is not counted.
	rows, err := st.Assessments(ctx, c.ID)
	if err != nil || len(rows) != 2 {
		t.Fatalf("the checks of the chat: %d %v", len(rows), err)
	}
	if err := st.FinishAssessment(ctx, c.ID, rows[0].ID, store.AssessFailed, nil, "boom"); err != nil {
		t.Fatal(err)
	}
	if n, err := st.CountAssessments(ctx, c.ID); err != nil || n != 1 {
		t.Fatalf("counted after a failure: %d %v", n, err)
	}
	if _, err := svc.Start(ctx, c.ID, "training_model", "model:1", 0, false, func(context.Context) (Report, error) {
		return NewReport("training_model", SetValidation, 5), nil
	}); err != nil {
		t.Fatalf("after a failed check: %v", err)
	}
}

// Only one check runs at a time: the second waits for the slot (R0.20).
func TestStartOneAtATime(t *testing.T) {
	st := testStore(t)
	svc := New(NewRunner(&fakeRuntime{answer: goodAnswer}, "img"), st, 12)
	ctx := context.Background()
	c := testChat(t, st, "one at a time")
	first := make(chan struct{})
	started := make(chan struct{})
	id1, err := svc.Start(ctx, c.ID, "training_model", "model:1", 0, false, func(context.Context) (Report, error) {
		close(started)
		<-first
		return NewReport("training_model", SetValidation, 5), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	id2, err := svc.Start(ctx, c.ID, "training_model", "model:2", 0, false, func(context.Context) (Report, error) {
		return NewReport("training_model", SetValidation, 5), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// While the first runs, the second stays queued.
	for i := 0; i < 10; i++ {
		row, err := st.Assessment(ctx, c.ID, id2)
		if err != nil {
			t.Fatal(err)
		}
		if row.State != store.AssessQueued {
			t.Fatalf("the second check runs next to the first: %q", row.State)
		}
		time.Sleep(20 * time.Millisecond)
	}
	close(first)
	waitFor(t, "both checks", func() bool {
		r1, e1 := st.Assessment(ctx, c.ID, id1)
		r2, e2 := st.Assessment(ctx, c.ID, id2)
		return e1 == nil && e2 == nil && r1.State == store.AssessFinished && r2.State == store.AssessFinished
	})
}

// A final check of one dataset happens once per chat (P4): the gateway refuses a second one, and the
// unique index of the table holds the same rule in the database.
func TestFinalOnceIndex(t *testing.T) {
	st := testStore(t)
	svc := New(NewRunner(&fakeRuntime{answer: goodAnswer}, "img"), st, 12)
	ctx := context.Background()
	c := testChat(t, st, "final once")
	id, err := svc.Start(ctx, c.ID, "training_model", "model:1", 7, true, func(context.Context) (Report, error) {
		return NewReport("training_model", SetHoldout, 25), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the final check", func() bool {
		row, err := st.Assessment(ctx, c.ID, id)
		return err == nil && row.State == store.AssessFinished
	})
	if _, err := svc.Start(ctx, c.ID, "training_model", "model:2", 7, true, func(context.Context) (Report, error) {
		return NewReport("training_model", SetHoldout, 25), nil
	}); err == nil || !strings.Contains(err.Error(), "already run in this chat") {
		t.Fatalf("a second final check: %v", err)
	}
	// Comparing rounds stays possible, also at the same dataset.
	if _, err := svc.Start(ctx, c.ID, "training_model", "model:2", 7, false, func(context.Context) (Report, error) {
		return NewReport("training_model", SetValidation, 8), nil
	}); err != nil {
		t.Fatalf("an interim check: %v", err)
	}
	// The database carries the rule as well: a second final row of the same dataset is a unique
	// violation, and a failed one leaves the index.
	if _, err := st.CreateAssessment(ctx, store.Assessment{ChatID: c.ID, Kind: "training_model", Subject: "sha:abc", EvalDatasetID: 9, Final: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateAssessment(ctx, store.Assessment{ChatID: c.ID, Kind: "training_model", Subject: "sha:def", EvalDatasetID: 9, Final: true}); err == nil {
		t.Fatal("the unique index let a second final check of the same dataset through")
	}
	rows, err := st.Assessments(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	last := rows[len(rows)-1]
	if err := st.FinishAssessment(ctx, c.ID, last.ID, store.AssessFailed, nil, "gateway restarted"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateAssessment(ctx, store.Assessment{ChatID: c.ID, Kind: "training_model", Subject: "sha:ghi", EvalDatasetID: 9, Final: true}); err != nil {
		t.Fatalf("after a failed final check: %v", err)
	}
}

// A check that broke may still carry a partial report (figures without verdict and header, L5).
func TestFailedKeepsFigures(t *testing.T) {
	st := testStore(t)
	svc := New(NewRunner(&fakeRuntime{answer: goodAnswer}, "img"), st, 12)
	ctx := context.Background()
	c := testChat(t, st, "partial")
	rep := NewReport("own_model", SetHoldout, 30)
	rep.Verdict = Usable
	rep.Finish()
	rep.Figures = ClassMetrics([]string{"a", "b"}, []string{"a", "a"}, nil)
	id, err := svc.Start(ctx, c.ID, "own_model", "sha:abc", 4, true, func(context.Context) (Report, error) {
		return rep, errPartial
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the failure", func() bool {
		row, err := st.Assessment(ctx, c.ID, id)
		return err == nil && row.State == store.AssessFailed
	})
	row, _ := st.Assessment(ctx, c.ID, id)
	if row.Error != errPartial.Error() {
		t.Errorf("error: %q", row.Error)
	}
	if !strings.Contains(string(row.Report), `"figures"`) {
		t.Fatalf("the partial report is lost: %s", row.Report)
	}
	var partial Report
	if err := json.Unmarshal(row.Report, &partial); err != nil {
		t.Fatal(err)
	}
	if partial.Verdict != "" || partial.Header != "" {
		t.Errorf("a failed check carries a verdict (%q) or a head (%q): %s", partial.Verdict, partial.Header, row.Report)
	}
	if partial.Figures == nil || partial.Measured != 30 {
		t.Errorf("the partial report lost what it had: %+v", partial)
	}
	// A failed check does not block the final check of the dataset.
	if ok, err := st.FinalUsed(ctx, c.ID, 4); err != nil || ok {
		t.Errorf("a failed check counts as the final one: %v %v", ok, err)
	}
}

// errPartial is the error of a check that broke after it had measured something.
var errPartial = &partialError{}

type partialError struct{}

func (*partialError) Error() string { return "the check ran out of time" }

// The capability "checks" names whether this setup can check and how many checks are left (R0.23).
func TestChecksCapability(t *testing.T) {
	st := testStore(t)
	svc := New(NewRunner(&fakeRuntime{answer: goodAnswer}, "img"), st, 3)
	ctx := context.Background()
	c := testChat(t, st, "capability")
	ok, detail := svc.ChecksState(ctx, c.ID)
	if !ok || !strings.Contains(detail, "classification models on held-out images in a gateway container") ||
		!strings.Contains(detail, "3 of 3 checks left in this chat") {
		t.Fatalf("without a check: %v %q", ok, detail)
	}
	if _, err := st.CreateAssessment(ctx, store.Assessment{ChatID: c.ID, Kind: "training_model", Subject: "model:1"}); err != nil {
		t.Fatal(err)
	}
	if _, detail := svc.ChecksState(ctx, c.ID); !strings.Contains(detail, "2 of 3 checks left in this chat") {
		t.Fatalf("after one check: %q", detail)
	}
	// Without a container runtime there is no entry that says available.
	off := New(nil, st, 3)
	if ok, detail := off.ChecksState(ctx, c.ID); ok || detail != "not configured on this instance" {
		t.Errorf("without a runner: %v %q", ok, detail)
	}
	if _, err := off.Start(ctx, c.ID, "training_model", "model:1", 0, true, nil); err == nil {
		t.Error("an unconfigured service started a check")
	}
}

// The observer kind of a check: running while it runs, done with the head of the report, failed with the
// error (R0.21).
func TestAssessmentWatchKind(t *testing.T) {
	st := testStore(t)
	svc := New(NewRunner(&fakeRuntime{answer: goodAnswer}, "img"), st, 12)
	ctx := context.Background()
	c := testChat(t, st, "watch")
	row, err := st.CreateAssessment(ctx, store.Assessment{ChatID: c.ID, Kind: "training_model", Subject: "model:1"})
	if err != nil {
		t.Fatal(err)
	}
	p, err := svc.poll(ctx, watch.Job{ID: "j1", ChatID: c.ID, Kind: KindName, Ref: row.ID}, nil)
	if err != nil || p.State != "running" {
		t.Fatalf("queued: %+v %v", p, err)
	}
	if err := st.SetAssessmentState(ctx, c.ID, row.ID, store.AssessRunning); err != nil {
		t.Fatal(err)
	}
	if err := svc.SetProgress(ctx, row.ID, Progress{Set: SetHoldout, Images: 120, Note: "checking 120 of 200 images"}); err != nil {
		t.Fatal(err)
	}
	if p, _ := svc.poll(ctx, watch.Job{ChatID: c.ID, Ref: row.ID}, nil); p.State != "running" ||
		!strings.Contains(p.Message, "120 of 200") {
		t.Fatalf("running: %+v", p)
	}
	rep := NewReport("training_model", SetHoldout, 200)
	rep.Verdict = Usable
	rep.Finish()
	raw, _ := json.Marshal(rep)
	if err := st.FinishAssessment(ctx, c.ID, row.ID, store.AssessFinished, raw, ""); err != nil {
		t.Fatal(err)
	}
	p, _ = svc.poll(ctx, watch.Job{ChatID: c.ID, Ref: row.ID}, nil)
	if p.State != "done" || !strings.Contains(p.Message, "Result: usable") || p.Percent == nil || *p.Percent != 100 {
		t.Fatalf("done: %+v", p)
	}
	// A failed check names its error.
	failed, err := st.CreateAssessment(ctx, store.Assessment{ChatID: c.ID, Kind: "own_model", Subject: "sha:abc"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishAssessment(ctx, c.ID, failed.ID, store.AssessFailed, nil, "the model could not be loaded"); err != nil {
		t.Fatal(err)
	}
	p, _ = svc.poll(ctx, watch.Job{ChatID: c.ID, Ref: failed.ID}, nil)
	if p.State != "failed" || !strings.Contains(p.Message, "could not be loaded") {
		t.Fatalf("failed: %+v", p)
	}
	// The route of the kind starts at POST /_agw/assess/checks and reads the check id out of the answer.
	method, route := CheckRoute()
	if method != http.MethodPost || !route.MatchString(platform.AssessPrefix+"/checks") {
		t.Errorf("route: %s %v", method, route)
	}
	if _, ok := RunIDFrom(platform.Request{}, platform.Result{Status: "ok", Body: `{"check_id":"x"}`}); !ok {
		t.Error("the check id is not read out of the answer")
	}
	if _, ok := RunIDFrom(platform.Request{}, platform.Result{Status: "ok", Body: `{"state":"queued"}`}); ok {
		t.Error("an answer without a check id gives a reference")
	}
}

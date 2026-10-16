// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package annotation

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"agw/internal/platform"
	"agw/internal/store"
	"agw/internal/watch"
)

// ---------------------------------------------------------------------------
// The measurement itself (pure)
// ---------------------------------------------------------------------------

// The sample of a check is evenly spread over the images and reproducible: no random source.
func TestSampleStride(t *testing.T) {
	for _, tc := range []struct {
		n, limit int
		want     []int
	}{
		{0, 10, nil},
		{5, 0, nil},
		{5, 10, []int{0, 1, 2, 3, 4}},
		{30, 10, []int{0, 3, 6, 9, 12, 15, 18, 21, 24, 27}},
		{30, 7, []int{0, 5, 10, 15, 20, 25}},
		{3, 3, []int{0, 1, 2}},
		{4, 3, []int{0, 2}},
	} {
		got := Sample(tc.n, tc.limit)
		if len(got) != len(tc.want) {
			t.Errorf("Sample(%d,%d) = %v, want %v", tc.n, tc.limit, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("Sample(%d,%d) = %v, want %v", tc.n, tc.limit, got, tc.want)
				break
			}
		}
	}
}

// obs builds observations: frame i carries the answer with the given class and confidence and the
// given manual class; an empty answer stays empty.
func obs(pairs ...struct {
	answer     string
	confidence float64
	manual     string
}) []Observation {
	out := make([]Observation, 0, len(pairs))
	for i, p := range pairs {
		o := Observation{Frame: i}
		if p.answer != "" {
			o.Answers = []Answer{{Label: p.answer, Confidence: p.confidence}}
		}
		if p.manual != "" {
			o.Manual = []ManualObject{{Class: p.manual}}
		}
		out = append(out, o)
	}
	return out
}

type pair = struct {
	answer     string
	confidence float64
	manual     string
}

// A classification: the mapping comes out of the confusion table, and the threshold is the one with the
// best hit rate among the answered images, guarded by the coverage of 80 percent.
func TestCalibrateClassification(t *testing.T) {
	// Eight images answer class_0 with 0.9 and were annotated "0"; two answer class_0 with 0.2 and were
	// annotated "1" (the function is wrong on those).
	in := []pair{}
	for i := 0; i < 8; i++ {
		in = append(in, pair{"class_0", 0.90, "0"})
	}
	in = append(in, pair{"class_0", 0.20, "1"}, pair{"class_0", 0.20, "1"})

	got := Calibrate(obs(in...), KindClassification)
	if got.Images != 10 {
		t.Fatalf("images: %d", got.Images)
	}
	if got.Mapping["class_0"] != "0" || len(got.Mapping) != 1 {
		t.Errorf("mapping: %v", got.Mapping)
	}
	if got.Confusion["class_0"]["0"] != 8 || got.Confusion["class_0"]["1"] != 2 {
		t.Errorf("confusion: %v", got.Confusion)
	}
	// 0.10 answers all ten images (8 of 10 right), 0.25 answers eight (all right): the hit rate of the
	// hypothesis "what the function answers is right" decides, and 80 percent coverage permits the answer
	// that drops the two wrong ones — the lowest threshold that reaches the best hit rate.
	if got.ProposedMinConfidence != 25 {
		t.Errorf("proposed threshold: %d", got.ProposedMinConfidence)
	}
	if got.Accuracy == nil || *got.Accuracy != 1.0 {
		t.Errorf("accuracy: %v", got.Accuracy)
	}
	if got.Coverage != 0.8 {
		t.Errorf("coverage: %v", got.Coverage)
	}
	if got.Precision != nil || got.F1 != nil {
		t.Errorf("a classification carries no precision or F1: %v %v", got.Precision, got.F1)
	}
	if !strings.Contains(got.Note, "Measured on 10") {
		t.Errorf("note: %q", got.Note)
	}

	// Without any answer the measurement says so and proposes the lowest threshold.
	empty := Calibrate(obs(pair{"", 0, "0"}), KindClassification)
	if empty.Accuracy == nil || *empty.Accuracy != 0 || empty.ProposedMinConfidence != 10 || empty.Coverage != 0 {
		t.Errorf("without an answer: %+v", empty)
	}
	if _, ok := empty.Confusion[""]; ok {
		t.Errorf("an empty class reached the confusion table: %v", empty.Confusion)
	}
}

// A detection: the mapping comes out of the boxes that stand on a manual object (IoU), and the
// threshold is the one with the best F1.
func TestCalibrateDetection(t *testing.T) {
	box := func(mm, cc float64) []float64 { return []float64{0, 0, mm, cc} }
	in := []Observation{
		{Frame: 0, Answers: []Answer{{Label: "cow", Confidence: 0.9, Points: box(10, 10)}},
			Manual: []ManualObject{{Class: "pig", Points: box(10, 10)}}},
		{Frame: 1, Answers: []Answer{{Label: "cow", Confidence: 0.3, Points: box(10, 10)}},
			Manual: []ManualObject{{Class: "pig", Points: box(10, 10)}}},
		// A box without a manual object: it is a false positive while the threshold is under 0.2.
		{Frame: 2, Answers: []Answer{{Label: "cow", Confidence: 0.2, Points: []float64{50, 50, 60, 60}}}},
	}
	got := Calibrate(in, KindDetection)
	if got.Mapping["cow"] != "pig" || len(got.Mapping) != 1 {
		t.Errorf("mapping: %v", got.Mapping)
	}
	if got.Confusion["cow"]["pig"] != 2 {
		t.Errorf("confusion: %v", got.Confusion)
	}
	if got.ProposedMinConfidence != 25 {
		t.Errorf("proposed threshold: %d", got.ProposedMinConfidence)
	}
	if got.Precision == nil || *got.Precision != 1 || got.Recall == nil || *got.Recall != 1 || got.F1 == nil || *got.F1 != 1 {
		t.Errorf("figures at the proposed threshold: %v %v %v", got.Precision, got.Recall, got.F1)
	}
	if got.Accuracy != nil {
		t.Errorf("a detector carries no accuracy: %v", got.Accuracy)
	}
	if got.Coverage != round(2.0/3.0) {
		t.Errorf("coverage: %v", got.Coverage)
	}
	// A box that overlaps too little is no match: the mapping stays empty.
	loose := []Observation{{Frame: 0, Answers: []Answer{{Label: "cow", Confidence: 0.9, Points: []float64{0, 0, 10, 10}}},
		Manual: []ManualObject{{Class: "pig", Points: []float64{40, 40, 50, 50}}}}}
	if loose := Calibrate(loose, KindDetection); len(loose.Mapping) != 0 {
		t.Errorf("a box without overlap was mapped: %v", loose.Mapping)
	}
}

// The uncertainty lists the weakest images first; an image without an answer is the weakest of all.
func TestUncertainOrder(t *testing.T) {
	in := obs(
		pair{"class_0", 0.55, "0"},
		pair{"class_0", 0.20, "0"},
		pair{"", 0, "0"},
		pair{"class_0", 0.70, "0"},
	)
	got := Uncertain(in, KindClassification, 0.60)
	if len(got) != 3 {
		t.Fatalf("uncertain: %+v", got)
	}
	if got[0].Frame != 2 || got[0].Confidence != 0 {
		t.Errorf("the image without an answer comes first: %+v", got[0])
	}
	if got[1].Frame != 1 || got[1].Confidence != 0.2 || got[1].TopLabel != "class_0" {
		t.Errorf("second: %+v", got[1])
	}
	if got[2].Frame != 0 || got[2].Confidence != 0.55 {
		t.Errorf("third: %+v", got[2])
	}

	// A detector: a box under the threshold makes the image uncertain, an image without a box too.
	boxes := []Observation{
		{Frame: 0, Answers: []Answer{{Label: "cow", Confidence: 0.9, Points: []float64{0, 0, 1, 1}},
			{Label: "cow", Confidence: 0.55, Points: []float64{2, 2, 3, 3}}}},
		{Frame: 1, Answers: []Answer{{Label: "cow", Confidence: 0.9, Points: []float64{0, 0, 1, 1}}}},
		{Frame: 2},
	}
	got = Uncertain(boxes, KindDetection, 0.60)
	if len(got) != 2 || got[0].Frame != 2 || got[1].Frame != 0 || got[1].Boxes != 2 || got[1].Confidence != 0.55 {
		t.Fatalf("detection: %+v", got)
	}
}

// The uncertain images of a finished job are answered page by page.
func TestJobPages(t *testing.T) {
	items := make([]UncertainImage, 120)
	for i := range items {
		items[i] = UncertainImage{Frame: i, Confidence: 0.1}
	}
	all, err := json.Marshal(uncertainAnswer{Checked: 120, Uncertain: items})
	if err != nil {
		t.Fatal(err)
	}
	row := store.AnnotationJob{Kind: JobUncertain, State: store.AnnotationJobFinished, Result: string(all)}
	raw, page, pages := pageOf(row, 1)
	if page != 1 || pages != 3 {
		t.Fatalf("page %d of %d", page, pages)
	}
	var first uncertainAnswer
	if err := json.Unmarshal(raw, &first); err != nil {
		t.Fatal(err)
	}
	if first.Checked != 120 || len(first.Uncertain) != 50 || first.Uncertain[0].Frame != 0 {
		t.Fatalf("first page: %+v", first)
	}
	raw, page, _ = pageOf(row, 3)
	var last uncertainAnswer
	if err := json.Unmarshal(raw, &last); err != nil {
		t.Fatal(err)
	}
	if page != 3 || len(last.Uncertain) != 20 || last.Uncertain[0].Frame != 100 {
		t.Fatalf("last page: %+v", last)
	}
	// A page that does not exist is cut to the last one, and a call without a page answers the first.
	if _, page, _ = pageOf(row, 99); page != 3 {
		t.Errorf("too high: %d", page)
	}
	if _, page, _ = pageOf(row, 0); page != 1 {
		t.Errorf("too low: %d", page)
	}
	// The result of a calibration and of a fill stands as it is.
	cal := store.AnnotationJob{Kind: JobCalibrate, Result: `{"images":3}`}
	raw, page, pages = pageOf(cal, 4)
	if string(raw) != `{"images":3}` || page != 1 || pages != 1 {
		t.Errorf("calibration: %s %d %d", raw, page, pages)
	}
}

// ---------------------------------------------------------------------------
// The job of a check
// ---------------------------------------------------------------------------

// v3Env prepares a dataset with a CVAT task of 30 images, the classes 0 and 1, a usable function of the
// user and the answers the function gives per frame.
func v3Env(t *testing.T, size int) *testEnv {
	t.Helper()
	e := newTestEnv(t)
	e.pf.setDataset(datasetJSON(5, "test", "AgriImageDataResource", []string{"0", "1"}, 7))
	e.fc.setTask(7, taskJSON(7, "test", size, labelsJSON("0", "1")))
	e.fc.setAnnotations(7, `{"version":0,"tags":[],"shapes":[],"tracks":[]}`)
	e.fc.setFunctions(`[{"id":"agw-test-m1-abcd1234","name":"mine","kind":"detector","labels":["class_0","class_1"]}]`)
	// The meta answers the size and the file name of every frame (the single-frame form of CVAT).
	e.fc.setMeta(`{"size":` + strconv.Itoa(size) + `,"width":100,"height":100,"name":"img.png"}`)
	e.fc.setJobs(`{"count":1,"results":[{"id":11,"start_frame":0,"stop_frame":` + strconv.Itoa(size-1) + `}]}`)
	return e
}

// waitJob waits for a job of this chat to end.
func (e *testEnv) waitJob(t *testing.T, chatID, jobID string) store.AnnotationJob {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		row, err := e.st.AnnotationJob(context.Background(), chatID, jobID)
		if err != nil {
			t.Fatalf("job %s: %v", jobID, err)
		}
		if store.AnnotationJobFinal(row.State) {
			return row
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("job %s did not end", jobID)
	return store.AnnotationJob{}
}

// startJob starts a check or a fill and returns the job id of the answer.
func (e *testEnv) startJob(t *testing.T, chatID, path, body string) string {
	t.Helper()
	res := e.do(t, chatID, http.MethodPost, platform.AnnotationPrefix+path, body)
	if res.Status != "ok" {
		t.Fatalf("%s: %+v", path, res)
	}
	var a jobAnswer
	if err := json.Unmarshal([]byte(res.Body), &a); err != nil || a.JobID == "" {
		t.Fatalf("%s: answer %s (%v)", path, res.Body, err)
	}
	if a.Steps == nil {
		t.Errorf("%s: the answer carries no steps", path)
	}
	return a.JobID
}

// jobResult reads the result of a finished job out of the answer of annotation_job.
func (e *testEnv) jobResult(t *testing.T, chatID, jobID string, v any) {
	t.Helper()
	res := e.do(t, chatID, http.MethodGet, platform.AnnotationPrefix+"/jobs/"+jobID, "")
	if res.Status != "ok" {
		t.Fatalf("annotation_job: %+v", res)
	}
	var a jobAnswer
	if err := json.Unmarshal([]byte(res.Body), &a); err != nil {
		t.Fatal(err)
	}
	if a.State != store.AnnotationJobFinished {
		t.Fatalf("job %s is %s: %s", jobID, a.State, res.Body)
	}
	if err := json.Unmarshal(a.Result, v); err != nil {
		t.Fatalf("result %s: %v", a.Result, err)
	}
}

// manualTags is a task annotation with manual tags: the map names the label CVAT gave the class of each
// frame (the task of the tests carries the classes 0 and 1 as the label ids 1 and 2).
func manualTags(byClass map[int64][]int) string {
	tags := []map[string]any{}
	for id, frames := range byClass {
		for _, f := range frames {
			tags = append(tags, map[string]any{"frame": f, "label_id": id, "source": "manual", "attributes": []any{}})
		}
	}
	b, _ := json.Marshal(map[string]any{"version": 0, "tags": tags, "shapes": []any{}, "tracks": []any{}})
	return string(b)
}

// A check without a mapping needs the classes of the function: a function that answers no raw_label and
// no mapping given ends as failed — there is nothing the calibration could hold on.
func TestCalibrateNeedsMappingWithoutRawLabel(t *testing.T) {
	e := v3Env(t, 30)
	chatID := e.chat(t)
	e.fc.setAnnotations(7, manualTags(map[int64][]int{1: {0, 1, 2, 3, 4}}))
	e.fc.setAnswers(map[int]string{0: `[{"label":"0","type":"tag","confidence":"0.90"}]`})

	jobID := e.startJob(t, chatID, "/checks", `{"dataset_id":5,"function":"agw-test-m1-abcd1234","mode":"calibrate"}`)
	row := e.waitJob(t, chatID, jobID)
	if row.State != store.AnnotationJobFailed || !strings.Contains(row.Error, "raw_label") {
		t.Fatalf("job: %+v", row)
	}
	if !strings.Contains(row.Error, "give mapping") {
		t.Errorf("the error does not say what to do: %q", row.Error)
	}
	// It stopped at the first answered image instead of measuring on.
	if asked := e.fc.askedFrames(); len(asked) != 1 {
		t.Errorf("frames asked: %v", asked)
	}
}

// A calibration measures the manually annotated images of the task, proposes the mapping and the
// threshold and names the sample it stands on.
func TestCheckCalibrateJob(t *testing.T) {
	e := v3Env(t, 30)
	chatID := e.chat(t)
	// Ten images with a manual tag: the first eight annotated "0", the last two "1".
	e.fc.setAnnotations(7, manualTags(map[int64][]int{1: {0, 1, 2, 3, 4, 5, 6, 7}, 2: {8, 9}}))
	answers := map[int]string{}
	for i := 0; i < 8; i++ {
		answers[i] = `[{"label":"0","raw_label":"class_0","type":"tag","confidence":"0.9000","scores":[0.9,0.1]}]`
	}
	answers[8] = `[{"label":"0","raw_label":"class_0","type":"tag","confidence":"0.2000","scores":[0.2,0.8]}]`
	answers[9] = `[{"label":"0","raw_label":"class_0","type":"tag","confidence":"0.2000","scores":[0.2,0.8]}]`
	e.fc.setAnswers(answers)

	jobID := e.startJob(t, chatID, "/checks", `{"dataset_id":5,"function":"agw-test-m1-abcd1234","mode":"calibrate"}`)
	row := e.waitJob(t, chatID, jobID)
	if row.State != store.AnnotationJobFinished {
		t.Fatalf("job: %+v", row)
	}
	var got Calibration
	e.jobResult(t, chatID, jobID, &got)
	if got.Images != 10 || got.Mapping["class_0"] != "0" {
		t.Fatalf("calibration: %+v", got)
	}
	if got.ProposedMinConfidence != 25 || got.Accuracy == nil || *got.Accuracy != 1 || got.Coverage != 0.8 {
		t.Errorf("figures: %+v", got)
	}
	if !strings.Contains(got.Note, "Measured on 10") || !strings.Contains(got.Note, "confirm it") {
		t.Errorf("note: %q", got.Note)
	}
	// Every call carries the low threshold and the mapping that keeps CVAT from dropping classes.
	if len(e.fc.funcBodies) != 10 {
		t.Fatalf("calls: %d", len(e.fc.funcBodies))
	}
	for _, b := range e.fc.funcBodies {
		if !strings.Contains(b, `"threshold":0.01`) {
			t.Errorf("the call does not use the low threshold: %s", b)
		}
		if !strings.Contains(b, `"class_1":{"name":"0"}`) {
			t.Errorf("the call does not map every class of the function: %s", b)
		}
	}
}

// An uncertainty names the weak images with their file and a link into the CVAT job of the frame; it
// checks every image of the task, not only the annotated ones.
func TestCheckUncertainLinks(t *testing.T) {
	e := v3Env(t, 4)
	chatID := e.chat(t)
	e.fc.setAnswers(map[int]string{
		0: `[{"label":"0","raw_label":"class_0","type":"tag","confidence":"0.9000"}]`,
		1: `[{"label":"0","raw_label":"class_0","type":"tag","confidence":"0.2000"}]`,
		2: `[]`,
		3: `[{"label":"0","raw_label":"class_0","type":"tag","confidence":"0.5000"}]`,
	})
	jobID := e.startJob(t, chatID, "/checks",
		`{"dataset_id":5,"function":"agw-test-m1-abcd1234","mode":"uncertain","uncertain_below":60}`)
	e.waitJob(t, chatID, jobID)
	var got uncertainAnswer
	e.jobResult(t, chatID, jobID, &got)
	if got.Checked != 4 || len(got.Uncertain) != 3 {
		t.Fatalf("uncertain: %+v", got)
	}
	if got.Uncertain[0].Frame != 2 || got.Uncertain[0].Confidence != 0 {
		t.Errorf("the image without an answer comes first: %+v", got.Uncertain[0])
	}
	for _, item := range got.Uncertain {
		if item.File != "img.png" {
			t.Errorf("file of frame %d: %q", item.Frame, item.File)
		}
		want := "https://cvat.example/tasks/7/jobs/11?frame=" + strconv.Itoa(item.Frame)
		if item.CVATURL != want {
			t.Errorf("link of frame %d: %q, want %q", item.Frame, item.CVATURL, want)
		}
	}
}

// The state of a job of another chat is not found (E8), and CVAT is not asked for it.
func TestJobOtherChat(t *testing.T) {
	e := v3Env(t, 4)
	chatID := e.chat(t)
	other := e.chat(t)
	row, err := e.st.AddAnnotationJob(context.Background(), store.AnnotationJob{ChatID: other,
		Kind: JobCalibrate, DatasetID: 5, TaskID: 7, Function: "f"})
	if err != nil {
		t.Fatal(err)
	}
	res := e.do(t, chatID, http.MethodGet, platform.AnnotationPrefix+"/jobs/"+row.ID, "")
	if res.Status != "error" || !strings.Contains(res.Message, "unknown job") {
		t.Fatalf("foreign job: %+v", res)
	}
}

// The service holds one slot for its background jobs: a second job waits for the running one and runs
// as soon as it is free (V3).
func TestOneJobAtATime(t *testing.T) {
	e := v3Env(t, 4)
	chatID := e.chat(t)
	e.fc.setAnswers(map[int]string{0: `[{"label":"0","raw_label":"class_0","type":"tag","confidence":"0.9000"}]`,
		1: `[{"label":"0","raw_label":"class_0","type":"tag","confidence":"0.9000"}]`,
		2: `[{"label":"0","raw_label":"class_0","type":"tag","confidence":"0.9000"}]`,
		3: `[{"label":"0","raw_label":"class_0","type":"tag","confidence":"0.9000"}]`})
	// The slot is taken, as a running job takes it.
	e.svc.sem <- struct{}{}
	jobID := e.startJob(t, chatID, "/checks",
		`{"dataset_id":5,"function":"agw-test-m1-abcd1234","mode":"uncertain","limit":2}`)
	// The job waits: it is laid down as queued and does not ask the function for an image.
	time.Sleep(100 * time.Millisecond)
	row, err := e.st.AnnotationJob(context.Background(), chatID, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != store.AnnotationJobQueued {
		t.Fatalf("a job ran although the slot was taken: %+v", row)
	}
	if asked := e.fc.askedFrames(); len(asked) != 0 {
		t.Fatalf("a waiting job asked the function: %v", asked)
	}
	<-e.svc.sem
	row = e.waitJob(t, chatID, jobID)
	if row.State != store.AnnotationJobFinished {
		t.Fatalf("the job did not run after the slot was free: %+v", row)
	}
	if asked := e.fc.askedFrames(); len(asked) != 2 {
		t.Errorf("frames asked: %v", asked)
	}
}

// The observer kind of a job reads the row of the job and reports its progress; the route of a started
// job is the one the tool uses.
func TestAnnotationJobWatchKind(t *testing.T) {
	e := v3Env(t, 4)
	chatID := e.chat(t)
	row, err := e.st.AddAnnotationJob(context.Background(), store.AnnotationJob{ChatID: chatID,
		Kind: JobCalibrate, DatasetID: 5, TaskID: 7, Function: "f"})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.st.SetAnnotationJobProgress(context.Background(), chatID, row.ID, 5, 10); err != nil {
		t.Fatal(err)
	}
	kind := JobWatch(e.st)
	if kind.Name != JobKindName || kind.Poll == nil || kind.Stop != nil {
		t.Fatalf("kind: %+v", kind)
	}
	if !JobRoutePath().MatchString(platform.AnnotationPrefix+"/checks") || !JobRoutePath().MatchString(platform.AnnotationPrefix+"/fills") {
		t.Errorf("the route of a job does not match the calls that start one: %s", JobRoutePath())
	}
	if JobRoutePath().MatchString(platform.AnnotationPrefix + "/jobs/" + row.ID) {
		t.Error("the route of a job matches the call that reads one")
	}
	// A queued or running job runs on; the line names the kind and how far it is.
	p, err := kind.Poll(context.Background(), watchJob(chatID, row.ID), nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.State != "running" || p.Percent == nil || *p.Percent != 50 || !strings.Contains(p.Message, "5 of 10") {
		t.Fatalf("running: %+v (%v)", p, p.Percent)
	}
	if err := e.st.FinishAnnotationJob(context.Background(), chatID, row.ID, store.AnnotationJobFinished, `{"images":1}`, ""); err != nil {
		t.Fatal(err)
	}
	if p, err = kind.Poll(context.Background(), watchJob(chatID, row.ID), nil); err != nil {
		t.Fatal(err)
	}
	if p.State != "done" || p.Percent == nil || *p.Percent != 100 {
		t.Fatalf("finished: %+v", p)
	}
	// A job that failed names its error, and a job that is gone is reported as failed.
	if err := e.st.FinishAnnotationJob(context.Background(), chatID, row.ID, store.AnnotationJobFailed, "", "the platform answered HTTP 500"); err != nil {
		t.Fatal(err)
	}
	if p, err = kind.Poll(context.Background(), watchJob(chatID, row.ID), nil); err != nil {
		t.Fatal(err)
	}
	if p.State != "failed" || !strings.Contains(p.Message, "HTTP 500") {
		t.Fatalf("failed: %+v", p)
	}
	if p, err = kind.Poll(context.Background(), watchJob(chatID, "11111111-2222-3333-4444-555555555555"), nil); err != nil {
		t.Fatal(err)
	}
	if p.State != "failed" {
		t.Fatalf("a job that is gone: %+v", p)
	}
	// The reference of an observed job is the job id of the answer.
	res := platform.Result{Status: "ok", Body: `{"job_id":"` + row.ID + `","kind":"fill"}`}
	ref, ok := JobRef(platform.Request{}, res)
	if !ok || ref != row.ID {
		t.Errorf("ref: %q %v", ref, ok)
	}
	if _, ok := JobRef(platform.Request{}, platform.Result{Status: "error", Message: "no"}); ok {
		t.Error("a failed call became a job")
	}
}

// watchJob is the row internal/watch hands to a kind.
func watchJob(chatID, ref string) watch.Job {
	return watch.Job{ID: "w1", ChatID: chatID, Kind: JobKindName, Ref: ref}
}

// The rounding of the figures keeps the answers readable and never turns a number into something else.
func TestRoundFigures(t *testing.T) {
	if got := round(2.0 / 3.0); got != 0.6667 {
		t.Errorf("round: %v", got)
	}
	if got := round(math.NaN()); got != 0 {
		t.Errorf("NaN: %v", got)
	}
	if got := round(math.Inf(1)); got != 0 {
		t.Errorf("infinite: %v", got)
	}
}

// The name of a class of an annotation: the id CVAT gave it, or its name when the task is known.
func TestClassName(t *testing.T) {
	classes := map[int64]string{1: "0", 2: "1"}
	if got := className(1, classes); got != "0" {
		t.Errorf("known: %q", got)
	}
	if got := className(9, classes); got != "#9" {
		t.Errorf("unknown: %q", got)
	}
}

// The frames of a check are the ones with a manual annotation (calibrate) or every frame (uncertain).
func TestCheckFrames(t *testing.T) {
	manual := map[int][]ManualObject{3: {{Class: "0"}}, 7: {{Class: "0"}}, 11: {{Class: "0"}}}
	cal := jobSpec{Kind: JobCalibrate, Limit: 2}
	if got := checkFrames(cal, 20, manual); len(got) != 2 || got[0] != 3 || got[1] != 11 {
		t.Errorf("calibrate: %v", got)
	}
	un := jobSpec{Kind: JobUncertain, Limit: 2}
	if got := checkFrames(un, 20, manual); len(got) != 2 || got[0] != 0 || got[1] != 10 {
		t.Errorf("uncertain: %v", got)
	}
}

// The link of an image points at the job that carries the frame; without one it names the frame of the
// task.
func TestImageURL(t *testing.T) {
	jobs := []Job{{ID: 11, StartFrame: 0, StopFrame: 99}}
	if got := imageURL("https://cvat.example/tasks/7", jobs, 5); got != "https://cvat.example/tasks/7/jobs/11?frame=5" {
		t.Errorf("with a job: %q", got)
	}
	if got := imageURL("https://cvat.example/tasks/7", nil, 5); got != "https://cvat.example/tasks/7?frame=5" {
		t.Errorf("without a job: %q", got)
	}
}

// hasFrame reports whether a body of a write carries exactly this frame (not a longer number).
func hasFrame(body string, frame int) bool {
	return regexp.MustCompile(`"frame":` + strconv.Itoa(frame) + `([^0-9]|$)`).MatchString(body)
}

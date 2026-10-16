// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package annotation

import (
	"strings"
	"sync/atomic"
	"testing"

	"agw/internal/store"
)

// tagAnswer is what a classification function answers: one tag with the class "0" of the task.
const tagAnswer = `[{"label":"0","type":"tag","confidence":"0.9000","raw_label":"class_0"}]`

// fillEnv prepares a fill of the dataset 5: the task 7 carries the classes 0 and 1, and the function
// answers a tag for every frame it is asked about.
func fillEnv(t *testing.T, size int, answers map[int]string) *testEnv {
	t.Helper()
	e := v3Env(t, size)
	if answers == nil {
		answers = map[int]string{}
		for i := 0; i < size; i++ {
			answers[i] = tagAnswer
		}
	}
	e.fc.setAnswers(answers)
	return e
}

// A fill writes only into the images without an annotation, and it leaves an image alone that gains an
// annotation while the job runs (E12): a machine object next to a manual one cannot be told apart
// afterwards.
func TestFillOnlyEmpty(t *testing.T) {
	e := fillEnv(t, 30, nil)
	chatID := e.chat(t)
	e.fc.setAnnotations(7, manualTags(map[int64][]int{1: {0, 5, 10}}))
	// The user annotates frame 1 in CVAT while the job runs: the second read of the annotations — the
	// one right before the first write — sees it.
	var reads atomic.Int32
	e.fc.setOnAnnotationsRead(func(cur string) string {
		if reads.Add(1) == 2 {
			return manualTags(map[int64][]int{1: {0, 1, 5, 10}})
		}
		return cur
	})

	jobID := e.startJob(t, chatID, "/fills",
		`{"dataset_id":5,"function":"agw-test-m1-abcd1234","mapping":{"class_0":"0"}}`)
	row := e.waitJob(t, chatID, jobID)
	if row.State != store.AnnotationJobFinished {
		t.Fatalf("job: %+v", row)
	}
	var got fillAnswer
	e.jobResult(t, chatID, jobID, &got)
	if got.Filled != 26 || got.SkippedAnnotated != 4 || got.EmptyResults != 0 {
		t.Fatalf("result: %+v", got)
	}
	// The annotated images were never asked about (27 empty frames of 30).
	asked := e.fc.askedFrames()
	if len(asked) != 27 {
		t.Fatalf("frames asked: %d (%v)", len(asked), asked)
	}
	for _, f := range asked {
		if f == 0 || f == 5 || f == 10 {
			t.Errorf("the function was asked about the annotated frame %d", f)
		}
	}
	bodies := e.fc.writeBodies()
	if len(bodies) != 2 {
		t.Fatalf("writes: %d", len(bodies))
	}
	all := strings.Join(bodies, "\n")
	for _, f := range []int{0, 1, 5, 10} {
		if hasFrame(all, f) {
			t.Errorf("frame %d was written: %s", f, all)
		}
	}
	for _, f := range []int{2, 26, 29} {
		if !hasFrame(all, f) {
			t.Errorf("frame %d is missing: %s", f, all)
		}
	}
	if n := strings.Count(all, `"label_id"`); n != 26 {
		t.Errorf("annotations written: %d, want 26", n)
	}
	if !strings.Contains(all, `"source":"auto"`) {
		t.Errorf("the annotations do not carry source auto: %s", all)
	}
	if !strings.Contains(all, `"version":0`) {
		t.Errorf("the write carries no version: %s", all)
	}
}

// What a function answers is checked before it goes into the task: a class the task does not know, a box
// outside the image and a number no JSON carries are not written (V3).
func TestFillValidatesResult(t *testing.T) {
	e := fillEnv(t, 4, map[int]string{
		0: tagAnswer,
		// A box far outside the image of 100x100 pixels.
		1: `[{"label":"0","type":"rectangle","points":[200,200,300,300],"confidence":"0.9000"}]`,
		// A class the task does not carry (no mapping leads from it either).
		2: `[{"label":"cow","type":"rectangle","points":[0,0,10,10],"confidence":"0.9000"}]`,
		// A NaN in the points: Python's json writes it, and no JSON carries it (the answer is refused).
		3: `[{"label":"0","type":"tag","points":[NaN],"confidence":"0.9000"}]`,
	})
	chatID := e.chat(t)
	jobID := e.startJob(t, chatID, "/fills",
		`{"dataset_id":5,"function":"agw-test-m1-abcd1234","min_confidence":50,"mapping":{"class_0":"0"}}`)
	row := e.waitJob(t, chatID, jobID)
	if row.State != store.AnnotationJobFinished {
		t.Fatalf("job: %+v", row)
	}
	var got fillAnswer
	e.jobResult(t, chatID, jobID, &got)
	if got.Filled != 1 || got.EmptyResults != 3 || got.SkippedAnnotated != 0 {
		t.Fatalf("result: %+v", got)
	}
	all := strings.Join(e.fc.writeBodies(), "\n")
	if !hasFrame(all, 0) {
		t.Errorf("the valid answer was not written: %s", all)
	}
	for _, f := range []int{1, 2, 3} {
		if hasFrame(all, f) {
			t.Errorf("frame %d was written although its answer is unusable: %s", f, all)
		}
	}
	if n := strings.Count(all, `"label_id"`); n != 1 {
		t.Errorf("annotations written: %d, want 1", n)
	}
	if !strings.Contains(all, `"tags"`) {
		t.Errorf("a classification is written as a tag: %s", all)
	}
}

// The mapping of a fill is checked before the job starts: a function label that does not exist and a
// dataset class that does not exist are refused, and nothing is written.
func TestFillMappingRefused(t *testing.T) {
	e := fillEnv(t, 4, nil)
	chatID := e.chat(t)
	for _, body := range []string{
		`{"dataset_id":5,"function":"agw-test-m1-abcd1234","mapping":{"cow":"0"}}`,
		`{"dataset_id":5,"function":"agw-test-m1-abcd1234","mapping":{"class_0":"Rind"}}`,
	} {
		res := e.do(t, chatID, "POST", "/_agw/annotation/fills", body)
		if res.Status != "error" {
			t.Errorf("%s: %+v", body, res)
		}
	}
	// Without a mapping only equal names are used, and here none matches.
	res := e.do(t, chatID, "POST", "/_agw/annotation/fills", `{"dataset_id":5,"function":"agw-test-m1-abcd1234"}`)
	if res.Status != "error" || !strings.Contains(res.Message, "no function label matches a dataset class") {
		t.Fatalf("without a mapping: %+v", res)
	}
	if len(e.fc.writeBodies()) != 0 {
		t.Errorf("something was written: %v", e.fc.writeBodies())
	}
}

// A fill without an image to fill says so and writes nothing.
func TestFillNothingToDo(t *testing.T) {
	e := fillEnv(t, 3, nil)
	chatID := e.chat(t)
	// Every frame of the task carries an annotation.
	e.fc.setAnnotations(7, manualTags(map[int64][]int{1: {0, 1, 2}}))
	jobID := e.startJob(t, chatID, "/fills",
		`{"dataset_id":5,"function":"agw-test-m1-abcd1234","mapping":{"class_0":"0"}}`)
	row := e.waitJob(t, chatID, jobID)
	if row.State != store.AnnotationJobFinished {
		t.Fatalf("job: %+v", row)
	}
	var got fillAnswer
	e.jobResult(t, chatID, jobID, &got)
	if got.Filled != 0 || got.SkippedAnnotated != 3 || got.EmptyResults != 0 {
		t.Fatalf("result: %+v", got)
	}
	if len(e.fc.askedFrames()) != 0 || len(e.fc.writeBodies()) != 0 {
		t.Errorf("the function was asked or something written: %v %v", e.fc.askedFrames(), e.fc.writeBodies())
	}
}

// The limits of a call are checked before a job exists, so that a call that cannot work does not become
// a background job.
func TestJobLimits(t *testing.T) {
	e := fillEnv(t, 4, nil)
	chatID := e.chat(t)
	for _, tc := range []struct{ path, body, want string }{
		{"/checks", `{"dataset_id":5,"function":"f","mode":"drop"}`, "mode must be"},
		{"/checks", `{"dataset_id":5,"function":"f","mode":"calibrate","limit":1001}`, "limit must be"},
		{"/checks", `{"dataset_id":5,"function":"f","mode":"calibrate","limit":0}`, "limit must be"},
		{"/checks", `{"dataset_id":5,"function":"f","mode":"calibrate","uncertain_below":0}`, "uncertain_below must be"},
		{"/checks", `{"dataset_id":5,"function":"f","mode":"calibrate","uncertain_below":100}`, "uncertain_below must be"},
		{"/fills", `{"dataset_id":5,"function":"f","min_confidence":0}`, "min_confidence must be"},
		{"/fills", `{"dataset_id":5,"function":"f","min_confidence":100}`, "min_confidence must be"},
		{"/fills", `{"function":"f"}`, "dataset_id missing"},
		{"/checks", `{"dataset_id":5}`, "function missing"},
	} {
		res := e.do(t, chatID, "POST", "/_agw/annotation"+tc.path, tc.body)
		if res.Status != "error" || !strings.Contains(res.Message, tc.want) {
			t.Errorf("%s %s: %+v", tc.path, tc.body, res)
		}
	}
	// A dataset of another user is refused before anything runs.
	e.pf.setDataset(datasetJSON(5, "other", "AgriImageDataResource", []string{"0"}, 7))
	res := e.do(t, chatID, "POST", "/_agw/annotation/checks", `{"dataset_id":5,"function":"f","mode":"calibrate"}`)
	if res.Status != "denied" {
		t.Errorf("foreign dataset: %+v", res)
	}
	// The answer of annotation_job names an unknown job and asks no CVAT.
	res = e.do(t, chatID, "GET", "/_agw/annotation/jobs/11111111-2222-3333-4444-555555555555", "")
	if res.Status != "error" || !strings.Contains(res.Message, "unknown job") {
		t.Errorf("unknown job: %+v", res)
	}
	// A job id that is not a uuid is refused by the path.
	res = e.do(t, chatID, "GET", "/_agw/annotation/jobs/not-a-job", "")
	if res.Status != "error" || !strings.Contains(res.Message, "unknown annotation call") {
		t.Errorf("bad id: %+v", res)
	}
}

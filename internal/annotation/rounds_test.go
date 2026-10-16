// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package annotation

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"agw/internal/assess"
	"agw/internal/platform"
	"agw/internal/store"
)

// The correction loop of V3c: a fill or a run of a task after its first one in the same chat is a round,
// the gateway counts them and refuses a round that the loop does not allow.

// fillBody is the call of a fill on the dataset of the tests, with the mapping of the function of
// filledTask.
const fillBody = `{"dataset_id":5,"function":"agw-test-m1-abcd1234","min_confidence":70,"mapping":{"class_0":"0"}}`

// roundsOf reads the counters of the task of the tests in a chat.
func roundsOf(t *testing.T, e *testEnv, chatID string) store.AnnotationTaskCounters {
	t.Helper()
	c, err := e.st.AnnotationTaskCountersOf(context.Background(), chatID, 7)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// correctionFill starts one correction round and waits for it.
func correctionFill(t *testing.T, e *testEnv, chatID string) string {
	t.Helper()
	jobID := e.startJob(t, chatID, "/fills", fillBody)
	if row := e.waitJob(t, chatID, jobID); row.State != store.AnnotationJobFinished {
		t.Fatalf("the correction round did not run: %+v", row)
	}
	return jobID
}

// storeRound writes a finished interim verification of the task of the tests into the store, as the job
// would leave it: the figure the correction loop compares its rounds with (V3c).
func storeRound(t *testing.T, e *testEnv, chatID, sourceID string, round int, score float64) {
	t.Helper()
	ctx := context.Background()
	params, _ := json.Marshal(jobSpec{Kind: JobVerify, SourceID: sourceID})
	row, err := e.st.AddAnnotationJob(ctx, store.AnnotationJob{ChatID: chatID, Kind: JobVerify, DatasetID: 5,
		TaskID: 7, Function: "agw-test-m1-abcd1234", Params: string(params), State: store.AnnotationJobRunning})
	if err != nil {
		t.Fatal(err)
	}
	figure := score
	raw, _ := json.Marshal(verifyAnswer{SourceID: sourceID, Round: round,
		Report:  assess.Report{Verdict: assess.Questionable, Set: assess.SetValidation, Measured: 5},
		Overall: Overall{Accuracy: &figure}})
	if err := e.st.FinishAnnotationJob(ctx, chatID, row.ID, store.AnnotationJobFinished, string(raw), ""); err != nil {
		t.Fatal(err)
	}
}

// A task may have three correction rounds in one chat; the initial pre-annotation is no round, and the
// fourth correction is refused before a job exists (V3c).
func TestRoundLimit(t *testing.T) {
	e, chatID, _ := filledTask(t)
	// The fill of filledTask is the initial pre-annotation of the task and is not counted.
	if c := roundsOf(t, e, chatID); c.Rounds != 0 {
		t.Fatalf("the initial pre-annotation counts as a round: %+v", c)
	}
	for round := 1; round <= MaxRounds; round++ {
		correctionFill(t, e, chatID)
		if c := roundsOf(t, e, chatID); c.Rounds != round {
			t.Fatalf("after round %d the counter is %d", round, c.Rounds)
		}
	}
	res := e.do(t, chatID, http.MethodPost, platform.AnnotationPrefix+"/fills", fillBody)
	if res.Status != "denied" || !strings.Contains(res.Message, "at most 3 correction rounds") {
		t.Fatalf("a fourth round is not refused: %+v", res)
	}
	// The refused call started no job: the fills of this chat stand at four, the initial one and three
	// rounds.
	if jobs := jobsOf(t, e, chatID); jobs != 1+MaxRounds {
		t.Errorf("jobs of this chat: %d, want %d", jobs, 1+MaxRounds)
	}
}

// jobsOf counts the fill jobs of a chat.
func jobsOf(t *testing.T, e *testEnv, chatID string) int {
	t.Helper()
	rows, err := e.st.AnnotationJobs(context.Background(), chatID)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, r := range rows {
		if r.Kind == JobFill {
			n++
		}
	}
	return n
}

// The rounds of a task stand per chat and task (V3c): a second chat of the same task begins with its own
// initial pre-annotation, and its rounds are counted apart.
func TestRoundsPerChatAndTask(t *testing.T) {
	e, chatID, _ := filledTask(t)
	other := e.chat(t)
	correctionFill(t, e, chatID)
	if c := roundsOf(t, e, chatID); c.Rounds != 1 {
		t.Fatalf("counters of the first chat: %+v", c)
	}
	if c := roundsOf(t, e, other); c.Rounds != 0 {
		t.Fatalf("the round of another chat was counted here: %+v", c)
	}
	// The first fill of the second chat is its initial pre-annotation, no round.
	correctionFill(t, e, other)
	if c := roundsOf(t, e, other); c.Rounds != 0 {
		t.Fatalf("the initial pre-annotation of a chat counts as a round: %+v", c)
	}
	// Its first correction is a round of its own.
	correctionFill(t, e, other)
	if c := roundsOf(t, e, other); c.Rounds != 1 {
		t.Fatalf("counters of the second chat: %+v", c)
	}
	if c := roundsOf(t, e, chatID); c.Rounds != 1 {
		t.Errorf("the counters of the first chat moved: %+v", c)
	}
}

// A round without enough validation images to compare has nothing to measure itself against and is
// refused with the number the loop needs (V3c).
func TestTooFewValidationImages(t *testing.T) {
	// Ten manually annotated images: the split puts the third and the eighth into the validation set.
	e, chatID, _ := partialTask(t, 10)
	before := roundsOf(t, e, chatID).Rounds
	res := e.do(t, chatID, http.MethodPost, platform.AnnotationPrefix+"/fills", fillBody)
	want := "too few validation images (2 of at least 5); ask the user to annotate more images (round needs new manual data)"
	if res.Status != "denied" || res.Message != want {
		t.Fatalf("refusal: %+v, want %q", res, want)
	}
	if after := roundsOf(t, e, chatID).Rounds; after != before {
		t.Errorf("a refused round was counted: %d -> %d", before, after)
	}
}

// partialTask prepares a chat like filledTask, with the named number of manually annotated images and a
// finished fill over the empty ones.
func partialTask(t *testing.T, manual int) (*testEnv, string, string) {
	t.Helper()
	e := newTestEnv(t)
	chatID := e.chat(t)
	e.pf.setDataset(datasetJSON(5, "test", "AgriImageDataResource", []string{"0", "1"}, 7))
	e.fc.setTask(7, taskJSON(7, "test", 30, labelsJSON("0", "1")))
	e.fc.setFunctions(`[{"id":"agw-test-m1-abcd1234","name":"mine","kind":"detector","labels":["class_0","class_1"]}]`)
	e.fc.setMeta(`{"size":30,"width":100,"height":100,"name":"img.png"}`)
	e.fc.setJobs(`{"count":1,"results":[{"id":11,"start_frame":0,"stop_frame":29}]}`)
	tags := []Tag{}
	for i := 0; i < manual; i++ {
		tags = append(tags, manualTag(int64(i+1), i))
	}
	e.fc.setAnnotations(7, annotationsWith(tags, nil))
	answer := `[{"label":"0","raw_label":"class_0","type":"tag","confidence":"0.9000","scores":[0.9,0.1]}]`
	answers := map[int]string{}
	for i := 0; i < 30; i++ {
		answers[i] = answer
	}
	e.fc.setAnswers(answers)
	fillID := e.startJob(t, chatID, "/fills", fillBody)
	if row := e.waitJob(t, chatID, fillID); row.State != store.AnnotationJobFinished {
		t.Fatalf("fill: %+v", row)
	}
	return e, chatID, fillID
}

// After a final verdict the held-out images are used, and no round may follow: another round would be
// chosen by the outcome (V3c, E22).
func TestNoRoundAfterFinal(t *testing.T) {
	e, chatID, fillID := filledTask(t)
	if row := e.verifyOf(t, chatID, fillID, true); row.State != store.AnnotationJobFinished {
		t.Fatalf("final verify: %+v", row)
	}
	res := e.do(t, chatID, http.MethodPost, platform.AnnotationPrefix+"/fills", fillBody)
	if res.Status != "denied" || !strings.Contains(res.Message, "no correction round after the final verdict") {
		t.Fatalf("a round after the final verdict is not refused: %+v", res)
	}
	// The verdict may be re-measured with final=false: the interim figure is what a report may name.
	if row := e.verifyOf(t, chatID, fillID, false); row.State != store.AnnotationJobFinished {
		t.Fatalf("interim verify after the final one: %+v", row)
	}
}

// A round has to earn itself: below the smallest improvement that is more than noise the loop stops, at
// the threshold it goes on (V3c, the rule of R0.8).
func TestStopWithoutImprovement(t *testing.T) {
	// The rule itself, without CVAT and without a function.
	state := CorrectionState{Validation: 5,
		Prev: &RoundScore{Round: 1, Score: 0.50}, Cur: &RoundScore{Round: 2, Score: 0.519}}
	if msg := RoundRefusal(state); msg == "" || !strings.Contains(msg, "no improvement on the validation images") {
		t.Errorf("+0.019 continues: %q", msg)
	} else if !strings.Contains(msg, "round 2") || !strings.Contains(msg, "(0.50 -> 0.52)") {
		t.Errorf("the refusal does not name the round and the figures: %q", msg)
	}
	state.Cur.Score = 0.52
	if msg := RoundRefusal(state); msg != "" {
		t.Errorf("+0.02 stops: %q", msg)
	}

	// The same on the service: two interim figures of the task, then the fill of the next round.
	e, chatID, fillID := filledTask(t)
	storeRound(t, e, chatID, fillID, 0, 0.50)
	// The two rows are ordered by their creation time, so they have to be apart in time.
	time.Sleep(5 * time.Millisecond)
	storeRound(t, e, chatID, "job-round-1", 1, 0.519)
	res := e.do(t, chatID, http.MethodPost, platform.AnnotationPrefix+"/fills", fillBody)
	if res.Status != "denied" || !strings.Contains(res.Message, "no improvement on the validation images in round 1 (0.50 -> 0.52); stop and report") {
		t.Fatalf("the round after a round without an improvement is not refused: %+v", res)
	}

	// With an improvement of 0.02 the round runs.
	e2, chat2, fill2 := filledTask(t)
	storeRound(t, e2, chat2, fill2, 0, 0.50)
	time.Sleep(5 * time.Millisecond)
	storeRound(t, e2, chat2, "job-round-1", 1, 0.52)
	correctionFill(t, e2, chat2)
	if c := roundsOf(t, e2, chat2); c.Rounds != 1 {
		t.Errorf("the round that improved was not counted: %+v", c)
	}
}

// A retraining is counted while the loop of a task of the chat runs, and only one per task (V3c).
func TestRetrainLimit(t *testing.T) {
	e, chatID, _ := filledTask(t)
	e.pf.setModel(5, modelOf(5, nil))
	e.pf.setModelFile("ONNX-MODEL")
	e.pf.setTaskLocation("/tasks/7")
	// Before any correction round there is no loop and no retraining: building a function is the normal
	// way of V2.
	if res := e.do(t, chatID, http.MethodPost, platform.AnnotationPrefix+"/functions", `{"model_id":5,"kind":"classification"}`); res.Status != "ok" {
		t.Fatalf("building a function outside the loop: %+v", res)
	}
	if c := roundsOf(t, e, chatID); c.Retrains != 0 {
		t.Fatalf("a build without a loop counted a retraining: %+v", c)
	}
	correctionFill(t, e, chatID)
	// The loop runs: the next build is the one retraining the task may take.
	e.pf.setModel(6, modelOf(6, nil))
	e.pf.setModelFile("ONNX-MODEL-2")
	if res := e.do(t, chatID, http.MethodPost, platform.AnnotationPrefix+"/functions", `{"model_id":6,"kind":"classification"}`); res.Status != "ok" {
		t.Fatalf("the first retraining of the loop: %+v", res)
	}
	if c := roundsOf(t, e, chatID); c.Retrains != 1 {
		t.Fatalf("the retraining was not counted: %+v", c)
	}
	e.pf.setModel(7, modelOf(7, nil))
	e.pf.setModelFile("ONNX-MODEL-3")
	res := e.do(t, chatID, http.MethodPost, platform.AnnotationPrefix+"/functions", `{"model_id":7,"kind":"classification"}`)
	if res.Status != "denied" || !strings.Contains(res.Message, "at most 1 retraining per task in a chat") {
		t.Fatalf("the second retraining is not refused: %+v", res)
	}
	// A build of the same model again is the same function and no retraining at all.
	e.pf.setModelFile("ONNX-MODEL-2")
	if res := e.do(t, chatID, http.MethodPost, platform.AnnotationPrefix+"/functions", `{"model_id":6,"kind":"classification"}`); res.Status != "ok" {
		t.Errorf("the same model was refused: %+v", res)
	}
}

// The rules of the loop are pure functions of what the store reports; the order of the reasons is the
// order of the report: the held-out images first, then the comparison, then the data, then the counter.
func TestRoundRefusalOrder(t *testing.T) {
	base := CorrectionState{Validation: 5, Rounds: MaxRounds,
		Prev: &RoundScore{Round: 2, Score: 0.9}, Cur: &RoundScore{Round: 3, Score: 0.5}}
	if msg := RoundRefusal(base); !strings.Contains(msg, "no improvement") {
		t.Errorf("the comparison does not come first: %q", msg)
	}
	base.Prev = nil
	if msg := RoundRefusal(base); !strings.Contains(msg, "at most 3 correction rounds") {
		t.Errorf("the counter is not named: %q", msg)
	}
	base.Final = true
	if msg := RoundRefusal(base); !strings.Contains(msg, "no correction round after the final verdict") {
		t.Errorf("the final verdict does not come first: %q", msg)
	}
	// Fewer validation images than the minimum, but more than the last measurement rested on: the user
	// annotated by hand since, and the round runs on new data.
	base = CorrectionState{Validation: 3, ValidationBefore: 2, Rounds: 1}
	if msg := RoundRefusal(base); msg != "" {
		t.Errorf("new manual images do not allow the round: %q", msg)
	}
}

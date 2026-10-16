// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package annotation

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"agw/internal/assess"
	"agw/internal/platform"
	"agw/internal/store"
)

// annotationsWith builds the answer of GET /api/tasks/{id}/annotations out of tags and shapes.
func annotationsWith(tags []Tag, shapes []Shape) string {
	if tags == nil {
		tags = []Tag{}
	}
	if shapes == nil {
		shapes = []Shape{}
	}
	b, _ := json.Marshal(map[string]any{"version": 0, "tags": tags, "shapes": shapes, "tracks": []any{}})
	return string(b)
}

// manualTag is one annotation the user made himself.
func manualTag(id int64, frame int) Tag {
	return Tag{ID: id, Frame: frame, LabelID: 1, Source: "manual", Attributes: []Attribute{}}
}

// autoTag is one annotation a run or a fill wrote.
func autoTag(id int64, frame int) Tag {
	return Tag{ID: id, Frame: frame, LabelID: 1, Source: sourceAuto, Attributes: []Attribute{}}
}

// registerTags writes tags into the register of the gateway as one source wrote them (V3b).
func registerTags(t *testing.T, e *testEnv, chatID, sourceID string, taskID int64, tags ...Tag) {
	t.Helper()
	objs := make([]store.AnnotationObject, 0, len(tags))
	for _, tag := range tags {
		objs = append(objs, store.AnnotationObject{SourceID: sourceID, ChatID: chatID, TaskID: taskID,
			Kind: store.ObjectTag, CVATID: tag.ID, Frame: tag.Frame, LabelID: tag.LabelID,
			Fingerprint: tagFingerprint(tag)})
	}
	if err := e.st.AddAnnotationObjects(context.Background(), objs); err != nil {
		t.Fatal(err)
	}
}

// clearRegister drops the rows a source left in the register, so that a test can set up exactly the
// annotations and the register it wants.
func clearRegister(t *testing.T, e *testEnv, chatID, sourceID string) {
	t.Helper()
	ctx := context.Background()
	objs, err := e.st.AnnotationObjects(ctx, chatID, sourceID)
	if err != nil {
		t.Fatal(err)
	}
	refs := make([]store.ObjectRef, 0, len(objs))
	for _, o := range objs {
		refs = append(refs, store.ObjectRef{TaskID: o.TaskID, Kind: o.Kind, CVATID: o.CVATID})
	}
	if err := e.st.DeleteAnnotationObjects(ctx, chatID, refs); err != nil {
		t.Fatal(err)
	}
}

// verifyOf runs mode verify of annotation_check and returns the finished job.
func (e *testEnv) verifyOf(t *testing.T, chatID, sourceID string, final bool) store.AnnotationJob {
	t.Helper()
	body := `{"mode":"verify","source_id":"` + sourceID + `"}`
	if final {
		body = `{"mode":"verify","source_id":"` + sourceID + `","final":true}`
	}
	jobID := e.startJob(t, chatID, "/checks", body)
	return e.waitJob(t, chatID, jobID)
}

// A verification measures with the function, the mapping and the threshold of the run or the fill it
// judges (V3b): the agent names neither of them, so that it cannot measure against a better setting than
// the one that produced the result. The single calls run at the low threshold of a check (so that the
// function answers as much as it can); the figures of the report apply the threshold of the judged source.
func TestVerifyUsesRunParams(t *testing.T) {
	e, chatID, fillID := filledTask(t)
	before := len(e.fc.askedFrames())

	row := e.verifyOf(t, chatID, fillID, false)
	if row.State != store.AnnotationJobFinished {
		t.Fatalf("verify: %+v", row)
	}
	var got verifyAnswer
	if json.Unmarshal([]byte(row.Result), &got) != nil {
		t.Fatalf("result not readable: %s", row.Result)
	}
	// The fill ran at 70 percent and the function answered 0.9: every image of the validation set is
	// counted as answered and answered right.
	if got.Overall.Accuracy == nil || *got.Overall.Accuracy != 1 || got.Verdict != assess.Usable {
		t.Fatalf("the verification did not measure with the fill: %+v (%v)", got.Overall, got.Reasons)
	}
	if got.Set != assess.SetValidation || got.Measured != 5 {
		t.Errorf("the interim verdict does not measure the validation set of the task: set %q, %d images", got.Set, got.Measured)
	}
	// It measured the calibration set (10 images) and the validation set (5), each with the mapping of the
	// fill and the low threshold of a single call.
	e.fc.mu.Lock()
	bodies := append([]string(nil), e.fc.funcBodies...)
	asked := append([]int(nil), e.fc.funcCalls...)
	e.fc.mu.Unlock()
	measured := asked[before:]
	if len(measured) != 15 {
		t.Fatalf("the verification measured %d images, expected the 10 of the calibration set and the 5 of the validation set: %v", len(measured), measured)
	}
	for _, frame := range measured {
		if !inSets(frame) {
			t.Errorf("the verification measured frame %d, which is neither in the calibration nor in the validation set", frame)
		}
	}
	for _, b := range bodies[len(bodies)-len(measured):] {
		if !strings.Contains(b, `"class_0":{"name":"0"}`) {
			t.Errorf("the verification does not use the mapping of the fill: %s", b)
		}
		if !strings.Contains(b, `"threshold":0.01`) {
			t.Errorf("the call does not use the low threshold of a check: %s", b)
		}
	}
}

// The threshold of the judged source decides the figures (V3b): a fill that ran at 95 percent counted the
// answers of 0.9 as no answer at all, and the same measurement ends questionable.
func TestVerifyUsesTheThresholdOfTheSource(t *testing.T) {
	e, chatID, fillID := filledTaskAt(t, 95)
	row := e.verifyOf(t, chatID, fillID, false)
	var got verifyAnswer
	if json.Unmarshal([]byte(row.Result), &got) != nil {
		t.Fatalf("result not readable: %s", row.Result)
	}
	if got.Overall.Accuracy == nil || *got.Overall.Accuracy != 0 {
		t.Fatalf("an answer under the threshold of the fill was counted: %+v", got.Overall)
	}
	if got.Verdict != assess.Questionable {
		t.Errorf("verdict: %+v", got.Verdict)
	}
}

// inSets reports whether a frame of the test task belongs to the calibration or the validation set: the
// 25 manually annotated frames 0..24 are counted off in fives, so the frames of the held-out set are the
// third and the fourth of every five.
func inSets(frame int) bool {
	if frame < 0 || frame > 24 {
		return false
	}
	switch frame % 5 {
	case 3, 4:
		return false
	}
	return true
}

// The held-out images are measured at most once per task and chat (V3b, E22): the second final verdict is
// refused with the sentence that names the task.
func TestFinalVerifyOnce(t *testing.T) {
	e, chatID, fillID := filledTask(t)
	if row := e.verifyOf(t, chatID, fillID, true); row.State != store.AnnotationJobFinished {
		t.Fatalf("first final verdict: %+v", row)
	}
	res := e.do(t, chatID, http.MethodPost, platform.AnnotationPrefix+"/checks",
		`{"mode":"verify","source_id":"`+fillID+`","final":true}`)
	if res.Status != "error" || !strings.Contains(res.Message, "already used in this chat") {
		t.Fatalf("second final verdict: %+v", res)
	}
	if !strings.Contains(res.Message, "annotation-job") && !strings.Contains(res.Message, "result stands") {
		t.Errorf("the refusal does not say that the result stands: %q", res.Message)
	}
	// An interim verdict stays possible: it measures the validation set.
	if row := e.verifyOf(t, chatID, fillID, false); row.State != store.AnnotationJobFinished {
		t.Errorf("interim verdict after the final one: %+v", row)
	}
}

// A verification of a source of another chat finds nothing (E8): the register is the boundary.
func TestVerifyOtherChat(t *testing.T) {
	e, _, fillID := filledTask(t)
	other := e.chat(t)
	res := e.do(t, other, http.MethodPost, platform.AnnotationPrefix+"/checks",
		`{"mode":"verify","source_id":"`+fillID+`"}`)
	if res.Status != "error" || !strings.Contains(res.Message, "unknown run or job") {
		t.Fatalf("a source of another chat: %+v", res)
	}
}

// A verification refuses a function, a mapping or a dataset that belongs to another source: it measures
// what the judged run or fill measured, nothing else (V3b).
func TestVerifyRefusesOwnSetting(t *testing.T) {
	e, chatID, fillID := filledTask(t)
	for _, body := range []string{
		`{"mode":"verify","source_id":"` + fillID + `","function":"agw-other"}`,
		`{"mode":"verify","source_id":"` + fillID + `","mapping":{"class_0":"1"}}`,
		`{"mode":"verify","source_id":"` + fillID + `","dataset_id":99}`,
		`{"mode":"verify"}`,
	} {
		res := e.do(t, chatID, http.MethodPost, platform.AnnotationPrefix+"/checks", body)
		if res.Status != "error" {
			t.Errorf("%s: %+v", body, res)
		}
	}
}

// filledTask prepares a chat with a task of 25 manually annotated frames and a finished fill over the
// five empty ones; the register of the fill holds what it wrote (V3b).
func filledTask(t *testing.T) (*testEnv, string, string) { return filledTaskAt(t, 70) }

// filledTaskAt is the same with the min_confidence of the fill named, so that a test can show which
// threshold the verdict rests on.
func filledTaskAt(t *testing.T, confidence int) (*testEnv, string, string) {
	t.Helper()
	e := newTestEnv(t)
	chatID := e.chat(t)
	e.pf.setDataset(datasetJSON(5, "test", "AgriImageDataResource", []string{"0", "1"}, 7))
	e.fc.setTask(7, taskJSON(7, "test", 30, labelsJSON("0", "1")))
	e.fc.setFunctions(`[{"id":"agw-test-m1-abcd1234","name":"mine","kind":"detector","labels":["class_0","class_1"]}]`)
	e.fc.setMeta(`{"size":30,"width":100,"height":100,"name":"img.png"}`)
	e.fc.setJobs(`{"count":1,"results":[{"id":11,"start_frame":0,"stop_frame":29}]}`)
	manual := []Tag{}
	for i := 0; i < 25; i++ {
		manual = append(manual, manualTag(int64(i+1), i))
	}
	e.fc.setAnnotations(7, annotationsWith(manual, nil))
	answer := `[{"label":"0","raw_label":"class_0","type":"tag","confidence":"0.9000","scores":[0.9,0.1]}]`
	answers := map[int]string{}
	for i := 0; i < 30; i++ {
		answers[i] = answer
	}
	e.fc.setAnswers(answers)
	fillID := e.startJob(t, chatID, "/fills", `{"dataset_id":5,"function":"agw-test-m1-abcd1234","min_confidence":`+
		strconv.Itoa(confidence)+`,"mapping":{"class_0":"0"}}`)
	if row := e.waitJob(t, chatID, fillID); row.State != store.AnnotationJobFinished {
		t.Fatalf("fill: %+v", row)
	}
	return e, chatID, fillID
}

// A fill registers what it wrote (V3b): only from the register can a rollback find it again.
func TestFillWritesRegister(t *testing.T) {
	e, chatID, fillID := filledTask(t)
	objs, err := e.st.AnnotationObjects(context.Background(), chatID, fillID)
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 5 {
		t.Fatalf("the fill registered %d objects, expected 5: %+v", len(objs), objs)
	}
	for _, o := range objs {
		if o.Kind != store.ObjectTag || o.SourceID != fillID || o.TaskID != 7 || o.LabelID != 1 {
			t.Errorf("registered object: %+v", o)
		}
		if o.Frame < 25 {
			t.Errorf("the fill registered an image it did not write: %+v", o)
		}
	}
}

// A run registers what it wrote when it reaches finished, once (V3b): a run over a task that carried no
// annotation wrote everything with source "auto".
func TestRunSnapshotOnFinished(t *testing.T) {
	e := newTestEnv(t)
	chatID := e.chat(t)
	e.pf.setDataset(datasetJSON(5, "test", "AgriImageDataResource", []string{"0", "1"}, 7))
	e.fc.setTask(7, taskJSON(7, "test", 30, labelsJSON("0", "1")))
	e.fc.setFunctions(`[{"id":"agw-test-m1-abcd1234","name":"mine","kind":"detector","labels":["class_0"]}]`)
	e.fc.setAnnotations(7, `{"version":0,"tags":[],"shapes":[],"tracks":[]}`)
	e.fc.setRun("run-1", `{"id":"run-1","status":"queued","progress":0}`)
	res := e.do(t, chatID, http.MethodPost, platform.AnnotationPrefix+"/runs",
		`{"dataset_id":5,"function":"agw-test-m1-abcd1234","mapping":{"class_0":"0"}}`)
	if res.Status != "ok" {
		t.Fatalf("run: %+v", res)
	}
	// The run is queued: nothing is registered yet.
	if objs, err := e.st.AnnotationObjects(context.Background(), chatID, "run-1"); err != nil || len(objs) != 0 {
		t.Fatalf("before the run ended: %v %v", objs, err)
	}
	// CVAT answers the run as finished and the task carries what it wrote.
	e.fc.setRun("run-1", `{"id":"run-1","status":"finished","progress":100}`)
	e.fc.setAnnotations(7, annotationsWith([]Tag{autoTag(1, 0), autoTag(2, 1), autoTag(3, 2)}, nil))
	if res := e.do(t, chatID, http.MethodGet, platform.AnnotationPrefix+"/runs/run-1", ""); res.Status != "ok" {
		t.Fatalf("annotation_run: %+v", res)
	}
	objs, err := e.st.AnnotationObjects(context.Background(), chatID, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 3 {
		t.Fatalf("the snapshot registered %d objects, expected 3: %+v", len(objs), objs)
	}
	// A second status call registers nothing twice.
	if res := e.do(t, chatID, http.MethodGet, platform.AnnotationPrefix+"/runs/run-1", ""); res.Status != "ok" {
		t.Fatalf("annotation_run again: %+v", res)
	}
	objs, err = e.st.AnnotationObjects(context.Background(), chatID, "run-1")
	if err != nil || len(objs) != 3 {
		t.Errorf("the snapshot ran twice: %v %v", objs, err)
	}
}

// The rollback takes back exactly the registered, automatic and unchanged annotations (V3b, E21): one the
// user corrected in CVAT stays and is reported.
func TestRollbackOnlyAutoAndUnchanged(t *testing.T) {
	e, chatID, fillID := filledTask(t)
	clearRegister(t, e, chatID, fillID)
	unchanged := []Tag{autoTag(11, 25), autoTag(12, 26)}
	for _, tag := range unchanged {
		registerTags(t, e, chatID, fillID, 7, tag)
	}
	// An object that was corrected in CVAT: the register knows it at frame 27, CVAT carries it at 28.
	registerTags(t, e, chatID, fillID, 7, autoTag(13, 27))
	// What the task carries now: the fill's five tags, one of them corrected, and a manual one.
	tags := []Tag{autoTag(11, 25), autoTag(12, 26), autoTag(13, 28), autoTag(14, 29), manualTag(15, 5)}
	e.fc.setAnnotations(7, annotationsWith(tags, nil))

	res := e.do(t, chatID, http.MethodPost, platform.AnnotationPrefix+"/rollbacks", `{"source_id":"`+fillID+`"}`)
	if res.Status != "ok" {
		t.Fatalf("rollback: %+v", res)
	}
	var got rollbackAnswer
	if err := json.Unmarshal([]byte(res.Body), &got); err != nil {
		t.Fatal(err)
	}
	if got.Deleted != 2 || got.ChangedKept != 1 || got.Missing != 0 {
		t.Fatalf("rollback: %+v", got)
	}
	// Only the two unchanged ids are named in the delete call, never the corrected one, never the manual
	// one and never the object the fill wrote without a register entry.
	body := lastWrite(t, e)
	if !strings.Contains(body, `"tags":[11,12]`) {
		t.Errorf("the delete call does not carry exactly the unchanged objects: %s", body)
	}
	for _, id := range []string{"13", "14", "15"} {
		if strings.Contains(body, "["+id+"]") || strings.Contains(body, ","+id+"]") || strings.Contains(body, "["+id+",") {
			t.Errorf("the delete call names id %s: %s", id, body)
		}
	}
	// The rows of what was taken back are gone from the register; the changed one stays.
	objs, err := e.st.AnnotationObjects(context.Background(), chatID, fillID)
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 1 || objs[0].CVATID != 13 {
		t.Errorf("register after the rollback: %+v", objs)
	}
}

// A rollback of another chat finds nothing (E8): the register is the boundary, whatever the agent claims.
func TestRollbackOtherChat(t *testing.T) {
	e, chatID, fillID := filledTask(t)
	clearRegister(t, e, chatID, fillID)
	registerTags(t, e, chatID, fillID, 7, autoTag(11, 25))
	other := e.chat(t)
	before := len(e.fc.writeBodies())
	res := e.do(t, other, http.MethodPost, platform.AnnotationPrefix+"/rollbacks", `{"source_id":"`+fillID+`"}`)
	if res.Status != "error" || !strings.Contains(res.Message, "no automatic annotation") {
		t.Fatalf("a rollback from another chat: %+v", res)
	}
	if len(e.fc.writeBodies()) != before {
		t.Errorf("CVAT was written for another chat: %v", e.fc.writeBodies())
	}
}

// A rollback never deletes what the user annotated himself (V3b, E21): the register holds the automatic
// objects only, and an object CVAT carries as "manual" is left alone even when its id is registered.
func TestRollbackKeepsManual(t *testing.T) {
	e, chatID, fillID := filledTask(t)
	clearRegister(t, e, chatID, fillID)
	registerTags(t, e, chatID, fillID, 7, autoTag(11, 25))
	// The same id, but the user changed the source in CVAT: nothing is deleted.
	e.fc.setAnnotations(7, annotationsWith([]Tag{manualTag(11, 25)}, nil))
	res := e.do(t, chatID, http.MethodPost, platform.AnnotationPrefix+"/rollbacks", `{"source_id":"`+fillID+`"}`)
	if res.Status != "ok" {
		t.Fatalf("rollback: %+v", res)
	}
	var got rollbackAnswer
	if json.Unmarshal([]byte(res.Body), &got) != nil {
		t.Fatal(res.Body)
	}
	if got.Deleted != 0 || got.ChangedKept != 1 {
		t.Fatalf("a manual annotation was taken back: %+v", got)
	}
	for _, b := range e.fc.writeBodies() {
		if strings.Contains(b, "[11]") {
			t.Errorf("the delete call names a manual object: %s", b)
		}
	}
}

// The scope suspicious takes back only what stands on the images the last verification named (V3b): it
// goes by what the gateway reported, not by a rule recomputed on other data.
func TestRollbackScopeSuspicious(t *testing.T) {
	e, chatID, fillID := filledTask(t)
	clearRegister(t, e, chatID, fillID)
	registerTags(t, e, chatID, fillID, 7, autoTag(11, 25), autoTag(12, 26))
	e.fc.setAnnotations(7, annotationsWith([]Tag{autoTag(11, 25), autoTag(12, 26)}, nil))
	// Without a verification there is nothing to go by.
	res := e.do(t, chatID, http.MethodPost, platform.AnnotationPrefix+"/rollbacks",
		`{"source_id":"`+fillID+`","scope":"suspicious"}`)
	if res.Status != "error" || !strings.Contains(res.Message, "no verification") {
		t.Fatalf("scope suspicious without a verification: %+v", res)
	}
	// A verification that named frame 26 changes that.
	storeVerify(t, e, chatID, fillID, []int{26})
	res = e.do(t, chatID, http.MethodPost, platform.AnnotationPrefix+"/rollbacks",
		`{"source_id":"`+fillID+`","scope":"suspicious"}`)
	if res.Status != "ok" {
		t.Fatalf("scope suspicious: %+v", res)
	}
	var got rollbackAnswer
	if json.Unmarshal([]byte(res.Body), &got) != nil {
		t.Fatal(res.Body)
	}
	if got.Deleted != 1 {
		t.Fatalf("scope suspicious took back %d objects, expected 1: %+v", got.Deleted, got)
	}
	body := lastWrite(t, e)
	if !strings.Contains(body, "[12]") {
		t.Errorf("the object of the named image was not the one taken back: %s", body)
	}
}

// storeVerify writes a finished verification of a source into the store, as the job would leave it.
func storeVerify(t *testing.T, e *testEnv, chatID, sourceID string, frames []int) {
	t.Helper()
	ctx := context.Background()
	params, _ := json.Marshal(jobSpec{Kind: JobVerify, SourceID: sourceID})
	row, err := e.st.AddAnnotationJob(ctx, store.AnnotationJob{ChatID: chatID, Kind: JobVerify, DatasetID: 5,
		TaskID: 7, Function: "agw-test-m1-abcd1234", Params: string(params),
		State: store.AnnotationJobRunning})
	if err != nil {
		t.Fatal(err)
	}
	items := make([]SuspiciousImage, 0, len(frames))
	for _, f := range frames {
		items = append(items, SuspiciousImage{Frame: f, Reason: "the box is almost empty"})
	}
	raw, _ := json.Marshal(verifyAnswer{Suspicious: items})
	if err := e.st.FinishAnnotationJob(ctx, chatID, row.ID, store.AnnotationJobFinished, string(raw), ""); err != nil {
		t.Fatal(err)
	}
}

// lastWrite is the body of the last write to CVAT.
func lastWrite(t *testing.T, e *testEnv) string {
	t.Helper()
	writes := e.fc.writeBodies()
	if len(writes) == 0 {
		t.Fatal("CVAT was not written")
	}
	return writes[len(writes)-1]
}

// A calibration measures only the images of the calibration set (V3b, E22): the validation and the
// held-out images stay untouched for the comparison of rounds and the verdict.
func TestCalibrateUsesOnlyCalibrationSet(t *testing.T) {
	e := v3Env(t, 30)
	chatID := e.chat(t)
	e.fc.setAnnotations(7, manualTags(map[int64][]int{1: {0, 1, 2, 3, 4, 5, 6, 7, 8, 9}}))
	e.fc.setAnswers(map[int]string{0: `[{"label":"0","raw_label":"class_0","type":"tag","confidence":"0.9"}]`})
	jobID := e.startJob(t, chatID, "/checks", `{"dataset_id":5,"function":"agw-test-m1-abcd1234","mode":"calibrate"}`)
	e.waitJob(t, chatID, jobID)
	for _, frame := range e.fc.askedFrames() {
		switch frame {
		case 0, 1, 5, 6:
		default:
			t.Errorf("the calibration measured frame %d, which is not in the calibration set", frame)
		}
	}
}

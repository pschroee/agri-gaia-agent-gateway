// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package assess

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"agw/internal/platform"
	"agw/internal/sandbox"
	"agw/internal/store"
)

// answerRuntime is the container runtime of the own-model tests: it reads the tar the check sends and
// answers one prediction per image, built by the test out of the images it finds in that tar. So a test can
// let the "model" answer by class or constantly, and it can look at what the check measured (R2.5).
type answerRuntime struct {
	t     *testing.T
	reply func(blob []byte) string
	blob  []byte
	spec  sandbox.Spec
}

func (r *answerRuntime) RunOnce(_ context.Context, spec sandbox.Spec, stdin io.Reader, _ int64) ([]byte, int, string, error) {
	r.spec = spec
	r.blob, _ = io.ReadAll(stdin)
	return []byte(r.reply(r.blob)), 0, "", nil
}

// ownModelBytes is the file the notebook run of these tests handed over.
var ownModelBytes = []byte("onnx-own")

// ownModelSHA is the SHA-256 of that file: the subject of its check (R2.6).
var ownModelSHA = Sha256Hex(ownModelBytes)

// notebookRun is a run of a chat with the datasets it received (R2.3).
func notebookRun(chatID, id string, datasets []int64) store.NotebookRun {
	return store.NotebookRun{ID: id, ChatID: chatID, HubUser: "agent-test", Server: "agw-test",
		Entry: "train.py", DatasetIDs: datasets, TimeoutMin: 60, StartedAt: time.Now()}
}

// classOfSHA names the class of every fixture image by the SHA-256 of its content.
func classOfSHA(groups ...[]testImage) map[string]string {
	out := map[string]string{}
	for _, list := range groups {
		for _, im := range list {
			out[Sha256Hex([]byte(im.Data))] = im.Tags[0]
		}
	}
	return out
}

// answerByClass is the answer of a "model" that answers by the class of the image: every image of class
// "0" gets p0, every image of class "1" gets p1. The images come out of the tar by their SHA-256, so the
// test never names one.
func answerByClass(t *testing.T, blob []byte, classes map[string]string, p0, p1 []float64) string {
	t.Helper()
	var b strings.Builder
	b.WriteString(`{"model":{"inputs":[{"name":"input","shape":[1,3,28,28],"type":"tensor(float)"}],`)
	b.WriteString(`"outputs":[{"name":"output","shape":[1,2],"type":"tensor(float)"}],"load_error":""},"predictions":[`)
	for i, name := range tarImageNames(t, blob) {
		sha := strings.TrimSuffix(strings.TrimPrefix(name, "images/"), ".png")
		probs := p0
		if classes[sha] == "1" {
			probs = p1
		}
		if i > 0 {
			b.WriteString(",")
		}
		top := 0
		if probs[1] > probs[0] {
			top = 1
		}
		fmt.Fprintf(&b, `{"id":%q,"ok":true,"error":"","top":%d,"probs":[%g,%g]}`, sha, top, probs[0], probs[1])
	}
	b.WriteString(`],"tensors":[]}`)
	return b.String()
}

// storedReport is a report as a test reads it out of the store: the shell of the report plus the figures of
// its kind (the shell alone carries them as a plain object).
type storedReport struct {
	Report
	Figures *OwnModelFigures `json:"figures"`
}

// startOwnCheck starts the check of the fetched file through the service and answers its stored report
// (R2.2). The call carries the file as an upload, the way the socket reads it for a call with a path.
func startOwnCheck(t *testing.T, svc *Service, st *store.Store, chatID, body string) storedReport {
	t.Helper()
	ctx := context.Background()
	res, err := svc.Do(ctx, chatID, platform.Request{Method: "POST", Path: platform.AssessPrefix + "/checks",
		Body: []byte(body),
		Uploads: []platform.Upload{{Field: "modelfile", Name: "model.onnx", SHA256: ownModelSHA,
			Data: ownModelBytes}}})
	if err != nil || res.Status != "ok" {
		t.Fatalf("the check did not start: %+v %v", res, err)
	}
	var started struct {
		CheckID string `json:"check_id"`
	}
	if err := json.Unmarshal([]byte(res.Body), &started); err != nil || started.CheckID == "" {
		t.Fatalf("the answer of the check: %q %v", res.Body, err)
	}
	var row store.Assessment
	waitFor(t, "the report of the own-model check", func() bool {
		r, err := st.Assessment(ctx, chatID, started.CheckID)
		if err != nil {
			return false
		}
		row = r
		return r.State == store.AssessFinished || r.State == store.AssessFailed
	})
	if row.State != store.AssessFinished {
		t.Fatalf("the check failed: %s", row.Error)
	}
	var rep storedReport
	if err := json.Unmarshal(row.Report, &rep); err != nil {
		t.Fatalf("the report is not readable: %v", err)
	}
	if rep.Figures == nil {
		t.Fatalf("the report carries no figures: %s", row.Report)
	}
	return rep
}

// ownFixture is the setup of an own-model check: a chat with the dataset a notebook run received, the
// source dataset it was split from (60 fresh images plus a copy of every image of the run's dataset) and
// the file a run handed over. Whether that file is a row of notebook_fetches, the tests decide.
type ownFixture struct {
	f         *fakePlatform
	st        *store.Store
	chat      store.Chat
	svc       *Service
	runtime   *answerRuntime
	runID     string
	classes   map[string]string
	seenCount int
}

func newOwnFixture(t *testing.T, fetched bool) *ownFixture {
	t.Helper()
	st := testStore(t)
	ctx := context.Background()
	chat := testChat(t, st, "own model")
	f := newFakePlatform(t)
	train := classImages(5) // five images of each class: enough for the class order (R0.6)
	source := classImages(30)
	for i := range source {
		source[i].Name = "fresh-" + source[i].Name
		source[i].Data = "fresh-" + source[i].Data
	}
	// A copy of every image the run received lies in the source as well: its content makes it a seen image,
	// whatever its file name is (P1).
	for _, im := range train {
		source = append(source, testImage{Name: "copy-" + im.Name, Data: im.Data, Tags: im.Tags})
	}
	f.dataset(7, "mnist-check-120-train", train)
	f.dataset(8, "mnist-check-120", source)
	if err := st.RecordSplit(ctx, store.Split{ChatID: chat.ID, SourceDatasetID: 8, TrainDatasetID: 7,
		TrainImages: len(train), KeptImages: 60}); err != nil {
		t.Fatal(err)
	}
	const runID = "r20260101120000-abcd"
	if err := st.CreateNotebookRun(ctx, notebookRun(chat.ID, runID, []int64{7})); err != nil {
		t.Fatal(err)
	}
	if fetched {
		if err := st.RecordNotebookFetch(ctx, store.NotebookFetch{ChatID: chat.ID, RunID: runID,
			Path: "model.onnx", SHA256: ownModelSHA, Size: int64(len(ownModelBytes))}); err != nil {
			t.Fatal(err)
		}
	}
	runtime := &answerRuntime{t: t, reply: func([]byte) string { return noPredictions }}
	svc := New(NewRunner(runtime, "img"), st, 12)
	svc.Plat = f.client()
	return &ownFixture{f: f, st: st, chat: chat, svc: svc, runtime: runtime, runID: runID,
		classes: classOfSHA(train, source), seenCount: len(train)}
}

// pathBody is the call of a check of the fetched file.
const pathBody = `{"path":"/workspace/notebook/r20260101120000-abcd/model.onnx","eval_dataset_id":8}`

// A file whose bytes came out of a notebook run of this chat is measured (R2.2); a file the gateway did not
// hand out itself has no origin and is not measured (P2).
func TestAssessPathOrigin(t *testing.T) {
	// Without a fetch row: nothing is measured, and the figures name the file without a run.
	fx := newOwnFixture(t, false)
	rep := startOwnCheck(t, fx.svc, fx.st, fx.chat.ID, pathBody)
	rep.Finish()
	if rep.Verdict != NotMeasured || len(rep.Reasons) != 1 || rep.Reasons[0] != ReasonFileOriginUnknown {
		t.Fatalf("verdict: %s %v", rep.Verdict, rep.Reasons)
	}
	if rep.Header != "Result: not measured – "+ReasonFileOriginUnknown {
		t.Errorf("header: %q", rep.Header)
	}
	if rep.Figures.File.Name != "model.onnx" || rep.Figures.File.SHA256 != ownModelSHA || rep.Figures.File.RunID != nil {
		t.Errorf("the file of the report: %+v", rep.Figures.File)
	}
	if rep.Measured != 0 || rep.Figures.EvalDataset.ID != 0 || len(rep.Figures.SeenDatasets) != 0 {
		t.Errorf("a report without a run names measurements: %+v", rep.Figures)
	}
	if rep.Figures.Basic != nil {
		t.Errorf("the file was run although it has no origin: %+v", rep.Figures.Basic)
	}
	if fx.runtime.blob != nil {
		t.Error("a check container ran for a file without an origin")
	}

	// With the fetch row the file is measured, and the report names the run it came out of.
	fx = newOwnFixture(t, true)
	fx.runtime.reply = func(blob []byte) string {
		return answerByClass(t, blob, fx.classes, []float64{0.95, 0.05}, []float64{0.05, 0.95})
	}
	rep = startOwnCheck(t, fx.svc, fx.st, fx.chat.ID, pathBody)
	rep.Finish()
	if rep.Verdict == NotMeasured {
		t.Fatalf("the file was not measured: %v", rep.Reasons)
	}
	if rep.Figures.File.RunID == nil || *rep.Figures.File.RunID != fx.runID || rep.Figures.File.Path != "model.onnx" {
		t.Errorf("the file of the report: %+v", rep.Figures.File)
	}
	// The model answers by class: the order is clear, the basic checks hold and the figures are good.
	if !rep.Figures.Basic.Loads || rep.Figures.Basic.Classes != 2 ||
		rep.Figures.Basic.Varied == nil || !*rep.Figures.Basic.Varied {
		t.Errorf("the basic checks: %+v", rep.Figures.Basic)
	}
	if rep.Figures.Overall.Accuracy < 0.9 {
		t.Errorf("accuracy: %.2f", rep.Figures.Overall.Accuracy)
	}
	// Nothing was declared: the figures name the default preprocessing of the templates (R2.4).
	if rep.Figures.Preprocess.Declared || rep.Figures.Preprocess.Color != "rgb" ||
		rep.Figures.Preprocess.Layout != "nchw" || rep.Figures.Preprocess.Scale != ScaleToUnit {
		t.Errorf("the default preprocessing: %+v", rep.Figures.Preprocess)
	}
	// The images the run received are seen images (they are in the source dataset as well): they do not
	// count as held out, and the same image is never twice in the job (P1, P11).
	names := tarImageNames(t, fx.runtime.blob)
	distinct := map[string]bool{}
	for _, n := range names {
		if distinct[n] {
			t.Errorf("the same image twice in the job: %s", n)
		}
		distinct[n] = true
	}
	if len(names) != fx.seenCount+rep.Measured {
		t.Errorf("%d images in the job, want %d (the seen images plus the measured set): %v",
			len(names), fx.seenCount+rep.Measured, names)
	}
	if !fx.f.called("GET", "/datasets/8/download") {
		t.Error("the evaluation dataset was not downloaded")
	}
}

// Every dataset a notebook run of this chat received counts as seen, whatever run it was (R2.3): the image
// of an earlier run is held out just as little, and the report names every dataset.
func TestSeenAllNotebookDatasets(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	chat := testChat(t, st, "seen datasets")
	f := newFakePlatform(t)
	older := classImages(3) // the dataset of the first run
	newer := classImages(2) // the dataset of the run that fetched the file
	source := classImages(30)
	for i := range source {
		source[i].Name = "fresh-" + source[i].Name
		source[i].Data = "fresh-" + source[i].Data
	}
	source = append(source, testImage{Name: "copy-of-old.png", Data: older[0].Data, Tags: older[0].Tags})
	f.dataset(5, "mnist-checks-old", older)
	f.dataset(7, "mnist-check-120-train", newer)
	f.dataset(8, "mnist-check-120", source)
	if err := st.RecordSplit(ctx, store.Split{ChatID: chat.ID, SourceDatasetID: 8, TrainDatasetID: 7,
		TrainImages: len(newer), KeptImages: 61}); err != nil {
		t.Fatal(err)
	}
	// The file comes out of the first run; the second one received the dataset the split names.
	if err := st.CreateNotebookRun(ctx, notebookRun(chat.ID, "r20260101120000-aaaa", []int64{5})); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateNotebookRun(ctx, notebookRun(chat.ID, "r20260101130000-bbbb", []int64{7})); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordNotebookFetch(ctx, store.NotebookFetch{ChatID: chat.ID, RunID: "r20260101120000-aaaa",
		Path: "model.onnx", SHA256: ownModelSHA, Size: int64(len(ownModelBytes))}); err != nil {
		t.Fatal(err)
	}
	runtime := &answerRuntime{t: t, reply: func([]byte) string { return noPredictions }}
	svc := New(NewRunner(runtime, "img"), st, 12)
	svc.Plat = f.client()
	// Without eval_dataset_id the check takes the source of the split of a dataset a run received (R2.2).
	rep := startOwnCheck(t, svc, st, chat.ID, `{"path":"/workspace/notebook/r20260101120000-aaaa/model.onnx"}`)
	if rep.Figures.EvalDataset.ID != 8 || rep.Figures.EvalDataset.Name != "mnist-check-120" {
		t.Errorf("the evaluation dataset: %+v", rep.Figures.EvalDataset)
	}
	var ids []int64
	for _, d := range rep.Figures.SeenDatasets {
		ids = append(ids, d.ID)
	}
	if len(ids) != 2 || ids[0] != 5 || ids[1] != 7 {
		t.Fatalf("the seen datasets: %v (the datasets of both runs count, R2.3)", ids)
	}
	// The image of the earlier run lies in the source dataset as a copy: it is a seen image, so only the
	// sample of seen images carries it, never the measured set.
	names := tarImageNames(t, runtime.blob)
	copySHA := "images/" + Sha256Hex([]byte(older[0].Data)) + ".png"
	count := 0
	for _, n := range names {
		if n == copySHA {
			count++
		}
	}
	if count != 1 {
		t.Errorf("the image of the earlier run counts %d times, want 1 (the sample of the seen images)", count)
	}
	if rep.Measured == 0 {
		t.Fatalf("nothing was measured: %v", rep.Reasons)
	}
	// Five seen images of each class, the probe (which lies in the measured set) and the measured set.
	if len(names) != 10+rep.Measured {
		t.Errorf("%d images in the job, want %d: %v", len(names), 10+rep.Measured, names)
	}
}

// The class order the agent declares is only compared with the order the gateway derives (R2.4): the same
// order leaves the verdict alone, another one is a reason of its own, and without a declaration nothing is
// compared.
func TestDeclaredClassOrderMismatch(t *testing.T) {
	file := FileRef{Name: "model.onnx", SHA256: ownModelSHA}
	pre := preprocessFigures(nil)
	base := judgeInput()
	base.Kind = KindOwnModel
	base.Model = SubjectRef{}
	base.SeenDatasets = []DatasetRef{{ID: 7, Name: "mnist-check-120-train"}}

	for _, tc := range []struct {
		declared []string
		verdict  Verdict
		reason   bool
	}{
		{[]string{"0", "1"}, Usable, false},
		{[]string{"1", "0"}, Questionable, true},
		{[]string{"0"}, Questionable, true},
		{[]string{"0", "1", "2"}, Questionable, true},
		{nil, Usable, false},
	} {
		in := base
		in.DeclaredClassNames = tc.declared
		rep := JudgeOwnModel(in, file, pre, nil)
		if rep.Verdict != tc.verdict {
			t.Errorf("declared %v: verdict %s (%v)", tc.declared, rep.Verdict, rep.Reasons)
			continue
		}
		got := false
		for _, r := range rep.Reasons {
			if r == ReasonDeclaredClassOrder {
				got = true
			}
		}
		if got != tc.reason {
			t.Errorf("declared %v: the reason is %v, want %v (%v)", tc.declared, got, tc.reason, rep.Reasons)
		}
	}

	// The figures an own-model check carries: the file, the preprocessing and, only with a declaration,
	// whether the declared order is the one the model shows.
	rep := JudgeOwnModel(base, file, pre, nil)
	figures, ok := rep.Figures.(*OwnModelFigures)
	if !ok {
		t.Fatalf("the figures: %T", rep.Figures)
	}
	if figures.File != file || figures.Preprocess.Declared || figures.MeasuredImages != 40 {
		t.Errorf("the figures of an own-model check: %+v", figures)
	}
	if figures.Model != nil {
		t.Errorf("a model the gateway did not see uploaded: %+v", figures.Model)
	}

	probe := []checkImage{{Item: Item{SHA: "s0", Class: "0"}}, {Item: Item{SHA: "s1", Class: "1"}}}
	answers := map[string]Prediction{
		"s0": {ID: "s0", OK: true, Top: 0, Probs: []float64{0.9, 0.1}},
		"s1": {ID: "s1", OK: true, Top: 1, Probs: []float64{0.1, 0.9}},
	}
	model := ModelInfo{Inputs: []TensorInfo{{Name: "input", Shape: []int{1, 3, 28, 28}}},
		Outputs: []TensorInfo{{Name: "output", Shape: []int{1, 2}}}}
	derived := map[int]string{0: "0", 1: "1"}
	if b := basicFigures(model, answers, probe, derived, true, []string{"0", "1"}); b.ClassOrder == nil || !*b.ClassOrder {
		t.Errorf("a matching declared order: %+v", b.ClassOrder)
	}
	if b := basicFigures(model, answers, probe, derived, true, []string{"1", "0"}); b.ClassOrder == nil || *b.ClassOrder {
		t.Errorf("a different declared order: %+v", b.ClassOrder)
	}
	if b := basicFigures(model, answers, probe, derived, true, nil); b.ClassOrder != nil {
		t.Errorf("without class_names there is nothing to compare: %+v", b.ClassOrder)
	}
	if b := basicFigures(model, answers, probe, derived, false, []string{"0", "1"}); b.ClassOrder == nil || *b.ClassOrder {
		t.Errorf("an unclear derived order: %+v", b.ClassOrder)
	}
}

// A model that answers the same for different images is questionable (R2.5), and the reasons name it.
func TestConstantOutputQuestionable(t *testing.T) {
	fx := newOwnFixture(t, true)
	// The same answer for every image: a model that does not look at its input.
	fx.runtime.reply = func(blob []byte) string {
		return answerByClass(t, blob, fx.classes, []float64{0.9, 0.1}, []float64{0.9, 0.1})
	}
	rep := startOwnCheck(t, fx.svc, fx.st, fx.chat.ID, pathBody)
	rep.Finish()
	if rep.Verdict != Questionable {
		t.Fatalf("verdict: %s %v", rep.Verdict, rep.Reasons)
	}
	found := false
	for _, r := range rep.Reasons {
		if r == ReasonConstantOutput {
			found = true
		}
	}
	if !found {
		t.Fatalf("the constant output is no reason: %v", rep.Reasons)
	}
	if len(rep.Reasons) < 2 {
		t.Errorf("the reason stands alone: %v", rep.Reasons)
	}
	if rep.Figures.Basic.Varied == nil || *rep.Figures.Basic.Varied {
		t.Errorf("varied: %+v", rep.Figures.Basic.Varied)
	}

	// A model that answers by class is not called constant: the probe holds images of both classes.
	fx = newOwnFixture(t, true)
	fx.runtime.reply = func(blob []byte) string {
		return answerByClass(t, blob, fx.classes, []float64{0.95, 0.05}, []float64{0.05, 0.95})
	}
	rep = startOwnCheck(t, fx.svc, fx.st, fx.chat.ID, pathBody)
	for _, r := range rep.Reasons {
		if r == ReasonConstantOutput {
			t.Errorf("a model that answers by class is called constant: %v", rep.Reasons)
		}
	}
}

// The variation check compares the answers of the probe images (R2.5): fewer than two answers that ran say
// nothing at all, the same answers are the constant output.
func TestVariedOutputs(t *testing.T) {
	probe := []checkImage{{Item: Item{SHA: "a"}}, {Item: Item{SHA: "b"}}, {Item: Item{SHA: "c"}}}
	same, other := []float64{0.9, 0.1}, []float64{0.1, 0.9}
	answers := func(list ...Prediction) map[string]Prediction {
		out := map[string]Prediction{}
		for _, p := range list {
			out[p.ID] = p
		}
		return out
	}
	ok := func(id string, probs []float64) Prediction {
		return Prediction{ID: id, OK: true, Top: 0, Probs: probs}
	}
	if got := variedOutputs(answers(ok("a", same), ok("b", same)), probe); got == nil || *got {
		t.Errorf("two equal answers: %v", got)
	}
	if got := variedOutputs(answers(ok("a", same), ok("b", other)), probe); got == nil || !*got {
		t.Errorf("two different answers: %v", got)
	}
	// A different number of values is a different answer: the outputs are compared value by value.
	if got := variedOutputs(answers(ok("a", same), ok("b", []float64{0.9})), probe); got == nil || !*got {
		t.Errorf("a shorter answer: %v", got)
	}
	if got := variedOutputs(answers(ok("a", same), Prediction{ID: "b"}), probe); got != nil {
		t.Errorf("one answer that ran: %v", got)
	}
	if got := variedOutputs(answers(), probe); got != nil {
		t.Errorf("no answer at all: %v", got)
	}
}

// The probe holds images of different classes: four images of one class may well get the same answer from a
// good model, so comparing them would call every such model constant (R2.5).
func TestProbeSpansClasses(t *testing.T) {
	validation := []checkImage{
		{Item: Item{SHA: "a0", Class: "0"}}, {Item: Item{SHA: "a1", Class: "0"}},
		{Item: Item{SHA: "a2", Class: "0"}}, {Item: Item{SHA: "a3", Class: "0"}},
		{Item: Item{SHA: "b0", Class: "1"}}, {Item: Item{SHA: "b1", Class: "1"}},
	}
	got := probeImages(validation, 4)
	if len(got) != 4 {
		t.Fatalf("the probe: %v", got)
	}
	classes := map[string]int{}
	for _, im := range got {
		classes[im.Class]++
	}
	if classes["0"] != 2 || classes["1"] != 2 {
		t.Errorf("the probe mixes the classes: %v", classes)
	}
	if got[0].SHA != "a0" || got[1].SHA != "b0" {
		t.Errorf("the probe is not deterministic: %v", got)
	}
	// Fewer images than asked for is no mistake (a small validation set).
	if got := probeImages([]checkImage{{Item: Item{SHA: "a0", Class: "0"}}}, 4); len(got) != 1 {
		t.Errorf("one image: %v", got)
	}
}

// A file that does not load is not measured, and the report names the error instead of failing the check
// (R2.5). The basic checks stand in the figures.
func TestOwnModelDoesNotLoad(t *testing.T) {
	fx := newOwnFixture(t, true)
	fx.runtime.reply = func([]byte) string {
		return `{"model":{"inputs":[],"outputs":[],"load_error":"No Op registered for Foo with name foo"},"predictions":[],"tensors":[]}`
	}
	rep := startOwnCheck(t, fx.svc, fx.st, fx.chat.ID, pathBody)
	rep.Finish()
	if rep.Verdict != NotMeasured || !strings.Contains(rep.Reasons[0], "does not load in onnxruntime") {
		t.Fatalf("verdict: %s %v", rep.Verdict, rep.Reasons)
	}
	if !strings.Contains(rep.Reasons[0], "No Op registered for Foo") {
		t.Errorf("the reason does not name the load error: %v", rep.Reasons)
	}
	if rep.Figures.Basic == nil || rep.Figures.Basic.Loads {
		t.Fatalf("the basic checks of a file that does not load: %+v", rep.Figures.Basic)
	}
	if rep.Figures.Basic.Varied != nil {
		t.Errorf("nothing was compared, so varied stays null: %+v", rep.Figures.Basic.Varied)
	}
	if rep.Figures.File.SHA256 != ownModelSHA {
		t.Errorf("the file of the report: %+v", rep.Figures.File)
	}
}

// A model with more than one input is not measured: a check feeds exactly one image tensor (R2.5).
func TestOwnModelTwoInputs(t *testing.T) {
	fx := newOwnFixture(t, true)
	fx.runtime.reply = func([]byte) string {
		return `{"model":{"inputs":[{"name":"image","shape":[1,3,28,28],"type":"tensor(float)"},` +
			`{"name":"mask","shape":[1,1,28,28],"type":"tensor(float)"}],` +
			`"outputs":[{"name":"output","shape":[1,2],"type":"tensor(float)"}],"load_error":""},"predictions":[],"tensors":[]}`
	}
	rep := startOwnCheck(t, fx.svc, fx.st, fx.chat.ID, pathBody)
	if rep.Verdict != NotMeasured || !strings.Contains(rep.Reasons[0], "has 2 inputs") {
		t.Fatalf("verdict: %s %v", rep.Verdict, rep.Reasons)
	}
	if rep.Figures.Basic == nil || len(rep.Figures.Basic.Inputs) != 2 {
		t.Errorf("the shapes of the file: %+v", rep.Figures.Basic)
	}
}

// A declaration of the preprocessing is fed to the check and named in the report (R2.4).
func TestOwnModelPreprocessDeclared(t *testing.T) {
	fx := newOwnFixture(t, true)
	fx.runtime.reply = func(blob []byte) string {
		return answerByClass(t, blob, fx.classes, []float64{0.95, 0.05}, []float64{0.05, 0.95})
	}
	rep := startOwnCheck(t, fx.svc, fx.st, fx.chat.ID,
		`{"path":"/workspace/notebook/r20260101120000-abcd/model.onnx","eval_dataset_id":8,`+
			`"preprocess":{"color":"gray","size":[28,28],"scale":0.0039,"mean":[0.5],"std":[0.5],"layout":"nchw"},`+
			`"class_names":["0","1"]}`)
	if !rep.Figures.Preprocess.Declared || rep.Figures.Preprocess.Color != "gray" ||
		rep.Figures.Preprocess.Scale != 0.0039 || len(rep.Figures.Preprocess.Mean) != 1 {
		t.Errorf("the declared preprocessing: %+v", rep.Figures.Preprocess)
	}
	if rep.Figures.Basic.ClassOrder == nil || !*rep.Figures.Basic.ClassOrder {
		t.Errorf("the declared class order: %+v", rep.Figures.Basic.ClassOrder)
	}
	// The job carries the preprocessing to the container (the container applies it).
	if !strings.Contains(string(fx.runtime.blob), `"color":"gray"`) &&
		!strings.Contains(string(fx.runtime.blob), `"layout":"nchw"`) {
		t.Errorf("the job does not carry the declared preprocessing")
	}
}

// A check needs the file of the call: without it there is nothing to measure (R2.2).
func TestAssessPathNeedsFile(t *testing.T) {
	fx := newOwnFixture(t, true)
	ctx := context.Background()
	res, err := fx.svc.Do(ctx, fx.chat.ID, platform.Request{Method: "POST", Path: platform.AssessPrefix + "/checks",
		Body: []byte(`{"path":"/workspace/model.onnx","eval_dataset_id":8}`)})
	if err != nil || res.Status != "error" || !strings.Contains(res.Message, "carries no model file") {
		t.Fatalf("without a file: %+v %v", res, err)
	}
	// model_id and path exclude each other (R2.2), and one of them is needed.
	for _, body := range []string{`{"model_id":3,"path":"/workspace/model.onnx"}`, `{}`, `{"eval_dataset_id":8}`} {
		res, err := fx.svc.Do(ctx, fx.chat.ID, platform.Request{Method: "POST", Path: platform.AssessPrefix + "/checks",
			Body: []byte(body)})
		if err != nil || res.Status != "error" {
			t.Errorf("accepted %s: %+v %v", body, res, err)
		}
	}
}

// The check of a file names the model it was uploaded as, when that already happened (R2.6).
func TestOwnModelNamesUploadedModel(t *testing.T) {
	fx := newOwnFixture(t, true)
	fx.f.model(21, "mnist_cnn", "onnx", ownModelBytes)
	if err := fx.st.RecordModelOrigin(context.Background(), store.ModelOrigin{ModelID: 21, ChatID: fx.chat.ID,
		Source: store.OriginNotebook, SourceSHA: ownModelSHA, SeenDatasetIDs: []int64{7}}); err != nil {
		t.Fatal(err)
	}
	fx.runtime.reply = func(blob []byte) string {
		return answerByClass(t, blob, fx.classes, []float64{0.95, 0.05}, []float64{0.05, 0.95})
	}
	rep := startOwnCheck(t, fx.svc, fx.st, fx.chat.ID, pathBody)
	if rep.Figures.Model == nil || rep.Figures.Model.ID != 21 || rep.Figures.Model.Name != "mnist_cnn" {
		t.Fatalf("the model of the report: %+v", rep.Figures.Model)
	}
}

// The line about the check of a file follows one wording for every verdict (R2.7, L8), and it rests on the
// last finished check with final=true (W4): an interim one does not decide, and it is not hidden by one
// that came after it.
func TestFileCheckLine(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	chat := testChat(t, st, "file checks")
	svc := New(NewRunner(&answerRuntime{t: t, reply: func([]byte) string { return noPredictions }}, "img"), st, 12)
	sha := strings.Repeat("ab", 32)
	if c, err := svc.FileCheckOf(ctx, chat.ID, sha); err != nil || c.Found {
		t.Fatalf("without a check: %+v %v", c, err)
	}
	interim := store.Assessment{ChatID: chat.ID, Kind: KindOwnModel, Subject: FileSubject(sha), Final: false,
		State:  store.AssessFinished,
		Report: []byte(`{"verdict":"usable","measured_images":12,"figures":{"overall":{"accuracy":0.75}}}`)}
	if _, err := st.CreateAssessment(ctx, interim); err != nil {
		t.Fatal(err)
	}
	c, err := svc.FileCheckOf(ctx, chat.ID, sha)
	if err != nil || !c.Found || c.Final || c.Text() != "none in this chat (interim only)" {
		t.Fatalf("an interim check: %+v %v", c, err)
	}
	final := store.Assessment{ChatID: chat.ID, Kind: KindOwnModel, Subject: FileSubject(sha), Final: true,
		State:  store.AssessFinished,
		Report: []byte(`{"verdict":"usable","measured_images":24,"figures":{"overall":{"accuracy":0.92}}}`)}
	if _, err := st.CreateAssessment(ctx, final); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateAssessment(ctx, interim); err != nil {
		t.Fatal(err)
	}
	c, err = svc.FileCheckOf(ctx, chat.ID, sha)
	if err != nil || c.Text() != "usable (24 held-out images, accuracy 0.92)" {
		t.Fatalf("the final check decides (W4): %+v %v", c, err)
	}
	// A not measured check names its first reason (L8).
	if _, err := st.CreateAssessment(ctx, store.Assessment{ChatID: chat.ID, Kind: KindOwnModel,
		Subject: FileSubject(strings.Repeat("cd", 32)), Final: true, State: store.AssessFinished,
		Report: []byte(`{"verdict":"not_measured","reasons":["origin unknown: this file was not fetched from a notebook run in this chat"],"figures":{"per_class":{}}}`)}); err != nil {
		t.Fatal(err)
	}
	if c, err := svc.FileCheckOf(ctx, chat.ID, strings.Repeat("cd", 32)); err != nil ||
		c.Text() != "not measured (origin unknown: this file was not fetched from a notebook run in this chat)" {
		t.Fatalf("a not measured check: %+v %v", c, err)
	}
	// The route the approval reads (R2.7).
	res, err := svc.Do(ctx, chat.ID, platform.Request{Method: "GET",
		Path: platform.AssessPrefix + "/files/" + sha + "/latest"})
	if err != nil || res.Status != "ok" || !strings.Contains(res.Body, `"line":"usable (24 held-out images, accuracy 0.92)"`) {
		t.Fatalf("the route: %+v %v", res, err)
	}
	res, _ = svc.Do(ctx, chat.ID, platform.Request{Method: "GET",
		Path: platform.AssessPrefix + "/files/" + strings.Repeat("ef", 32) + "/latest"})
	if !strings.Contains(res.Body, `"line":"none in this chat"`) {
		t.Errorf("an unchecked file: %q", res.Body)
	}
	if res, _ := svc.Do(ctx, chat.ID, platform.Request{Method: "GET",
		Path: platform.AssessPrefix + "/files/not-a-sha/latest"}); res.Status != "error" {
		t.Errorf("a path that is no SHA-256: %+v", res)
	}
}

// A model uploaded from a file of a run keeps its origin (R2.6): a later check of the model finds the check
// of its file, and a file the gateway did not hand over records nothing (P2).
func TestOriginAfterUpload(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	chat := testChat(t, st, "uploaded")
	if err := st.CreateNotebookRun(ctx, notebookRun(chat.ID, "r20260101120000-aaaa", []int64{7, 9})); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateNotebookRun(ctx, notebookRun(chat.ID, "r20260101130000-bbbb", []int64{5})); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordNotebookFetch(ctx, store.NotebookFetch{ChatID: chat.ID, RunID: "r20260101120000-aaaa",
		Path: "model.onnx", SHA256: ownModelSHA, Size: int64(len(ownModelBytes))}); err != nil {
		t.Fatal(err)
	}
	if err := RecordUploadedModel(ctx, st, chat.ID, 42, ownModelSHA); err != nil {
		t.Fatal(err)
	}
	o, err := st.ModelOriginOf(ctx, chat.ID, 42)
	if err != nil {
		t.Fatal(err)
	}
	if o.Source != store.OriginNotebook || o.SourceSHA != ownModelSHA || o.TrainContainerID != 0 {
		t.Fatalf("the origin: %+v", o)
	}
	// Seen are the datasets of every run of the chat (R2.3).
	if len(o.SeenDatasetIDs) != 3 || o.SeenDatasetIDs[0] != 5 || o.SeenDatasetIDs[1] != 7 || o.SeenDatasetIDs[2] != 9 {
		t.Errorf("the seen datasets: %v", o.SeenDatasetIDs)
	}
	if m, found, err := st.ModelOriginOfSHA(ctx, chat.ID, ownModelSHA); err != nil || !found || m.ModelID != 42 {
		t.Errorf("the model behind the file: %+v %v %v", m, found, err)
	}
	// A file that no notebook run handed over leaves no row (P2).
	if err := RecordUploadedModel(ctx, st, chat.ID, 43, strings.Repeat("cd", 32)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ModelOriginOf(ctx, chat.ID, 43); err != store.ErrNotFound {
		t.Errorf("an unknown file recorded an origin: %v", err)
	}
}

// The check of a model whose file was checked is the check of that file (W4): the model id and the SHA are
// two keys to the same check.
func TestModelCheckOfUploadedModel(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	chat := testChat(t, st, "model and file")
	svc := New(NewRunner(&answerRuntime{t: t, reply: func([]byte) string { return noPredictions }}, "img"), st, 12)
	if err := st.RecordModelOrigin(ctx, store.ModelOrigin{ModelID: 21, ChatID: chat.ID,
		Source: store.OriginNotebook, SourceSHA: ownModelSHA, SeenDatasetIDs: []int64{7}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateAssessment(ctx, store.Assessment{ChatID: chat.ID, Kind: KindOwnModel,
		Subject: FileSubject(ownModelSHA), Final: true, State: store.AssessFinished,
		Report: []byte(`{"verdict":"questionable","measured_images":24,"reasons":["the accuracy 0.55 is not at least 0.10 above the majority baseline 0.50"],"figures":{"overall":{"accuracy":0.55}}}`)}); err != nil {
		t.Fatal(err)
	}
	c, err := svc.ModelCheckOf(ctx, chat.ID, 21)
	if err != nil {
		t.Fatal(err)
	}
	if c.Text() != "questionable (24 held-out images, accuracy 0.55)" {
		t.Fatalf("the check of the model: %+v", c)
	}
	// A model without a check of its own and without a file stays unchecked.
	if c, err := svc.ModelCheckOf(ctx, chat.ID, 22); err != nil || c.Found {
		t.Fatalf("a model without a check: %+v %v", c, err)
	}
	// The check of the model itself decides when it is newer.
	if _, err := st.CreateAssessment(ctx, store.Assessment{ChatID: chat.ID, Kind: KindTrainingModel,
		Subject: "model:21", Final: true, State: store.AssessFinished,
		Report: []byte(`{"verdict":"usable","measured_images":40,"figures":{"overall":{"accuracy":0.97}}}`)}); err != nil {
		t.Fatal(err)
	}
	if c, err := svc.ModelCheckOf(ctx, chat.ID, 21); err != nil || c.Text() != "usable (40 held-out images, accuracy 0.97)" {
		t.Fatalf("the newer check of the model: %+v %v", c, err)
	}
}

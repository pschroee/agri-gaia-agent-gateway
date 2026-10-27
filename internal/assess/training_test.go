// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package assess

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"

	"agw/internal/store"
)

// judgeInput is a measured result the tests change one field of: 20 validation and 40 hold-out images of
// two classes, an accuracy of 0.90 against a majority baseline of 0.50.
func judgeInput() JudgeInput {
	p := 0.95
	return JudgeInput{
		Kind:             KindTrainingModel,
		Final:            true,
		Model:            SubjectRef{ID: 3, Name: "mnist-effnet"},
		EvalDataset:      DatasetRef{ID: 41, Name: "mnist-check-120"},
		SeenDatasets:     []DatasetRef{{ID: 42, Name: "mnist-check-120-train"}},
		ValidationImages: 20, HoldoutImages: 40,
		Classes: []string{"0", "1"},
		PerClass: map[string]ClassFigures{
			"0": {Precision: &p, Recall: 0.95, F1: 0.93, Support: 20},
			"1": {Precision: &p, Recall: 0.85, F1: 0.87, Support: 20},
		},
		Accuracy: 0.90, MacroF1: 0.90, Counted: 40,
		Baseline: Baseline{Class: "0", Accuracy: 0.50, MacroF1: 0.40},
		PValue:   0.0001,
		Mapping:  map[int]string{0: "0", 1: "1"}, MappingOK: true,
		Outputs: 2, OutputShape: "[1,2]",
		SeenImages: 20, SeenAccuracy: 0.95,
	}
}

// A model that is clearly better than the majority baseline, with a clear class order and no weak
// class, is usable (R1.9).
func TestJudgeUsable(t *testing.T) {
	rep := JudgeClassification(judgeInput())
	rep.Finish()
	if rep.Verdict != Usable {
		t.Fatalf("verdict: %s %v", rep.Verdict, rep.Reasons)
	}
	if rep.Header != "Result: usable (held-out images, statistics only, no image review)" {
		t.Errorf("header: %q", rep.Header)
	}
	if len(rep.Reasons) != 0 || len(rep.Suggestions) != 0 {
		t.Errorf("a usable result carries reasons or suggestions: %v %v", rep.Reasons, rep.Suggestions)
	}
	if rep.ImageReview.Done || rep.ImageReview.Reason != "held-out images are never shown to a language model" {
		t.Errorf("image review: %+v", rep.ImageReview)
	}
	if rep.Set != SetHoldout || rep.Measured != 40 {
		t.Errorf("set: %q %d", rep.Set, rep.Measured)
	}
}

// An accuracy at the majority baseline is questionable: the model is no better than always answering the
// most frequent class (R1.9, P10).
func TestJudgeBaselineOnly(t *testing.T) {
	in := judgeInput()
	in.Accuracy, in.MacroF1, in.PValue = 0.50, 0.50, 0.42
	rep := JudgeClassification(in)
	if rep.Verdict != Questionable {
		t.Fatalf("verdict: %s", rep.Verdict)
	}
	joined := strings.Join(rep.Reasons, " | ")
	if !strings.Contains(joined, "not at least 0.10 above the majority baseline 0.50") {
		t.Errorf("reasons: %v", rep.Reasons)
	}
	if !strings.Contains(joined, "not better than the majority baseline") {
		t.Errorf("the binomial test is not a reason: %v", rep.Reasons)
	}
	rep.Finish()
	if rep.Header != "Result: questionable (held-out images, statistics only, no image review)" {
		t.Errorf("header: %q", rep.Header)
	}
}

// A class whose hit rate falls below the limit is a reason of its own, and the figures name exactly that
// class and that figure (R1.9, L19).
func TestJudgeClassRecall(t *testing.T) {
	in := judgeInput()
	in.PerClass["1"] = ClassFigures{Precision: in.PerClass["0"].Precision, Recall: 0.30, F1: 0.46, Support: 20, Below: []string{"recall"}}
	rep := JudgeClassification(in)
	if rep.Verdict != Questionable {
		t.Fatalf("verdict: %s", rep.Verdict)
	}
	if len(rep.Reasons) != 1 || !strings.Contains(rep.Reasons[0], `class "1"`) {
		t.Fatalf("reasons: %v", rep.Reasons)
	}
	figures := rep.Figures.(*TrainingFigures)
	if len(figures.PerClass["1"].Below) != 1 || figures.PerClass["1"].Below[0] != "recall" {
		t.Errorf("below: %+v", figures.PerClass["1"])
	}
	// A class with fewer than three images says nothing, whatever its hit rate is.
	in.PerClass["1"] = ClassFigures{Recall: 0.0, Support: 2}
	if rep := JudgeClassification(in); rep.Verdict != Usable {
		t.Errorf("a class of two images decides the verdict: %v", rep.Reasons)
	}
}

func TestJudgeTooFewBoth(t *testing.T) {
	in := judgeInput()
	in.ValidationImages, in.HoldoutImages = 2, 4
	rep := JudgeClassification(in)
	if rep.Verdict != NotMeasured || len(rep.Reasons) != 2 ||
		rep.Reasons[0] != TooFewValidation(2) || rep.Reasons[1] != TooFewHoldout(4) {
		t.Fatalf("%s %v", rep.Verdict, rep.Reasons)
	}
}

// Too few images are not measured: fewer than 5 validation images and fewer than 20 held-out images
// (R1.9, L15, L16).
func TestJudgeTooFew(t *testing.T) {
	in := judgeInput()
	in.ValidationImages, in.HoldoutImages = 4, 19
	rep := JudgeClassification(in)
	rep.Finish()
	if rep.Verdict != NotMeasured || !strings.Contains(rep.Reasons[0], "too few validation images (4 of at least 5)") {
		t.Fatalf("validation: %s %v", rep.Verdict, rep.Reasons)
	}
	if rep.Header != "Result: not measured – too few validation images (4 of at least 5)" {
		t.Errorf("header: %q", rep.Header)
	}
	if len(rep.Suggestions) != 1 || rep.Suggestions[0].ID != "more_data" {
		t.Errorf("suggestions: %+v", rep.Suggestions)
	}
	in.ValidationImages, in.HoldoutImages = 6, 19
	rep = JudgeClassification(in)
	if rep.Verdict != NotMeasured || !strings.Contains(rep.Reasons[0], "too few held-out images (19 of at least 20)") {
		t.Fatalf("hold-out: %s %v", rep.Verdict, rep.Reasons)
	}
}

// An output that is no classification is not measured: detectors and segmentations need a postprocessing
// the gateway does not have (R1.9, P8).
func TestJudgeNotClassification(t *testing.T) {
	in := judgeInput()
	in.NotClassification, in.Outputs, in.OutputShape = true, 0, "[1,25200,85]"
	rep := JudgeClassification(in)
	if rep.Verdict != NotMeasured || !strings.Contains(rep.Reasons[0], "not a classification model") {
		t.Fatalf("verdict: %s %v", rep.Verdict, rep.Reasons)
	}
	if !strings.Contains(rep.Reasons[0], "[1,25200,85]") {
		t.Errorf("the reason does not name the shape: %v", rep.Reasons)
	}
	// A model with more outputs than classes is questionable, not unmeasured (R1.9).
	in = judgeInput()
	in.Outputs = 3
	rep = JudgeClassification(in)
	if rep.Verdict != Questionable || !strings.Contains(strings.Join(rep.Reasons, " "), "3 outputs for 2 classes") {
		t.Fatalf("outputs: %s %v", rep.Verdict, rep.Reasons)
	}
}

// Hints change nothing about the verdict: overfitting, underfitting and a gap to the platform's figure
// (R1.10, R1.11).
func TestHintsAndSuggestions(t *testing.T) {
	in := judgeInput()
	in.SeenImages, in.SeenAccuracy = 20, 0.99
	in.Accuracy, in.MacroF1 = 0.70, 0.70
	in.PValue = 0.001
	in.PlatformFigure = &PlatformFigure{Name: "MulticlassAccuracy", Value: 0.96, Source: "training log", Note: "measured by the template"}
	in.MissingClass = "2"
	rep := JudgeClassification(in)
	if rep.Verdict != Usable {
		t.Fatalf("verdict: %s %v", rep.Verdict, rep.Reasons)
	}
	joined := strings.Join(rep.Hints, " | ")
	for _, want := range []string{"likely overfitting", "differs from the measured accuracy", `class "2" of the training data`} {
		if !strings.Contains(joined, want) {
			t.Errorf("hints lack %q: %v", want, rep.Hints)
		}
	}
	ids := suggestionIDs(rep)
	if !containsString(ids, "regularize") || !containsString(ids, "check_preprocessing") {
		t.Errorf("suggestions: %v", ids)
	}
	// Underfitting: the model does not even reach 0.80 on the images it saw.
	in = judgeInput()
	in.SeenImages, in.SeenAccuracy = 20, 0.55
	in.Accuracy, in.MacroF1 = 0.30, 0.25
	in.PValue = 0.9
	rep = JudgeClassification(in)
	if rep.Verdict != Questionable {
		t.Fatalf("underfitting: %s", rep.Verdict)
	}
	if ids := suggestionIDs(rep); !containsString(ids, "more_epochs") {
		t.Errorf("no more_epochs: %v", ids)
	}
	// nan losses in the log are a hint.
	in = judgeInput()
	in.LogNaN = true
	if rep := JudgeClassification(in); !strings.Contains(strings.Join(rep.Hints, " "), "nan losses") {
		t.Errorf("the nan hint is missing: %v", rep.Hints)
	}
}

func suggestionIDs(rep Report) []string {
	out := make([]string, 0, len(rep.Suggestions))
	for _, s := range rep.Suggestions {
		out = append(out, s.ID)
	}
	return out
}

// The report of a round carries the previous check and the change per figure; the shared stop rule names
// when another round is not worth it (R1.12).
func TestPreviousAndDelta(t *testing.T) {
	prev := &CheckComparison{CheckID: "11111111-2222-3333-4444-555555555555",
		Model: SubjectRef{ID: 2, Name: "effnet-1"}, Verdict: Questionable, Accuracy: 0.62, MacroF1: 0.60}
	in := judgeInput()
	in.Final, in.Previous = false, prev
	in.ValidationImages, in.HoldoutImages = 12, 24
	in.Accuracy, in.MacroF1 = 0.70, 0.68
	rep := JudgeClassification(in)
	figures := rep.Figures.(*TrainingFigures)
	if figures.Previous == nil || figures.Previous.Model.ID != 2 {
		t.Fatalf("previous: %+v", figures.Previous)
	}
	if d := figures.Delta["accuracy"]; d < 0.079 || d > 0.081 {
		t.Errorf("delta accuracy: %v", d)
	}
	if rep.Set != SetValidation || rep.Measured != 12 {
		t.Errorf("an interim check measures the validation images: %q %d", rep.Set, rep.Measured)
	}
	if ids := suggestionIDs(rep); containsString(ids, "stop") {
		t.Errorf("an improvement stops the loop: %v", ids)
	}
	// A round without an improvement names the stop as its own suggestion.
	in.Accuracy, in.MacroF1, in.PValue = 0.52, 0.50, 0.30
	rep = JudgeClassification(in)
	if ids := suggestionIDs(rep); !containsString(ids, "stop") {
		t.Errorf("no stop suggestion: %v", ids)
	} else if !strings.Contains(rep.Suggestions[len(rep.Suggestions)-1].Text, "no improvement on the validation images") {
		t.Errorf("the stop suggestion does not carry the rule's sentence: %+v", rep.Suggestions)
	}
}

// A retraining is suggested for the first questionable result of a dataset only (R1.11).
func TestRetrainSuggestedOnce(t *testing.T) {
	in := judgeInput()
	in.Accuracy, in.PValue = 0.52, 0.30
	if ids := suggestionIDs(JudgeClassification(in)); !containsString(ids, "retrain") {
		t.Fatalf("the first questionable result does not suggest a retraining: %v", ids)
	}
	in.Previous = &CheckComparison{CheckID: "x", Model: SubjectRef{ID: 2, Name: "old"}, Verdict: Questionable, Accuracy: 0.51}
	rep := JudgeClassification(in)
	ids := suggestionIDs(rep)
	if containsString(ids, "retrain") {
		t.Errorf("a second round suggests a retraining again: %v", ids)
	}
	if !containsString(ids, "other_architecture") {
		t.Errorf("two questionable results in a row do not name the architecture: %v", ids)
	}
}

// The report never names an image of the measured set: no file name, no SHA-256, no count per image
// (P11).
func TestReportHidesHoldout(t *testing.T) {
	in := judgeInput()
	in.Mapping = map[int]string{0: "0", 1: "1"}
	rep := JudgeClassification(in)
	rep.Note = reportNote(in, 3, 40)
	rep.Finish()
	raw, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, forbidden := range []string{"sha", ".png", ".jpg", "c0ffee"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("the report carries %q: %s", forbidden, text)
		}
	}
	// The note names the dataset and the skipped images, not the measured ones.
	if !strings.Contains(rep.Note, "Images of dataset mnist-check-120") ||
		!strings.Contains(rep.Note, "3 images were skipped (no or several tags)") {
		t.Errorf("note: %q", rep.Note)
	}
}

// A final check measures the hold-out set, an interim check the validation images (R1.8, P3).
func TestFinalUsesHoldoutValidationOtherwise(t *testing.T) {
	in := judgeInput()
	in.ValidationImages, in.HoldoutImages = 12, 24
	final := JudgeClassification(in)
	if final.Set != SetHoldout || final.Measured != 24 {
		t.Errorf("final: %q %d", final.Set, final.Measured)
	}
	final.Finish()
	if !strings.Contains(final.Header, "(held-out images, statistics only, no image review)") {
		t.Errorf("final header: %q", final.Header)
	}
	in.Final = false
	interim := JudgeClassification(in)
	if interim.Set != SetValidation || interim.Measured != 12 {
		t.Errorf("interim: %q %d", interim.Set, interim.Measured)
	}
	interim.Finish()
	if interim.Header != "Result: usable (validation images, interim, no image review)" {
		t.Errorf("interim header: %q", interim.Header)
	}
	if interim.ImageReview.Reason != "image review is not available yet" {
		t.Errorf("interim image review: %+v", interim.ImageReview)
	}
}

// --- the run, with a fake platform and a store ---

// modelZip is what GET /models/<id>/download answers for a training model: model.onnx and train.log.
func modelZip(model, log string) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range map[string]string{"model.onnx": model, "train.log": log} {
		w, err := zw.Create(name)
		if err != nil {
			panic(err)
		}
		_, _ = io.WriteString(w, content)
	}
	_ = zw.Close()
	return buf.Bytes()
}

// lightningLog is a training log with the metric table of Lightning.
const lightningLog = `Epoch 2: 100%|██████| 2/2 [00:01<00:00]
┏━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━┓
┃       Test metric       ┃ DataLoader 0    ┃
┡━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━┩
│      MulticlassAccuracy │        0.9583   │
│   MulticlassPrecision   │        0.9583   │
└─────────────────────────┴─────────────────┘
`

// A model the gateway did not see being created is not measured: the origin comes from the gateway alone
// (R1.8, P2).
func TestAssessUnknownOrigin(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	chat := testChat(t, st, "unknown origin")
	f := newFakePlatform(t)
	f.model(3, "mnist-effnet", "onnx", modelZip("onnx-bytes", lightningLog))
	svc := New(NewRunner(&fakeRuntime{answer: goodAnswer}, "img"), st, 12)
	svc.Plat = f.client()
	rep, err := svc.runTrainingCheck(ctx, chat.ID, "", assessRequest{ModelID: 3})
	if err != nil {
		t.Fatal(err)
	}
	rep.Finish()
	if rep.Verdict != NotMeasured || rep.Reasons[0] != ReasonOriginUnknown {
		t.Fatalf("verdict: %s %v", rep.Verdict, rep.Reasons)
	}
	if rep.Header != "Result: not measured – "+ReasonOriginUnknown {
		t.Errorf("header: %q", rep.Header)
	}
	// Nothing was read for this model, so the platform's figure is absent, not null (H114).
	if rep.Figures != nil {
		t.Errorf("a report without a read carries figures: %+v", rep.Figures)
	}
	raw, _ := json.Marshal(rep)
	if strings.Contains(string(raw), "platform_figure") {
		t.Errorf("the report names a platform figure: %s", raw)
	}
	if f.called("GET", "/models/3/download") {
		t.Error("the model was downloaded although the origin is unknown")
	}
}

// A model that was trained on the whole evaluation dataset has no held-out images: the check answers so
// and suggests the split (R1.8, R1.11).
func TestAssessNoHeldOut(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	chat := testChat(t, st, "no held-out")
	f := newFakePlatform(t)
	images := classImages(30)
	f.dataset(7, "mnist-check-120", images)
	f.model(3, "mnist-effnet", "onnx", modelZip("onnx-bytes", lightningLog))
	if err := st.RecordModelOrigin(ctx, store.ModelOrigin{ModelID: 3, ChatID: chat.ID,
		Source: store.OriginTraining, TrainContainerID: 9, SeenDatasetIDs: []int64{7}}); err != nil {
		t.Fatal(err)
	}
	svc := New(NewRunner(&fakeRuntime{answer: goodAnswer}, "img"), st, 12)
	svc.Plat = f.client()
	rep, err := svc.runTrainingCheck(ctx, chat.ID, "", assessRequest{ModelID: 3})
	if err != nil {
		t.Fatal(err)
	}
	rep.Finish()
	if rep.Verdict != NotMeasured || rep.Reasons[0] != NoHeldOut("mnist-check-120") {
		t.Fatalf("verdict: %s %v", rep.Verdict, rep.Reasons)
	}
	if !strings.Contains(rep.Header, "no held-out images: the model was trained on all images of mnist-check-120") {
		t.Errorf("header: %q", rep.Header)
	}
	if ids := suggestionIDs(rep); !containsString(ids, "split_dataset") {
		t.Errorf("no split_dataset suggestion: %v", ids)
	}
	// The log was read before the exit, so the platform's figure stands in the report (L6, R1.13).
	figures, ok := rep.Figures.(*TrainingFigures)
	if !ok || figures.PlatformFigure == nil {
		t.Fatalf("the platform figure is missing: %+v", rep.Figures)
	}
	if figures.PlatformFigure.Value != 0.9583 || figures.PlatformFigure.Name != "MulticlassAccuracy" {
		t.Errorf("platform figure: %+v", figures.PlatformFigure)
	}
	if figures.PlatformFigure.Source != "training log" || !strings.Contains(figures.PlatformFigure.Note, "own internal test split") {
		t.Errorf("platform figure note: %+v", figures.PlatformFigure)
	}
}

// When the model's archive carries no train.log, the gateway reads the end of the training container's
// log for the platform's own figure (R1.13).
func TestAssessTrainLogFallback(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	chat := testChat(t, st, "log fallback")
	f := newFakePlatform(t)
	f.dataset(7, "mnist-check-120", classImages(10))
	// The download answers the model file alone (no ZIP, so no train.log inside).
	f.model(3, "mnist-effnet", "onnx", []byte("onnx-bytes"))
	f.trainLog(9, "Epoch 2\n"+lightningLog)
	if err := st.RecordModelOrigin(ctx, store.ModelOrigin{ModelID: 3, ChatID: chat.ID,
		Source: store.OriginTraining, TrainContainerID: 9, SeenDatasetIDs: []int64{7}}); err != nil {
		t.Fatal(err)
	}
	svc := New(NewRunner(&fakeRuntime{answer: goodAnswer}, "img"), st, 12)
	svc.Plat = f.client()
	rep, err := svc.runTrainingCheck(ctx, chat.ID, "", assessRequest{ModelID: 3})
	if err != nil {
		t.Fatal(err)
	}
	figures, ok := rep.Figures.(*TrainingFigures)
	if !ok || figures.PlatformFigure == nil {
		t.Fatalf("no platform figure from the container log: %+v", rep.Figures)
	}
	if figures.PlatformFigure.Value != 0.9583 || figures.PlatformFigure.Name != "MulticlassAccuracy" {
		t.Errorf("platform figure: %+v", figures.PlatformFigure)
	}
	if !strings.Contains(figures.PlatformFigure.Note, "about 4 images") {
		t.Errorf("the note does not name the test split of the seen images: %+v", figures.PlatformFigure)
	}
	if !f.called("GET", "/train/containers/9/logs") {
		t.Error("the container log was not read")
	}
}

// An image whose bytes occur in a dataset the model saw is a seen image, whatever its file name is (P1).
func TestHeldOutExcludesDuplicates(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	chat := testChat(t, st, "duplicates")
	f := newFakePlatform(t)
	seen := []testImage{
		{Name: "seen0a.png", Data: "seen-0-a", Tags: []string{"0"}},
		{Name: "seen0b.png", Data: "seen-0-b", Tags: []string{"0"}},
		{Name: "seen1a.png", Data: "seen-1-a", Tags: []string{"1"}},
		{Name: "seen1b.png", Data: "seen-1-b", Tags: []string{"1"}},
	}
	f.dataset(7, "mnist-check-120-train", seen)
	// The source dataset holds 60 fresh images and one copy of a seen image under another name.
	source := classImages(30)
	for i := range source {
		source[i].Name = "fresh-" + source[i].Name
		source[i].Data = "fresh-" + source[i].Data
	}
	source = append(source, testImage{Name: "copy-of-seen.png", Data: "seen-0-a", Tags: []string{"0"}})
	f.dataset(8, "mnist-check-120", source)
	f.model(3, "mnist-effnet", "onnx", modelZip("onnx-bytes", lightningLog))
	if err := st.RecordModelOrigin(ctx, store.ModelOrigin{ModelID: 3, ChatID: chat.ID,
		Source: store.OriginTraining, TrainContainerID: 9, SeenDatasetIDs: []int64{7}}); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordSplit(ctx, store.Split{ChatID: chat.ID, SourceDatasetID: 8, TrainDatasetID: 7,
		TrainImages: 4, KeptImages: 61}); err != nil {
		t.Fatal(err)
	}
	runtime := &fakeRuntime{answer: noPredictions}
	svc := New(NewRunner(runtime, "img"), st, 12)
	svc.Plat = f.client()
	rep, err := svc.runTrainingCheck(ctx, chat.ID, "", assessRequest{ModelID: 3})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Verdict == NotMeasured {
		t.Fatalf("the check measured nothing: %v", rep.Reasons)
	}
	names := tarImageNames(t, runtime.stdin)
	if len(names) != 4+20 {
		t.Fatalf("the check measured %d images, want 24 (4 seen sample + 20 validation): %v", len(names), names)
	}
	distinct := map[string]bool{}
	for _, n := range names {
		if distinct[n] {
			t.Fatalf("the same image is in the job twice: %s", n)
		}
		distinct[n] = true
	}
	// The copy of a seen image is a seen image: it is not in the measured set (the seen sample holds the
	// image it copies, so its SHA-256 appears once, never twice).
	duplicate := "images/" + Sha256Hex([]byte("seen-0-a")) + ".png"
	count := 0
	for _, n := range names {
		if n == duplicate {
			count++
		}
	}
	if count != 1 {
		t.Errorf("the duplicate counts %d times in the job, want 1 (the seen sample only)", count)
	}
	figures, ok := rep.Figures.(*TrainingFigures)
	if !ok || figures.MeasuredImages != 20 {
		t.Errorf("figures: %+v", rep.Figures)
	}
	// The split of the source is what chose the evaluation dataset (R1.5).
	if figures.EvalDataset.ID != 8 || figures.EvalDataset.Name != "mnist-check-120" {
		t.Errorf("evaluation dataset: %+v", figures.EvalDataset)
	}
	if len(figures.SeenDatasets) != 1 || figures.SeenDatasets[0].ID != 7 {
		t.Errorf("seen datasets: %+v", figures.SeenDatasets)
	}
}

// classImages builds n images of two classes with distinct content.
func classImages(n int) []testImage {
	out := make([]testImage, 0, 2*n)
	for class := 0; class < 2; class++ {
		for i := 0; i < n; i++ {
			out = append(out, testImage{
				Name: fmt.Sprintf("img%d_%02d.png", class, i),
				Data: fmt.Sprintf("content-%d-%d", class, i),
				Tags: []string{fmt.Sprint(class)},
			})
		}
	}
	return out
}

// noPredictions is what the container runtime of a test answers: the model described properly, but no
// prediction per image. A test of the run checks what goes into the container (which images were
// measured), not the figures; those come from the judgement tests above.
const noPredictions = `{"model":{"inputs":[{"name":"input","shape":[1,3,28,28],"type":"tensor(float)"}],
 "outputs":[{"name":"output","shape":[1,2],"type":"tensor(float)"}],"load_error":""},"predictions":[],"tensors":[]}`

// The line an approval adds about a check follows one wording for every verdict (R1.14, L8): the verdict
// words in lower case and with spaces, a not measured check with its first reason.
func TestModelCheckLine(t *testing.T) {
	for _, tc := range []struct {
		check ModelCheck
		want  string
	}{
		{ModelCheck{}, "none in this chat"},
		{ModelCheck{Found: true, Final: false, Verdict: Usable, Measured: 12, Accuracy: 0.75}, "none in this chat (interim only)"},
		{ModelCheck{Found: true, Final: true, Verdict: Usable, Measured: 24, Accuracy: 0.921}, "usable (24 held-out images, accuracy 0.92)"},
		{ModelCheck{Found: true, Final: true, Verdict: Questionable, Measured: 24, Accuracy: 0.5}, "questionable (24 held-out images, accuracy 0.50)"},
		{ModelCheck{Found: true, Final: true, Verdict: NotMeasured, Reason: "no held-out images: the model was trained on all images of mnist-check-120"},
			"not measured (no held-out images: the model was trained on all images of mnist-check-120)"},
		{ModelCheck{Found: true, Final: true, Verdict: NotMeasured}, "not measured"},
	} {
		if got := tc.check.Text(); got != tc.want {
			t.Errorf("verdict %q: %q, want %q", tc.check.Verdict, got, tc.want)
		}
	}
}

// --- the output_labels of a training model (#226) ---

// swappedInput is a model whose outputs mean "1" first and "0" second.
func swappedInput(labels []string) JudgeInput {
	in := judgeInput()
	in.Mapping = map[int]string{0: "1", 1: "0"}
	in.OutputLabels = labels
	return in
}

// output_labels in another order than the outputs show make the training model questionable, and the
// suggestion names the labels the model has to carry.
func TestJudgeOutputLabelsOrderDiffers(t *testing.T) {
	rep := JudgeClassification(swappedInput([]string{"0", "1"}))
	if rep.Verdict != Questionable {
		t.Fatalf("verdict: %s %v", rep.Verdict, rep.Reasons)
	}
	want := `the output_labels of the model (["0","1"]) name the outputs in another order than the model shows on the images it saw (["1","0"])`
	if len(rep.Reasons) != 1 || rep.Reasons[0] != want {
		t.Errorf("reasons: %v", rep.Reasons)
	}
	var fix *Suggestion
	for i := range rep.Suggestions {
		if rep.Suggestions[i].ID == "fix_output_labels" {
			fix = &rep.Suggestions[i]
		}
	}
	if fix == nil || !strings.Contains(fix.Text, `set output_labels to ["1","0"] with {{tool:update_model}}`) {
		t.Errorf("suggestions: %+v", rep.Suggestions)
	}
	if f, ok := rep.Figures.(*TrainingFigures); !ok || len(f.OutputLabels) != 2 || f.OutputLabels[0] != "0" {
		t.Errorf("figures: %+v", rep.Figures)
	}
}

// The same order: no reason, the verdict is the one without labels.
func TestJudgeOutputLabelsMatch(t *testing.T) {
	in := judgeInput()
	in.OutputLabels = []string{"0", "1"}
	rep := JudgeClassification(in)
	if rep.Verdict != Usable || len(rep.Reasons) != 0 {
		t.Fatalf("verdict: %s %v", rep.Verdict, rep.Reasons)
	}
	for _, h := range rep.Hints {
		if strings.Contains(h, "output_labels") {
			t.Errorf("hint: %v", rep.Hints)
		}
	}
}

// No labels recorded: a hint with the order the model shows, the verdict does not change.
func TestJudgeOutputLabelsAbsentHint(t *testing.T) {
	rep := JudgeClassification(swappedInput(nil))
	if rep.Verdict != Usable {
		t.Fatalf("verdict: %s %v", rep.Verdict, rep.Reasons)
	}
	want := `the model has no output_labels; in the order the model shows they are ["1","0"] (set them with {{tool:update_model}})`
	if !containsString(rep.Hints, want) {
		t.Errorf("hints: %v", rep.Hints)
	}
}

// An unclear class order: only the existing reason, the labels are not compared.
func TestJudgeOutputLabelsMappingUnclear(t *testing.T) {
	in := swappedInput([]string{"0", "1"})
	in.MappingOK, in.MappingReason = false, "the outputs are not clear"
	rep := JudgeClassification(in)
	if rep.Verdict != Questionable || len(rep.Reasons) == 0 {
		t.Fatalf("verdict: %s %v", rep.Verdict, rep.Reasons)
	}
	for _, r := range rep.Reasons {
		if strings.HasPrefix(r, ReasonOutputLabelsPrefix) {
			t.Errorf("the labels were compared: %v", rep.Reasons)
		}
	}
}

// An own model keeps the comparison with the declared class names and ignores output_labels (#113).
func TestJudgeOwnModelDeclaredUnchanged(t *testing.T) {
	in := swappedInput([]string{"0", "1"})
	in.Kind = KindOwnModel
	in.DeclaredClassNames = []string{"1", "0"}
	rep := JudgeClassification(in)
	if rep.Verdict != Usable {
		t.Fatalf("verdict: %s %v", rep.Verdict, rep.Reasons)
	}
	in.DeclaredClassNames = []string{"0", "1"}
	rep = JudgeClassification(in)
	if rep.Verdict != Questionable || rep.Reasons[0] != ReasonDeclaredClassOrder {
		t.Fatalf("verdict: %s %v", rep.Verdict, rep.Reasons)
	}
}

// The check reads output_labels from the platform record into the figures.
func TestRunTrainingCheckReadsOutputLabels(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	chat := testChat(t, st, "output labels")
	f := newFakePlatform(t)
	f.dataset(7, "mnist-check-120", classImages(10))
	f.model(3, "mnist-effnet", "onnx", []byte("onnx-bytes"))
	md := f.models[3]
	md.outputLabels = []string{"0", "1"}
	f.models[3] = md
	f.trainLog(9, "Epoch 2\n"+lightningLog)
	if err := st.RecordModelOrigin(ctx, store.ModelOrigin{ModelID: 3, ChatID: chat.ID,
		Source: store.OriginTraining, TrainContainerID: 9, SeenDatasetIDs: []int64{7}}); err != nil {
		t.Fatal(err)
	}
	svc := New(NewRunner(&fakeRuntime{answer: goodAnswer}, "img"), st, 12)
	svc.Plat = f.client()
	rep, err := svc.runTrainingCheck(ctx, chat.ID, "", assessRequest{ModelID: 3})
	if err != nil {
		t.Fatal(err)
	}
	figures, ok := rep.Figures.(*TrainingFigures)
	if !ok || len(figures.OutputLabels) != 2 || figures.OutputLabels[0] != "0" || figures.OutputLabels[1] != "1" {
		t.Fatalf("figures: %+v", rep.Figures)
	}
}

// A stored report with the reason sets LabelOrderWrong; one without does not.
func TestModelCheckOfLabelOrderWrong(t *testing.T) {
	reason := OutputLabelsOrder([]string{"0", "1"}, map[int]string{0: "1", 1: "0"})
	body, _ := json.Marshal(map[string]any{"verdict": "questionable", "measured_images": 40,
		"reasons": []string{"another reason", reason}})
	c, err := modelCheckOf(store.Assessment{Final: true, Report: body})
	if err != nil || !c.LabelOrderWrong {
		t.Fatalf("check: %+v %v", c, err)
	}
	c, err = modelCheckOf(store.Assessment{Final: true, Report: []byte(`{"verdict":"usable","measured_images":40}`)})
	if err != nil || c.LabelOrderWrong {
		t.Fatalf("check: %+v %v", c, err)
	}
}

// The report of a training check carries the confusion matrix over the classes of the data, zero cells included
// (#248), and the report states whether it is the final one (B11).
func TestTrainingFiguresConfusion(t *testing.T) {
	in := judgeInput()
	in.Confusion = fullConfusion(map[string]map[string]int{"0": {"0": 19, "1": 1}, "1": {"1": 17, "0": 3}}, in.Classes)
	rep := JudgeClassification(in)
	f, ok := rep.Figures.(*TrainingFigures)
	if !ok {
		t.Fatalf("figures: %T", rep.Figures)
	}
	if f.Confusion["0"]["0"] != 19 || f.Confusion["1"]["0"] != 3 {
		t.Errorf("confusion: %v", f.Confusion)
	}
	raw, err := json.Marshal(rep)
	if err != nil || !strings.Contains(string(raw), `"confusion":{"0":{"0":19,"1":1}`) {
		t.Errorf("report json: %s %v", raw, err)
	}
	// A class the model never predicts still has its column, with zeros.
	z := fullConfusion(map[string]map[string]int{"a": {"a": 2}}, []string{"a", "b"})
	if len(z) != 2 || len(z["a"]) != 2 || z["b"]["a"] != 0 || z["a"]["b"] != 0 {
		t.Errorf("zero cells: %v", z)
	}
	// A class outside the list that occurs is added.
	if x := fullConfusion(map[string]map[string]int{"a": {"c": 1}}, []string{"a"}); x["a"]["c"] != 1 || len(x["c"]) != 2 {
		t.Errorf("extra class: %v", x)
	}
}

// The figures of an own-model check carry the matrix too.
func TestOwnModelFiguresConfusion(t *testing.T) {
	in := judgeInput()
	in.Kind = KindOwnModel
	in.Confusion = fullConfusion(map[string]map[string]int{"0": {"0": 20}, "1": {"1": 20}}, in.Classes)
	rep := JudgeOwnModel(in, FileRef{Name: "m.onnx"}, PreprocessFigures{}, nil)
	f, ok := rep.Figures.(*OwnModelFigures)
	if !ok || f.Confusion["1"]["1"] != 20 {
		t.Fatalf("figures: %T %+v", rep.Figures, rep.Figures)
	}
}

// Report.Final follows JudgeInput.Final, for a measured and for a not measured report (B11).
func TestReportFinalSet(t *testing.T) {
	for _, final := range []bool{true, false} {
		in := judgeInput()
		in.Final = final
		if rep := JudgeClassification(in); rep.Final != final {
			t.Errorf("final=%v: report.final=%v", final, rep.Final)
		}
		in.HoldoutImages = 3
		if rep := JudgeClassification(in); rep.Verdict != NotMeasured || rep.Final != final {
			t.Errorf("not measured, final=%v: %v %v", final, rep.Verdict, rep.Final)
		}
	}
}

// overall carries the number of wrong images; a confusion table gives it, and without one it follows from the accuracy
// and the measured images (#254, E4).
func TestOverallWrongCount(t *testing.T) {
	in := judgeInput() // 40 hold-out images, accuracy 0.90
	in.Confusion = map[string]map[string]int{"0": {"0": 19, "1": 1}, "1": {"0": 2, "1": 18}}
	f := JudgeClassification(in).Figures.(*TrainingFigures)
	if f.Overall.Wrong != 3 || f.Overall.Correct != 37 {
		t.Errorf("2 classes: %+v", f.Overall)
	}
	in.Confusion = nil
	f = JudgeClassification(in).Figures.(*TrainingFigures)
	if f.Overall.Wrong != 4 || f.Overall.Correct != 36 {
		t.Errorf("no confusion: %+v", f.Overall)
	}
	// ten classes, one image wrong of 40: the count is 1, whatever precision and recall look like
	in = judgeInput()
	in.Confusion = map[string]map[string]int{}
	for c := 0; c < 10; c++ {
		k := fmt.Sprint(c)
		in.Confusion[k] = map[string]int{k: 4}
	}
	in.Confusion["3"] = map[string]int{"3": 3, "5": 1}
	f = JudgeClassification(in).Figures.(*TrainingFigures)
	if f.Overall.Wrong != 1 || f.Overall.Correct != 39 {
		t.Errorf("10 classes: %+v", f.Overall)
	}
}

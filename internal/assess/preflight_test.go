// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package assess

import (
	"context"
	"strings"
	"testing"

	"agw/internal/store"
)

func TestCollapsedOutput(t *testing.T) {
	two := func(n0, n1 int) ([]int, []string) {
		var idx []int
		var truth []string
		for i := 0; i < n0; i++ {
			idx, truth = append(idx, 1), append(truth, "0")
		}
		for i := 0; i < n1; i++ {
			idx, truth = append(idx, 1), append(truth, "1")
		}
		return idx, truth
	}
	idx, truth := two(10, 10)
	if o, k, n, ok := collapsedOutput(idx, truth); !ok || o != 1 || k != 20 || n != 20 {
		t.Errorf("all alike: %d %d %d %v", o, k, n, ok)
	}
	// exactly at the threshold: 18 of 20 collapse, 17 do not
	idx[0], idx[1] = 0, 0
	if _, k, _, ok := collapsedOutput(idx, truth); !ok || k != 18 {
		t.Errorf("18 of 20 should collapse: %d %v", k, ok)
	}
	idx[2] = 0
	if _, _, _, ok := collapsedOutput(idx, truth); ok {
		t.Error("17 of 20 should not collapse")
	}
	// one class in the sample: one output may be right
	idx, truth = two(10, 0)
	if _, _, _, ok := collapsedOutput(idx, truth); ok {
		t.Error("a sample of one class is no finding")
	}
}

func TestPreprocessShort(t *testing.T) {
	p := PreprocessFigures{Color: "gray", Size: []int{28, 28}, Scale: ScaleToUnit, Mean: []float64{0.5}, Std: []float64{0.5}}
	if got := p.Short(); got != "gray 28x28, scale 1/255, mean 0.5, std 0.5" {
		t.Errorf("short: %q", got)
	}
}

// startFinal starts a final check of the fetched file and answers its report and the row.
func startFinalOwn(t *testing.T, fx *ownFixture, body string) (storedReport, store.Assessment) {
	t.Helper()
	rep := startOwnCheck(t, fx.svc, fx.st, fx.chat.ID, body)
	rows, err := fx.st.Assessments(context.Background(), fx.chat.ID)
	if err != nil || len(rows) == 0 {
		t.Fatalf("rows: %v %v", rows, err)
	}
	return rep, rows[len(rows)-1]
}

const finalBody = `{"path":"/workspace/notebook/r20260101120000-abcd/model.onnx","eval_dataset_id":8,"final":true}`

// A final check whose class order is not derivable ends as not measured without a single hold-out image going
// through the model; the final test is not used up (#181, NT1, NT2, NT3, NT4).
func TestFinalPreflightNotMeasured(t *testing.T) {
	fx := newOwnFixture(t, true)
	calls := 0
	var sent [][]string
	fx.runtime.reply = func(blob []byte) string {
		calls++
		sent = append(sent, tarImageNames(t, blob))
		// every image gets output 1: the check feeds the images differently than the training did
		return answerByClass(t, blob, fx.classes, []float64{0.05, 0.95}, []float64{0.05, 0.95})
	}
	rep, row := startFinalOwn(t, fx, finalBody)
	if rep.Verdict != NotMeasured || !rep.HoldoutNotRun {
		t.Fatalf("verdict %s, holdout_not_run %v", rep.Verdict, rep.HoldoutNotRun)
	}
	if calls != 1 || len(sent[0]) != fx.seenCount {
		t.Fatalf("%d container runs, first with %d images (want 1 with the %d seen ones)", calls, len(sent[0]), fx.seenCount)
	}
	if len(rep.Reasons) != 2 || !strings.Contains(rep.Reasons[0], "class order of the model is unclear") ||
		!strings.Contains(rep.Reasons[1], "answers output 1 for 10 of 10 images it was trained on: the check probably feeds the images differently") {
		t.Errorf("reasons: %v", rep.Reasons)
	}
	if len(rep.Suggestions) != 1 || rep.Suggestions[0].ID != "check_preprocess" {
		t.Errorf("suggestions: %+v", rep.Suggestions)
	}
	if row.HoldoutMeasured {
		t.Error("the row still counts as a measured final check")
	}
	used, err := fx.st.FinalUsed(context.Background(), fx.chat.ID, 8)
	if err != nil || used {
		t.Errorf("final used: %v %v", used, err)
	}
	// the second try is possible and goes through
	fx.runtime.reply = func(blob []byte) string {
		return answerByClass(t, blob, fx.classes, []float64{0.95, 0.05}, []float64{0.05, 0.95})
	}
	rep2, _ := startFinalOwn(t, fx, finalBody)
	if rep2.Verdict == NotMeasured || rep2.HoldoutNotRun {
		t.Fatalf("second try: %s %v", rep2.Verdict, rep2.Reasons)
	}
}

// A clear class order: the preflight lets the check through, the second run carries the hold-out images.
func TestFinalPreflightPasses(t *testing.T) {
	fx := newOwnFixture(t, true)
	var sizes []int
	fx.runtime.reply = func(blob []byte) string {
		sizes = append(sizes, len(tarImageNames(t, blob)))
		return answerByClass(t, blob, fx.classes, []float64{0.95, 0.05}, []float64{0.05, 0.95})
	}
	rep, row := startFinalOwn(t, fx, finalBody)
	if len(sizes) != 2 || sizes[0] != fx.seenCount || sizes[1] <= sizes[0] {
		t.Fatalf("container runs: %v", sizes)
	}
	if rep.Verdict == NotMeasured || rep.HoldoutNotRun || !row.HoldoutMeasured {
		t.Fatalf("report: %s %v measured %v", rep.Verdict, rep.Reasons, row.HoldoutMeasured)
	}
	// and now the final test is used up
	if used, _ := fx.st.FinalUsed(context.Background(), fx.chat.ID, 8); !used {
		t.Error("a measured final check did not use up the final test")
	}
}

// An interim check of a collapsing model stays questionable and names the second reason (NT8).
func TestInterimCollapseHint(t *testing.T) {
	fx := newOwnFixture(t, true)
	fx.runtime.reply = func(blob []byte) string {
		return answerByClass(t, blob, fx.classes, []float64{0.05, 0.95}, []float64{0.05, 0.95})
	}
	rep := startOwnCheck(t, fx.svc, fx.st, fx.chat.ID, pathBody)
	if rep.Verdict != Questionable || rep.HoldoutNotRun {
		t.Fatalf("verdict %s %v", rep.Verdict, rep.Reasons)
	}
	found := false
	for _, r := range rep.Reasons {
		if strings.Contains(r, "images it was trained on") {
			found = true
		}
	}
	if !found {
		t.Errorf("no collapse reason: %v", rep.Reasons)
	}
	for _, sg := range rep.Suggestions {
		if sg.ID == "retrain" {
			t.Errorf("retrain suggested for a model that is fed differently: %+v", rep.Suggestions)
		}
	}
}

// outputs/model.json of the run goes before the call; a different declaration adds a hint (NT6).
func TestOwnModelUsesRunMeta(t *testing.T) {
	fx := newOwnFixture(t, true)
	meta := `{"class_names":["0","1"],"preprocess":{"color":"gray","size":[28,28],"scale":0.00392156862745098}}`
	if err := fx.st.RecordNotebookFetch(context.Background(), store.NotebookFetch{ChatID: fx.chat.ID, RunID: fx.runID,
		Path: "model.onnx", SHA256: ownModelSHA, Size: int64(len(ownModelBytes)), ModelMeta: []byte(meta)}); err != nil {
		t.Fatal(err)
	}
	fx.runtime.reply = func(blob []byte) string {
		return answerByClass(t, blob, fx.classes, []float64{0.95, 0.05}, []float64{0.05, 0.95})
	}
	body := `{"path":"/workspace/notebook/r20260101120000-abcd/model.onnx","eval_dataset_id":8,"preprocess":{"color":"rgb","mean":[0.5],"std":[0.5]}}`
	rep := startOwnCheck(t, fx.svc, fx.st, fx.chat.ID, body)
	if rep.Figures.Preprocess.Source != "run" || rep.Figures.Preprocess.Color != "gray" || !rep.Figures.Preprocess.Declared {
		t.Errorf("preprocess: %+v", rep.Figures.Preprocess)
	}
	hinted := false
	for _, h := range rep.Hints {
		if strings.Contains(h, "the call declared other preprocessing than outputs/model.json of run "+fx.runID) {
			hinted = true
		}
	}
	if !hinted {
		t.Errorf("no hint about the different declaration: %v", rep.Hints)
	}
	// without a declaration in the call there is no hint
	rep = startOwnCheck(t, fx.svc, fx.st, fx.chat.ID, pathBody)
	if rep.Figures.Preprocess.Source != "run" {
		t.Errorf("source: %+v", rep.Figures.Preprocess)
	}
	for _, h := range rep.Hints {
		if strings.Contains(h, "declared other preprocessing") {
			t.Errorf("hint without a different declaration: %v", rep.Hints)
		}
	}
}

// Marking the hold-out set as not run frees the final test; two measured final checks still violate the index.
func TestFinalUsedIgnoresNotRun(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	c := testChat(t, st, "not run")
	a, err := st.CreateAssessment(ctx, store.Assessment{ChatID: c.ID, Kind: "own_model", Subject: "sha:a", EvalDatasetID: 9, Final: true})
	if err != nil {
		t.Fatal(err)
	}
	if used, _ := st.FinalUsed(ctx, c.ID, 9); !used {
		t.Fatal("a measured final check does not count")
	}
	if err := st.MarkHoldoutNotRun(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if used, _ := st.FinalUsed(ctx, c.ID, 9); used {
		t.Fatal("a check without hold-out run still counts")
	}
	b, err := st.CreateAssessment(ctx, store.Assessment{ChatID: c.ID, Kind: "own_model", Subject: "sha:b", EvalDatasetID: 9, Final: true})
	if err != nil {
		t.Fatalf("second final check after a not-run one: %v", err)
	}
	if _, err := st.CreateAssessment(ctx, store.Assessment{ChatID: c.ID, Kind: "own_model", Subject: "sha:c", EvalDatasetID: 9, Final: true}); err == nil {
		t.Fatal("two measured final checks got through the index")
	}
	_ = b
}

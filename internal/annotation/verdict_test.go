// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package annotation

import (
	"strings"
	"testing"

	"agw/internal/assess"
	"agw/internal/store"
)

// hasText reports whether s carries sub; the tests read single lines of a report with it.
func hasText(s, sub string) bool { return strings.Contains(s, sub) }

// The split of the manually annotated images is 40/20/40 and repeatable (V3b, E22): the two first of
// every five images go into the calibration set, the third into the validation set and the last two into
// the held-out set. Without random numbers two runs of the same task give the same assignment.
func TestPartForIndex(t *testing.T) {
	counts := map[string]int{}
	for i := 0; i < 100; i++ {
		counts[PartForIndex(i)]++
	}
	if counts[store.SplitCalibration] != 40 || counts[store.SplitValidation] != 20 || counts[store.SplitHoldout] != 40 {
		t.Fatalf("40/20/40 over 100 images: %v", counts)
	}
	// The rule is the same before and after a whole cycle, and a negative index (which cannot occur, but
	// must not panic) follows it as well.
	if PartForIndex(7) != PartForIndex(2) || PartForIndex(-1) != PartForIndex(4) {
		t.Errorf("the rule is not periodic: %v %v", PartForIndex(7), PartForIndex(-1))
	}
}

// obsItem describes one image of a classification: the manual class and the answered class with its
// confidence per image; an empty answer is an image the function answered nothing on.
type obsItem struct {
	frame      int
	manual     string
	answer     string
	confidence float64
}

// sameObs builds n observations of two classes the function answers right; the share of the first class
// is what makes the accuracy of a test.
func sameObs(n, right int, kind string) []Observation {
	out := make([]Observation, 0, n)
	for i := 0; i < n; i++ {
		class := "0"
		if i%2 == 1 {
			class = "1"
		}
		answer := class
		confidence := 0.9
		if i >= right {
			if class == "0" {
				answer = "1"
			} else {
				answer = "0"
			}
		}
		out = append(out, obsItem{frame: i, manual: class, answer: answer, confidence: confidence}.one(kind))
	}
	return out
}

// one turns the description of one image into an observation; a detector gets a box that stands on the
// manual object of the same class.
func (it obsItem) one(kind string) Observation {
	o := Observation{Frame: it.frame, Manual: []ManualObject{{Class: it.manual}}}
	if kind == KindDetection {
		o.Manual[0].Points = []float64{0, 0, 10, 10}
	}
	if it.answer != "" {
		a := Answer{Label: it.answer, Confidence: it.confidence}
		if kind == KindDetection {
			a.Points = []float64{0, 0, 10, 10}
		}
		o.Answers = []Answer{a}
	}
	return o
}

// verifyInput builds the input of a verdict over a classification that is measured right everywhere.
func verifyInput(measured []Observation, final bool, review bool) VerifyInput {
	return VerifyInput{
		Kind: KindClassification, Final: final, Measured: measured,
		Classes: []string{"0", "1"}, Mapping: map[string]string{"0": "0", "1": "1"}, Threshold: 0.5,
		ImageReview: assess.ImageReview{Done: review, Model: "m", Reason: imageReviewReason},
		Objects:     TaskObjects{Frames: len(measured)},
	}
}

// A final verdict rests on the held-out set, an interim one on the validation set (V3b, E22); the set is
// what the report names and what the header line carries.
func TestVerifyValidationVsHoldout(t *testing.T) {
	measured := sameObs(24, 24, KindClassification)
	rep := Verify(verifyInput(measured, false, false))
	if rep.Report.Set != assess.SetValidation || rep.Report.Final {
		t.Fatalf("interim: %+v", rep.Report)
	}
	if rep.Report.Measured != 24 || rep.Overall.Accuracy == nil || *rep.Overall.Accuracy != 1 {
		t.Errorf("figures: %+v", rep.Report)
	}
	if !hasText(rep.Report.Header, "validation images") || !hasText(rep.Report.Header, "interim") {
		t.Errorf("header of an interim report: %q", rep.Report.Header)
	}
	rep = Verify(verifyInput(measured, true, false))
	if rep.Report.Set != assess.SetHoldout || !rep.Report.Final {
		t.Fatalf("final: %+v", rep.Report)
	}
	if !hasText(rep.Report.Header, "held-out images") || !hasText(rep.Report.Header, "statistics only") {
		t.Errorf("header of a final report: %q", rep.Report.Header)
	}
}

// The figures of a classification stand per class and name the support (V3b): precision, recall and F1
// of every class, and the accuracy over the whole set.
func TestVerifyPerClassClassification(t *testing.T) {
	// 20 images, ten of class 0 and ten of class 1; one image of class 0 is answered with class 1.
	measured := []Observation{}
	for i := 0; i < 20; i++ {
		item := obsItem{frame: i, manual: "0", answer: "0", confidence: 0.9}
		if i%2 == 1 {
			item.manual, item.answer = "1", "1"
		}
		if i == 4 {
			item.answer = "1"
		}
		measured = append(measured, item.one(KindClassification))
	}
	rep := Verify(verifyInput(measured, true, false))
	if rep.Report.Verdict != assess.Usable {
		t.Fatalf("verdict: %+v (%v)", rep.Report.Verdict, rep.Report.Reasons)
	}
	if rep.Overall.Accuracy == nil || *rep.Overall.Accuracy != 0.95 {
		t.Fatalf("accuracy: %+v", rep.Overall)
	}
	class0 := rep.PerClass["0"]
	if class0.Support != 10 || class0.Recall != 0.9 {
		t.Errorf("class 0: %+v", class0)
	}
	class1 := rep.PerClass["1"]
	if class1.Support != 10 || class1.Recall != 1 || class1.Precision == nil || *class1.Precision != round(10.0/11.0) {
		t.Errorf("class 1: %+v", class1)
	}
	if len(class0.Below) != 0 || len(class1.Below) != 0 {
		t.Errorf("a class was flagged although every hit rate stands at or above the limit: %+v", rep.PerClass)
	}
}

// A class with at least three measured images and a hit rate under the limit is named in the reasons,
// and the figures that are under their limit are marked (V3b; the card paints exactly those red).
func TestVerifyPerClassBelow(t *testing.T) {
	measured := []Observation{}
	for i := 0; i < 20; i++ {
		manual, answer := "0", "0"
		if i%2 == 1 {
			manual, answer = "1", "1"
		}
		if i >= 8 {
			answer = "0" // six of the ten images of class 1 are missed
		}
		measured = append(measured, obsItem{frame: i, manual: manual, answer: answer, confidence: 0.9}.one(KindClassification))
	}
	rep := Verify(verifyInput(measured, true, false))
	if rep.Report.Verdict != assess.Questionable {
		t.Fatalf("verdict: %+v (%v)", rep.Report.Verdict, rep.Report.Reasons)
	}
	class1 := rep.PerClass["1"]
	if class1.Support != 10 || class1.Recall != 0.4 {
		t.Fatalf("class 1: %+v", class1)
	}
	if len(class1.Below) != 1 || class1.Below[0] != "recall" {
		t.Errorf("the figure under its limit is not named: %+v", class1)
	}
	if len(rep.Report.Reasons) == 0 || !hasText(rep.Report.Reasons[0], `class "1"`) {
		t.Errorf("the weak class is not named in the reasons: %v", rep.Report.Reasons)
	}
}

// The figures of a detector stand per class as well, out of the boxes that overlap a manual object at
// the threshold (V3b).
func TestVerifyPerClassDetection(t *testing.T) {
	measured := []Observation{
		obsItem{frame: 0, manual: "0", answer: "0", confidence: 0.9}.one(KindDetection),
		obsItem{frame: 1, manual: "0", answer: "0", confidence: 0.9}.one(KindDetection),
		obsItem{frame: 2, manual: "0", answer: "", confidence: 0}.one(KindDetection), // a missed object
		obsItem{frame: 3, manual: "1", answer: "1", confidence: 0.9}.one(KindDetection),
		obsItem{frame: 4, manual: "1", answer: "0", confidence: 0.9}.one(KindDetection), // a box of the wrong class
	}
	in := verifyInput(measured, true, false)
	in.Kind = KindDetection
	rep := Verify(in)
	if rep.Overall.MacroF1 == nil || rep.Overall.Accuracy != nil {
		t.Fatalf("the overall figure of a detector is the macro F1 and nothing else: %+v", rep.Overall)
	}
	class0 := rep.PerClass["0"]
	if class0.Support != 3 || class0.Recall != round(2.0/3.0) {
		t.Errorf("class 0: %+v", class0)
	}
	if class0.Precision == nil || *class0.Precision != round(2.0/3.0) {
		t.Errorf("class 0 precision counts the box of the wrong class as a false positive: %+v", class0)
	}
	class1 := rep.PerClass["1"]
	if class1.Support != 2 || class1.Recall != 0.5 || class1.Precision == nil || *class1.Precision != 1 {
		t.Errorf("class 1: %+v", class1)
	}
}

// Without an image review the verdict needs more images and a higher figure than with one (V3b): the same
// measurement is questionable without a second view and usable with it.
func TestVerdictThresholdsWithoutImageReview(t *testing.T) {
	// 20 held-out images, 16 of them right: accuracy 0.80.
	measured := sameObs(20, 16, KindClassification)
	rep := Verify(verifyInput(measured, true, false))
	if rep.Report.Verdict != assess.Questionable {
		t.Fatalf("without an image review 0.80 is not usable: %+v (%v)", rep.Report.Verdict, rep.Report.Reasons)
	}
	if len(rep.Report.Reasons) == 0 || !hasText(rep.Report.Reasons[0], "0.80") || !hasText(rep.Report.Reasons[0], "0.85") {
		t.Errorf("the reason does not name the figure and the limit: %v", rep.Report.Reasons)
	}
	// Just above the limit: usable, with a hint that it is a close call.
	rep = Verify(verifyInput(sameObs(20, 18, KindClassification), true, false))
	if rep.Report.Verdict != assess.Usable {
		t.Fatalf("0.90 is usable: %+v (%v)", rep.Report.Verdict, rep.Report.Reasons)
	}
	// Just below: still questionable.
	rep = Verify(verifyInput(sameObs(20, 16, KindClassification), true, false))
	if rep.Report.Verdict != assess.Questionable {
		t.Errorf("just below the limit: %+v", rep.Report.Verdict)
	}
}

// With an image review the same measurement passes: the second view carries part of the judgement, so the
// bounds are lower (V3b).
func TestVerdictThresholdsWithImageReview(t *testing.T) {
	measured := sameObs(20, 16, KindClassification) // accuracy 0.80
	rep := Verify(verifyInput(measured, true, true))
	if rep.Report.Verdict != assess.Usable {
		t.Fatalf("with an image review 0.80 is usable: %+v (%v)", rep.Report.Verdict, rep.Report.Reasons)
	}
	if !hasText(rep.Report.Header, "image review with m") {
		t.Errorf("the header does not name the image review: %q", rep.Report.Header)
	}
	// Below the lower bound it is questionable in both cases.
	rep = Verify(verifyInput(sameObs(20, 15, KindClassification), true, true)) // 0.75
	if rep.Report.Verdict != assess.Questionable {
		t.Errorf("0.75 with an image review: %+v", rep.Report.Verdict)
	}
	// Too many images flagged by the review: questionable even with good figures.
	in := verifyInput(sameObs(20, 20, KindClassification), true, true)
	in.Flagged = 6 // 0.30 of 20
	rep = Verify(in)
	if rep.Report.Verdict != assess.Questionable {
		t.Errorf("a review that flags a third of the images: %+v", rep.Report.Verdict)
	}
}

// Too few held-out images carry no verdict (V3b): without an image review below 20, with one below 10. The
// report names the number and the bound.
func TestVerdictNotMeasured(t *testing.T) {
	rep := Verify(verifyInput(sameObs(19, 19, KindClassification), true, false))
	if rep.Report.Verdict != assess.NotMeasured {
		t.Fatalf("19 held-out images without an image review: %+v", rep.Report.Verdict)
	}
	if len(rep.Report.Reasons) == 0 || !hasText(rep.Report.Reasons[0], "19 of at least 20") {
		t.Errorf("reason: %v", rep.Report.Reasons)
	}
	if !hasText(rep.Report.Header, "19 of at least 20") {
		t.Errorf("header: %q", rep.Report.Header)
	}
	if len(rep.Report.Hints) == 0 || !hasText(rep.Report.Hints[0], "Too few held-out images (19 of at least 20)") {
		t.Errorf("the card reads the line out of the hints: %v", rep.Report.Hints)
	}
	if len(rep.Report.Suggestions) != 1 || rep.Report.Suggestions[0].ID != SuggestionMoreManual {
		t.Errorf("suggestions: %+v", rep.Report.Suggestions)
	}
	// With an image review the bound is 10.
	rep = Verify(verifyInput(sameObs(9, 9, KindClassification), true, true))
	if rep.Report.Verdict != assess.NotMeasured || !hasText(rep.Report.Reasons[0], "9 of at least 10") {
		t.Fatalf("9 held-out images with an image review: %+v (%v)", rep.Report.Verdict, rep.Report.Reasons)
	}
	// An interim figure has its own, smaller bound; below it there is no figure either.
	rep = Verify(verifyInput(sameObs(4, 4, KindClassification), false, false))
	if rep.Report.Verdict != assess.NotMeasured || !hasText(rep.Report.Reasons[0], "4 of at least 5") {
		t.Errorf("4 validation images: %+v (%v)", rep.Report.Verdict, rep.Report.Reasons)
	}
}

// Every report says whether the images were looked at, also one that measured nothing (V3b): the agent
// must not be able to read a report as if a model had seen the pictures.
func TestImageReviewFieldAlwaysSet(t *testing.T) {
	for _, measured := range [][]Observation{sameObs(24, 24, KindClassification), sameObs(3, 3, KindClassification)} {
		rep := Verify(verifyInput(measured, true, false)).Report
		if rep.ImageReview.Done || rep.ImageReview.Reason == "" {
			t.Errorf("the report does not say why no model looked at the images: %+v", rep.ImageReview)
		}
		if !hasText(rep.Header, "no image review") {
			t.Errorf("the header does not carry it: %q", rep.Header)
		}
	}
}

// A mapping the calibration set contradicts is named as a suggestion, and the mapping it names is the one
// the calibration set gives (V3b).
func TestSuggestRemapOnSwappedMapping(t *testing.T) {
	measured := sameObs(20, 12, KindClassification)
	in := verifyInput(measured, true, false)
	in.Mapping = map[string]string{"0": "0", "1": "1"}
	in.Calibration = &Calibration{Images: 20, Mapping: map[string]string{"0": "1", "1": "0"},
		ProposedMinConfidence: 50}
	rep := Verify(in)
	if rep.Report.Verdict != assess.Questionable {
		t.Fatalf("verdict: %+v", rep.Report.Verdict)
	}
	found := false
	for _, s := range rep.Report.Suggestions {
		if s.ID == SuggestionRemap {
			found = true
			if !hasText(s.Text, `"0": "1"`) {
				t.Errorf("the suggestion does not name the mapping of the calibration set: %q", s.Text)
			}
		}
	}
	if !found {
		t.Errorf("no remap among the suggestions: %+v", rep.Report.Suggestions)
	}
	// A mapping that agrees is not named.
	in.Calibration.Mapping = map[string]string{"0": "0", "1": "1"}
	for _, s := range Verify(in).Report.Suggestions {
		if s.ID == SuggestionRemap {
			t.Errorf("a mapping that agrees was named: %q", s.Text)
		}
	}
}

// The threshold a suggestion names is the one of the calibration set, never one picked on the measured
// images (V3b): the measured set must not be used to choose a setting.
func TestOtherThresholdFromCalibrationOnly(t *testing.T) {
	measured := sameObs(20, 12, KindClassification)
	in := verifyInput(measured, true, false)
	in.Threshold = 0.5
	in.Calibration = &Calibration{Images: 20, Mapping: map[string]string{"0": "0", "1": "1"}, ProposedMinConfidence: 80}
	rep := Verify(in)
	found := false
	for _, s := range rep.Report.Suggestions {
		if s.ID == SuggestionOtherThreshold {
			found = true
			if !hasText(s.Text, "80 percent") || !hasText(s.Text, "50 percent") {
				t.Errorf("the suggestion does not name both thresholds: %q", s.Text)
			}
		}
	}
	if !found {
		t.Fatalf("no threshold suggestion: %+v", rep.Report.Suggestions)
	}
	// Without a calibration set there is no such suggestion: the measured images are not a source for it.
	in.Calibration = nil
	for _, s := range Verify(in).Report.Suggestions {
		if s.ID == SuggestionOtherThreshold || s.ID == SuggestionRemap {
			t.Errorf("a setting was suggested without a calibration set: %+v", s)
		}
	}
}

// The images of the held-out set are never named as suspicious (V3b): the verdict must not be workable
// towards by looking at exactly the images it rests on.
func TestSuspiciousNeverHoldout(t *testing.T) {
	// Two images with a speck of a box, one of them in the held-out set.
	auto := map[int][]AutoObject{
		1: {{Class: "0", Type: "rectangle", Points: []float64{0, 0, 1, 1}}},
		2: {{Class: "0", Type: "rectangle", Points: []float64{0, 0, 1, 1}}},
	}
	objects := TaskObjects{Frames: 3, Auto: auto,
		Manual:  map[int][]ManualObject{0: {{Class: "0"}}},
		Sizes:   map[int]FrameSize{1: {Width: 1000, Height: 1000}, 2: {Width: 1000, Height: 1000}},
		Holdout: map[int]bool{1: true}}
	got := suspiciousImages(objects, KindClassification)
	if len(got) != 1 || got[0].Frame != 2 {
		t.Fatalf("the held-out image was named: %+v", got)
	}
	if !hasText(got[0].Reason, "almost empty") {
		t.Errorf("reason: %q", got[0].Reason)
	}
}

// Without annotations to compare there is no plausibility, and a missing register does not look like a
// wrong class distribution (V3b).
func TestPlausibilityWithoutAnnotations(t *testing.T) {
	in := verifyInput(sameObs(20, 20, KindClassification), true, false)
	in.Objects = TaskObjects{Frames: 20}
	rep := Verify(in)
	if rep.Plausibility.Measured {
		t.Errorf("the plausibility claims to be measured: %+v", rep.Plausibility)
	}
	if rep.Report.Verdict != assess.Usable {
		t.Errorf("a missing register changed the verdict: %+v (%v)", rep.Report.Verdict, rep.Report.Reasons)
	}
}

// The plausibility names a class distribution that stands far from the manual one, and a class the manual
// annotations know and the automatic ones do not (V3b).
func TestPlausibilityReasons(t *testing.T) {
	in := verifyInput(sameObs(20, 20, KindClassification), true, false)
	in.Objects = TaskObjects{Frames: 20,
		Auto:   map[int][]AutoObject{0: {{Class: "0"}, {Class: "0"}}, 1: {{Class: "0"}, {Class: "0"}}},
		Manual: map[int][]ManualObject{2: {{Class: "1"}}, 3: {{Class: "1"}}},
	}
	rep := Verify(in)
	if !rep.Plausibility.Measured || rep.Plausibility.TVD != 1 {
		t.Fatalf("plausibility: %+v", rep.Plausibility)
	}
	var namedTVD, namedClass bool
	for _, r := range rep.Report.Reasons {
		if hasText(r, "distributed differently") {
			namedTVD = true
		}
		if hasText(r, `class "1"`) {
			namedClass = true
		}
	}
	if !namedTVD {
		t.Errorf("the class distribution was not named: %v", rep.Report.Reasons)
	}
	if !namedClass {
		t.Errorf("the missing class was not named: %v", rep.Report.Reasons)
	}
}

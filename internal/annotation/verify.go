// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package annotation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"agw/internal/assess"
	"agw/internal/store"
)

// The verification of a pre-annotation result (V3b). After a run (V1) or a fill (V3), the gateway
// measures the same function with the same mapping and the same threshold on the images the user
// annotated himself and that were held back from the calibration (E22), and judges a classification by
// its accuracy and a detector by its macro F1 — together with what the automatic annotations look like
// against the manual ones. The verdict is the answer of the agent: usable, questionable, or not
// measured when too few held-out images stand behind it.
//
// Everything here that decides is a pure function of the measurement: Verify takes the observations and
// the annotations and returns the report; no CVAT, no store, no clock. The job that measures is at the
// end of this file.

// VerifyInput is everything the verdict rests on. The measurement fills it; Verify is a pure function
// of it, so the rules are testable without CVAT and without a function.
type VerifyInput struct {
	// Kind is what was measured: a classification or a detection (KindOf).
	Kind string
	// Final: the report rests on the held-out set; false means the validation set (an interim figure).
	Final bool
	// Measured is the set the figures rest on.
	Measured []Observation
	// Classes are the classes of the dataset, in the order they are reported.
	Classes []string
	// Mapping is the mapping the judged run or fill used (function label -> dataset class). It is the
	// same mapping, never a better one: what is judged is the setting that produced the result.
	Mapping map[string]string
	// Threshold is the min_confidence of the judged run or fill, in 0..1.
	Threshold float64
	// SourceID is the run id of CVAT or the job id of the gateway that is judged; the suggestions name
	// it, so that the agent can take the automatic annotations of exactly this source back.
	SourceID string
	// Calibration is the measurement of the calibration set of the same task, with the same function
	// (nil: it was not measured). Every suggestion about a setting comes from it and never from the
	// measured set — a figure that was picked on the measured images would be no measurement at all.
	Calibration *Calibration
	// SetSizes are the sizes of the three sets of the task, for the note of the report.
	CalibrationImages int
	ValidationImages  int
	HoldoutImages     int
	// Objects is what the task carries: the annotations of the source and the manual ones.
	Objects TaskObjects
	// ImageReview says whether a model looked at the images of the measured set (V3d); without it the
	// stricter thresholds hold.
	ImageReview assess.ImageReview
	// Flagged is how many images of the measured set the image review complained about.
	Flagged int
}

// VerifyResult is what Verify decided.
type VerifyResult struct {
	// Report is the shell of the report: kind, set, size, verdict, reasons, hints, suggestions, note and
	// the header line the agent starts its reply with.
	Report assess.Report
	// PerClass and Overall are the figures of the measured set.
	PerClass map[string]assess.ClassFigures
	Overall  Overall
	// Plausibility and Suspicious are what the automatic annotations look like.
	Plausibility Plausibility
	Suspicious   []SuspiciousImage
}

// Verify measures and judges a pre-annotation result (V3b). It is a pure function: verdict, reasons,
// hints and suggestions come from the rules in verdict.go and nowhere else.
func Verify(in VerifyInput) VerifyResult {
	kind := in.Kind
	if kind != KindDetection {
		kind = KindClassification
	}
	perClass := map[string]assess.ClassFigures{}
	var overall Overall
	if kind == KindDetection {
		perClass, overall = detectionPerClass(in.Measured, in.Mapping, in.Threshold, in.Classes)
	} else {
		perClass, overall = classificationFigures(in.Measured, in.Mapping, in.Threshold, in.Classes)
	}
	plausibility := plausibilityOf(in.Objects, kind)
	suspicious := suspiciousImages(in.Objects, kind)

	set, minImages := assess.SetValidation, MinValidationImages
	if in.Final {
		set, minImages = assess.SetHoldout, MinHoldoutWithoutReview
		if in.ImageReview.Done {
			minImages = MinHoldoutWithReview
		}
	}
	// Measured is what the figures rest on: an image whose manual annotation disappeared from CVAT in
	// the meantime carries no truth and is not counted.
	counted := 0
	for _, o := range in.Measured {
		if manualClass(o) != "" {
			counted++
		}
	}
	rep := assess.NewReport(JobVerify, set, counted)
	rep.Final = in.Final
	rep.ImageReview = in.ImageReview
	rep.Note = verifyNote(in.Final, counted, in.CalibrationImages, in.ValidationImages, in.HoldoutImages)

	scoreLimit := MinScoreWithoutReview
	if in.ImageReview.Done {
		scoreLimit = MinScoreWithReview
	}
	score := scoreOf(kind, overall)

	var reasons, hints []string
	if counted < minImages {
		rep.Verdict = assess.NotMeasured
		reasons = append(reasons, tooFewImages(setName(set), counted, minImages))
		hints = append(hints, tooFewImagesLine(setName(set), counted, minImages))
		rep.Reasons, rep.Hints = reasons, hints
		rep.Suggestions = []assess.Suggestion{suggestMoreManual(counted, minImages)}
		rep.Finish()
		return VerifyResult{Report: rep, PerClass: perClass, Overall: overall,
			Plausibility: plausibility, Suspicious: suspicious}
	}

	if score < scoreLimit {
		reasons = append(reasons, scoreReason(kind, score, scoreLimit))
	}
	for _, class := range sortedClasses(perClass) {
		cf := perClass[class]
		if len(cf.Below) > 0 {
			reasons = append(reasons, flagReason(class, cf.Recall, cf.Support))
		}
	}
	if plausibility.Measured {
		limit := MaxTVD
		if !in.ImageReview.Done {
			limit = MaxTVDNoReview
		}
		if plausibility.TVD > limit {
			reasons = append(reasons, tvdReason(plausibility.TVD, limit))
		}
		for _, class := range sortedShares(plausibility.ClassShareManual) {
			share := plausibility.ClassShareManual[class]
			if share < MinClassShare {
				continue
			}
			if plausibility.ClassShareAuto[class] == 0 {
				reasons = append(reasons, missingClassReason(class, share))
			}
		}
		switch {
		case kind == KindClassification && plausibility.EmptyShareAuto > MaxEmptyShare:
			reasons = append(reasons, emptyReason(plausibility.EmptyShareAuto))
		case kind == KindDetection && math.Abs(plausibility.EmptyShareAuto-plausibility.EmptyShareManual) > MaxEmptyShare:
			reasons = append(reasons, emptyGapReason(math.Abs(plausibility.EmptyShareAuto-plausibility.EmptyShareManual),
				plausibility.EmptyShareAuto, plausibility.EmptyShareManual))
		}
		if ratio := shareRatio(plausibility.BoxesPerImageAuto, plausibility.BoxesPerImageManual); ratio != 0 {
			reasons = append(reasons, boxesReason(plausibility.BoxesPerImageAuto, plausibility.BoxesPerImageManual))
		}
		if ratio := shareRatio(plausibility.MedianRelAreaRatio, 1); ratio != 0 {
			reasons = append(reasons, areaReason(plausibility.MedianRelAreaRatio))
		}
	}
	if in.ImageReview.Done && counted > 0 && float64(in.Flagged)/float64(counted) > MaxFlaggedShare {
		reasons = append(reasons, flaggedReason(in.Flagged, counted))
	}

	if len(reasons) == 0 {
		rep.Verdict = assess.Usable
		if score-scoreLimit <= HintMargin {
			hints = append(hints, marginHint(score, scoreLimit))
		}
		if score < HintGoodScore {
			hints = append(hints, goodScoreHint(score))
		}
	} else {
		rep.Verdict = assess.Questionable
		rep.Suggestions = verifySuggestions(in, score < scoreLimit, suspicious)
	}
	rep.Reasons, rep.Hints = reasons, hints
	rep.Finish()
	return VerifyResult{Report: rep, PerClass: perClass, Overall: overall,
		Plausibility: plausibility, Suspicious: suspicious}
}

// verifySuggestions names what to do about a questionable result (V3b, P15). The agent reports them, it
// does not invent them; V3c carries out exactly these.
func verifySuggestions(in VerifyInput, weak bool, suspicious []SuspiciousImage) []assess.Suggestion {
	out := []assess.Suggestion{}
	if in.Calibration != nil {
		if !SameMapping(in.Calibration.Mapping, in.Mapping) && len(in.Calibration.Mapping) > 0 {
			out = append(out, suggestRemap(in.Calibration.Mapping))
		}
		if proposed := float64(in.Calibration.ProposedMinConfidence) / 100; math.Abs(proposed-in.Threshold) >= 0.05 {
			out = append(out, suggestOtherThreshold(proposed, in.Threshold))
		}
	}
	scope := "all"
	if len(suspicious) > 0 {
		scope = "suspicious"
	}
	out = append(out, suggestRollback(in.SourceID, scope))
	if weak {
		out = append(out, suggestRetrain())
	}
	return out
}

// scoreOf is the figure the verdict compares: the accuracy of a classification, the macro F1 of a
// detector.
func scoreOf(kind string, o Overall) float64 {
	if kind == KindDetection {
		if o.MacroF1 != nil {
			return *o.MacroF1
		}
		return 0
	}
	if o.Accuracy != nil {
		return *o.Accuracy
	}
	return 0
}

// shareRatio is the ratio of two averages when it lies outside [ShareRatioLow, ShareRatioHigh]; 0 when
// it does not or when one of them is unknown. Only from a ratio that is off does a reason follow, and a
// ratio of 0 is never a reason: an unknown figure is not a bad one.
func shareRatio(auto, manual float64) float64 {
	if auto <= 0 || manual <= 0 {
		return 0
	}
	ratio := auto / manual
	if ratio >= ShareRatioLow && ratio <= ShareRatioHigh {
		return 0
	}
	return ratio
}

// setName is the name of a set as a sentence reads it.
func setName(set string) string {
	if set == assess.SetValidation {
		return "validation"
	}
	return "held-out"
}

// scoreReason names a figure under its bound.
func scoreReason(kind string, score, limit float64) string {
	name := "accuracy"
	if kind == KindDetection {
		name = "macro F1"
	}
	return name + " " + trim(score) + " is below " + trim(limit)
}

// flaggedReason names the share of images the image review complained about.
func flaggedReason(flagged, images int) string {
	return trim(float64(flagged)/float64(images)) + " of the measured images were flagged by the image review"
}

// trim writes a figure the way a sentence reads it (two digits).
func trim(f float64) string { return fmt.Sprintf("%.2f", f) }

// ---------------------------------------------------------------------------
// The figures of a classification and of a detector
// ---------------------------------------------------------------------------

// classificationFigures counts the measured set as the figures of a classification: the manual class of
// every image against the class the function means, mapped onto the classes of the dataset. An image the
// function answers nothing on (or only classes the mapping does not name) is counted as NoAnswer, so
// that a function that answers rarely does not look accurate.
func classificationFigures(obs []Observation, mapping map[string]string, threshold float64, classes []string) (map[string]assess.ClassFigures, Overall) {
	truth := make([]string, 0, len(obs))
	pred := make([]string, 0, len(obs))
	for _, o := range obs {
		manual := manualClass(o)
		if manual == "" {
			continue // no truth on this image (its annotation is gone): it is not measurable
		}
		truth = append(truth, manual)
		pred = append(pred, predictedClass(o, mapping, threshold))
	}
	fig := assess.ClassMetrics(truth, pred, classes)
	accuracy, macroF1 := fig.Accuracy, fig.MacroF1
	return fig.PerClass, Overall{Accuracy: &accuracy, MacroF1: &macroF1}
}

// predictedClass is the class the function means for one image: the mapping of its best answer at or
// above the threshold, NoAnswer when it answers nothing there or only classes the mapping does not name.
func predictedClass(o Observation, mapping map[string]string, threshold float64) string {
	best, bestConfidence := "", math.Inf(-1)
	for _, a := range o.Answers {
		if a.Confidence < threshold {
			continue
		}
		if a.Confidence > bestConfidence {
			best, bestConfidence = a.Label, a.Confidence
		}
	}
	if class, ok := mapping[best]; ok && class != "" {
		return class
	}
	return NoAnswer
}

// detectionPerClass counts the measured set as the figures of a detector: the answered boxes at the
// threshold are matched with the manual objects by overlap (iouMatch), and every class gets its
// precision, recall and F1. An answer of a class the weight of one match is a false positive of that
// class and a missed object of the manual one — the same counting the calibration uses (V3).
func detectionPerClass(obs []Observation, mapping map[string]string, threshold float64, classes []string) (map[string]assess.ClassFigures, Overall) {
	counts := map[string]*detCounts{}
	at := func(class string) *detCounts {
		if counts[class] == nil {
			counts[class] = &detCounts{}
		}
		return counts[class]
	}
	for _, o := range obs {
		answers := make([]Answer, 0, len(o.Answers))
		for _, a := range o.Answers {
			if a.Confidence < threshold || len(a.Points) != 4 {
				continue
			}
			if _, ok := mapping[a.Label]; !ok {
				continue // a class CVAT drops is not answered at all
			}
			answers = append(answers, a)
		}
		manual := make([]ManualObject, 0, len(o.Manual))
		for _, m := range o.Manual {
			if len(m.Points) != 4 || m.Class == "" {
				continue
			}
			manual = append(manual, m)
			at(m.Class).support++
		}
		matched := matchManual(answers, manual)
		used := make([]bool, len(manual))
		for i, idx := range matched {
			class := mapping[answers[i].Label]
			if idx < 0 {
				at(class).fp++
				continue
			}
			used[idx] = true
			if class == manual[idx].Class {
				at(class).tp++
			} else {
				at(class).fp++
				at(manual[idx].Class).fn++
			}
		}
		for i, m := range manual {
			if !used[i] {
				at(m.Class).fn++
			}
		}
	}
	out := map[string]assess.ClassFigures{}
	var sum float64
	n := 0
	for _, class := range unionClasses(classes, counts) {
		count := counts[class]
		if count == nil {
			count = &detCounts{}
		}
		cf := assess.ClassFigures{Support: count.support}
		if count.tp+count.fp > 0 {
			p := float64(count.tp) / float64(count.tp+count.fp)
			cf.Precision = &p
		}
		if count.tp+count.fn > 0 {
			cf.Recall = float64(count.tp) / float64(count.tp+count.fn)
		}
		if cf.Precision != nil && *cf.Precision+cf.Recall > 0 {
			cf.F1 = 2 * *cf.Precision * cf.Recall / (*cf.Precision + cf.Recall)
		}
		if cf.Support >= assess.BelowMinImages && cf.Recall < assess.BelowRecall {
			cf.Below = []string{"recall"}
		}
		out[class] = cf
		// The macro figure goes over the classes the data has or the function answers: a class neither
		// side names would only drag the average down.
		if cf.Support > 0 || count.tp+count.fp > 0 {
			sum += cf.F1
			n++
		}
	}
	overall := Overall{}
	if n > 0 {
		macro := sum / float64(n)
		overall.MacroF1 = &macro
	}
	return out, overall
}

// detCounts counts what one class got: hits, boxes on nothing of the class, and missed objects.
type detCounts struct {
	tp, fp, fn, support int
}

// matchManual pairs the answered boxes of one image with its manual objects and names, per answer, which
// manual object it stands for (-1: none). The boxes are taken in the order of their confidence, each
// manual object at most once, from an overlap of iouMatch (V3).
func matchManual(answers []Answer, manual []ManualObject) []int {
	out := make([]int, len(answers))
	for i := range out {
		out[i] = -1
	}
	order := make([]int, 0, len(answers))
	for i, a := range answers {
		if len(a.Points) == 4 {
			order = append(order, i)
		}
	}
	sort.SliceStable(order, func(i, j int) bool { return answers[order[i]].Confidence > answers[order[j]].Confidence })
	taken := make([]bool, len(manual))
	for _, i := range order {
		best, bestIoU := -1, iouMatch
		for j, m := range manual {
			if taken[j] {
				continue
			}
			if v := iou(answers[i].Points, m.Points); v >= bestIoU {
				best, bestIoU = j, v
			}
		}
		if best < 0 {
			continue
		}
		taken[best] = true
		out[i] = best
	}
	return out
}

// unionClasses lists the classes of a report: the classes of the dataset and every class that was
// counted, sorted.
func unionClasses(classes []string, counts map[string]*detCounts) []string {
	seen := map[string]bool{}
	for _, c := range classes {
		seen[c] = true
	}
	for c := range counts {
		seen[c] = true
	}
	out := make([]string, 0, len(seen))
	for c := range seen {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// The plausibility and the suspicious images
// ---------------------------------------------------------------------------

// plausibilityOf measures what the automatic annotations look like against the manual ones of the same
// task (V3b). It rests on the register of the source and on the manual annotations, never on the
// measured set: that must stay untouched for the verdict.
func plausibilityOf(o TaskObjects, kind string) Plausibility {
	p := Plausibility{ClassShareAuto: map[string]float64{}, ClassShareManual: map[string]float64{}}
	autoCount, manualCount := map[string]int{}, map[string]int{}
	autoObjects, manualObjects := 0, 0
	for _, objs := range o.Auto {
		for _, ob := range objs {
			if ob.Class == "" {
				continue
			}
			autoCount[ob.Class]++
			autoObjects++
		}
	}
	for _, objs := range o.Manual {
		for _, ob := range objs {
			if ob.Class == "" {
				continue
			}
			manualCount[ob.Class]++
			manualObjects++
		}
	}
	p.FilledImages = framesWithObjects(o.Auto)
	p.ManualImages = framesWithManual(o.Manual)
	p.Measured = autoObjects > 0 || manualObjects > 0
	share := func(counts map[string]int, total int) map[string]float64 {
		out := map[string]float64{}
		if total == 0 {
			return out
		}
		for c, n := range counts {
			out[c] = round(float64(n) / float64(total))
		}
		return out
	}
	p.ClassShareAuto = share(autoCount, autoObjects)
	p.ClassShareManual = share(manualCount, manualObjects)
	sum := 0.0
	for c := range shareOf(autoCount, manualCount) {
		a, b := 0.0, 0.0
		if autoObjects > 0 {
			a = float64(autoCount[c]) / float64(autoObjects)
		}
		if manualObjects > 0 {
			b = float64(manualCount[c]) / float64(manualObjects)
		}
		sum += math.Abs(a - b)
	}
	p.TVD = round(sum / 2)
	empty := 0
	for f := 0; f < o.Frames; f++ {
		if len(o.Auto[f]) == 0 && len(o.Manual[f]) == 0 {
			empty++
		}
	}
	// The share of the images the source worked on that came out empty: the frames it wrote on plus the
	// ones that are still empty are the frames it was given (a fill only gets the empty ones).
	if d := p.FilledImages + empty; d > 0 {
		p.EmptyShareAuto = round(float64(empty) / float64(d))
	}
	if o.Frames > 0 {
		p.EmptyShareManual = round(1 - float64(p.ManualImages)/float64(o.Frames))
	}
	if p.FilledImages > 0 {
		p.BoxesPerImageAuto = round(float64(autoObjects) / float64(p.FilledImages))
	}
	if p.ManualImages > 0 {
		p.BoxesPerImageManual = round(float64(manualObjects) / float64(p.ManualImages))
	}
	if kind == KindDetection && len(o.Sizes) > 0 {
		auto := autoRelativeAreas(o.Auto, o.Sizes)
		manual := manualRelativeAreas(o.Manual, o.Sizes)
		if len(auto) > 0 && len(manual) > 0 {
			if m := median(manual); m > 0 {
				p.MedianRelAreaRatio = round(median(auto) / m)
			}
		}
	}
	return p
}

// shareOf is the set of the classes that occur on either side.
func shareOf(a, b map[string]int) map[string]bool {
	out := map[string]bool{}
	for c := range a {
		out[c] = true
	}
	for c := range b {
		out[c] = true
	}
	return out
}

// framesWithObjects counts the frames that carry at least one object.
func framesWithObjects(byFrame map[int][]AutoObject) int {
	n := 0
	for _, objs := range byFrame {
		if len(objs) > 0 {
			n++
		}
	}
	return n
}

// framesWithManual counts the frames that carry at least one manual object.
func framesWithManual(byFrame map[int][]ManualObject) int {
	n := 0
	for _, objs := range byFrame {
		if len(objs) > 0 {
			n++
		}
	}
	return n
}

// autoRelativeAreas lists the box areas of the automatic side in share of the image.
func autoRelativeAreas(byFrame map[int][]AutoObject, sizes map[int]FrameSize) []float64 {
	out := []float64{}
	for frame, objs := range byFrame {
		size, ok := sizes[frame]
		if !ok {
			continue
		}
		for _, ob := range objs {
			if a, ok := relativeArea(ob.Points, size); ok {
				out = append(out, a)
			}
		}
	}
	return out
}

// manualRelativeAreas lists the box areas of the manual side in share of the image.
func manualRelativeAreas(byFrame map[int][]ManualObject, sizes map[int]FrameSize) []float64 {
	out := []float64{}
	for frame, objs := range byFrame {
		size, ok := sizes[frame]
		if !ok {
			continue
		}
		for _, ob := range objs {
			if a, ok := relativeArea(ob.Points, size); ok {
				out = append(out, a)
			}
		}
	}
	return out
}

// relativeArea is the share of the image one box covers; false when the box is no box or the size is
// unknown.
func relativeArea(points []float64, size FrameSize) (float64, bool) {
	if len(points) != 4 || size.Width <= 0 || size.Height <= 0 {
		return 0, false
	}
	w := math.Abs(points[2] - points[0])
	h := math.Abs(points[3] - points[1])
	return w * h / (float64(size.Width) * float64(size.Height)), true
}

// median is the middle value of a list of figures (the lower one of two middle values, so that no
// figure is invented).
func median(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	return sorted[(len(sorted)-1)/2]
}

// suspiciousImages lists the images worth a look (V3b): a box that is almost empty or almost the whole
// image, a box far longer than wide, far more boxes than on any manually annotated image and, for a
// classification, an image the function answered nothing on. The frames of the held-out set are never
// named, so that the verdict cannot be worked towards by looking at exactly them.
func suspiciousImages(o TaskObjects, kind string) []SuspiciousImage {
	out := []SuspiciousImage{}
	maxManual := 0
	for _, objs := range o.Manual {
		n := 0
		for _, ob := range objs {
			if len(ob.Points) == 4 {
				n++
			}
		}
		if n > maxManual {
			maxManual = n
		}
	}
	frames := make([]int, 0, len(o.Auto))
	for frame := range o.Auto {
		frames = append(frames, frame)
	}
	sort.Ints(frames)
	for _, frame := range frames {
		if o.Holdout[frame] {
			continue
		}
		objs := o.Auto[frame]
		reasons := map[string]bool{}
		for _, ob := range objs {
			if len(ob.Points) != 4 {
				continue
			}
			size, known := o.Sizes[frame]
			if !known || size.Width <= 0 || size.Height <= 0 {
				continue // without the size of the image neither area nor ratio says anything
			}
			area, _ := relativeArea(ob.Points, size)
			if area < MinRelArea {
				reasons["the box is almost empty"] = true
			}
			if area > MaxRelArea {
				reasons["the box covers almost the whole image"] = true
			}
			w := math.Abs(ob.Points[2] - ob.Points[0])
			h := math.Abs(ob.Points[3] - ob.Points[1])
			if lo := math.Min(w, h); lo > 0 && math.Max(w, h)/lo > MaxAspect {
				reasons["the box is far longer than wide"] = true
			}
		}
		if maxManual > 0 && len(objs) > MaxBoxesFactor*maxManual {
			reasons["far more boxes than on any annotated image"] = true
		}
		if len(reasons) > 0 {
			out = append(out, SuspiciousImage{Frame: frame, Reason: joinReasons(reasons)})
		}
	}
	if kind == KindClassification {
		for frame := 0; frame < o.Frames; frame++ {
			if o.Holdout[frame] || len(o.Auto[frame]) > 0 || len(o.Manual[frame]) > 0 {
				continue
			}
			out = append(out, SuspiciousImage{Frame: frame, Reason: "the function answered nothing on this image"})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Reason != out[j].Reason {
			return out[i].Reason < out[j].Reason
		}
		return out[i].Frame < out[j].Frame
	})
	if len(out) > MaxSuspicious {
		out = out[:MaxSuspicious]
	}
	return out
}

// joinReasons writes the reasons of one image as one line, sorted.
func joinReasons(reasons map[string]bool) string {
	list := make([]string, 0, len(reasons))
	for r := range reasons {
		list = append(list, r)
	}
	sort.Strings(list)
	return strings.Join(list, ", ")
}

// sortedClasses lists the class keys of the figures, sorted.
func sortedClasses(perClass map[string]assess.ClassFigures) []string {
	out := make([]string, 0, len(perClass))
	for c := range perClass {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// sortedShares lists the class keys of a share table, sorted.
func sortedShares(shares map[string]float64) []string {
	out := make([]string, 0, len(shares))
	for c := range shares {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// The job of a verification
// ---------------------------------------------------------------------------

// verifyAnswer is the result of a finished verify job (V3b). It embeds the report of internal/assess, so
// that the card and the skill read it like every other check of the gateway, and carries the figures the
// pre-annotation adds on top of it.
type verifyAnswer struct {
	stepList
	assess.Report
	// SourceID is the run or the fill this measurement judged (V3c): the report names it, so that the
	// round after it knows what it compares itself with.
	SourceID          string                         `json:"source_id"`
	CalibrationImages int                            `json:"calibration_images"`
	PerClass          map[string]assess.ClassFigures `json:"per_class"`
	Overall           Overall                        `json:"overall"`
	Plausibility      Plausibility                   `json:"plausibility"`
	Suspicious        []SuspiciousImage              `json:"suspicious"`
	// Round is the correction round this measurement belongs to (V3c): 0 is the initial pre-annotation,
	// every further number a round after it. Previous and Delta carry the round before this one and what
	// changed; both stand in an interim report only, because a final verdict rests on the held-out images
	// and comparing those with the validation figures of the loop would say nothing.
	Round    int              `json:"round,omitempty"`
	Previous *RoundComparison `json:"previous,omitempty"`
	Delta    *RoundDelta      `json:"delta,omitempty"`
}

// sourceSpec is the setting a judged run or fill used (V3b). A verification measures with exactly this
// function, this mapping and this threshold: what is judged is the setting that produced the result, not
// a better one.
type sourceSpec struct {
	DatasetID     int64
	TaskID        int64
	Function      string
	Mapping       map[string]string
	MinConfidence int
}

// verifySource reads the run or the fill a verify call judges. A check has no result of its own and is
// refused; a source of another chat is not found (E8).
func (s *Service) verifySource(ctx context.Context, chatID, sourceID string) (sourceSpec, error) {
	if row, err := s.St.AnnotationRun(ctx, chatID, sourceID); err == nil {
		mapping := map[string]string{}
		if json.Unmarshal([]byte(row.Mapping), &mapping) != nil {
			mapping = map[string]string{}
		}
		return sourceSpec{DatasetID: row.DatasetID, TaskID: row.TaskID, Function: row.Function,
			Mapping: mapping, MinConfidence: row.MinConfidence}, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return sourceSpec{}, err
	}
	row, err := s.St.AnnotationJob(ctx, chatID, sourceID)
	if errors.Is(err, store.ErrNotFound) {
		return sourceSpec{}, fmt.Errorf("unknown run or job %q in this chat; a verification judges a run (annotation-run) or a fill (annotation-job)", sourceID)
	}
	if err != nil {
		return sourceSpec{}, err
	}
	if row.Kind != JobFill {
		return sourceSpec{}, fmt.Errorf("%q is a %s job; only the result of a pre-annotation run or of a fill can be judged", sourceID, row.Kind)
	}
	var spec jobSpec
	if json.Unmarshal([]byte(row.Params), &spec) != nil {
		return sourceSpec{}, errors.New("the job this verification names is not readable")
	}
	return sourceSpec{DatasetID: row.DatasetID, TaskID: row.TaskID, Function: row.Function,
		Mapping: spec.Mapping, MinConfidence: spec.MinConfidence}, nil
}

// runVerify judges the result of a run or a fill (V3b): it measures the same function with the same
// mapping and threshold on the held-out set (final) or the validation set, compares it with what the
// user annotated himself, looks at the automatic annotations of the source against the manual ones and
// reports a verdict. The measurement of the calibration set happens next to it, only so that the
// suggestions of the report come from there and never from the measured images.
func (s *Service) runVerify(ctx context.Context, jobID, chatID string, spec jobSpec) (any, error) {
	r := &recorder{chatID: chatID}
	user, err := s.user(ctx, chatID)
	if err != nil {
		return nil, err
	}
	d, err := s.ownDataset(ctx, chatID, r, spec.DatasetID)
	if err != nil {
		return nil, err
	}
	taskID, err := taskIDOf(d)
	if err != nil {
		return nil, err
	}
	ses, err := s.currentSession(ctx, chatID, r)
	if err != nil {
		return nil, err
	}
	fn, err := s.usableFunction(ctx, r, ses, user, spec.Function)
	if err != nil {
		return nil, err
	}
	t, err := s.C.Task(ctx, ses, taskID)
	r.cvat("GET /api/tasks/"+strconv.FormatInt(taskID, 10), err)
	if err != nil {
		return nil, err
	}
	labels := t.Labels
	if len(labels) == 0 {
		labels, err = s.C.Labels(ctx, ses, taskID)
		r.cvat("GET /api/labels", err)
		if err != nil {
			return nil, err
		}
	}
	classes := classesOf(labels)
	mapping, err := runMapping(spec.Mapping, fn.Labels, d.AnnotationLabels)
	if err != nil {
		return nil, err
	}
	names := mappingNames(mapping)
	ann, err := s.C.TaskAnnotations(ctx, ses, taskID)
	r.cvat("GET /api/tasks/"+strconv.FormatInt(taskID, 10)+"/annotations", err)
	if err != nil {
		return nil, err
	}
	manual := manualObjects(ann, classes)
	split, err := s.splitOf(ctx, chatID, taskID, manual)
	if err != nil {
		return nil, err
	}
	registered, err := s.St.AnnotationObjects(ctx, chatID, spec.SourceID)
	if err != nil {
		return nil, err
	}
	auto := automaticObjects(registered, ann, classes)
	holdout := map[int]bool{}
	for _, frame := range framesInPart(split, store.SplitHoldout) {
		holdout[frame] = true
	}
	meta, merr := s.C.TaskMeta(ctx, ses, taskID)
	r.cvat("GET /api/tasks/"+strconv.FormatInt(taskID, 10)+"/data/meta", merr)
	sizes := map[int]FrameSize{}
	if merr == nil {
		for frame := 0; frame < t.Size; frame++ {
			if m := meta.Frame(frame); m.Width > 0 && m.Height > 0 {
				sizes[frame] = FrameSize{Width: m.Width, Height: m.Height}
			}
		}
	}

	// The calibration set is measured next to the measured set: the suggestions of the report come from
	// it alone (V3b), never from the images the verdict rests on.
	calibFrames := sampleFrames(framesInPart(split, store.SplitCalibration), jobLimitCalibrate)
	call, err := checkMapping(jobSpec{Kind: JobCalibrate, Mapping: spec.Mapping}, fn, d.AnnotationLabels)
	if err != nil {
		return nil, err
	}
	progress := func(done, total int) { s.jobProgress(ctx, chatID, jobID, done, total) }
	calibObs, err := s.measure(ctx, r, ses, taskID, fn, calibFrames, call, manual, false, progress)
	if err != nil {
		return nil, err
	}
	part := store.SplitValidation
	if spec.Final {
		part = store.SplitHoldout
	}
	frames := sampleFrames(framesInPart(split, part), jobLimitMax)
	obs, err := s.measure(ctx, r, ses, taskID, fn, frames, call, manual, false, progress)
	if err != nil {
		return nil, err
	}
	kind := KindOf(obs)
	if kind == KindClassification {
		if k := KindOf(calibObs); k == KindDetection {
			kind = k
		}
	}
	cal := Calibrate(calibObs, kind)
	in := VerifyInput{
		Kind: kind, Final: spec.Final, Measured: obs, Classes: classNames(labels),
		Mapping: names, Threshold: float64(spec.MinConfidence) / 100, SourceID: spec.SourceID,
		Calibration:       &cal,
		CalibrationImages: len(calibFrames),
		ValidationImages:  len(framesInPart(split, store.SplitValidation)),
		HoldoutImages:     len(framesInPart(split, store.SplitHoldout)),
		Objects:           TaskObjects{Frames: t.Size, Manual: manual, Auto: auto, Sizes: sizes, Holdout: holdout},
		ImageReview:       assess.ImageReview{Done: false, Reason: imageReviewReason},
	}
	res := Verify(in)
	s.linkSuspicious(ctx, r, ses, taskID, res.Suspicious)
	answer := verifyAnswer{stepList: r.steps(), Report: res.Report, SourceID: spec.SourceID,
		CalibrationImages: in.CalibrationImages, PerClass: res.PerClass, Overall: res.Overall,
		Plausibility: res.Plausibility, Suspicious: res.Suspicious}
	// The round of the loop this measurement belongs to and the round before it (V3c): an interim report
	// carries both, so that the user sees whether a correction helped. A final verdict names the round
	// only — it rests on the held-out images, and those are compared with nothing.
	round, prev, prevClasses, err := s.roundReport(ctx, chatID, taskID)
	if err != nil {
		return nil, err
	}
	answer.Round = round
	if !spec.Final {
		answer.Previous = prev
		answer.Delta = roundDelta(prev, prevClasses, res.PerClass, res.Overall)
	}
	return answer, nil
}

// automaticObjects is the automatic side of a task as the plausibility rules read it (V3b): exactly the
// annotations the judged source wrote, taken from its register and read out of the task, so that only
// what the gateway itself registered is counted and an object the user deleted in CVAT is gone here too.
func automaticObjects(registered []store.AnnotationObject, ann Annotations, classes map[int64]string) map[int][]AutoObject {
	tags := map[int64]Tag{}
	for _, t := range ann.Tags {
		tags[t.ID] = t
	}
	shapes := map[int64]Shape{}
	for _, sh := range ann.Shapes {
		shapes[sh.ID] = sh
	}
	tracks := map[int64]Track{}
	for _, tr := range ann.Tracks {
		tracks[tr.ID] = tr
	}
	out := map[int][]AutoObject{}
	for _, row := range registered {
		ob := AutoObject{Class: className(row.LabelID, classes)}
		switch row.Kind {
		case store.ObjectTag:
			t, ok := tags[row.CVATID]
			if !ok {
				continue
			}
			ob.Type = "tag"
			ob.Points = nil
			out[t.Frame] = append(out[t.Frame], ob)
		case store.ObjectShape:
			sh, ok := shapes[row.CVATID]
			if !ok || sh.Outside {
				continue
			}
			ob.Type = sh.Type
			ob.Points = boxOfPoints(sh.Points)
			out[sh.Frame] = append(out[sh.Frame], ob)
		case store.ObjectTrack:
			tr, ok := tracks[row.CVATID]
			if !ok {
				continue
			}
			ob.Type = "rectangle"
			for _, sh := range tr.Shapes {
				source := sh.Source
				if source == "" {
					source = tr.Source
				}
				if source != sourceAuto || sh.Outside {
					continue
				}
				one := ob
				one.Points = boxOfPoints(sh.Points)
				out[sh.Frame] = append(out[sh.Frame], one)
			}
		}
	}
	return out
}

// linkSuspicious adds the file name and the CVAT link to the images worth a look (V3b). A meta or a job
// list that does not answer leaves the file name empty; the link stands either way.
func (s *Service) linkSuspicious(ctx context.Context, r *recorder, ses session, taskID int64, items []SuspiciousImage) {
	if len(items) == 0 {
		return
	}
	base := strings.TrimRight(s.PublicURL, "/") + "/tasks/" + strconv.FormatInt(taskID, 10)
	jobs, err := s.C.Jobs(ctx, ses, taskID)
	r.cvat("GET /api/jobs task "+strconv.FormatInt(taskID, 10), err)
	if err != nil {
		jobs = nil
	}
	meta, merr := s.C.TaskMeta(ctx, ses, taskID)
	r.cvat("GET /api/tasks/"+strconv.FormatInt(taskID, 10)+"/data/meta", merr)
	for i := range items {
		items[i].CVATURL = imageURL(base, jobs, items[i].Frame)
		if merr == nil {
			items[i].File = truncate(meta.Frame(items[i].Frame).Name, 120)
		}
	}
}

// classNames lists the classes of a task in the order CVAT gave them.
func classNames(labels []Label) []string {
	out := make([]string, 0, len(labels))
	for _, l := range labels {
		if l.Name != "" {
			out = append(out, l.Name)
		}
	}
	return out
}

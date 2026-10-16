// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package annotation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strconv"
	"strings"

	"agw/internal/store"
)

// The checks of V3: before a model runs over all images, the gateway measures it on the images the
// user annotated himself and proposes the class mapping and a threshold (calibrate); afterwards it
// names the images whose answer is weak (uncertain). Both work with single calls of the function
// (POST /api/lambda/functions/{id}) and with the numbers that come with them: CVAT keeps no
// confidence of a run, and the order of the classes of a platform model is not reproducible (1.5).
//
// The measurement itself is a pure function over the observations (Calibrate, Uncertain), so it can be
// tested without CVAT.

// Answer is one object a function found on one image, in the form the checks work with.
type Answer struct {
	// Label is the class as the function names it: raw_label when CVAT answers one, else the mapped
	// class of the call.
	Label string `json:"label"`
	// Confidence is the score in 0..1.
	Confidence float64 `json:"confidence"`
	// Points is the box [x1,y1,x2,y2]; empty for a classification.
	Points []float64 `json:"points,omitempty"`
}

// ManualObject is one annotation the user made himself (source != "auto").
type ManualObject struct {
	Class  string    `json:"class"`
	Points []float64 `json:"points,omitempty"`
}

// Observation is one image a check measured: what the function answered and what the user annotated.
type Observation struct {
	Frame   int            `json:"frame"`
	Answers []Answer       `json:"answers"`
	Manual  []ManualObject `json:"manual"`
}

// UncertainImage is one image whose answer is weak (V3): the numbers the function gave and the link
// into the CVAT UI, so that the user corrects exactly this image.
type UncertainImage struct {
	Frame      int     `json:"frame"`
	File       string  `json:"file,omitempty"`
	TopLabel   string  `json:"top_label"`
	Confidence float64 `json:"confidence"`
	Boxes      int     `json:"boxes"`
	CVATURL    string  `json:"cvat_url,omitempty"`
}

// Calibration is the answer of a calibration: what the function answered compared with the manual
// annotations of the user.
type Calibration struct {
	// Kind tells what was measured (classification or detection); it is not part of the answer.
	Kind string `json:"-"`
	// Images is how many manually annotated images were measured.
	Images int `json:"images"`
	// Mapping is the proposed mapping of the classes the function answered onto the classes of the
	// dataset. It comes out of the confusion table, never out of a guess.
	Mapping map[string]string `json:"mapping"`
	// Confusion counts, per class the function answered, which manual class stood on the same image.
	Confusion map[string]map[string]int `json:"confusion"`
	// Accuracy is the share of the answered images the function gets right with the proposed mapping at
	// the proposed threshold (an image it does not answer at all is not counted — Coverage says how
	// many those are); null for a detector.
	Accuracy *float64 `json:"accuracy"`
	// Precision, Recall and F1 stand at the proposed threshold; null for a classification.
	Precision *float64 `json:"precision"`
	Recall    *float64 `json:"recall"`
	F1        *float64 `json:"f1"`
	// ProposedMinConfidence is the proposed threshold in whole percent.
	ProposedMinConfidence int `json:"proposed_min_confidence"`
	// Coverage is the share of the measured images the function answers at the proposed threshold.
	Coverage float64 `json:"coverage"`
	Note     string  `json:"note"`
}

// The steps of the threshold search (V3): 10 to 90 percent in steps of 5.
const (
	thresholdLow  = 0.10
	thresholdHigh = 0.90
	thresholdStep = 0.05
	// coverageWanted is the share of the images a threshold of a classification must answer at least;
	// a threshold that answers fewer images would look good on a handful of them.
	coverageWanted = 0.80
	// iouMatch is the overlap from which an answered box counts as the same object as a manual one.
	iouMatch = 0.5
	// callThreshold is the threshold of the single call of a check: low, so that the function answers as
	// much as it can — the check decides afterwards what counts as an answer (V3).
	callThreshold = 0.01
	// decisions are the digits the figures of an answer are rounded to.
	decisions = 4
	// maxClassName bounds a class name that comes from CVAT; it is data and stays short.
	maxClassName = 64
)

// thresholdSteps are the thresholds the calibration tries, rising.
func thresholdSteps() []float64 {
	var out []float64
	for t := thresholdLow; t <= thresholdHigh+1e-9; t += thresholdStep {
		out = append(out, math.Round(t*100)/100)
	}
	return out
}

// Sample picks at most limit indices out of n, evenly spread: every k-th in the order of the images,
// the first one always in. That is reproducible (no random source) and still covers the whole task
// (V3).
func Sample(n, limit int) []int {
	if n <= 0 || limit <= 0 {
		return nil
	}
	if limit >= n {
		out := make([]int, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, i)
		}
		return out
	}
	stride := (n + limit - 1) / limit
	out := make([]int, 0, limit)
	for i := 0; i < n && len(out) < limit; i += stride {
		out = append(out, i)
	}
	return out
}

// KindOf tells a classification from a detection: an answer with a box, or a manual annotation with
// one, makes it a detection; without either it is a classification. V3 has no other signal a function
// carries (a function of the operator list names no kind).
func KindOf(obs []Observation) string {
	for _, o := range obs {
		for _, a := range o.Answers {
			if len(a.Points) > 0 {
				return KindDetection
			}
		}
		for _, m := range o.Manual {
			if len(m.Points) > 0 {
				return KindDetection
			}
		}
	}
	return KindClassification
}

// topAnswer is the object of an image with the highest confidence; ok is false when the function
// answered nothing for that image.
func topAnswer(o Observation) (Answer, bool) {
	if len(o.Answers) == 0 {
		return Answer{}, false
	}
	best := o.Answers[0]
	for _, a := range o.Answers[1:] {
		if a.Confidence > best.Confidence {
			best = a
		}
	}
	return best, true
}

// manualClass is the class of the manual annotation of a classification image: the first one in the
// order of its name. A classification image carries one tag; several would not be decidable, and a
// deterministic choice keeps the measurement repeatable.
func manualClass(o Observation) string {
	best := ""
	for _, m := range o.Manual {
		if m.Class != "" && (best == "" || m.Class < best) {
			best = m.Class
		}
	}
	return best
}

// Calibrate measures a function on the images of the observations: it builds the class mapping out of
// the confusion table and then searches the threshold — for a classification the best hit rate among
// the thresholds that still answer at least 80 percent of the images, for a detector the best F1.
func Calibrate(obs []Observation, kind string) Calibration {
	out := Calibration{Kind: kind, Images: len(obs), Mapping: map[string]string{},
		Confusion: map[string]map[string]int{}, ProposedMinConfidence: int(math.Round(thresholdLow * 100))}
	if len(obs) == 0 {
		out.Note = calibrationNote(0)
		return out
	}
	steps := thresholdSteps()
	if kind == KindDetection {
		return calibrateDetection(obs, out, steps)
	}
	return calibrateClassification(obs, out, steps)
}

// calibrateClassification is the measurement of a classification: the mapping out of the confusion of
// the top answer of every image, then the threshold with the best hit rate among the answered images —
// guarded by coverageWanted, so that a threshold is not proposed on a handful of them.
func calibrateClassification(obs []Observation, out Calibration, steps []float64) Calibration {
	out.Mapping, out.Confusion = classificationMapping(obs)
	best, bestAcc, bestCov, found := thresholdLow, 0.0, 0.0, false
	// The steps rise, so the first hit rate that is reached wins the lowest threshold.
	search := func(needCoverage bool) {
		for _, th := range steps {
			correct, covered := classificationHits(obs, out.Mapping, th)
			if covered == 0 {
				continue
			}
			cov := float64(covered) / float64(len(obs))
			if needCoverage && cov < coverageWanted {
				continue
			}
			acc := float64(correct) / float64(covered)
			if !found || acc > bestAcc+1e-9 {
				found, best, bestAcc, bestCov = true, th, acc, cov
			}
		}
	}
	search(true)
	if !found {
		// No threshold answers enough images (the function says nothing about a part of them): the best
		// hit rate over all steps then, and the coverage of the answer shows the gap.
		search(false)
	}
	out.ProposedMinConfidence = int(math.Round(best * 100))
	out.Accuracy = ptr(round(bestAcc))
	out.Coverage = round(bestCov)
	out.Note = calibrationNote(len(obs))
	return out
}

// calibrateDetection is the measurement of a detector: the mapping out of the boxes that stand on a
// manual object, then the threshold with the best F1.
func calibrateDetection(obs []Observation, out Calibration, steps []float64) Calibration {
	out.Mapping, out.Confusion = detectionMapping(obs)
	best, bestF1, found := thresholdLow, -1.0, false
	for _, th := range steps {
		p, r, f1 := detectionFigures(obs, out.Mapping, th)
		if !found || f1 > bestF1+1e-9 {
			found, best, bestF1 = true, th, f1
			out.Precision, out.Recall, out.F1 = ptr(p), ptr(r), ptr(f1)
		}
	}
	if !found {
		out.Precision, out.Recall, out.F1 = ptr(0), ptr(0), ptr(0)
	}
	out.ProposedMinConfidence = int(math.Round(best * 100))
	out.Coverage = detectionCoverage(obs, out.Mapping, best)
	out.Note = calibrationNote(len(obs))
	return out
}

// calibrationNote is the note of a calibration: it names the sample it stands on, so that neither the
// agent nor the user reads a measurement of a handful of images as a property of the model.
func calibrationNote(images int) string {
	return fmt.Sprintf("Measured on %d manually annotated image(s); a small sample, say so.", images)
}

// classificationMapping builds the confusion table of a classification and the mapping out of it: per
// class the function answered, the manual class that stood on the same image most often.
func classificationMapping(obs []Observation) (map[string]string, map[string]map[string]int) {
	confusion := map[string]map[string]int{}
	for _, o := range obs {
		a, ok := topAnswer(o)
		if !ok || a.Label == "" {
			continue
		}
		if confusion[a.Label] == nil {
			confusion[a.Label] = map[string]int{}
		}
		if manual := manualClass(o); manual != "" {
			confusion[a.Label][manual]++
		}
	}
	return mappingOf(confusion), confusion
}

// classificationHits counts how many images are right and how many the function answers at all at one
// threshold. An image without an answer is neither right nor answered.
func classificationHits(obs []Observation, mapping map[string]string, threshold float64) (correct, covered int) {
	for _, o := range obs {
		a, ok := topAnswer(o)
		if !ok || a.Confidence < threshold {
			continue
		}
		covered++
		if mapping[a.Label] != "" && mapping[a.Label] == manualClass(o) {
			correct++
		}
	}
	return correct, covered
}

// detectionMapping matches the answered boxes with the manual ones and names, per class the function
// answered, the manual class that stands on those boxes most often (V3).
func detectionMapping(obs []Observation) (map[string]string, map[string]map[string]int) {
	confusion := map[string]map[string]int{}
	for _, o := range obs {
		for _, m := range matchBoxes(o.Answers, o.Manual) {
			if confusion[m.Answer.Label] == nil {
				confusion[m.Answer.Label] = map[string]int{}
			}
			if m.Manual.Class != "" {
				confusion[m.Answer.Label][m.Manual.Class]++
			}
		}
	}
	return mappingOf(confusion), confusion
}

// mappingOf is the mapping out of a confusion table: per class the most frequent manual class.
func mappingOf(confusion map[string]map[string]int) map[string]string {
	out := map[string]string{}
	for label, counts := range confusion {
		if cls := mostFrequent(counts); cls != "" {
			out[label] = cls
		}
	}
	return out
}

// Match is one answered box and the manual object it stands for.
type Match struct {
	Answer Answer
	Manual ManualObject
}

// matchBoxes pairs the answered boxes of one image with the manual objects: greedily by confidence,
// each manual object at most once, from an overlap of iouMatch (V3).
func matchBoxes(answers []Answer, manual []ManualObject) []Match {
	boxes := make([]Answer, 0, len(answers))
	for _, a := range answers {
		if len(a.Points) == 4 {
			boxes = append(boxes, a)
		}
	}
	sort.SliceStable(boxes, func(i, j int) bool { return boxes[i].Confidence > boxes[j].Confidence })
	taken := make([]bool, len(manual))
	var out []Match
	for _, a := range boxes {
		best, bestIoU := -1, iouMatch
		for i, m := range manual {
			if taken[i] || len(m.Points) != 4 {
				continue
			}
			if v := iou(a.Points, m.Points); v >= bestIoU {
				best, bestIoU = i, v
			}
		}
		if best < 0 {
			continue
		}
		taken[best] = true
		out = append(out, Match{Answer: a, Manual: manual[best]})
	}
	return out
}

// detectionFigures counts precision, recall and F1 of one threshold over all images: an answered box
// at or above the threshold is a hit when it stands on a manual object of the same class; an answered
// box without such a manual object is a false positive, a manual object without one a false negative.
// A class without a mapping (CVAT drops it) is not counted at all.
func detectionFigures(obs []Observation, mapping map[string]string, threshold float64) (precision, recall, f1 float64) {
	var tp, fp, fn int
	for _, o := range obs {
		answers := make([]Answer, 0, len(o.Answers))
		for _, a := range o.Answers {
			if a.Confidence < threshold {
				continue
			}
			if _, ok := mapping[a.Label]; !ok {
				continue
			}
			answers = append(answers, a)
		}
		matches := matchBoxes(answers, o.Manual)
		done := make([]int, len(matches))
		for i := range done {
			done[i] = -1
		}
		for i := range matches {
			done[i] = matchIndex(o.Manual, matches[i].Manual, done)
			if mapping[matches[i].Answer.Label] == matches[i].Manual.Class {
				tp++
			} else {
				// A box of a wrong class takes the manual object with it (the greedy match) and misses it.
				fp++
				fn++
			}
		}
		fp += len(answers) - len(matches)
		for i, m := range o.Manual {
			if len(m.Points) != 4 || m.Class == "" || containsInt(done, i) {
				continue
			}
			fn++
		}
	}
	if tp+fp > 0 {
		precision = float64(tp) / float64(tp+fp)
	}
	if tp+fn > 0 {
		recall = float64(tp) / float64(tp+fn)
	}
	if precision+recall > 0 {
		f1 = 2 * precision * recall / (precision + recall)
	}
	return round(precision), round(recall), round(f1)
}

// matchIndex is the index of a manual object among the manual objects of the image, without taking one
// twice (the same object can stand several times in the list).
func matchIndex(manual []ManualObject, m ManualObject, taken []int) int {
	for i, x := range manual {
		if containsInt(taken, i) {
			continue
		}
		if x.Class == m.Class && samePoints(x.Points, m.Points) {
			return i
		}
	}
	return -1
}

// containsInt reports whether v is in list.
func containsInt(list []int, v int) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// detectionCoverage is the share of the images a detector answers at all at one threshold.
func detectionCoverage(obs []Observation, mapping map[string]string, threshold float64) float64 {
	if len(obs) == 0 {
		return 0
	}
	answered := 0
	for _, o := range obs {
		for _, a := range o.Answers {
			if a.Confidence < threshold {
				continue
			}
			if _, ok := mapping[a.Label]; ok {
				answered++
				break
			}
		}
	}
	return round(float64(answered) / float64(len(obs)))
}

// Uncertain lists the images whose answer is weak: a classification whose highest confidence stays
// under the threshold, a detection with a box under it or with no box at all. The weakest image comes
// first (V3).
func Uncertain(obs []Observation, kind string, below float64) []UncertainImage {
	out := []UncertainImage{}
	for _, o := range obs {
		if kind == KindDetection {
			boxes := make([]Answer, 0, len(o.Answers))
			for _, a := range o.Answers {
				if len(a.Points) == 4 {
					boxes = append(boxes, a)
				}
			}
			if len(boxes) == 0 {
				out = append(out, UncertainImage{Frame: o.Frame})
				continue
			}
			sort.SliceStable(boxes, func(i, j int) bool { return boxes[i].Confidence < boxes[j].Confidence })
			if low := boxes[0]; low.Confidence < below {
				out = append(out, UncertainImage{Frame: o.Frame, TopLabel: low.Label,
					Confidence: round(low.Confidence), Boxes: len(boxes)})
			}
			continue
		}
		a, ok := topAnswer(o)
		if !ok {
			out = append(out, UncertainImage{Frame: o.Frame})
			continue
		}
		if a.Confidence < below {
			out = append(out, UncertainImage{Frame: o.Frame, TopLabel: a.Label,
				Confidence: round(a.Confidence), Boxes: len(o.Answers)})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Confidence != out[j].Confidence {
			return out[i].Confidence < out[j].Confidence
		}
		return out[i].Frame < out[j].Frame
	})
	return out
}

// mostFrequent is the key with the highest count; a tie goes to the smaller key, so that a measurement
// stays repeatable.
func mostFrequent(counts map[string]int) string {
	best, bestN := "", 0
	for k, n := range counts {
		if n > bestN || (n == bestN && (best == "" || k < best)) {
			best, bestN = k, n
		}
	}
	return best
}

// iou is the overlap of two boxes [x1,y1,x2,y2]; 0 when one of them has no area.
func iou(a, b []float64) float64 {
	if len(a) != 4 || len(b) != 4 {
		return 0
	}
	ax1, ay1, ax2, ay2 := math.Min(a[0], a[2]), math.Min(a[1], a[3]), math.Max(a[0], a[2]), math.Max(a[1], a[3])
	bx1, by1, bx2, by2 := math.Min(b[0], b[2]), math.Min(b[1], b[3]), math.Max(b[0], b[2]), math.Max(b[1], b[3])
	w := math.Min(ax2, bx2) - math.Max(ax1, bx1)
	h := math.Min(ay2, by2) - math.Max(ay1, by1)
	if w <= 0 || h <= 0 {
		return 0
	}
	inter := w * h
	union := (ax2-ax1)*(ay2-ay1) + (bx2-bx1)*(by2-by1) - inter
	if union <= 0 {
		return 0
	}
	return inter / union
}

// samePoints reports whether two boxes are the same.
func samePoints(a, b []float64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ptr is the address of a figure.
func ptr(f float64) *float64 { return &f }

// round cuts a figure to the digits an answer carries; a value that is not a number becomes 0.
func round(f float64) float64 {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0
	}
	p := math.Pow(10, decisions)
	return math.Round(f*p) / p
}

// ---------------------------------------------------------------------------
// The job of a check
// ---------------------------------------------------------------------------

// calibrateAnswer is the result of a finished calibration job.
type calibrateAnswer struct {
	Calibration
}

// uncertainAnswer is the result of a finished uncertainty job; the status call cuts it into pages.
type uncertainAnswer struct {
	Checked   int              `json:"checked"`
	Uncertain []UncertainImage `json:"uncertain"`
}

// runCheck works through the images of a check job: it selects them, asks the function about each one
// and evaluates the answers.
func (s *Service) runCheck(ctx context.Context, jobID, chatID string, spec jobSpec) (any, error) {
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
	call, err := checkMapping(spec, fn, d.AnnotationLabels)
	if err != nil {
		return nil, err
	}
	ann, err := s.C.TaskAnnotations(ctx, ses, taskID)
	r.cvat("GET /api/tasks/"+strconv.FormatInt(taskID, 10)+"/annotations", err)
	if err != nil {
		return nil, err
	}
	manual := manualObjects(ann, classesOf(labels))
	frames := checkFrames(spec, t.Size, manual)
	if len(frames) == 0 {
		return nil, errors.New(noImagesText(spec))
	}
	obs := make([]Observation, 0, len(frames))
	for i, frame := range frames {
		objs, err := s.C.CallFunction(ctx, ses, fn.ID, FunctionCall{Task: taskID, Frame: frame,
			Quality: "original", Threshold: call.threshold, Mapping: call.mapping})
		r.cvat("POST /api/lambda/functions/"+fn.ID+" frame "+strconv.Itoa(frame), err)
		if err != nil {
			// A single call that fails does not silently count as "the function answered nothing": that
			// would corrupt the measurement. The job ends with what it got through.
			return partialCheck(spec, obs), fmt.Errorf("frame %d: %w", frame, err)
		}
		if spec.Kind == JobCalibrate && call.needRaw && !hasRawLabel(objs) {
			// Only a calibration holds on raw_label: it is what tells one class of the function from
			// another while the mapping of the call says nothing (V3). An uncertainty only reads scores.
			return nil, errors.New("this function answers no raw_label and no mapping was given; give mapping, so that the calibration has something to hold on (annotation-functions shows its labels)")
		}
		obs = append(obs, Observation{Frame: frame, Answers: answersOf(objs), Manual: manual[frame]})
		if (i+1)%progressEvery == 0 || i+1 == len(frames) {
			s.jobProgress(ctx, chatID, jobID, i+1, len(frames))
		}
	}
	kind := KindOf(obs)
	if spec.Kind == JobUncertain {
		items := Uncertain(obs, kind, float64(spec.UncertainBelow)/100)
		s.linkImages(ctx, r, ses, taskID, items)
		return uncertainAnswer{Checked: len(obs), Uncertain: items}, nil
	}
	cal := Calibrate(obs, kind)
	if call.synthetic {
		cal.Note += " The calls mapped every class of the function onto the first class of the dataset, so the mapping above is what the function answered on these images; have the user confirm it."
	}
	return calibrateAnswer{cal}, nil
}

// partialCheck is what a check job that broke can still report: the images it got through. Without it
// a failed job would leave the agent without any number at all.
func partialCheck(spec jobSpec, obs []Observation) any {
	if spec.Kind != JobUncertain || len(obs) == 0 {
		return nil
	}
	return uncertainAnswer{Checked: len(obs), Uncertain: Uncertain(obs, KindOf(obs), float64(spec.UncertainBelow)/100)}
}

// noImagesText names what a check found nothing to measure on.
func noImagesText(spec jobSpec) string {
	if spec.Kind == JobUncertain {
		return "the CVAT task of this dataset has no images"
	}
	return "the CVAT task of this dataset has no image with a manual annotation; annotate some images in CVAT first, or check the task (annotation-summary counts them)"
}

// linkImages adds the file name and the CVAT link of an uncertain image: the job of the frame and the
// frame itself (V3). A meta or a job list that does not answer leaves the field empty; the numbers of
// the answer stand.
func (s *Service) linkImages(ctx context.Context, r *recorder, ses session, taskID int64, items []UncertainImage) {
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

// imageURL is the link of one image in the CVAT UI: the job that carries the frame (CVAT splits a task
// into jobs), else the task itself with the frame.
func imageURL(base string, jobs []Job, frame int) string {
	for _, j := range jobs {
		if j.ID > 0 && frame >= j.StartFrame && frame <= j.StopFrame {
			return base + "/jobs/" + strconv.FormatInt(j.ID, 10) + "?frame=" + strconv.Itoa(frame)
		}
	}
	return base + "?frame=" + strconv.Itoa(frame)
}

// callMapping is the mapping one single call carries.
type callMapping struct {
	mapping map[string]Descr
	// threshold is the threshold of the call: low, so that the function answers as much as it can.
	threshold float64
	// synthetic: the gateway built the mapping (every class of the function onto the first class of the
	// dataset), so that CVAT drops nothing (views.py:326) and raw_label arrives.
	synthetic bool
	// needRaw: the function must answer raw_label, because the mapping of the call says nothing about
	// its classes (a check without a mapping given).
	needRaw bool
}

// checkMapping is the mapping of the single calls of a check. A mapping the agent gave is checked like
// the mapping of a run (I7); without one the gateway maps every class of the function onto the first
// class of the dataset, so that CVAT keeps what the function answered and the check reads the classes
// out of raw_label (V3).
func checkMapping(spec jobSpec, fn Function, classes []string) (callMapping, error) {
	if len(classes) == 0 {
		return callMapping{}, errors.New("the dataset has no classes (annotation_labels)")
	}
	if len(spec.Mapping) > 0 {
		m, err := runMapping(spec.Mapping, fn.Labels, classes)
		if err != nil {
			return callMapping{}, err
		}
		return callMapping{mapping: m, threshold: callThreshold}, nil
	}
	if len(fn.Labels) == 0 {
		return callMapping{}, errors.New("the function names no classes and no mapping was given; give mapping of the classes of the function onto the classes of the dataset")
	}
	m := map[string]Descr{}
	for _, l := range fn.Labels {
		m[l] = Descr{Name: classes[0]}
	}
	return callMapping{mapping: m, threshold: callThreshold, synthetic: true, needRaw: true}, nil
}

// checkFrames selects the images a check measures: a calibration measures the images with a manual
// annotation, an uncertainty every image of the task, each evenly spread over the task up to the limit
// (V3).
func checkFrames(spec jobSpec, size int, manual map[int][]ManualObject) []int {
	if spec.Kind == JobUncertain {
		return Sample(size, spec.Limit)
	}
	frames := make([]int, 0, len(manual))
	for frame := range manual {
		frames = append(frames, frame)
	}
	sort.Ints(frames)
	out := make([]int, 0, len(frames))
	for _, i := range Sample(len(frames), spec.Limit) {
		out = append(out, frames[i])
	}
	return out
}

// answersOf brings the objects of a single call into the form the checks work with: the class as the
// function names it (raw_label when CVAT answers one, else the mapped class) and the score in 0..1.
func answersOf(objs []FunctionObject) []Answer {
	out := make([]Answer, 0, len(objs))
	for _, o := range objs {
		label := rawLabelOf(o)
		if label == "" {
			continue
		}
		out = append(out, Answer{Label: label, Confidence: confidenceOf(o), Points: boxOf(o)})
	}
	return out
}

// rawLabelOf is the class as the function names it; CVAT rewrites label through the mapping of the
// call, raw_label survives it.
func rawLabelOf(o FunctionObject) string {
	if o.RawLabel != "" {
		return truncate(o.RawLabel, maxClassName)
	}
	return truncate(o.Label, maxClassName)
}

// hasRawLabel reports whether at least one object of an answer carries the class as the function names
// it; a function of the operator list may answer without it (V3).
func hasRawLabel(objs []FunctionObject) bool {
	for _, o := range objs {
		if o.RawLabel != "" {
			return true
		}
	}
	return false
}

// confidenceOf is the score of an object in 0..1; a value outside that range is cut, because a
// threshold is compared against it.
func confidenceOf(o FunctionObject) float64 {
	v := o.Confidence.Float()
	if math.IsNaN(v) || v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// boxOf is the box of an object: four finite numbers, else nothing (a tag carries no points).
func boxOf(o FunctionObject) []float64 {
	if len(o.Points) != 4 || !finite(o.Points) {
		return nil
	}
	return []float64{roundCoord(math.Min(o.Points[0], o.Points[2])), roundCoord(math.Min(o.Points[1], o.Points[3])),
		roundCoord(math.Max(o.Points[0], o.Points[2])), roundCoord(math.Max(o.Points[1], o.Points[3]))}
}

// finite reports whether every number of a box is a real number (not NaN and not infinite).
func finite(p []float64) bool {
	for _, v := range p {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
	}
	return true
}

// roundCoord cuts a coordinate to the digits an answer carries.
func roundCoord(v float64) float64 { return math.Round(v*100) / 100 }

// manualObjects groups the manual annotations of a task by frame: what the user made himself
// (source != "auto"). A shape that marks an object as absent counts for nothing, as everywhere else.
func manualObjects(a Annotations, classes map[int64]string) map[int][]ManualObject {
	out := map[int][]ManualObject{}
	add := func(frame int, labelID int64, points []float64) {
		out[frame] = append(out[frame], ManualObject{Class: className(labelID, classes), Points: points})
	}
	for _, t := range a.Tags {
		if t.Source == sourceAuto {
			continue
		}
		add(t.Frame, t.LabelID, nil)
	}
	for _, sh := range a.Shapes {
		if sh.Source == sourceAuto || sh.Outside {
			continue
		}
		add(sh.Frame, sh.LabelID, boxOfPoints(sh.Points))
	}
	for _, tr := range a.Tracks {
		for _, sh := range tr.Shapes {
			source := sh.Source
			if source == "" {
				source = tr.Source
			}
			if source == sourceAuto || sh.Outside {
				continue
			}
			add(sh.Frame, tr.LabelID, boxOfPoints(sh.Points))
		}
	}
	return out
}

// annotationsByFrame lists the frames that carry at least one annotation, of any source (E21): these
// are the images a fill leaves alone. A shape that marks an object as absent counts for nothing.
func annotationsByFrame(a Annotations) map[int]bool {
	out := map[int]bool{}
	for _, t := range a.Tags {
		out[t.Frame] = true
	}
	for _, sh := range a.Shapes {
		if !sh.Outside {
			out[sh.Frame] = true
		}
	}
	for _, tr := range a.Tracks {
		for _, sh := range tr.Shapes {
			if !sh.Outside {
				out[sh.Frame] = true
			}
		}
	}
	return out
}

// classesOf maps the classes of a task by the identifier CVAT gave them.
func classesOf(labels []Label) map[int64]string {
	out := make(map[int64]string, len(labels))
	for _, l := range labels {
		out[l.ID] = l.Name
	}
	return out
}

// className names the class of an annotation; a class CVAT does not name keeps its identifier.
func className(id int64, classes map[int64]string) string {
	if name, ok := classes[id]; ok && name != "" {
		return truncate(name, maxClassName)
	}
	return "#" + strconv.FormatInt(id, 10)
}

// boxOfPoints is the box of an annotation: four finite numbers, else nothing.
func boxOfPoints(p []float64) []float64 {
	if len(p) != 4 || !finite(p) {
		return nil
	}
	return p
}

// jobProgress writes how far a job is; a failure of the gateway's own bookkeeping is logged and the
// job runs on (the result of the job does not depend on it).
func (s *Service) jobProgress(ctx context.Context, chatID, jobID string, done, total int) {
	if err := s.St.SetAnnotationJobProgress(ctx, chatID, jobID, done, total); err != nil {
		slog.Warn("annotation job progress not stored", "chat", chatID, "job", jobID, "error", err)
	}
}

// ---------------------------------------------------------------------------
// The function a call may use
// ---------------------------------------------------------------------------

// usableFunction reads the function a call names and checks that this user may run it (E7): the user's
// own (V2, and only when it is ready) or one the operator installed, and a detector — only a detector
// runs over images on its own, and only a detector answers one image at a time (V1, V3).
//
// The listing of CVAT is the truth about a function that runs; the register of the gateway tells about
// one that is still building, failed or vanished (V2).
func (s *Service) usableFunction(ctx context.Context, r *recorder, ses session, user, id string) (Function, error) {
	all, err := s.C.Functions(ctx, ses)
	r.cvat("GET /api/lambda/functions", err)
	if err != nil {
		return Function{}, err
	}
	rows, err := s.St.AnnotationFunctionsOf(ctx, user)
	if err != nil {
		return Function{}, err
	}
	row, mine := findFunction(rows, id)
	var fn *Function
	prefix := FunctionNamePrefix(user)
	for i := range all {
		if all[i].ID != id {
			continue
		}
		if contains(s.Operator, all[i].ID) || strings.HasPrefix(all[i].ID, prefix) {
			fn = &all[i]
		}
		break
	}
	if mine && row.State != store.AnnotationFunctionReady && fn == nil {
		return Function{}, deny("function %q is %s; annotation_functions follows the build, a run needs state %s",
			id, row.State, store.AnnotationFunctionReady)
	}
	if fn == nil {
		return Function{}, deny("function %q is not usable: only your own functions (annotation_function) and functions installed by the operator; a function receives every image of the run", id)
	}
	if fn.Kind != "detector" {
		return Function{}, fmt.Errorf("function %q has kind %q; only a detector runs over all images of a task", fn.ID, fn.Kind)
	}
	return *fn, nil
}

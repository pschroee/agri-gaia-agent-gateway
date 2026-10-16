// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package annotation

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"agw/internal/assess"
	"agw/internal/store"
)

// The rules of the verdict on a pre-annotation result (V3b). Every number stands here once, with the
// reason it has, so that the work can name them and two reports cannot judge differently. The judgement
// itself is Verify (verify.go), a pure function of what was measured.

// PartForIndex is the set a manually annotated image belongs to, at its position among the manually
// annotated images of the task (V3b, E22). The images are sorted by frame number and counted off: the
// first two of every five go into the calibration set (40 %), the third into the validation set (20 %)
// and the last two into the held-out set (40 %). Without a random source the assignment is repeatable,
// and the table keeps it when more images are annotated later — a frame never changes its set.
func PartForIndex(index int) string {
	switch ((index % 5) + 5) % 5 {
	case 0, 1:
		return store.SplitCalibration
	case 2:
		return store.SplitValidation
	}
	return store.SplitHoldout
}

// The thresholds of the verdict (V3b). They are constants of the gateway, so that a report and the
// work around it name the same numbers.
const (
	// MinHoldoutWithoutReview is the smallest held-out set a final verdict may rest on when no model
	// has looked at the images: without a second view the figures have to carry the whole judgement,
	// and with a handful of images a hit rate says nothing.
	MinHoldoutWithoutReview = 20
	// MinHoldoutWithReview is the same bound when the images were looked at as well (V3d): the second
	// view carries part of the judgement, so the figures may rest on fewer images.
	MinHoldoutWithReview = 10
	// MinValidationImages is the smallest validation set an interim verdict rests on. It is lower than
	// the held-out bound because it names a tendency, not the result; the number is the one the result
	// checks of the gateway use (assess.MinValidationImages).
	MinValidationImages = 5
	// MinScoreWithoutReview is the score a classification (accuracy) or a detector (macro F1) must
	// reach for "usable" without an image review.
	MinScoreWithoutReview = 0.85
	// MinScoreWithReview is the same bound with an image review; it is lower for the same reason as
	// MinHoldoutWithReview. The decision table of E22/V3b names both sets.
	MinScoreWithReview = 0.80
	// MaxFlaggedShare is the share of the measured images the image review may complain about; more
	// than that and the figures rest on a model that does not fit the data.
	MaxFlaggedShare = 0.20

	// HintMargin: a score that is within this distance above its bound is named as a close call, so
	// that a usable result is not read as a comfortable one.
	HintMargin = 0.15
	// HintGoodScore: a score below this is usable but not good; the report says so as a hint.
	HintGoodScore = 0.90

	// MaxTVD is how far the class shares of the automatic annotations may stand from the manual ones
	// (total variation distance). Above it the two sets are distributed differently enough that the
	// automatic ones do not look like the user's idea of the data.
	MaxTVD = 0.25
	// MaxTVDNoReview is the same bound without an image review, where nothing else carries the
	// judgement: stricter, because a distribution that is off is the only warning left.
	MaxTVDNoReview = 0.20
	// MaxEmptyShare is the share of the images a source left empty (a classification) above which the
	// result does not look like a result.
	MaxEmptyShare = 0.20
	// MinClassShare is the share a class must have among the manual annotations before its absence
	// among the automatic ones is suspicious; below it a class may simply be rare.
	MinClassShare = 0.05
	// ShareRatioLow and ShareRatioHigh bound the ratio between the automatic and the manual side: the
	// average number of objects per annotated image and the median relative box area must lie between
	// them. A detector that draws a third of the boxes the user draws (or three times as many) does
	// not look like the same task.
	ShareRatioLow  = 0.5
	ShareRatioHigh = 2.0

	// MinRelArea and MaxRelArea bound the box a detector may draw on an image. Below the first it is a
	// speck that no user would annotate, above the second it is the whole image; both are the "wrong
	// object" case a screenshot shows at once.
	MinRelArea = 0.0005
	MaxRelArea = 0.9
	// MaxAspect is the largest ratio between the longer and the shorter side of a box; beyond it the
	// box is a line and not an object.
	MaxAspect = 10.0
	// MaxBoxesFactor: an image may carry at most this many times the number of boxes the fullest
	// manually annotated image carries.
	MaxBoxesFactor = 3
	// MaxSuspicious is how many suspicious images one report names; more would fill the chat with
	// links without being looked at.
	MaxSuspicious = 50
	// NoAnswer is the class an answered classification is compared with when the function answered
	// nothing at the threshold. It is no class of a dataset and is named in the answer, so that the
	// share of images without an answer is visible instead of silently counting as right.
	NoAnswer = "(no answer)"
)

// The suggestion identifiers of a report (V3b); the agent follows them within the compute calls that
// are left, and V3c carries them out. The identifiers are fixed, the texts belong to them.
const (
	SuggestionMoreManual     = "more_manual"
	SuggestionOtherThreshold = "other_threshold"
	SuggestionRemap          = "remap"
	SuggestionRollback       = "rollback"
	SuggestionRetrain        = "retrain"
)

// The figures of a verify report (V3b). They stand next to the verdict in the answer of the job.
type VerifyFigures struct {
	// CalibrationImages is the size of the calibration set the mapping and the threshold of the judged
	// run or fill were chosen on; it is not part of the measurement.
	CalibrationImages int `json:"calibration_images"`
	// PerClass: precision, recall, F1 and support per class of the measured set; below names the
	// figures under their limit (assess.ClassFigures).
	PerClass map[string]assess.ClassFigures `json:"per_class"`
	Overall  Overall                        `json:"overall"`
	// Plausibility: the automatic annotations against the manual ones of the same task.
	Plausibility Plausibility `json:"plausibility"`
	// Suspicious: the images worth a look, at most MaxSuspicious of them.
	Suspicious []SuspiciousImage `json:"suspicious"`
}

// Overall is the figure over the whole measured set: the accuracy of a classification, the macro F1 of
// a detector. The field that does not apply stands null, so that no report shows a zero that was never
// measured.
type Overall struct {
	Accuracy *float64 `json:"accuracy,omitempty"`
	MacroF1  *float64 `json:"macro_f1,omitempty"`
}

// Plausibility is what the automatic annotations look like against the manual ones of the same task
// (V3b). Every figure is a share or an average over the images of the task, so that a run over a
// handful of images cannot look like a run over the whole dataset.
type Plausibility struct {
	// FilledImages and ManualImages: how many images carry at least one annotation of that side.
	FilledImages int `json:"filled_images"`
	ManualImages int `json:"manual_images"`
	// ClassShareAuto and ClassShareManual: the share of every class among the annotations of that side.
	ClassShareAuto   map[string]float64 `json:"class_share_auto"`
	ClassShareManual map[string]float64 `json:"class_share_manual"`
	// TVD: how far the two distributions stand apart (half the sum of the absolute differences).
	TVD float64 `json:"tvd"`
	// EmptyShareAuto: the share of the images the source worked on that carry no automatic annotation.
	// EmptyShareManual: the share of the task the user left without an annotation.
	EmptyShareAuto   float64 `json:"empty_share_auto"`
	EmptyShareManual float64 `json:"empty_share_manual"`
	// BoxesPerImageAuto and BoxesPerImageManual: the average number of objects on an annotated image.
	BoxesPerImageAuto   float64 `json:"boxes_per_image_auto"`
	BoxesPerImageManual float64 `json:"boxes_per_image_manual"`
	// MedianRelAreaRatio: the median box area of the automatic side over the manual one, in share of
	// the image; 0 when one of the two sides has no box or the size of the images is unknown.
	MedianRelAreaRatio float64 `json:"median_rel_area_ratio"`
	// Measured: the plausibility rests on the annotations of the whole task; false when none of them
	// was registered, so that a report does not read empty figures as a measurement.
	Measured bool `json:"measured"`
}

// SuspiciousImage is one image worth a look (V3b): the frame, why it stands there and the link into
// the CVAT UI, so that the user opens exactly this image.
type SuspiciousImage struct {
	Frame  int    `json:"frame"`
	File   string `json:"file,omitempty"`
	Reason string `json:"reason"`
	// CVATURL opens the image in the CVAT UI (V3).
	CVATURL string `json:"cvat_url,omitempty"`
}

// TaskObjects is what a CVAT task carries for the plausibility rules and the suspicious images (V3b):
// the manual annotations of the user, the automatic ones of the judged run or fill, the size of its
// images and the frames of the held-out set.
type TaskObjects struct {
	// Frames is how many images the task has.
	Frames int
	Manual map[int][]ManualObject
	// Auto are the annotations the judged run or fill wrote (its register), not every automatic
	// annotation of the task: a rollback and a verdict go by what the source wrote.
	Auto map[int][]AutoObject
	// Sizes is the size of the image of a frame; a frame that is missing has no known size, and the
	// box rules that need one are left out.
	Sizes map[int]FrameSize
	// Holdout are the frames of the held-out set. They are never named as suspicious: the verdict must
	// not be workable towards through them.
	Holdout map[int]bool
}

// AutoObject is one annotation the judged run or fill wrote, as the rules read it.
type AutoObject struct {
	Class  string    `json:"class"`
	Type   string    `json:"type"`
	Points []float64 `json:"points,omitempty"`
}

// FrameSize is the size of one image of a task.
type FrameSize struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

// ---------------------------------------------------------------------------
// The reasons, hints and suggestions
// ---------------------------------------------------------------------------

// tooFewImages names a set that is too small for the figures to mean anything (V3b). The wording is
// the one of the result checks (assess.TooFewHoldout), extended by the set it names. It stands as the
// first reason of the report — the header puts it right after the dash — and again as a hint, so that
// a card that reads only the hints shows the numbers as well.
func tooFewImages(set string, n, min int) string {
	return "too few " + set + " images (" + strconv.Itoa(n) + " of at least " + strconv.Itoa(min) + ")"
}

// tooFewImagesLine is the same sentence as a hint, starting with a capital letter (V3b).
func tooFewImagesLine(set string, n, min int) string {
	line := []rune(tooFewImages(set, n, min))
	if len(line) > 0 {
		line[0] = []rune(strings.ToUpper(string(line[0])))[0]
	}
	return string(line)
}

// verifyNote is the note of a report (V3b): it names what the figures rest on and how many images each
// set of the task has, so that neither the agent nor the user reads a figure of a handful of images as
// a property of the function.
func verifyNote(final bool, measured, calibration, validation, holdout int) string {
	var b strings.Builder
	if final {
		fmt.Fprintf(&b, "Measured on %d manually annotated image(s) that were not used for calibration; the filled images may differ.", measured)
	} else {
		fmt.Fprintf(&b, "Measured on %d manually annotated image(s) held back from the calibration; an interim figure, the verdict falls on the held-out images.", measured)
	}
	fmt.Fprintf(&b, " The task has %d calibration, %d validation and %d held-out image(s).", calibration, validation, holdout)
	return b.String()
}

// flagReason is the reason a class is named in a report (V3b).
func flagReason(class string, recall float64, support int) string {
	return fmt.Sprintf("class %q hits only %.2f of its %d images", class, recall, support)
}

// tvdReason names a class distribution that stands too far from the manual one.
func tvdReason(tvd, limit float64) string {
	return fmt.Sprintf("the classes of the automatic annotations are distributed differently from the manual ones (%.2f of at most %.2f)", tvd, limit)
}

// missingClassReason names a class the manual annotations know and the automatic ones do not.
func missingClassReason(class string, share float64) string {
	return fmt.Sprintf("class %q has %.0f%% of the manual annotations and none of the automatic ones", class, share*100)
}

// emptyReason names the share of images a classification left without an answer.
func emptyReason(share float64) string {
	return fmt.Sprintf("%.0f%% of the images the function worked on carry no annotation", share*100)
}

// emptyGapReason names the gap between the automatic and the manual empty share of a detector.
func emptyGapReason(gap, empty, manual float64) string {
	return fmt.Sprintf("the function left %.0f%% of its images empty where the user left %.0f%% of the task empty", empty*100, manual*100)
}

// boxesReason names how far the average number of boxes per image stands from the manual side.
func boxesReason(auto, manual float64) string {
	return fmt.Sprintf("the function draws %.2f objects per image where the user draws %.2f", auto, manual)
}

// areaReason names how far the box size stands from the manual side.
func areaReason(ratio float64) string {
	return fmt.Sprintf("the median box area of the function is %.2f times the median of the user's boxes", ratio)
}

// marginHint names a score that sits close above its bound (V3b).
func marginHint(score, limit float64) string {
	return fmt.Sprintf("the figure %.2f is only %.2f above the limit %.2f; a handful of images more would make it firm", score, score-limit, limit)
}

// goodScoreHint names a usable figure that is not a good one (V3b).
func goodScoreHint(score float64) string {
	return fmt.Sprintf("the figure %.2f is usable but not good (%.2f and above would be)", score, HintGoodScore)
}

// suggestMoreManual names how many images more the held-out set needs (V3b, P15).
func suggestMoreManual(have, want int) assess.Suggestion {
	n := want - have
	if n < 1 {
		n = 1
	}
	return assess.Suggestion{ID: SuggestionMoreManual, Text: fmt.Sprintf(
		"annotate %d image(s) more by hand and run the pre-annotation again; a final verdict needs at least %d held-out images, this task has %d",
		n, want, have)}
}

// suggestOtherThreshold names the threshold the calibration set answers best at (V3b). It comes from
// the calibration set alone: the measured set must not be used to pick a setting.
func suggestOtherThreshold(proposed float64, used float64) assess.Suggestion {
	return assess.Suggestion{ID: SuggestionOtherThreshold, Text: fmt.Sprintf(
		"the calibration set of this task answers best at a threshold of %d percent; the judged run used %d percent",
		int(proposed*100+0.5), int(used*100+0.5))}
}

// suggestRemap names the mapping the calibration set gives, when it differs from the one used (V3b).
func suggestRemap(mapping map[string]string) assess.Suggestion {
	return assess.Suggestion{ID: SuggestionRemap, Text: "the classes of the function seem to stand in another order; the calibration set of this task gives the mapping " +
		mappingText(mapping) + " — have the user confirm it before the next run"}
}

// suggestRollback names taking the automatic annotations back (V3b).
func suggestRollback(sourceID, scope string) assess.Suggestion {
	return assess.Suggestion{ID: SuggestionRollback, Text: fmt.Sprintf(
		"take the automatic annotations back (annotation-rollback --source-id %s --scope %s) and fill again with the corrected setting; what the user annotated by hand stays",
		sourceID, scope)}
}

// suggestRetrain names the way back through a better model (V3b).
func suggestRetrain() assess.Suggestion {
	return assess.Suggestion{ID: SuggestionRetrain, Text: "train a model on the corrected images, build a function from it (annotation-function) and check it again before the next fill"}
}

// mappingText writes a mapping as one sorted line, so that two reports of the same mapping read alike.
func mappingText(mapping map[string]string) string {
	if len(mapping) == 0 {
		return "{}"
	}
	keys := make([]string, 0, len(mapping))
	for k := range mapping {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%q: %q", k, mapping[k])
	}
	b.WriteByte('}')
	return b.String()
}

// SameMapping reports whether two mappings name the same pairs; the mapping of a call and the mapping
// of a report are two maps and comparing them needs this.
func SameMapping(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package assess

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"agw/internal/platform"
	"agw/internal/store"
)

// The training check (R1): after a training with a platform template the gateway measures the model the
// user took over on the images that training did not see. It reads the model and the training log, loads
// the datasets the model could have seen (P2, from model_origins), keeps every image back whose SHA-256
// occurs in one of them, and judges the figures. The agent reports the verdict; it never computes one.
// The rules of the verdict, the hints and the suggestions sit in JudgeClassification, a pure function.

// KindTrainingModel is the kind of a check on a model that came out of a training (R1).
const KindTrainingModel = "training_model"

// The thresholds of the verdict (R1.9) and of the hints (R1.10). They are constants of the gateway, so
// that the work can name them and every report uses the same ones.
const (
	// MinValidationImages is the smallest validation set a comparison of rounds rests on.
	MinValidationImages = 5
	// MinGainOverBaseline: how far the accuracy must be above the majority baseline.
	MinGainOverBaseline = 0.10
	// MaxPValue: the one-sided binomial test "better than the majority baseline" must stay below it.
	MaxPValue = 0.05
	// GoodAccuracy: below it a usable result still carries a hint (a verdict is not a quality mark).
	GoodAccuracy = 0.80
	// MaxSeenGap: a wider gap between the accuracy on seen images and on the measured set hints at
	// overfitting.
	MaxSeenGap = 0.20
	// MaxPlatformGap: a wider gap to the platform's own figure points at another preprocessing or a
	// different class order.
	MaxPlatformGap = 0.15
	// SeenSamplePerClass is how many seen images per class the class mapping is derived from (R1.8).
	SeenSamplePerClass = 10
)

// The reasons of a not measured training check (L15, L16). They begin small: the header puts the first
// one right after the dash (W3).
const (
	// ReasonOriginUnknown: the gateway did not see the model being created in this chat (P2).
	ReasonOriginUnknown = "origin unknown: the gateway did not see this model being created in this chat"
	// ReasonNotClassification: the model's output is no classification (detectors, segmentations, P8).
	ReasonNotClassification = "not a classification model"
	// ReasonNotONNX: without an ONNX export the platform would have to build the container from PyTorch.
	ReasonNotONNX = "not an ONNX model: the platform saved no ONNX export"
	// ReasonNoEvalDataset: the gateway does not know which dataset to measure on.
	ReasonNoEvalDataset = "no evaluation dataset: the gateway does not know which dataset the model was trained on; name one with eval_dataset_id"
	// ReasonDeclaredClassOrder: the class order the agent declared for its own model (R2.4) contradicts
	// the order the gateway derived from the images the model saw.
	ReasonDeclaredClassOrder = "declared class order differs from the order the model shows on training images"
	// ReasonOutputLabelsPrefix begins the reason of a training model whose output_labels name the
	// outputs in another order than the model shows; the deployment check finds it by this prefix.
	ReasonOutputLabelsPrefix = "the output_labels of the model"
)

// OutputLabelsOrder is the reason of a training model whose recorded output_labels contradict the
// order its outputs show on the images it saw.
func OutputLabelsOrder(labels []string, m map[int]string) string {
	return fmt.Sprintf("%s (%s) name the outputs in another order than the model shows on the images it saw (%s)",
		ReasonOutputLabelsPrefix, jsonList(labels), jsonList(orderedLabels(m)))
}

// orderedLabels lists the class of every output in index order (0 ... n-1); a gap stays empty.
func orderedLabels(m map[int]string) []string {
	n := 0
	for i := range m {
		if i+1 > n {
			n = i + 1
		}
	}
	out := make([]string, n)
	for i, c := range m {
		if i >= 0 {
			out[i] = c
		}
	}
	return out
}

// jsonList renders a list of strings as a JSON array.
func jsonList(l []string) string {
	b, err := json.Marshal(l)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// fixOutputLabels is the suggestion that names the labels the model has to carry.
func fixOutputLabels(m map[int]string) Suggestion {
	return Suggestion{ID: "fix_output_labels",
		Text: "set output_labels to " + jsonList(orderedLabels(m)) + " with {{tool:update_model}}, then build any inference image of this model again ({{tool:build_inference_image}}): the image carries the labels"}
}

// outputLabelsAbsentHint is the hint for a training model that carries no output_labels.
func outputLabelsAbsentHint(m map[int]string) string {
	return "the model has no output_labels; in the order the model shows they are " + jsonList(orderedLabels(m)) +
		" (set them with {{tool:update_model}})"
}

// outputLabelsWrong reports whether the check of a training model found its output_labels in another
// order than the outputs show.
func outputLabelsWrong(in JudgeInput) bool {
	return in.Kind == KindTrainingModel && in.MappingOK && len(in.OutputLabels) > 0 &&
		declaredOrderDiffers(in.Mapping, in.OutputLabels)
}

// TooFewValidation is the reason of a not measured report with too few validation images (L16).
func TooFewValidation(n int) string {
	return "too few validation images (" + itoa(n) + " of at least " + itoa(MinValidationImages) + ")"
}

// NoHeldOut is the reason of a not measured report whose evaluation dataset holds no image the model did
// not see: the model was trained on all of them (L15).
func NoHeldOut(dataset string) string {
	return "no held-out images: the model was trained on all images of " + dataset
}

// --- the figures of a training check ---

// SubjectRef names a platform object of the report (a model).
type SubjectRef struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// DatasetRef names a dataset of the report.
type DatasetRef struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// OverallFigures are the figures over the whole measured set.
type OverallFigures struct {
	Accuracy float64 `json:"accuracy"`
	MacroF1  float64 `json:"macro_f1"`
	// Correct and Wrong: the number of images the model got right and wrong in the measured set (#254). The agent
	// reads the count here instead of deriving it from precision and recall, which misleads.
	Correct int `json:"correct"`
	Wrong   int `json:"wrong"`
}

// overallFigures builds the overall figures of a measurement. The counts come from the confusion table when there is
// one (its diagonal is right, the rest wrong); without it, from the accuracy and the number of measured images.
func overallFigures(in JudgeInput) OverallFigures {
	o := OverallFigures{Accuracy: in.Accuracy, MacroF1: in.MacroF1}
	total, correct := 0, 0
	for truth, row := range in.Confusion {
		for pred, n := range row {
			total += n
			if pred == truth {
				correct += n
			}
		}
	}
	if total == 0 {
		total = measuredImages(in)
		correct = int(math.Round(in.Accuracy * float64(total)))
	}
	o.Correct, o.Wrong = correct, total-correct
	return o
}

// BaselineFigures are the figures of the majority class applied to the measured set (P10).
type BaselineFigures struct {
	Kind     string  `json:"kind"`
	Class    string  `json:"class"`
	Accuracy float64 `json:"accuracy"`
	MacroF1  float64 `json:"macro_f1"`
	// PValue: the tail probability of the one-sided binomial test "better than this baseline".
	PValue float64 `json:"p_value"`
}

// SeenSample is what the model reaches on the images it saw (the sample the class order came from).
type SeenSample struct {
	Images   int     `json:"images"`
	Accuracy float64 `json:"accuracy"`
}

// CheckComparison is the previous check a report compares itself with (R1.12).
type CheckComparison struct {
	CheckID  string     `json:"check_id"`
	Model    SubjectRef `json:"model"`
	Verdict  Verdict    `json:"verdict"`
	Accuracy float64    `json:"accuracy"`
	MacroF1  float64    `json:"macro_f1"`
}

// TrainingFigures is the figures of a training check ("figures im Bericht", R1). platform_figure is
// absent when the log was not read at all (an unknown origin) and null when it was read and carries no
// figure (H114).
type TrainingFigures struct {
	Model          SubjectRef              `json:"model"`
	EvalDataset    DatasetRef              `json:"eval_dataset"`
	SeenDatasets   []DatasetRef            `json:"seen_datasets"`
	MeasuredImages int                     `json:"measured_images"`
	PerClass       map[string]ClassFigures `json:"per_class"`
	Overall        OverallFigures          `json:"overall"`
	Baseline       BaselineFigures         `json:"baseline"`
	SeenSample     SeenSample              `json:"seen_sample"`
	// Confusion: truth -> prediction -> number of images over the classes of the data, zero cells
	// included. It is for the check card only: the agent's route of the check strips it (#248).
	Confusion map[string]map[string]int `json:"confusion,omitempty"`
	// Mapping: the class every output of the model means, as the gateway derived it (P9).
	Mapping map[string]string `json:"mapping"`
	// OutputLabels: the output_labels of the platform record of the model (training models only).
	OutputLabels []string `json:"output_labels,omitempty"`
	// PlatformFigure: what the template measured itself; nil: the log carried no figure.
	PlatformFigure *PlatformFigure `json:"platform_figure"`
	// LogNaN: the training log names nan losses (R1.13).
	LogNaN bool `json:"log_nan,omitempty"`
	// Previous and Delta: the last interim check of the same dataset in this chat and the change.
	Previous *CheckComparison   `json:"previous,omitempty"`
	Delta    map[string]float64 `json:"delta,omitempty"`
}

// JudgeInput is everything the judgement rests on (R1.9). The measurement fills it; JudgeClassification
// is a pure function of it, so the rules are testable without a platform, a container or a model.
type JudgeInput struct {
	Kind         string
	Final        bool
	Model        SubjectRef
	EvalDataset  DatasetRef
	SeenDatasets []DatasetRef
	// ValidationImages and HoldoutImages: the sizes of the two held-out sets (P3).
	ValidationImages int
	HoldoutImages    int
	// NoHeldOut: the evaluation dataset holds no image the model did not see.
	NoHeldOut bool
	// NoHeldOutReason: what to say instead of the training wording when NoHeldOut (R2: the notebook
	// server of this chat received every image of the dataset). Empty: the training wording.
	NoHeldOutReason string
	// NotMeasuredReason: a reason of an own-model check (R2.5) that was decided from the file itself —
	// it does not load, or it does not take exactly one input. It makes the verdict not measured,
	// whatever the figures say.
	NotMeasuredReason string
	// ConstantOutput: the model answered the same for different images (R2.5): one more reason.
	ConstantOutput bool
	// NotClassification: the output of the model is neither [1, C] nor [C] (P8).
	NotClassification bool
	// OutputShape: what the model answered, for the reason.
	OutputShape string
	// Outputs: the number of outputs the model has (0: no run happened). It must be the number of
	// classes in the data (R1.9).
	Outputs int
	// Counted: the pairs of truth and prediction the figures rest on (0: nothing was counted, e.g.
	// because no class order could be derived).
	Counted int
	// Classes: the classes of the data.
	Classes  []string
	PerClass map[string]ClassFigures
	// Confusion: truth -> prediction -> number of images (see TrainingFigures.Confusion).
	Confusion map[string]map[string]int
	Accuracy  float64
	MacroF1   float64
	Baseline  Baseline
	PValue    float64
	// Mapping and MappingOK: the class every output means, derived from seen images (R0.6, P9).
	Mapping       map[int]string
	MappingOK     bool
	MappingReason string
	// DeclaredClassNames: the class names the agent declared for its own model, in the order of the
	// model's outputs (R2.4). Empty: none were declared, and then nothing is compared.
	DeclaredClassNames []string
	// OutputLabels: the output_labels of the platform record of a training model, in output order.
	OutputLabels []string
	// NonFinite: at least one prediction of the measured set was not a finite number.
	NonFinite bool
	// SkippedPredictions: how many images of the measured set the model could not run.
	SkippedPredictions int
	// SeenImages and SeenAccuracy: what the model reaches on the sample of images it saw.
	SeenImages   int
	SeenAccuracy float64
	// PlatformFigure: the figure the template printed itself (nil: the log carried none).
	PlatformFigure *PlatformFigure
	LogNaN         bool
	// MissingClass: a class of the training data that does not occur in the measured set.
	MissingClass string
	// ImageReview: what stands in the report about the image review before the judgement runs (R4). The
	// zero value: no review was decided yet, and judgeReport fills the default reason. The measurement
	// overwrites it with the review's own result.
	ImageReview ImageReview
	// Previous: the last interim check of the same dataset in this chat (R1.12).
	Previous *CheckComparison
	// Collapse: the model answered one output for nearly all seen images while the class order is unclear
	// (#181); nil otherwise. PreSummary: the preprocessing of an own model for the reason (empty for a
	// platform template).
	Collapse   *Collapse
	PreSummary string
}

// JudgeClassification turns a measurement into the report of a training check (R1.9–R1.11). It is a
// pure function: verdict, reasons, hints and suggestions come from the rules here and nowhere else. The
// caller sets Kind, Note and Finish; the figures of the report come from this function.
func JudgeClassification(in JudgeInput) Report {
	if in.PerClass == nil {
		in.PerClass = map[string]ClassFigures{}
	}
	return judgeReport(in, &TrainingFigures{
		Model: in.Model, EvalDataset: in.EvalDataset, SeenDatasets: in.SeenDatasets,
		MeasuredImages: measuredImages(in), PerClass: in.PerClass, Confusion: in.Confusion,
		Overall:      overallFigures(in),
		Baseline:     BaselineFigures{Kind: "majority", Class: in.Baseline.Class, Accuracy: in.Baseline.Accuracy, MacroF1: in.Baseline.MacroF1, PValue: in.PValue},
		SeenSample:   SeenSample{Images: in.SeenImages, Accuracy: in.SeenAccuracy},
		Mapping:      mappingNames(in.Mapping),
		OutputLabels: in.OutputLabels,
		// PlatformFigure stays a typed nil when the log held none: the key is written as null.
		PlatformFigure: in.PlatformFigure,
		LogNaN:         in.LogNaN,
	})
}

// ModelFigures are the figures of a model check. A training check (R1) and an own-model check (R2)
// carry different ones; the rules over them are the same, so judgeReport takes them as they are. Both
// keep the comparison with the previous round of the same dataset (R1.12).
type ModelFigures interface {
	setComparison(prev *CheckComparison, delta map[string]float64)
}

func (f *TrainingFigures) setComparison(prev *CheckComparison, delta map[string]float64) {
	f.Previous, f.Delta = prev, delta
}

// measuredImages is the number of images the figures of a check rest on: the hold-out set of a final
// check, the validation set otherwise (P3).
func measuredImages(in JudgeInput) int {
	if in.Final {
		return in.HoldoutImages
	}
	return in.ValidationImages
}

// judgeReport is the one rule set of a model check (R1.9–R1.11, R2.4): the measurement is in, the
// figures of its kind are in, and what comes back is the verdict with its reasons, hints and
// suggestions. It never touches the measurement.
func judgeReport(in JudgeInput, figures ModelFigures) Report {
	set := SetValidation
	if in.Final {
		set = SetHoldout
	}
	rep := NewReport(in.Kind, set, measuredImages(in))
	rep.Final = in.Final
	rep.ImageReview = imageReviewOf(in)
	rep.Figures = figures

	// Not measured: there is nothing to judge on (R1.9).
	switch {
	case in.NotMeasuredReason != "":
		// An own-model check that found out about the file itself that it cannot be measured (R2.5): the
		// file does not load, or it takes no single image input.
		rep.Verdict = NotMeasured
		rep.Reasons = []string{in.NotMeasuredReason}
		return rep
	case in.NoHeldOut:
		rep.Verdict = NotMeasured
		reason := in.NoHeldOutReason
		if reason == "" {
			reason = NoHeldOut(in.EvalDataset.Name)
		}
		rep.Reasons = []string{reason}
		rep.Suggestions = []Suggestion{suggestionSplitDataset}
		return rep
	case in.NotClassification:
		rep.Verdict = NotMeasured
		reason := ReasonNotClassification
		if in.OutputShape != "" {
			reason += ": the model answered output shape " + in.OutputShape
		}
		rep.Reasons = []string{reason}
		return rep
	case in.ValidationImages < MinValidationImages || in.HoldoutImages < MinHoldoutImages:
		rep.Verdict = NotMeasured
		if in.ValidationImages < MinValidationImages {
			rep.Reasons = append(rep.Reasons, TooFewValidation(in.ValidationImages))
		}
		if in.HoldoutImages < MinHoldoutImages {
			rep.Reasons = append(rep.Reasons, TooFewHoldout(in.HoldoutImages))
		}
		rep.Suggestions = []Suggestion{suggestionMoreData}
		return rep
	}

	// Questionable: one reason per rule that does not hold (R1.9).
	var reasons []string
	if !in.MappingOK {
		reasons = append(reasons, ClassOrderUnclear(in.MappingReason))
		if in.Collapse != nil {
			reasons = append(reasons, collapseReason(*in.Collapse, in.PreSummary))
		}
	} else if declaredOrderDiffers(in.Mapping, in.DeclaredClassNames) {
		// An own model (R2.4): the order the agent declared contradicts the one the model shows. The
		// gateway keeps its own order; the difference is what the user has to know.
		reasons = append(reasons, ReasonDeclaredClassOrder)
	} else if outputLabelsWrong(in) {
		// A training model: the labels the platform recorded contradict the order the model shows. A
		// deployment of it answers with wrong names, so this makes the verdict questionable.
		reasons = append(reasons, OutputLabelsOrder(in.OutputLabels, in.Mapping))
	}
	if in.Outputs > 0 && len(in.Classes) > 0 && in.Outputs != len(in.Classes) {
		reasons = append(reasons, fmt.Sprintf("the model has %d outputs for %d classes", in.Outputs, len(in.Classes)))
	}
	if in.NonFinite {
		reasons = append(reasons, "the model answered values that are not finite numbers")
	}
	if in.ConstantOutput {
		reasons = append(reasons, ReasonConstantOutput)
	}
	if in.Counted == 0 {
		// Without a counted pair the figures say nothing: that is no usable result, whatever the reason
		// for it is.
		if in.MappingOK {
			reasons = append(reasons, "no image of the measured set could be counted")
		}
	} else {
		if in.Accuracy < in.Baseline.Accuracy+MinGainOverBaseline-minImprovementEpsilon {
			reasons = append(reasons, fmt.Sprintf("the accuracy %.2f is not at least %.2f above the majority baseline %.2f",
				in.Accuracy, MinGainOverBaseline, in.Baseline.Accuracy))
		}
		if in.PValue >= MaxPValue {
			reasons = append(reasons, fmt.Sprintf("not better than the majority baseline with less than %g probability by chance (p = %.2g)",
				MaxPValue, in.PValue))
		}
		reasons = append(reasons, weakClasses(in.PerClass)...)
	}
	if len(reasons) == 0 {
		rep.Verdict = Usable
	} else {
		rep.Verdict = Questionable
		rep.Reasons = reasons
	}

	// Hints change nothing about the verdict (R1.10); the suggestions name what to do next (R1.11).
	rep.Hints = hints(in, rep.Verdict)
	rep.Suggestions = suggestions(in, rep.Verdict)
	if in.Previous != nil {
		figures.setComparison(in.Previous, deltaOf(in, in.Previous))
	}
	return rep
}

// declaredOrderDiffers reports whether the class order the agent declared for its own model (R2.4)
// contradicts the order the gateway derived from images the model saw. classNames[i] is what the agent
// says the output i means; nothing declared (or nothing derived): nothing differs. A list that does not
// name as many classes as the model has outputs differs — that is a statement about the outputs.
func declaredOrderDiffers(derived map[int]string, classNames []string) bool {
	if len(classNames) == 0 || len(derived) == 0 {
		return false
	}
	if len(classNames) != len(derived) {
		return true
	}
	for i, name := range classNames {
		if got, ok := derived[i]; !ok || got != name {
			return true
		}
	}
	return false
}

// weakClasses names the classes whose hit rate is below the limit (R1.9); each names one reason.
func weakClasses(perClass map[string]ClassFigures) []string {
	names := make([]string, 0, len(perClass))
	for c := range perClass {
		names = append(names, c)
	}
	sort.Strings(names)
	var out []string
	for _, c := range names {
		f := perClass[c]
		if f.Support >= BelowMinImages && f.Recall < BelowRecall {
			out = append(out, fmt.Sprintf("the model finds only %.0f%% of the images of class %q (below %.2f)",
				f.Recall*100, c, BelowRecall))
		}
	}
	return out
}

// hints are the observations that do not change the verdict (R1.10).
func hints(in JudgeInput, verdict Verdict) []string {
	var out []string
	if in.Kind == KindTrainingModel && in.MappingOK && len(in.OutputLabels) == 0 && len(in.Mapping) > 0 {
		out = append(out, outputLabelsAbsentHint(in.Mapping))
	}
	if in.MissingClass != "" {
		out = append(out, fmt.Sprintf("class %q of the training data does not occur in the measured images", in.MissingClass))
	}
	if in.Counted > 0 && in.SeenImages > 0 && in.SeenAccuracy-in.Accuracy > MaxSeenGap {
		out = append(out, fmt.Sprintf("the model reaches %.2f on the images it saw and %.2f on the measured ones: likely overfitting",
			in.SeenAccuracy, in.Accuracy))
	}
	if in.Counted > 0 && in.PlatformFigure != nil && abs(in.PlatformFigure.Value-in.Accuracy) > MaxPlatformGap {
		out = append(out, fmt.Sprintf("the platform's figure %.2f differs from the measured accuracy %.2f: check preprocessing and class order",
			in.PlatformFigure.Value, in.Accuracy))
	}
	if in.LogNaN {
		out = append(out, "the training log names nan losses: the training did not converge")
	}
	if verdict == Usable && in.Accuracy < GoodAccuracy {
		out = append(out, "usable means clearly better than the baseline; whether it is good enough is the user's decision")
	}
	return out
}

// suggestions are the steps the gateway names for a result that is not usable (R1.11, P15). The agent
// follows them within the compute calls that are left or proposes them to the user.
func suggestions(in JudgeInput, verdict Verdict) []Suggestion {
	if verdict == NotMeasured {
		return nil
	}
	var out []Suggestion
	if outputLabelsWrong(in) {
		out = append(out, fixOutputLabels(in.Mapping))
	}
	if in.Counted > 0 && in.SeenImages > 0 && in.SeenAccuracy < GoodAccuracy {
		out = append(out, suggestionMoreEpochs)
	}
	// The gap decides (R1.11), also for a usable result: the hint names the overfitting either way.
	if in.Counted > 0 && in.SeenImages > 0 && in.SeenAccuracy-in.Accuracy > MaxSeenGap {
		out = append(out, suggestionRegularize)
	}
	if in.Counted > 0 && in.PlatformFigure != nil && abs(in.PlatformFigure.Value-in.Accuracy) > MaxPlatformGap {
		out = append(out, suggestionCheckPreprocessing)
	}
	if verdict != Questionable {
		return out
	}
	if in.Previous == nil {
		// No retraining was checked for this dataset in this chat yet (R1.11).
		return append(out, suggestionRetrain)
	}
	if in.Previous.Verdict == Questionable {
		out = append(out, suggestionOtherArchitecture)
	}
	if ok, msg := MayContinue(in.Previous.Accuracy, in.Accuracy, 2); !ok && msg != "" {
		// The stop of the loop (R0.8): the gateway names it, the agent does not judge it.
		out = append(out, Suggestion{ID: "stop", Text: msg})
	}
	return out
}

// The suggestions of a training check (R1.11); the identifiers are fixed, the texts belong to them.
var (
	suggestionSplitDataset = Suggestion{ID: "split_dataset",
		Text: "split the dataset before the next training ({{tool:split_dataset}}) so that the result can be checked on held-out images"}
	suggestionMoreData = Suggestion{ID: "more_data",
		Text: "the check needs at least 20 held-out and 5 validation images: provide more annotated images and split again"}
	suggestionMoreEpochs = Suggestion{ID: "more_epochs",
		Text: "the model does not even reach 0.80 on the images it saw (underfitting): train once more with more epochs"}
	suggestionRegularize = Suggestion{ID: "regularize",
		Text: "the model is much better on the images it saw than on the measured ones (overfitting): fewer epochs or label-smoothing"}
	suggestionCheckPreprocessing = Suggestion{ID: "check_preprocessing",
		Text: "the platform's figure differs from the measured accuracy: check the preprocessing and the class order of the model"}
	suggestionOtherArchitecture = Suggestion{ID: "other_architecture",
		Text: "two checks in a row are questionable: try another architecture or more images"}
	suggestionRetrain = Suggestion{ID: "retrain",
		Text: "train once more (more epochs, or another learning rate) and check the new model with final=false"}
)

// imageReviewReason is why the measured images were not shown to a language model (L7, P14): an image
// build is checked with statistics only, and a final check never shows the hold-out images.
func imageReviewReason(final bool) string {
	if final {
		return "held-out images are never shown to a language model"
	}
	return "image review is not available yet"
}

// imageReviewOf fills the image review of a report from the judgement input (R4): a review the caller
// already decided on stands as it is; otherwise the default reason of this kind of check stands.
func imageReviewOf(in JudgeInput) ImageReview {
	review := in.ImageReview
	if !review.Done && review.Reason == "" {
		review.Reason = imageReviewReason(in.Final)
	}
	return review
}

// mappingNames renders the derived mapping with the output index as a string key, because JSON objects
// have string keys: {"0": "cow", "1": "pig"}.
func mappingNames(m map[int]string) map[string]string {
	out := make(map[string]string, len(m))
	for i, c := range m {
		out[strconv.Itoa(i)] = c
	}
	return out
}

// deltaOf is the change of the figures against the previous check (R1.12).
func deltaOf(in JudgeInput, prev *CheckComparison) map[string]float64 {
	return map[string]float64{
		"accuracy": in.Accuracy - prev.Accuracy,
		"macro_f1": in.MacroF1 - prev.MacroF1,
	}
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

// registerTraining adds the paths of the training check (R1): the split, the check itself and the
// question what the last check of a model says (R1.14). They live under the check prefix, so delegation,
// class, approval and log apply to them like to every other call.
func (s *Service) registerTraining() {
	prefix := regexp.QuoteMeta(platform.AssessPrefix)
	s.Handle(http.MethodPost, regexp.MustCompile(`^`+prefix+`/splits$`), s.splitRoute)
	s.Handle(http.MethodPost, regexp.MustCompile(`^`+prefix+`/checks$`), s.checksRoute)
	s.Handle(http.MethodGet, regexp.MustCompile(`^`+prefix+`/models/([0-9]+)/latest$`), s.modelCheck)
}

// splitRoute answers POST /_agw/assess/splits (R1.1).
func (s *Service) splitRoute(ctx context.Context, chatID string, req platform.Request, _ []string) (platform.Result, error) {
	return s.splitDataset(ctx, chatID, req)
}

// checksRoute answers POST /_agw/assess/checks (R1.7): the check of a platform model.
func (s *Service) checksRoute(ctx context.Context, chatID string, req platform.Request, _ []string) (platform.Result, error) {
	return s.assessModel(ctx, chatID, req)
}

// --- the handler ---

// assessAnswer is the immediate answer of assess_model: the check runs in the background (R0.20).
type assessAnswer struct {
	CheckID string `json:"check_id"`
	State   string `json:"state"`
	Note    string `json:"note"`
}

// assessModel is POST /_agw/assess/checks: it starts the check of a platform model (R1.7, source
// model_id) or of a file of the sandbox (R2.2, source path) in the background. The evaluation dataset is
// resolved here already, so that the rule "one final check per dataset and chat" (P4) holds before the
// row is written.
func (s *Service) assessModel(ctx context.Context, chatID string, req platform.Request) (platform.Result, error) {
	if !s.Configured() || s.Plat == nil {
		return unavailableResult(), nil
	}
	var body assessRequest
	if err := json.Unmarshal(req.Body, &body); err != nil {
		return errResult("the check call is unreadable: " + err.Error()), nil
	}
	switch {
	case body.ModelID > 0 && body.Path != "":
		return errResult("give either model_id or path, not both"), nil
	case body.ModelID <= 0 && body.Path == "":
		return errResult("model_id or path missing"), nil
	case body.Path != "":
		return s.assessOwnFile(ctx, chatID, body, req)
	}
	evalID := body.EvalDatasetID
	if evalID == 0 {
		evalID = s.defaultEvalDataset(ctx, chatID, body.ModelID)
	}
	subject := modelSubject(body.ModelID)
	id, err := s.Start(ctx, chatID, KindTrainingModel, subject, evalID, body.Final, func(ctx context.Context, checkID string) (Report, error) {
		return s.runTrainingCheck(ctx, chatID, checkID, body)
	})
	if err != nil {
		return errResult(err.Error()), nil
	}
	return jsonResult(assessAnswer{CheckID: id, State: store.AssessQueued,
		Note: "follow it with {{tool:assess_status}}; the gateway also wakes you when it is done"})
}

// modelSubject is the subject of a check on a platform model (R0: subject "model:<id>").
func modelSubject(id int64) string { return "model:" + strconv.FormatInt(id, 10) }

// defaultEvalDataset names the dataset a check measures on when the agent names none (R1.5): the source
// of the split whose training dataset the model saw. Without such a split the training dataset itself is
// the answer — then every image of it is a seen one, and the check says so.
func (s *Service) defaultEvalDataset(ctx context.Context, chatID string, modelID int64) int64 {
	if s.St == nil {
		return 0
	}
	o, err := s.St.ModelOriginOf(ctx, chatID, modelID)
	if err != nil || len(o.SeenDatasetIDs) == 0 {
		return 0
	}
	for _, seen := range o.SeenDatasetIDs {
		if sp, err := s.St.SplitOfTrainDataset(ctx, chatID, seen); err == nil && sp.SourceDatasetID > 0 {
			return sp.SourceDatasetID
		}
	}
	return o.SeenDatasetIDs[0]
}

// checkImage is one image of a check: what the split knows about it (SHA-256 and class), the bytes and
// the file ending (the script opens the file, so the ending must fit).
type checkImage struct {
	Item
	Data []byte
	Ext  string
}

// runTrainingCheck is the background part of a training check (R1.8): the origin, the model, the log,
// the datasets the model could have seen, the held-out images, one container run and the judgement.
func (s *Service) runTrainingCheck(ctx context.Context, chatID, checkID string, body assessRequest) (Report, error) {
	started := time.Now()
	in := JudgeInput{Kind: KindTrainingModel, Final: body.Final}
	origin, err := s.St.ModelOriginOf(ctx, chatID, body.ModelID)
	if errors.Is(err, store.ErrNotFound) {
		// Nothing was read for this model, so the report carries no figures at all: the platform figure
		// is absent, not null (H114).
		rep := notMeasuredReport(in, ReasonOriginUnknown)
		rep.Figures = nil
		return rep, nil
	}
	if err != nil {
		return Report{}, err
	}
	model, err := s.modelInfo(ctx, chatID, body.ModelID)
	if err != nil {
		return Report{}, err
	}
	in.Model = SubjectRef{ID: model.ID, Name: model.Name}
	in.OutputLabels = model.OutputLabels
	modelBytes, logText, err := s.modelFile(ctx, chatID, body.ModelID)
	if err != nil {
		return Report{}, err
	}
	if logText == "" {
		// The archive carried no train.log (or another name): the end of the training container's log is
		// the second way to the platform's own figure (R1.13).
		logText = s.containerLog(ctx, chatID, origin.TrainContainerID)
	}
	tl := ParseTrainLog(logText)
	in.LogNaN = tl.NaN

	evalID := body.EvalDatasetID
	if evalID == 0 {
		evalID = s.defaultEvalDataset(ctx, chatID, body.ModelID)
	}
	if evalID <= 0 {
		return notMeasuredReport(in, ReasonNoEvalDataset), nil
	}
	in.EvalDataset = DatasetRef{ID: evalID}
	if info, err := s.datasetInfo(ctx, chatID, evalID); err == nil {
		in.EvalDataset.Name = info.Name
	}
	// R4: for an interim check the rule of #111 decides whether the pictures may go to the model at all.
	// When it says no, the report carries its reason (R0.9, L7); the final check keeps the held-out reason.
	reviewModel := ""
	if !body.Final {
		reviewModel = s.chatModel(ctx, chatID)
		if allowed, why := s.reviewAllowed(reviewModel, in.EvalDataset.Name); !allowed {
			in.ImageReview = ImageReview{Reason: why}
			reviewModel = ""
		}
	}
	// The datasets the model could have seen (P2): every image in one of them is a seen image, whatever
	// its file name is (P1).
	seen, seenImages, seenRefs, seenCount, notClass, err := s.seenImages(ctx, chatID, origin.SeenDatasetIDs)
	if err != nil {
		return Report{}, err
	}
	in.SeenDatasets = seenRefs
	in.PlatformFigure = platformFigure(tl, seenCount)
	if notClass {
		// The training data carries boxes or polygons: the model is no classification model, and the
		// gateway measures classification only (P8). The platform's figure stands in the report.
		in.NotClassification = true
		return JudgeClassification(in), nil
	}

	if model.Format != "onnx" {
		rep := notMeasuredReport(in, ReasonNotONNX+" (format "+model.Format+")")
		rep.Suggestions = []Suggestion{{ID: "export_onnx",
			Text: "train again with export_config in {{tool:create_training}} so that the platform saves model.onnx"}}
		return rep, nil
	}

	items, skipped, err := s.datasetItems(ctx, chatID, evalID)
	if errors.Is(err, ErrNotClassification) {
		// The evaluation dataset is annotated with boxes or polygons, so its images are no
		// classification (P8).
		in.NotClassification = true
		return JudgeClassification(in), nil
	}
	if err != nil {
		return Report{}, err
	}
	// Every image whose SHA-256 occurs in a seen dataset was seen, whatever it is called (P1).
	var heldOut []checkImage
	for _, im := range items {
		if _, isSeen := seen[im.SHA]; !isSeen {
			heldOut = append(heldOut, im)
		}
	}
	if len(heldOut) == 0 {
		in.NoHeldOut = true
		return JudgeClassification(in), nil
	}
	validation, holdout := PartitionImages(heldOut)
	in.ValidationImages, in.HoldoutImages = len(validation), len(holdout)
	in.Classes = classesOf(seen, heldOut)
	if len(validation) < MinValidationImages || len(holdout) < MinHoldoutImages {
		return JudgeClassification(in), nil
	}
	measured := validation
	if body.Final {
		measured = holdout
	}
	// R4/L12: while the check runs, the card shows the statistics step and — when the image review is
	// allowed — the model and the review step. The final check has no review step (the hold-out images
	// are never shown to a model).
	if reviewModel != "" {
		set := SetValidation
		if body.Final {
			set = SetHoldout
		}
		_ = s.SetProgress(ctx, checkID, Progress{Set: set, Images: len(measured), ReviewModel: reviewModel,
			Steps: []ProgressStep{{Name: "statistics", K: len(measured), M: len(measured)}}})
	}
	sample := seenSample(seenImages)
	// The preprocessing of the templates (R1.8): RGB, the size of the model, values times 1/255,
	// NCHW, no mean and no std (efficientnet_dataset.py).
	pre := Pre{Color: "rgb", Scale: ScaleToUnit, Layout: "nchw"}
	if body.Final {
		// #181: the hold-out images only go through the model when the class order is clear.
		pf, err := s.preflight(ctx, Job{Model: modelBytes, Pre: pre}, sample, "")
		if err != nil {
			return Report{}, err
		}
		if pf.Stop {
			rep := notMeasuredReport(in, "")
			rep.Reasons = preflightReasons(pf)
			rep.Suggestions = []Suggestion{suggestionRetrain}
			rep.HoldoutNotRun = true
			return rep, nil
		}
	}
	out, err := s.Run.Run(ctx, Job{
		Model:  modelBytes,
		Images: append(imagesOf(sample), imagesOf(measured)...),
		Pre:    pre,
	})
	if err != nil {
		return Report{}, err
	}
	return s.judgeRun(ctx, chatID, checkID, reviewModel, started, in, out, sample, measured, seen, skipped), nil
}

// notMeasuredReport builds a report for a reason that was decided before the measurement (not ONNX, no
// evaluation dataset). It carries the figures with the platform's figure, which was read out of the log
// right after the model was downloaded (L6).
func notMeasuredReport(in JudgeInput, reason string) Report {
	rep := NewReport(in.Kind, SetValidation, 0)
	rep.Final = in.Final
	rep.Verdict = NotMeasured
	rep.Reasons = []string{reason}
	rep.ImageReview = imageReviewOf(in)
	rep.Figures = figuresOf(in)
	return rep
}

// figuresOf builds the figures of a report without a measurement: the model, the datasets and the
// platform's figure; everything that rests on a run stays empty.
func figuresOf(in JudgeInput) *TrainingFigures {
	return &TrainingFigures{
		Model: in.Model, EvalDataset: in.EvalDataset, SeenDatasets: in.SeenDatasets,
		PerClass: map[string]ClassFigures{}, Mapping: map[string]string{},
		PlatformFigure: in.PlatformFigure, LogNaN: in.LogNaN, OutputLabels: in.OutputLabels,
	}
}

// judgeRun turns the raw predictions into the report: the class order from the seen sample, the figures
// from the measured set, the baseline, the test and the judgement (R1.8, R1.9). For an interim check the
// image review of R4 runs after the measurement: it looks at misclassified validation images and adds a
// hint and a suggestion, never a better verdict.
func (s *Service) judgeRun(ctx context.Context, chatID, checkID, reviewModel string, started time.Time, in JudgeInput, out Output,
	sample, measured []checkImage, seen map[string]Item, skipped int) Report {
	byID := s.measure(&in, out, sample, measured, seen)
	in.Previous = s.previousCheck(ctx, chatID, in.EvalDataset.ID, modelSubject(in.Model.ID))
	rep := JudgeClassification(in)
	rep.Note = reportNote(in, skipped, len(measured))
	if reviewModel != "" && !in.Final && rep.Verdict != NotMeasured {
		review := s.reviewMisclassified(ctx, checkID, chatID, reviewModel, started, in, measured, byID)
		rep.ImageReview = review.ImageReview
		rep.Hints = append(rep.Hints, review.Hints...)
		rep.Suggestions = append(rep.Suggestions, review.Suggestions...)
	}
	return rep
}

// measure reads the raw predictions of one container run into the judgement input (R1.8, R2.5): the class
// order from the sample of the images the model saw, the figures of the measured set, the baseline and
// the test. It answers the predictions by SHA-256, so that a caller can look at single images again (the
// own-model check asks a probe whether the model answers differently at all).
func (s *Service) measure(in *JudgeInput, out Output, sample, measured []checkImage, seen map[string]Item) map[string]Prediction {
	byID := map[string]Prediction{}
	for _, p := range out.Predictions {
		byID[p.ID] = p
		if p.OK && !Finite(p) {
			in.NonFinite = true
		}
	}
	shape, outputs, notClassification := classificationShape(out.Model)
	in.OutputShape, in.Outputs, in.NotClassification = shape, outputs, notClassification

	// The class order comes from the sample of seen images (P9); the measured set stays untouched.
	sampleIdx, sampleTruth := sampleAnswers(byID, sample)
	mapping, ok, reason := DeriveMapping(sampleIdx, sampleTruth, outputs)
	in.Mapping, in.MappingOK, in.MappingReason = mapping, ok, reason
	if !ok {
		if o, k, n, collapsed := collapsedOutput(sampleIdx, sampleTruth); collapsed {
			in.Collapse = &Collapse{Output: o, K: k, N: n}
		}
	}
	if ok && len(sampleIdx) > 0 {
		correct := 0
		for i := range sampleIdx {
			if mapping[sampleIdx[i]] == sampleTruth[i] {
				correct++
			}
		}
		in.SeenImages = len(sampleIdx)
		in.SeenAccuracy = float64(correct) / float64(len(sampleIdx))
	}

	// The figures of the measured set: only predictions that ran and that the mapping knows.
	var truth, pred []string
	for _, im := range measured {
		p, has := byID[im.SHA]
		if !has || !p.OK || len(p.Probs) != outputs {
			in.SkippedPredictions++
			continue
		}
		if !ok {
			continue // without a class order there is nothing to count
		}
		c, known := mapping[p.Top]
		if !known {
			in.MappingOK = false
			continue
		}
		truth = append(truth, im.Class)
		pred = append(pred, c)
	}
	in.Counted = len(truth)
	in.Classes = classesOf(seen, measured)
	figures := ClassMetrics(truth, pred, in.Classes)
	in.PerClass = figures.PerClass
	in.Confusion = fullConfusion(figures.Confusion, in.Classes)
	in.Accuracy, in.MacroF1 = figures.Accuracy, figures.MacroF1
	counts := map[string]int{}
	for _, it := range seen {
		counts[it.Class]++
	}
	in.Baseline = Majority(counts, truth)
	passed := 0
	for i := range truth {
		if truth[i] == pred[i] {
			passed++
		}
	}
	in.PValue = BetterThanBaseline(passed, len(truth), in.Baseline.Accuracy)
	in.MissingClass = missingClass(seen, truth)
	return byID
}

// reportNote is the line under the header (H114): it names the dataset and the check, never an image.
func reportNote(in JudgeInput, skipped, measured int) string {
	names := make([]string, 0, len(in.SeenDatasets))
	for _, d := range in.SeenDatasets {
		if d.Name != "" {
			names = append(names, d.Name)
		} else {
			names = append(names, "dataset "+strconv.FormatInt(d.ID, 10))
		}
	}
	if len(names) == 0 {
		names = append(names, "the training dataset")
	}
	note := fmt.Sprintf("Images of dataset %s that the model did not see in training (checked by SHA-256 against %s)",
		in.EvalDataset.Name, strings.Join(names, ", "))
	if skipped > 0 {
		note += fmt.Sprintf("; %d images were skipped (no or several tags)", skipped)
	}
	if in.SkippedPredictions > 0 {
		note += fmt.Sprintf("; %d of %d measured images could not be run", in.SkippedPredictions, measured)
	}
	return note + "."
}

// previousCheck reads the last interim check of the same evaluation dataset in this chat that belongs to
// another model (R1.12); nil when there is none.
func (s *Service) previousCheck(ctx context.Context, chatID string, evalDatasetID int64, subject string) *CheckComparison {
	if s.St == nil || evalDatasetID <= 0 {
		return nil
	}
	row, ok, err := s.St.PreviousInterimCheck(ctx, chatID, evalDatasetID, subject)
	if err != nil || !ok {
		return nil
	}
	var rep struct {
		Verdict Verdict `json:"verdict"`
		Figures struct {
			Model   SubjectRef `json:"model"`
			Overall struct {
				Accuracy float64 `json:"accuracy"`
				MacroF1  float64 `json:"macro_f1"`
			} `json:"overall"`
		} `json:"figures"`
	}
	if json.Unmarshal(row.Report, &rep) != nil {
		return nil
	}
	return &CheckComparison{CheckID: row.ID, Model: rep.Figures.Model, Verdict: rep.Verdict,
		Accuracy: rep.Figures.Overall.Accuracy, MacroF1: rep.Figures.Overall.MacroF1}
}

// --- helpers of the run ---

// datasetItems loads a dataset of the platform and answers its annotated images with their bytes.
func (s *Service) datasetItems(ctx context.Context, chatID string, id int64) ([]checkImage, int, error) {
	files, err := s.datasetFiles(ctx, chatID, id)
	if err != nil {
		return nil, 0, err
	}
	items, stats, err := SplitOf(files)
	if err != nil {
		return nil, 0, err
	}
	out := make([]checkImage, 0, len(items))
	for _, it := range items {
		data, name := imageOfSHA(files, it.SHA)
		if name == "" {
			return nil, 0, errors.New("the dataset archive does not hold an image the annotation names")
		}
		out = append(out, checkImage{Item: it, Data: data, Ext: endOf(name)})
	}
	return out, stats.Skipped, nil
}

// seenImages loads every dataset the model could have seen and hashes its images (P1, P2). It answers
// the images by SHA-256 (with their class), the images themselves (for the sample of the class order),
// the names for the report, how many images the training saw (for the note of the platform's figure) and
// whether one of the datasets is no classification dataset (boxes or polygons, P8).
func (s *Service) seenImages(ctx context.Context, chatID string, ids []int64) (map[string]Item, []checkImage, []DatasetRef, int, bool, error) {
	seen := map[string]Item{}
	seenImages := []checkImage{}
	var refs []DatasetRef
	images := 0
	notClass := false
	for _, id := range ids {
		if id <= 0 {
			continue
		}
		files, err := s.datasetFiles(ctx, chatID, id)
		if err != nil {
			return nil, nil, nil, 0, false, err
		}
		items, _, err := SplitOf(files)
		if errors.Is(err, ErrNotClassification) {
			// The images were still seen; only their tags are no classification.
			items, notClass = nil, true
			images += len(files.Images)
		} else if err != nil {
			return nil, nil, nil, 0, false, err
		} else {
			images += len(items)
		}
		ref := DatasetRef{ID: id}
		if info, err := s.datasetInfo(ctx, chatID, id); err == nil {
			ref.Name = info.Name
		}
		refs = append(refs, ref)
		for _, it := range items {
			data, name := imageOfSHA(files, it.SHA)
			if name == "" {
				continue
			}
			if _, ok := seen[it.SHA]; ok {
				continue
			}
			seen[it.SHA] = it
			seenImages = append(seenImages, checkImage{Item: it, Data: data, Ext: endOf(name)})
		}
	}
	return seen, seenImages, refs, images, notClass, nil
}

// imageOfSHA finds the image bytes of a SHA-256 in an archive (they were hashed right before).
func imageOfSHA(files DatasetFiles, sha string) (data []byte, name string) {
	for n, d := range files.Images {
		if Sha256Hex(d) == sha {
			return d, n
		}
	}
	return nil, ""
}

// seenSample picks up to SeenSamplePerClass images per class out of the images the model saw (R1.8).
func seenSample(images []checkImage) []checkImage {
	sort.Slice(images, func(i, j int) bool {
		if images[i].Class != images[j].Class {
			return images[i].Class < images[j].Class
		}
		return images[i].SHA < images[j].SHA
	})
	perClass := map[string]int{}
	var out []checkImage
	for _, im := range images {
		if perClass[im.Class] >= SeenSamplePerClass {
			continue
		}
		perClass[im.Class]++
		out = append(out, im)
	}
	return out
}

// imagesOf turns check images into the images of a container job.
func imagesOf(list []checkImage) []Image {
	out := make([]Image, 0, len(list))
	for _, im := range list {
		out = append(out, Image{ID: im.SHA, Ext: im.Ext, Data: im.Data})
	}
	return out
}

// PartitionImages splits the held-out images per class into the validation set and the hold-out set
// (P3); this is assess.Partition on the images themselves.
func PartitionImages(items []checkImage) (validation, holdout []checkImage) {
	plain := make([]Item, 0, len(items))
	for _, im := range items {
		plain = append(plain, im.Item)
	}
	v, h := Partition(plain)
	bySHA := map[string]checkImage{}
	for _, im := range items {
		bySHA[im.SHA] = im
	}
	for _, it := range v {
		if im, ok := bySHA[it.SHA]; ok {
			validation = append(validation, im)
		}
	}
	for _, it := range h {
		if im, ok := bySHA[it.SHA]; ok {
			holdout = append(holdout, im)
		}
	}
	return validation, holdout
}

// classesOf are the classes of the data: those of the images the model saw plus those of the measured
// set (sorted, so that two reports name them alike).
func classesOf(seen map[string]Item, items []checkImage) []string {
	set := map[string]bool{}
	for _, it := range seen {
		set[it.Class] = true
	}
	for _, it := range items {
		set[it.Class] = true
	}
	out := make([]string, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// missingClass names a class of the seen data that the measured set does not carry (R1.10).
func missingClass(seen map[string]Item, truth []string) string {
	have := map[string]bool{}
	for _, c := range truth {
		have[c] = true
	}
	names := map[string]bool{}
	for _, it := range seen {
		if !have[it.Class] {
			names[it.Class] = true
		}
	}
	if len(names) == 0 {
		return ""
	}
	out := make([]string, 0, len(names))
	for c := range names {
		out = append(out, c)
	}
	sort.Strings(out)
	return out[0]
}

// classificationShape reads the output shape of the model (R1.9, P8): only [1, C] and [C] count as
// classification. shape is the plain text of the shape, for the reason of a not measured report.
func classificationShape(info ModelInfo) (shape string, outputs int, notClassification bool) {
	if len(info.Outputs) != 1 {
		return shapeText(info), 0, true
	}
	s := info.Outputs[0].Shape
	switch {
	case len(s) == 2 && s[0] == 1 && s[1] > 0:
		return shapeText(info), s[1], false
	case len(s) == 1 && s[0] > 0:
		return shapeText(info), s[0], false
	}
	return shapeText(info), 0, true
}

// shapeText renders the output shapes of the model for a reason.
func shapeText(info ModelInfo) string {
	parts := make([]string, 0, len(info.Outputs))
	for _, o := range info.Outputs {
		dims := make([]string, 0, len(o.Shape))
		for _, d := range o.Shape {
			dims = append(dims, strconv.Itoa(d))
		}
		parts = append(parts, "["+strings.Join(dims, ",")+"]")
	}
	return strings.Join(parts, ", ")
}

// endOf is the file ending of an image name (the script opens the file by its ending).
func endOf(name string) string {
	e := path.Ext(name)
	if e == "" {
		return ".png"
	}
	return e
}

// platformFigure builds the platform's figure for the report (R1.13); nil when the log held no figure.
func platformFigure(tl TrainLog, images int) *PlatformFigure {
	if !tl.Have {
		return nil
	}
	f := tl.Figure
	f.Source = "training log"
	f.Note = PlatformFigureNote(images)
	return &f
}

// --- platform reads of the check ---

// modelMeta is the part of a model the check reads.
type modelMeta struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Format string `json:"format"`
	// OutputLabels: the class labels the platform recorded, in output order (null: none).
	OutputLabels []string `json:"output_labels"`
}

// modelInfo reads the metadata of a model (a reading call with the user's token).
func (s *Service) modelInfo(ctx context.Context, chatID string, id int64) (modelMeta, error) {
	var out modelMeta
	req := platform.Request{Method: http.MethodGet, Path: "/models/" + strconv.FormatInt(id, 10)}
	res, stop, err := platform.ReadJSON(ctx, s.doer(chatID), req, &out)
	s.log(ctx, chatID, "GET "+req.Path, callResult(res, err))
	if stop {
		return modelMeta{}, callError("GET "+req.Path, res, err)
	}
	return out, nil
}

// modelFile downloads a model (R1.8): the platform answers the model file itself or, when the training
// log belongs to it, a ZIP with model.onnx and train.log. The second answer is the text of the log ("":
// the archive carried none).
func (s *Service) modelFile(ctx context.Context, chatID string, id int64) ([]byte, string, error) {
	callPath := "/models/" + strconv.FormatInt(id, 10) + "/download"
	file, err := s.downloadToTemp(ctx, chatID, callPath)
	if err != nil {
		return nil, "", err
	}
	defer os.Remove(file)
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, "", err
	}
	zr, err := zip.OpenReader(file)
	if err != nil {
		return data, "", nil // a single file, not an archive: the model itself
	}
	defer zr.Close()
	model := data
	logText := ""
	for _, f := range zr.File {
		switch path.Base(f.Name) {
		case "model.onnx":
			b, err := readZipFile(f)
			if err != nil {
				return nil, "", err
			}
			model = b
		case "train.log":
			b, err := readZipFile(f)
			if err != nil {
				return nil, "", err
			}
			logText = string(b)
		}
	}
	return model, logText, nil
}

// logTailLines is how many lines the fallback reads from a training container's log: Lightning writes its
// metric table at the very end, and without a tail the backend answers the whole log.
const logTailLines = 2000

// containerLog reads the end of the log of a training container (R1.13, the fallback when the model's
// archive carries no train.log). An unreadable answer is no log: the report then has no platform figure.
func (s *Service) containerLog(ctx context.Context, chatID string, trainID int64) string {
	if trainID <= 0 {
		return ""
	}
	req := platform.Request{Method: http.MethodGet,
		Path:  "/train/containers/" + strconv.FormatInt(trainID, 10) + "/logs",
		Query: map[string]string{"tail": strconv.Itoa(logTailLines)}}
	res, err := s.Plat.Do(ctx, chatID, req)
	s.log(ctx, chatID, "GET "+req.Path, callResult(res, err))
	if err != nil || res.Status != "ok" || res.HTTPStatus < 200 || res.HTTPStatus >= 300 {
		return ""
	}
	return res.Body
}

// --- the last check of a model (R1.14) ---

// ModelCheck is what the gateway knows about the check of a model in a chat (R1.14, W4): the last
// finished check with final=true decides, an interim one only makes the wording say so.
type ModelCheck struct {
	Found    bool    `json:"found"`
	Final    bool    `json:"final"`
	Verdict  Verdict `json:"verdict,omitempty"`
	Measured int     `json:"measured_images,omitempty"`
	Accuracy float64 `json:"accuracy,omitempty"`
	Reason   string  `json:"reason,omitempty"`
	// LabelOrderWrong: the check found the output_labels of the model in another order than its outputs.
	LabelOrderWrong bool `json:"label_order_wrong,omitempty"`
}

// Text is the wording the approval adds (R1.14, L8): the verdict words in lower case and with spaces.
func (c ModelCheck) Text() string {
	switch {
	case !c.Found:
		return "none in this chat"
	case !c.Final:
		return "none in this chat (interim only)"
	case c.Verdict == Usable || c.Verdict == Questionable:
		return fmt.Sprintf("%s (%d held-out images, accuracy %.2f)", c.Verdict, c.Measured, c.Accuracy)
	case c.Verdict == NotMeasured:
		if c.Reason != "" {
			return "not measured (" + c.Reason + ")"
		}
		return "not measured"
	}
	return "none in this chat"
}

// ModelCheckOf reads the last finished check of a model in a chat (R1.14, W4): the check of the model
// itself or — for a model uploaded from a file (R2.6) — the check of that file, whichever is newer. The
// wording of the answer is ModelCheck.Text.
func (s *Service) ModelCheckOf(ctx context.Context, chatID string, modelID int64) (ModelCheck, error) {
	subjects := []string{modelSubject(modelID)}
	if s.St != nil {
		o, err := s.St.ModelOriginOf(ctx, chatID, modelID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return ModelCheck{}, err
		}
		if err == nil && o.SourceSHA != "" {
			subjects = append(subjects, FileSubject(o.SourceSHA))
		}
	}
	return s.checkOfSubjects(ctx, chatID, subjects...)
}

// modelCheck is GET /_agw/assess/models/<id>/latest (R1.14): the line an approval adds about the check
// of a model. The gateway writes the wording, so that every place that names a check names it alike.
func (s *Service) modelCheck(ctx context.Context, chatID string, req platform.Request, groups []string) (platform.Result, error) {
	id, err := strconv.ParseInt(groups[1], 10, 64)
	if err != nil {
		return errResult("not a model id"), nil
	}
	c, err := s.ModelCheckOf(ctx, chatID, id)
	if err != nil {
		return platform.Result{}, err
	}
	return jsonResult(map[string]any{"model_id": id, "line": c.Text()})
}

// fullConfusion completes the confusion table with the zero cells over the classes of the data (and every
// further class that occurs), so that the check card draws a square without guessing (#248).
func fullConfusion(conf map[string]map[string]int, classes []string) map[string]map[string]int {
	all := append([]string(nil), classes...)
	known := map[string]bool{}
	for _, c := range classes {
		known[c] = true
	}
	var extra []string
	for t, row := range conf {
		for _, c := range append([]string{t}, keysOf(row)...) {
			if !known[c] {
				known[c] = true
				extra = append(extra, c)
			}
		}
	}
	sort.Strings(extra)
	all = append(all, extra...)
	out := make(map[string]map[string]int, len(all))
	for _, t := range all {
		out[t] = make(map[string]int, len(all))
		for _, p := range all {
			out[t][p] = conf[t][p]
		}
	}
	return out
}

func keysOf(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

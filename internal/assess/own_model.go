// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package assess

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"agw/internal/platform"
	"agw/internal/store"
)

// The own-model check (R2): before a model the agent wrote itself goes to the platform with upload_model,
// the gateway measures the file it fetched out of a notebook run. It loads the file, reads its shapes,
// looks at its predictions and measures it on the images the notebook server of this chat never received.
// What the gateway cannot see is what the code learned: it sees which datasets a run got, and every image
// of those counts as seen (P2, R2.3). The rules of the verdict are the ones of the training check
// (judgeReport); only the figures and the file's own checks are R2's.

// KindOwnModel is the kind of a check on a model from the agent's own code (R2).
const KindOwnModel = "own_model"

// ReasonFileOriginUnknown: the file is none the gateway handed out of a run of this chat (R2.2, P2).
const ReasonFileOriginUnknown = "origin unknown: this file was not fetched from a notebook run in this chat"

// ReasonConstantOutput: the model answers the same for different images (R2.5).
const ReasonConstantOutput = "the model returns the same output for different images"

// ReasonOwnNoEvalDataset: without a named dataset the gateway knows no images to measure on (R2.2).
const ReasonOwnNoEvalDataset = "no evaluation dataset: the gateway does not know which dataset a notebook run of this chat received; name one with eval_dataset_id"

// FileSubject is the subject of a check on a file of the sandbox (R2.6): the SHA-256 of the bytes the
// gateway read. A check keeps this subject after the upload, so the file stays what was measured.
func FileSubject(sha256 string) string { return "sha:" + strings.ToLower(sha256) }

// SHAFromSubject is the SHA-256 behind a subject like "sha:<hex>" (ok false for every other subject).
func SHAFromSubject(subject string) (string, bool) {
	sha, ok := strings.CutPrefix(subject, "sha:")
	if !ok || !shaRe.MatchString(sha) {
		return "", false
	}
	return strings.ToLower(sha), true
}

// shaRe is the shape of a SHA-256 in a subject or in the check paths (R2.7).
var shaRe = regexp.MustCompile(`^` + platform.AssessSHARe + `$`)

// --- the figures of an own-model check ---

// FileRef names the file an own-model check measured (R2): the name notebook_fetch gave it in the
// sandbox, the SHA-256 of the bytes the gateway read, and the run they came out of. RunID is null for a
// file that no run of this chat handed over — that is what makes the origin unknown.
type FileRef struct {
	Name   string  `json:"name"`
	SHA256 string  `json:"sha256"`
	RunID  *string `json:"run_id"`
	// Path: the path inside the run the file came out of (empty without a run).
	Path string `json:"path,omitempty"`
}

// PreprocessFigures is the preprocessing the check fed the model (R2.4): what the agent declared, or the
// default of the platform templates. declared says which of the two it is, so that the user can tell.
type PreprocessFigures struct {
	Color  string    `json:"color"`
	Size   []int     `json:"size,omitempty"`
	Scale  float64   `json:"scale"`
	Mean   []float64 `json:"mean,omitempty"`
	Std    []float64 `json:"std,omitempty"`
	Layout string    `json:"layout"`
	// Declared: the agent named this preprocessing (or the run's outputs/model.json did); false: the
	// default of the platform templates. Kept for the frontend; true for source "run" and "declared".
	Declared bool `json:"declared"`
	// Source: "run" (outputs/model.json of the notebook run), "declared" (the call) or "default" (#181).
	Source string `json:"source"`
}

// BasicFigures is what the check found out about the file itself, without measuring anything (R2.5).
type BasicFigures struct {
	// Loads: the model loaded in onnxruntime; LoadError is why not.
	Loads     bool   `json:"loads"`
	LoadError string `json:"load_error,omitempty"`
	// Inputs and Outputs are the shapes the file declares.
	Inputs  []TensorInfo `json:"inputs"`
	Outputs []TensorInfo `json:"outputs"`
	// Classes: the number of outputs the model shows (0: it is no classification, R1.9).
	Classes int `json:"classes"`
	// Varied: different images gave different answers (R2.5); null when fewer than two of the four probe
	// images could be run, so that nothing could be compared.
	Varied *bool `json:"varied"`
	// ClassOrder: whether the derived class order is the one the agent declared (R2.4). Only with a
	// declared class_names; null without them.
	ClassOrder *bool `json:"class_order"`
}

// OwnModelFigures is the figures of an own-model check (R2). It carries what a training check carries —
// the dataset, the seen datasets, the figures per class, the baseline, the mapping and the sample of seen
// images — plus the file, the preprocessing it was fed with and the checks of the file itself.
type OwnModelFigures struct {
	// File: the file that was measured.
	File FileRef `json:"file"`
	// Model: the model this file was uploaded as, when that already happened (R2.6); null before.
	Model          *SubjectRef             `json:"model"`
	EvalDataset    DatasetRef              `json:"eval_dataset"`
	SeenDatasets   []DatasetRef            `json:"seen_datasets"`
	MeasuredImages int                     `json:"measured_images"`
	PerClass       map[string]ClassFigures `json:"per_class"`
	// Confusion: truth -> prediction -> number of images (see TrainingFigures.Confusion).
	Confusion  map[string]map[string]int `json:"confusion,omitempty"`
	Overall    OverallFigures            `json:"overall"`
	Baseline   BaselineFigures           `json:"baseline"`
	SeenSample SeenSample                `json:"seen_sample"`
	// Mapping: the class every output of the model means, as the gateway derived it (P9). The order the
	// agent declared is only compared with it (R2.4).
	Mapping    map[string]string  `json:"mapping"`
	Preprocess PreprocessFigures  `json:"preprocess"`
	Basic      *BasicFigures      `json:"basic"`
	Previous   *CheckComparison   `json:"previous,omitempty"`
	Delta      map[string]float64 `json:"delta,omitempty"`
}

func (f *OwnModelFigures) setComparison(prev *CheckComparison, delta map[string]float64) {
	f.Previous, f.Delta = prev, delta
}

// JudgeOwnModel turns a measurement into the report of an own-model check (R2). The rules are the ones of
// judgeReport; the figures are R2's own, and the file, the preprocessing and the basic checks are named.
func JudgeOwnModel(in JudgeInput, file FileRef, pre PreprocessFigures, basic *BasicFigures) Report {
	if in.PerClass == nil {
		in.PerClass = map[string]ClassFigures{}
	}
	var model *SubjectRef
	if in.Model.ID > 0 {
		m := in.Model
		model = &m
	}
	return judgeReport(in, &OwnModelFigures{
		File: file, Model: model, EvalDataset: in.EvalDataset, SeenDatasets: in.SeenDatasets,
		MeasuredImages: measuredImages(in), PerClass: in.PerClass, Confusion: in.Confusion,
		Overall:    overallFigures(in),
		Baseline:   BaselineFigures{Kind: "majority", Class: in.Baseline.Class, Accuracy: in.Baseline.Accuracy, MacroF1: in.Baseline.MacroF1, PValue: in.PValue},
		SeenSample: SeenSample{Images: in.SeenImages, Accuracy: in.SeenAccuracy},
		Mapping:    mappingNames(in.Mapping),
		Preprocess: pre, Basic: basic,
	})
}

// noHeldOutOwn is the reason of an own-model check whose evaluation dataset holds no image a notebook run
// of this chat did not receive (R2.2): the source of a check is the dataset the run got.
func NoHeldOutOwn(dataset string) string {
	return "no held-out images: a notebook run of this chat received every image of " + dataset
}

// --- the tool call ---

// assessRequest is the body of the check call (the tool fills it; the agent cannot reach the path).
type assessRequest struct {
	ModelID       int64            `json:"model_id"`
	Path          string           `json:"path"`
	EvalDatasetID int64            `json:"eval_dataset_id"`
	Final         bool             `json:"final"`
	ClassNames    []string         `json:"class_names"`
	Preprocess    *PreprocessInput `json:"preprocess"`
}

// PreprocessInput is the preprocessing the agent declared for its own model (R2.4): the fields the
// container job carries (Pre, R0.12). It is data, not code.
type PreprocessInput struct {
	Color  string    `json:"color"`
	Size   []int     `json:"size"`
	Scale  float64   `json:"scale"`
	Mean   []float64 `json:"mean"`
	Std    []float64 `json:"std"`
	Layout string    `json:"layout"`
}

// Pre is the input of the check container; empty fields fall back to what the models are fed with
// elsewhere (rgb, 1/255, nchw, no mean and no std).
func (p *PreprocessInput) Pre() Pre {
	if p == nil {
		return Pre{}
	}
	return Pre{Color: p.Color, Size: p.Size, Scale: p.Scale, Mean: p.Mean, Std: p.Std, Layout: p.Layout}
}

// preprocessFigures names the preprocessing for the report (R2.4): the declared one, or the default of
// the platform templates with declared false.
func preprocessFigures(p *PreprocessInput) PreprocessFigures {
	f := PreprocessFigures{Color: "rgb", Scale: ScaleToUnit, Layout: "nchw", Declared: p != nil, Source: "default"}
	if p == nil {
		return f
	}
	f.Source = "declared"
	f.Color, f.Size, f.Scale, f.Mean, f.Std, f.Layout = p.Color, p.Size, p.Scale, p.Mean, p.Std, p.Layout
	if f.Color == "" {
		f.Color = "rgb"
	}
	if f.Scale == 0 {
		f.Scale = ScaleToUnit
	}
	if f.Layout == "" {
		f.Layout = "nchw"
	}
	return f
}

// fileInput is what the check knows about the file the agent pointed at: the name it got in the sandbox,
// the SHA-256 and the bytes the socket read (R2.2).
type fileInput struct {
	Name   string
	SHA256 string
	Data   []byte
}

// assessOwnFile starts the check of a model file of the sandbox (R2.2). The bytes come from the upload of
// this very call, so what the check measures is exactly what a later upload_model would send. The subject
// of the check is the SHA-256 of those bytes, never the path the agent named: a moved or replaced file
// gets another subject.
func (s *Service) assessOwnFile(ctx context.Context, chatID string, body assessRequest, req platform.Request) (platform.Result, error) {
	if len(req.Uploads) != 1 {
		return errResult("the call carries no model file; give the path of a file that {{tool:notebook_fetch}} wrote into the sandbox"), nil
	}
	file := fileInput{Name: req.Uploads[0].Name, SHA256: strings.ToLower(req.Uploads[0].SHA256), Data: req.Uploads[0].Data}
	if !shaRe.MatchString(file.SHA256) {
		return errResult("the file could not be read (no SHA-256)"), nil
	}
	// #184: a final check that names other preprocessing than outputs/model.json of the run is refused before
	// anything starts, so that no assessment row, no container run and no hold-out image is used up.
	if body.Final && body.Preprocess != nil {
		fetch, found, err := s.St.NotebookFetchOf(ctx, chatID, file.SHA256)
		if err != nil {
			return platform.Result{}, err
		}
		if found {
			if runPre := runPreprocess(fetch.ModelMeta); runPre != nil {
				if d := preprocessDiffs(body.Preprocess, runPre); len(d) > 0 {
					return refusedPreprocess(fetch.RunID, d), nil
				}
			}
		}
	}
	seen, err := s.St.NotebookSeenDatasetIDs(ctx, chatID)
	if err != nil {
		return platform.Result{}, err
	}
	evalID := body.EvalDatasetID
	if evalID == 0 {
		evalID = s.defaultEvalDatasetForSeen(ctx, chatID, seen)
	}
	id, err := s.Start(ctx, chatID, KindOwnModel, FileSubject(file.SHA256), evalID, body.Final,
		func(ctx context.Context, _ string) (Report, error) {
			return s.runOwnModelCheck(ctx, chatID, body, file, seen)
		})
	if err != nil {
		return errResult(err.Error()), nil
	}
	return jsonResult(assessAnswer{CheckID: id, State: store.AssessQueued,
		Note: "follow it with {{tool:assess_status}}; the gateway also wakes you when it is done"})
}

// defaultEvalDatasetForSeen names the dataset a check measures on when the agent names none (R1.5, R2.2):
// the source of the split whose training dataset was one the notebook server received. Without such a
// split the seen dataset itself is the answer — then every image of it is a seen one, and the check says
// so.
func (s *Service) defaultEvalDatasetForSeen(ctx context.Context, chatID string, seen []int64) int64 {
	if s.St == nil {
		return 0
	}
	for _, id := range seen {
		if id <= 0 {
			continue
		}
		if sp, err := s.St.SplitOfTrainDataset(ctx, chatID, id); err == nil && sp.SourceDatasetID > 0 {
			return sp.SourceDatasetID
		}
	}
	for _, id := range seen {
		if id > 0 {
			return id
		}
	}
	return 0
}

// runOwnModelCheck is the background part of an own-model check (R2.2–R2.5): the origin of the file, the
// datasets the notebook server of this chat received, the held-out images, one container run and the
// judgement.
func (s *Service) runOwnModelCheck(ctx context.Context, chatID string, body assessRequest, file fileInput,
	seenIDs []int64) (Report, error) {
	fetch, found, err := s.St.NotebookFetchOf(ctx, chatID, file.SHA256)
	if err != nil {
		return Report{}, err
	}
	// NT5/NT6: outputs/model.json of the run, read when the file was fetched, goes before the call.
	hint := ""
	if found && len(fetch.ModelMeta) > 0 {
		body, hint = withRunMeta(body, fetch.ModelMeta, fetch.RunID)
	}
	in := JudgeInput{Kind: KindOwnModel, Final: body.Final, DeclaredClassNames: body.ClassNames}
	pre := preprocessFigures(body.Preprocess)
	if found && body.Preprocess != nil && runHasPreprocess(fetch.ModelMeta) {
		pre.Source = "run"
	}
	fileRef := FileRef{Name: file.Name, SHA256: file.SHA256}
	if found {
		run, path := fetch.RunID, fetch.Path
		fileRef.RunID, fileRef.Path = &run, path
	}
	// A file the gateway did not hand out itself has no origin: without a run there is no way to know
	// what the code could have seen, and nothing is measured (R2.2, P2).
	if !found {
		return notMeasuredOwn(in, fileRef, pre, nil, ReasonFileOriginUnknown), nil
	}
	// The model this file was uploaded as, when that already happened (R2.6): a check after the upload
	// names the model next to the file.
	if o, ok, err := s.St.ModelOriginOfSHA(ctx, chatID, file.SHA256); err != nil {
		return Report{}, err
	} else if ok {
		in.Model = SubjectRef{ID: o.ModelID}
		if info, err := s.modelInfo(ctx, chatID, o.ModelID); err == nil {
			in.Model.Name = info.Name
		}
	}
	evalID := body.EvalDatasetID
	if evalID == 0 {
		evalID = s.defaultEvalDatasetForSeen(ctx, chatID, seenIDs)
	}
	if evalID <= 0 {
		return notMeasuredOwn(in, fileRef, pre, nil, ReasonOwnNoEvalDataset), nil
	}
	in.EvalDataset = DatasetRef{ID: evalID}
	if info, err := s.datasetInfo(ctx, chatID, evalID); err == nil {
		in.EvalDataset.Name = info.Name
	}
	// Every dataset a run of this chat ever received counts as seen, whatever run it was (R2.3).
	seen, seenImages, seenRefs, _, notClass, err := s.seenImages(ctx, chatID, seenIDs)
	if err != nil {
		return Report{}, err
	}
	in.SeenDatasets = seenRefs
	if notClass {
		// The data the notebook server received is annotated with boxes or polygons: the model is no
		// classification model, and the gateway measures classification only (P8).
		in.NotClassification = true
		return JudgeOwnModel(in, fileRef, pre, nil), nil
	}
	items, skipped, err := s.datasetItems(ctx, chatID, evalID)
	if errors.Is(err, ErrNotClassification) {
		in.NotClassification = true
		return JudgeOwnModel(in, fileRef, pre, nil), nil
	}
	if err != nil {
		return Report{}, err
	}
	var heldOut []checkImage
	for _, im := range items {
		if _, isSeen := seen[im.SHA]; !isSeen {
			heldOut = append(heldOut, im)
		}
	}
	if len(heldOut) == 0 {
		in.NoHeldOut, in.NoHeldOutReason = true, NoHeldOutOwn(in.EvalDataset.Name)
		return JudgeOwnModel(in, fileRef, pre, nil), nil
	}
	validation, holdout := PartitionImages(heldOut)
	in.ValidationImages, in.HoldoutImages = len(validation), len(holdout)
	in.Classes = classesOf(seen, heldOut)
	if len(validation) < MinValidationImages || len(holdout) < MinHoldoutImages {
		return JudgeOwnModel(in, fileRef, pre, nil), nil
	}
	measured := validation
	if body.Final {
		measured = holdout
	}
	sample := seenSample(seenImages)
	// The probe asks whether the model answers differently at all (R2.5): four images of the validation
	// set, which is untouched by the hold-out images of a final check.
	probe := probeImages(validation, ProbeImages)
	if body.Final {
		// #181: the hold-out images only go through the model when the class order is clear.
		pf, err := s.preflight(ctx, Job{Model: file.Data, Pre: body.Preprocess.Pre(), KeepLoadError: true}, sample, pre.Short())
		if err != nil {
			return Report{}, err
		}
		if pf.Stop {
			rep := notMeasuredOwn(in, fileRef, pre, nil, "")
			rep.Reasons = preflightReasons(pf)
			rep.Suggestions = []Suggestion{suggestionCheckPreprocess}
			rep.HoldoutNotRun = true
			if hint != "" {
				rep.Hints = append(rep.Hints, hint)
			}
			return rep, nil
		}
	}
	out, err := s.Run.Run(ctx, Job{
		Model:  file.Data,
		Images: jobImages(sample, probe, measured),
		Pre:    body.Preprocess.Pre(),
		// Whether the file loads is one of the things an own model is checked for: the answer names the
		// error instead of failing the check (R2.5).
		KeepLoadError: true,
	})
	if err != nil {
		return Report{}, err
	}
	rep := s.judgeOwnRun(ctx, chatID, in, out, fileRef, pre, sample, probe, measured, seen, skipped)
	if hint != "" {
		rep.Hints = append(rep.Hints, hint)
	}
	return rep, nil
}

// withRunMeta puts the content of outputs/model.json of the run into the call (NT6): its class names and
// its preprocessing replace what the call declared. A call that declared something else gets a hint.
func withRunMeta(body assessRequest, meta json.RawMessage, runID string) (assessRequest, string) {
	var m platform.ModelMeta
	if json.Unmarshal(meta, &m) != nil {
		return body, ""
	}
	hint := ""
	var pre *PreprocessInput
	if len(m.Preprocess) > 0 {
		pre = runPreprocess(meta)
		if pre == nil {
			return body, ""
		}
	}
	if len(preprocessDiffs(body.Preprocess, pre)) > 0 ||
		(body.ClassNames != nil && m.ClassNames != nil && strings.Join(body.ClassNames, "\x00") != strings.Join(m.ClassNames, "\x00")) {
		hint = fmt.Sprintf("the call declared other preprocessing than outputs/model.json of run %s; the check used the run's file", runID)
	}
	if m.ClassNames != nil {
		body.ClassNames = m.ClassNames
	}
	if pre != nil {
		body.Preprocess = pre
	}
	return body, hint
}

// runHasPreprocess reports whether outputs/model.json of the run names a preprocessing.
func runHasPreprocess(meta json.RawMessage) bool {
	var m platform.ModelMeta
	return len(meta) > 0 && json.Unmarshal(meta, &m) == nil && len(m.Preprocess) > 0
}

// runPreprocess reads the preprocessing out of outputs/model.json of the run; nil without one.
func runPreprocess(meta json.RawMessage) *PreprocessInput {
	var m platform.ModelMeta
	if len(meta) == 0 || json.Unmarshal(meta, &m) != nil || len(m.Preprocess) == 0 {
		return nil
	}
	pre := &PreprocessInput{}
	if json.Unmarshal(m.Preprocess, pre) != nil {
		return nil
	}
	return pre
}

// ReasonPreprocessDiffersFromRun is the reason code of a final check refused for its preprocessing (#184).
const ReasonPreprocessDiffersFromRun = "preprocess_differs_from_run"

// refusedPreprocess is the answer of a final check whose preprocessing differs from outputs/model.json of
// the run: nothing was started or measured, the final test stays available (#184).
func refusedPreprocess(runID string, diffs []string) platform.Result {
	return platform.Result{Status: "error", Decision: platform.DecisionError, Reason: ReasonPreprocessDiffersFromRun,
		Message: platform.RefusedPrefix + fmt.Sprintf("the preprocessing of this final check differs from outputs/model.json of run %s: %s. "+
			"Nothing was measured and the final test is still available. Leave out preprocess (the gateway then uses outputs/model.json) "+
			"or write a corrected outputs/model.json in a new run.", runID, strings.Join(diffs, "; "))}
}

// ProbeImages is how many images of the validation set ask whether the model answers differently (R2.5).
const ProbeImages = 4

// probeImages picks the probe of the variation check (R2.5): up to n images of the validation set, one
// per class in turn, so that the question "did all of them get the same answer?" is asked about images of
// different classes — a model that does not look at its input answers all of them alike, while four
// images of one class may well get the same answer from a good model. The images come sorted per class by
// SHA-256 (Partition), so two checks of the same file ask the same images.
func probeImages(validation []checkImage, n int) []checkImage {
	byClass := map[string][]checkImage{}
	var classes []string
	for _, im := range validation {
		if _, has := byClass[im.Class]; !has {
			classes = append(classes, im.Class)
		}
		byClass[im.Class] = append(byClass[im.Class], im)
	}
	sort.Strings(classes)
	var out []checkImage
	for round := 0; len(out) < n; round++ {
		before := len(out)
		for _, c := range classes {
			if round < len(byClass[c]) && len(out) < n {
				out = append(out, byClass[c][round])
			}
		}
		if len(out) == before {
			break // every class is exhausted
		}
	}
	return out
}

// jobImages is the list of images one check container gets: the sample of the images the model saw (for
// the class order), the probe and the measured set, each image once (P11: the check names no file of the
// user, the tar holds them under their SHA-256).
func jobImages(groups ...[]checkImage) []Image {
	seen := map[string]bool{}
	var out []Image
	for _, list := range groups {
		for _, im := range list {
			if seen[im.SHA] {
				continue
			}
			seen[im.SHA] = true
			out = append(out, Image{ID: im.SHA, Ext: im.Ext, Data: im.Data})
		}
	}
	return out
}

// judgeOwnRun turns the raw predictions of an own-model check into the report: the basic checks of the
// file (R2.5), the measurement (measure) and the judgement of the shared rules (R2.4).
func (s *Service) judgeOwnRun(ctx context.Context, chatID string, in JudgeInput, out Output, file FileRef,
	pre PreprocessFigures, sample, probe, measured []checkImage, seen map[string]Item, skipped int) Report {
	in.PreSummary = pre.Short()
	byID := s.measure(&in, out, sample, measured, seen)
	basic := basicFigures(out.Model, byID, probe, in.Mapping, in.MappingOK, in.DeclaredClassNames)
	if basic.Varied != nil && !*basic.Varied {
		in.ConstantOutput = true
	}
	// Two things about the file itself make a measurement impossible (R2.5): the model does not load, or
	// it does not take exactly one input. The basic checks stay in the report either way.
	switch {
	case out.Model.LoadError != "":
		in.NotMeasuredReason = "the file does not load in onnxruntime: " + out.Model.LoadError
	case len(out.Model.Inputs) != 1:
		in.NotMeasuredReason = fmt.Sprintf("the model has %d inputs; a check feeds exactly one image tensor", len(out.Model.Inputs))
	}
	in.Previous = s.previousCheck(ctx, chatID, in.EvalDataset.ID, FileSubject(file.SHA256))
	rep := JudgeOwnModel(in, file, pre, basic)
	rep.Note = ownReportNote(in, file, skipped, len(measured))
	if in.Collapse != nil {
		// NT4: a new training does not help when the check feeds the images differently.
		for i, sg := range rep.Suggestions {
			if sg.ID == suggestionRetrain.ID {
				rep.Suggestions[i] = suggestionCheckPreprocess
			}
		}
	}
	return rep
}

// basicFigures is what the check found out about the file itself (R2.5): whether it loads, its input and
// output shapes, the number of classes it shows, whether it answers differently at all and — only when
// the agent declared class names — whether the order it declared is the one the model shows.
func basicFigures(model ModelInfo, byID map[string]Prediction, probe []checkImage, mapping map[int]string,
	mappingOK bool, declared []string) *BasicFigures {
	_, classes, _ := classificationShape(model)
	b := &BasicFigures{
		Loads: model.LoadError == "", LoadError: model.LoadError,
		Inputs: model.Inputs, Outputs: model.Outputs, Classes: classes,
		Varied: variedOutputs(byID, probe),
	}
	if declared != nil {
		// The derived order is the one the gateway trusts (P9); the declared one is only compared with it.
		ok := mappingOK && !declaredOrderDiffers(mapping, declared)
		b.ClassOrder = &ok
	}
	return b
}

// variedOutputs asks whether the model answers differently for different images (R2.5): it compares the
// answers of the probe images. nil when fewer than two of them could be run — then nothing is compared
// (the reasons of the verdict name the images that failed instead).
func variedOutputs(byID map[string]Prediction, probe []checkImage) *bool {
	var answers [][]float64
	for _, im := range probe {
		p, ok := byID[im.SHA]
		if !ok || !p.OK || len(p.Probs) == 0 {
			continue
		}
		answers = append(answers, p.Probs)
	}
	if len(answers) < 2 {
		return nil
	}
	same := true
	for _, a := range answers[1:] {
		if !sameProbs(answers[0], a) {
			same = false
			break
		}
	}
	varied := !same
	return &varied
}

// sameProbs reports whether two answers are the same output, value by value. Exact equality is the point:
// a model that answers the very same numbers for different images does not look at its input.
func sameProbs(a, b []float64) bool {
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

// notMeasuredOwn is a report of an own-model check that was decided before the file ran: the origin is
// unknown or there is no dataset to measure on. The file, the preprocessing and — when the file was run
// and only the dataset is missing — the basic checks stand in the figures.
func notMeasuredOwn(in JudgeInput, file FileRef, pre PreprocessFigures, basic *BasicFigures, reason string) Report {
	rep := JudgeOwnModel(in, file, pre, basic)
	rep.Verdict = NotMeasured
	rep.Reasons = []string{reason}
	return rep
}

// ownReportNote is the line under the header of an own-model check (H114): it names the eval dataset and
// the datasets the notebook server of this chat received, never an image and never the number of images
// (that stands in the card under the sample).
func ownReportNote(in JudgeInput, file FileRef, skipped, measured int) string {
	names := make([]string, 0, len(in.SeenDatasets))
	for _, d := range in.SeenDatasets {
		if d.Name != "" {
			names = append(names, d.Name)
		} else {
			names = append(names, "dataset "+itoa(int(d.ID)))
		}
	}
	against := "no dataset"
	if len(names) > 0 {
		against = strings.Join(names, ", ")
	}
	note := fmt.Sprintf("Images of dataset %s that never went into a notebook run of this chat (checked by SHA-256 against %s)",
		in.EvalDataset.Name, against)
	if skipped > 0 {
		note += fmt.Sprintf("; %d images were skipped (no or several tags)", skipped)
	}
	if in.SkippedPredictions > 0 {
		note += fmt.Sprintf("; %d of %d measured images could not be run", in.SkippedPredictions, measured)
	}
	return note + "."
}

// --- the origin of an uploaded model (R2.6) ---

// RecordUploadedModel records where a model came from that was uploaded from a file of the sandbox (R2.6).
// Only a file a notebook run of this chat handed over gets a row: so a check of that file stays the check
// of the model (W4), and a file the agent brought along itself keeps no origin (P2). The datasets the runs
// of the chat received are what the model could have seen (R2.3). main.go wires it as the hook of an
// upload (platform.OnModelUploaded).
func RecordUploadedModel(ctx context.Context, st *store.Store, chatID string, modelID int64, sha256 string) error {
	if st == nil || modelID <= 0 {
		return nil
	}
	sha := strings.ToLower(sha256)
	if !shaRe.MatchString(sha) {
		return nil
	}
	if _, found, err := st.NotebookFetchOf(ctx, chatID, sha); err != nil || !found {
		return err
	}
	seen, err := st.NotebookSeenDatasetIDs(ctx, chatID)
	if err != nil {
		return err
	}
	return st.RecordModelOrigin(ctx, store.ModelOrigin{ModelID: modelID, ChatID: chatID,
		Source: store.OriginNotebook, SourceSHA: sha, SeenDatasetIDs: seen})
}

// --- the line about the check of a file (R2.7) ---

// registerOwnModel adds the paths of the own-model check (R2). They live under the check prefix, so
// delegation, class, approval and log apply to them like to every other call.
func (s *Service) registerOwnModel() {
	prefix := regexp.QuoteMeta(platform.AssessPrefix)
	// What the last finished check of a file says (R2.7): the approval of an upload names it.
	s.Handle(http.MethodGet, regexp.MustCompile(`^`+prefix+`/files/(`+platform.AssessSHARe+`)/latest$`), s.fileCheck)
}

// FileCheckOf reads the last finished final check of a file in a chat (R2.7): the approval of an upload
// names its verdict before the file leaves the sandbox.
func (s *Service) FileCheckOf(ctx context.Context, chatID, sha256 string) (ModelCheck, error) {
	return s.checkOfSubjects(ctx, chatID, FileSubject(sha256))
}

// fileCheck is GET /_agw/assess/files/<sha256>/latest (R2.7): the line an upload approval adds about the
// file it sends. The gateway writes the wording, so that every place that names a check names it alike.
func (s *Service) fileCheck(ctx context.Context, chatID string, req platform.Request, groups []string) (platform.Result, error) {
	sha := strings.ToLower(groups[1])
	if !shaRe.MatchString(sha) {
		return errResult("not a SHA-256"), nil
	}
	c, err := s.FileCheckOf(ctx, chatID, sha)
	if err != nil {
		return platform.Result{}, err
	}
	return jsonResult(map[string]any{"sha256": sha, "line": c.Text()})
}

// checkOfSubjects reads what the last finished check of any of these subjects says (W4): the verdict the
// lines of the approvals name. The last finished check with final=true decides; with only finished
// interim checks the answer says so, and without any finished check nothing was checked.
func (s *Service) checkOfSubjects(ctx context.Context, chatID string, subjects ...string) (ModelCheck, error) {
	if s.St == nil {
		return ModelCheck{}, nil
	}
	var best *store.Assessment
	for _, subject := range subjects {
		row, found, err := s.St.LastFinishedFinalCheck(ctx, chatID, subject)
		if err != nil {
			return ModelCheck{}, err
		}
		if found && (best == nil || row.CreatedAt.After(best.CreatedAt)) {
			r := row
			best = &r
		}
	}
	// The newest finished check of any kind: the order of output_labels is part of the model's metadata, not
	// of the held-out images, so an interim check after the final one sees the labels as they are now (#226).
	var latest *store.Assessment
	for _, subject := range subjects {
		row, found, err := s.St.LastFinishedCheck(ctx, chatID, subject)
		if err != nil {
			return ModelCheck{}, err
		}
		if found && (latest == nil || row.CreatedAt.After(latest.CreatedAt)) {
			r := row
			latest = &r
		}
	}
	labelsWrong := false
	if latest != nil {
		lc, err := modelCheckOf(*latest)
		if err != nil {
			return ModelCheck{}, err
		}
		labelsWrong = lc.LabelOrderWrong
	}
	if best != nil {
		out, err := modelCheckOf(*best)
		if err != nil {
			return ModelCheck{}, err
		}
		if latest != nil && latest.CreatedAt.After(best.CreatedAt) {
			out.LabelOrderWrong = labelsWrong
		}
		return out, nil
	}
	// No final check: an interim one is not a verdict, but the wording may say that there is one (L8).
	if latest != nil {
		return ModelCheck{Found: true, LabelOrderWrong: labelsWrong}, nil
	}
	return ModelCheck{}, nil
}

// modelCheckOf reads the verdict of one stored check row.
func modelCheckOf(row store.Assessment) (ModelCheck, error) {
	out := ModelCheck{Found: true, Final: row.Final}
	var rep struct {
		Verdict  Verdict  `json:"verdict"`
		Measured int      `json:"measured_images"`
		Reasons  []string `json:"reasons"`
		Figures  struct {
			Overall struct {
				Accuracy float64 `json:"accuracy"`
			} `json:"overall"`
		} `json:"figures"`
	}
	if json.Unmarshal(row.Report, &rep) != nil {
		return ModelCheck{}, nil
	}
	out.Verdict = rep.Verdict
	out.Measured = rep.Measured
	out.Accuracy = rep.Figures.Overall.Accuracy
	if len(rep.Reasons) > 0 {
		out.Reason = rep.Reasons[0]
	}
	for _, r := range rep.Reasons {
		if strings.HasPrefix(r, ReasonOutputLabelsPrefix) {
			out.LabelOrderWrong = true
		}
	}
	return out, nil
}

// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

// The tools of the training check (R1): the split of a dataset before a training and the check of the
// model afterwards. Both work on virtual paths under AssessPrefix, which the manager answers with the
// check service (G1.5), so delegation, effect class, approval and log run as for every other call, and
// the gateway decides what a check runs on (the agent never names the measured set).
//
// init appends the tools, so that other features can add theirs in parallel.

// AssessSplitPath and AssessCheckPath are the paths of the training check (R1.1, R1.7).
const (
	AssessSplitPath = AssessPrefix + "/splits"
	AssessCheckPath = AssessPrefix + "/checks"
)

func init() {
	Tools = append(Tools, splitDataset, assessModel)
	registerDescriber(describeSplitDataset)
	registerDescriber(describeAssessModel)
	registerDescriber(describeBuildImageWithCheck)
}

// describeSplitDataset writes the summary of the split (R1.3): what will be created, from which dataset
// and how the images are split. The exact numbers come out of the call when it runs; the approval names
// what the gateway can read before it — the number of images of the source dataset.
func describeSplitDataset(c SummaryCall, s *summarizer) (string, []string, bool) {
	if c.Method != http.MethodPost || c.Path != AssessSplitPath {
		return "", nil, false
	}
	id := scalarText(s.body["dataset_id"])
	name, _ := s.body["name"].(string)
	share := 70
	if v := scalarText(s.body["train_share"]); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			share = n
		}
	}
	for _, k := range []string{"dataset_id", "name", "train_share"} {
		if _, ok := s.body[k]; ok {
			s.usedBody[k] = true
		}
	}
	sourceName, images, classes, readErr := s.datasetForSplit(id)
	if name == "" {
		switch {
		case sourceName != "":
			name = sourceName + "-train"
		default:
			if n, isPh := placeholder(id); isPh {
				name = fmt.Sprintf("<name of the dataset from step %d>-train", n)
			} else {
				name = "<source name>-train"
			}
		}
	}
	head := fmt.Sprintf("Create dataset %q from %s for held-out checks", name, s.datasetText(id, sourceName, readErr))
	details := []string{fmt.Sprintf("Training share: %d %%", share)}
	if images > 0 {
		train := int(float64(images)*float64(share)/100 + 0.5)
		details = append(details,
			fmt.Sprintf("About %d of %d images go into %q; about %d are kept back", train, images, name, images-train),
			"The split is counted per class, by the SHA-256 of the images; the exact numbers stand in the answer of the tool.")
	}
	if images > 0 {
		kept := strconv.Itoa(images - int(float64(images)*float64(share)/100+0.5))
		details = append(details, kept+" images are kept back; the agent never sees which.")
	} else {
		details = append(details,
			"The images are split when the step runs; the exact numbers stand in the answer of the tool.",
			"The other images are kept back; the agent never sees which.")
	}
	if len(classes) > 0 {
		details = append(details, "Classes: "+quoteAll(classes))
	}
	details = append(details, "Downloading "+s.datasetText(id, sourceName, readErr)+" also syncs its CVAT annotations.")
	return head, details, true
}

// describeAssessModel writes the summary of the check (R1.7). The check downloads its inputs and runs
// the model in a container of the gateway; the agent never names the measured images.
func describeAssessModel(c SummaryCall, s *summarizer) (string, []string, bool) {
	if c.Method != http.MethodPost || c.Path != AssessCheckPath {
		return "", nil, false
	}
	modelID := scalarText(s.body["model_id"])
	evalID := scalarText(s.body["eval_dataset_id"])
	final := false
	if v, ok := s.body["final"].(bool); ok {
		final = v
	}
	for _, k := range []string{"model_id", "eval_dataset_id", "final"} {
		if _, ok := s.body[k]; ok {
			s.usedBody[k] = true
		}
	}
	modelName := ""
	if plainID(modelID) {
		if m, _ := s.object("/models/" + modelID); m != nil {
			modelName, _ = m["name"].(string)
		}
	}
	head := "Check model " + s.modelText(modelID, map[string]any{"name": modelName}, "")
	if evalID != "" {
		name, _, _, readErr := s.datasetForSplit(evalID)
		head += " on " + s.datasetText(evalID, name, readErr)
	} else {
		head += " on the dataset the gateway saw it being trained on"
	}
	details := []string{
		"The gateway downloads the model and its training log, and the datasets it measures on; downloading a dataset also syncs its CVAT annotations.",
		"It runs the model in a container of its own, in the background, on images the model did not see, and reports the figures and a verdict (usable, questionable, not measured).",
	}
	if final {
		details = append(details, "final: the hold-out set is measured; this works once per dataset and chat.")
	} else {
		details = append(details, "Without final the validation images are measured, for the comparison of rounds.")
	}
	return head, details, true
}

// datasetText names a dataset for a summary: its name when it could be read, otherwise its id.
func (s *summarizer) datasetText(id, name, readErr string) string {
	if _, isPh := placeholder(id); isPh {
		return s.idText("dataset", id)
	}
	if name != "" {
		return "dataset " + id + " " + strconv.Quote(name)
	}
	if readErr != "" && s.read != nil {
		return "dataset " + id + " (name not readable: " + readErr + ")"
	}
	return "dataset " + id
}

// datasetForSplit reads the name, the number of files and the classes of a dataset for a summary.
func (s *summarizer) datasetForSplit(id string) (name string, images int, classes []string, readErr string) {
	if _, isPh := placeholder(id); isPh {
		return "", 0, nil, ""
	}
	if !plainID(id) {
		return "", 0, nil, "not a dataset id"
	}
	m, err := s.object("/datasets/" + id)
	name, _ = m["name"].(string)
	switch n := m["filecount"].(type) {
	case json.Number:
		if v, cerr := n.Int64(); cerr == nil {
			images = int(v)
		}
	case float64:
		images = int(n)
	}
	if l, ok := m["annotation_labels"].([]any); ok {
		for _, e := range l {
			if str, isStr := e.(string); isStr {
				classes = append(classes, str)
			}
		}
	}
	return name, images, classes, err
}

// MsgTrainShareFraction is the answer to a train_share given as a fraction such as 0.7.
const MsgTrainShareFraction = "train_share is a whole percentage: 50, 60, 70, 80 or 90 (70 means 70 %), not a fraction such as 0.7"

// isFraction reports whether v reads as a number strictly between 0 and 1.
func isFraction(v any) bool {
	var f float64
	var err error
	switch x := v.(type) {
	case json.Number:
		f, err = x.Float64()
	case float64:
		f = x
	case string:
		f, err = strconv.ParseFloat(strings.TrimSpace(x), 64)
	default:
		return false
	}
	return err == nil && f > 0 && f < 1
}

// splitDataset is the tool of R1.1: it creates a training dataset and keeps about a third of the
// annotated images back for a check.
var splitDataset = Tool{
	Name: "split_dataset", CLI: "split-dataset",
	Desc: "Splits an annotated classification dataset into a training dataset and images that are kept back for a check " +
		"(train_share is a whole percentage — 50, 60, 70, 80 or 90, default 70 — of the images per class that go into a new " +
		"dataset, by default named <dataset>-train; the rest is kept back and the gateway alone decides which). Train on the training dataset afterwards: only then can {{tool:assess_model}} measure " +
		"the model on images it did not see. Needs approval: it creates a dataset and downloads the source, which also " +
		"syncs its CVAT annotations. The answer names both counts and check_images (the validation and held-out images the later check will find; it needs at least 5 and 20); " +
		"when they are too few it carries check_warning and, if a smaller train_share would do, suggested_train_share.",
	Write: true,
	Params: []Param{
		{Name: "dataset_id", Type: "integer", Desc: "id of the annotated classification dataset (one tag per image)", Required: true},
		{Name: "name", Type: "string", Desc: "name of the new training dataset (default <source>-train)"},
		{Name: "train_share", Type: "integer", Desc: "share of the images that goes into the training dataset: 50, 60, 70, 80 or 90 (default 70)"},
	},
	build: func(a Args) (Request, error) {
		id, ok, err := a.int("dataset_id")
		if err != nil {
			return Request{}, err
		}
		if !ok || id <= 0 {
			return Request{}, errors.New("dataset_id missing")
		}
		name, _, err := a.str("name")
		if err != nil {
			return Request{}, err
		}
		if isFraction(a["train_share"]) {
			return Request{}, errors.New(MsgTrainShareFraction)
		}
		share, hasShare, err := a.int("train_share")
		if err != nil {
			return Request{}, err
		}
		body := map[string]any{"dataset_id": id}
		if name != "" {
			body["name"] = name
		}
		if hasShare {
			if share < 50 || share > 90 || share%10 != 0 {
				return Request{}, errors.New("train_share must be 50, 60, 70, 80 or 90")
			}
			body["train_share"] = share
		}
		b, err := json.Marshal(body)
		return Request{Method: http.MethodPost, Path: AssessSplitPath, Body: b}, err
	},
}

// assessModel is the tool of R1.7 (a platform model, R1) and R2.2 (a file of the sandbox, R2): it starts
// a check in the background.
var assessModel = Tool{
	Name: "assess_model", CLI: "assess-model",
	Desc: "Starts a check of a result: the gateway runs the model in a container of its own on images the model " +
		"did not see and reports accuracy, the figures per class, the majority baseline and a verdict " +
		"(usable, questionable, not measured). It answers at once with a check_id; follow it with {{tool:assess_status}}, whose " +
		"report starts with the header to begin your reply with (the gateway also wakes you when it is done). " +
		"final=true measures the hold-out set and works once per dataset and chat: use it only when the model will not " +
		"change any more; compare a retrained model with final=false. Needs approval; it downloads the model, the " +
		"training log and the datasets (which syncs their CVAT annotations) and reads the model's own figures. " +
		"Give the model either as model_id (a platform model of this chat) or as path (a file the {{tool:notebook_fetch}} of " +
		"this chat wrote into the sandbox, e.g. /workspace/notebook/<run>/model.onnx): the gateway checks the file " +
		"itself, before {{tool:upload_model}}, and answers not measured with an unknown origin for a file that no run of this " +
		"chat handed over.",
	Write: true,
	Params: []Param{
		{Name: "model_id", Type: "integer", Desc: "id of the platform model to check (either this or path)"},
		{Name: "path", Type: "string", Desc: "model file in the sandbox that {{tool:notebook_fetch}} brought out of a run of this chat (either this or model_id)", Path: true},
		{Name: "eval_dataset_id", Type: "integer", Desc: "dataset to measure on (default: the dataset the seen training dataset was split from)"},
		{Name: "final", Type: "boolean", Desc: "true: measure the hold-out set (once per dataset and chat), default false"},
		{Name: "class_names", Type: "array", Desc: "for path: the class names in the order the outputs of your model mean them, e.g. [\"0\",\"1\"] (taken from outputs/model.json of the run when it wrote one); the gateway compares this order with the order it derives from images the model saw"},
		{Name: "preprocess", Type: "object", Desc: "for path: how your model expects an image: {color: rgb|gray, size: [h,w], scale, mean, std, layout: nchw|nhwc}; default the platform template (rgb, 1/255, nchw, no mean/std); taken from outputs/model.json of the run when it wrote one; with final=true a preprocess that differs from that file is refused before anything runs"},
	},
	build: func(a Args) (Request, error) {
		id, ok, err := a.int("model_id")
		if err != nil {
			return Request{}, err
		}
		path, hasPath, err := a.str("path")
		if err != nil {
			return Request{}, err
		}
		asModel := ok && id > 0
		switch {
		case asModel && hasPath:
			return Request{}, errors.New("give either model_id or path, not both")
		case !asModel && !hasPath:
			return Request{}, errors.New("model_id or path missing")
		}
		body := map[string]any{}
		files := []File(nil)
		if id > 0 {
			body["model_id"] = id
		} else {
			// The bytes are read by the socket and travel as the upload of this call (R2.2), so what the
			// check measures is what a later upload_model sends; the path is named for the approval.
			body["path"] = path
			files = []File{{Field: "modelfile", Path: path}}
		}
		eval, hasEval, err := a.int("eval_dataset_id")
		if err != nil {
			return Request{}, err
		}
		if hasEval {
			if eval <= 0 {
				return Request{}, errors.New("eval_dataset_id must be a dataset id")
			}
			body["eval_dataset_id"] = eval
		}
		final, err := argBool(a, "final")
		if err != nil {
			return Request{}, err
		}
		if final {
			body["final"] = true
		}
		names, err := a.strings("class_names")
		if err != nil {
			return Request{}, err
		}
		if names != nil {
			if id > 0 {
				return Request{}, errors.New("class_names belong to path: the class order of a platform model comes from its training data")
			}
			if err := checkClassNames(names); err != nil {
				return Request{}, err
			}
			body["class_names"] = names
		}
		pre, hasPre, err := a.object("preprocess")
		if err != nil {
			return Request{}, err
		}
		if hasPre {
			if id > 0 {
				return Request{}, errors.New("preprocess belongs to path: a platform model is measured with the template's preprocessing")
			}
			checked, names, err := checkPreprocess(pre)
			if err != nil {
				return Request{}, err
			}
			// An empty declaration is none: the call does not carry it, and the check uses the default of
			// the platform templates (R2.4).
			if names {
				body["preprocess"] = json.RawMessage(checked)
			}
		}
		b, err := json.Marshal(body)
		if err != nil {
			return Request{}, err
		}
		return Request{Method: http.MethodPost, Path: AssessCheckPath, Body: b, Files: files}, nil
	},
}

// argBool reads an optional flag. The CLI and some models send it as text ("true"), the MCP schema as a
// real boolean; both are accepted.
func argBool(a Args, name string) (bool, error) {
	v, ok := a[name]
	if !ok || v == nil {
		return false, nil
	}
	switch x := v.(type) {
	case bool:
		return x, nil
	case string:
		b, err := strconv.ParseBool(strings.TrimSpace(x))
		if err != nil {
			return false, errors.New(name + " must be true or false")
		}
		return b, nil
	}
	return false, errors.New(name + " must be true or false")
}

// --- the line about the model check in an approval (R1.14) ---

// describeBuildImageWithCheck writes the summary of POST /container-images/build and adds one line about
// the check of the model (R1.14). The text of the built-in template is the base; the line follows the
// detail that names the model, so that the two lines about the model stand together (L9).
func describeBuildImageWithCheck(c SummaryCall, s *summarizer) (string, []string, bool) {
	if c.Method != http.MethodPost || c.Path != "/container-images/build" {
		return "", nil, false
	}
	head, details := s.buildImage()
	line, ok := s.modelCheckLine(scalarText(s.body["model_id"]))
	if !ok {
		return head, details, true
	}
	for i, d := range details {
		if strings.HasPrefix(d, "Model:") {
			return head, slices.Insert(details, i+1, line), true
		}
	}
	return head, append(details, line), true
}

// modelCheckLine asks the gateway what the last finished check of this model in this chat says (R1.14)
// and renders it in one line. ok is false when the answer cannot be read: then the approval carries no
// such line instead of a wrong one.
func (s *summarizer) modelCheckLine(modelID string) (string, bool) {
	if p, isPh := placeholder(modelID); isPh {
		return fmt.Sprintf("Model check: none yet (the model is created by step %d)", p), true
	}
	if modelID == "" || !plainID(modelID) {
		return "Model check: none in this chat", true
	}
	v, errText := s.get(AssessPrefix+"/models/"+modelID+"/latest", nil)
	if errText != "" {
		return "", false
	}
	m, ok := v.(map[string]any)
	if !ok {
		return "", false
	}
	line, _ := m["line"].(string)
	if line == "" {
		return "Model check: none in this chat", true
	}
	return "Model check: " + line, true
}

// --- where a model came from (R1.6) ---

// ModelIDOf reads the id of the model out of an answer of the platform (the model that was just
// created): modelSummary keeps the platform's answer under "model" and repeats id and name next to it.
func ModelIDOf(res Result) int64 {
	var doc struct {
		ID json.Number `json:"id"`
	}
	if json.Unmarshal([]byte(res.Body), &doc) != nil || doc.ID == "" {
		return 0
	}
	id, err := doc.ID.Int64()
	if err != nil {
		return 0
	}
	return id
}

// chatKey carries the chat of a platform call through the steps of a tool (R1.6): only the socket knows
// it when it runs a tool, and only there is the origin of a created object to be had.
type chatKey struct{}

// WithChat attaches the chat a platform call belongs to. The socket sets it when it runs a tool with
// steps; a raw request carries none, and then no origin is recorded.
func WithChat(ctx context.Context, chatID string) context.Context {
	if chatID == "" {
		return ctx
	}
	return context.WithValue(ctx, chatKey{}, chatID)
}

// ChatIDFrom returns the chat from WithChat (otherwise empty).
func ChatIDFrom(ctx context.Context) string {
	s, _ := ctx.Value(chatKey{}).(string)
	return s
}

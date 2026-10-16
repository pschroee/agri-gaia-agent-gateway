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
		name = sourceName + "-train"
	}
	head := fmt.Sprintf("Create dataset %q from %s for held-out checks", name, s.datasetText(id, sourceName, readErr))
	details := []string{fmt.Sprintf("Training share: %d %%", share)}
	if images > 0 {
		train := int(float64(images)*float64(share)/100 + 0.5)
		details = append(details,
			fmt.Sprintf("About %d of %d images go into %q; about %d are kept back", train, images, name, images-train),
			"The split is counted per class, by the SHA-256 of the images; the exact numbers stand in the answer of the tool.")
	}
	kept := "the rest"
	if images > 0 {
		kept = strconv.Itoa(images - int(float64(images)*float64(share)/100+0.5))
	}
	details = append(details, kept+" images are kept back; the agent never sees which.")
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
	if _, isPh := placeholder(id); !plainID(id) || isPh {
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

// splitDataset is the tool of R1.1: it creates a training dataset and keeps about a third of the
// annotated images back for a check.
var splitDataset = Tool{
	Name: "split_dataset", CLI: "split-dataset",
	Desc: "Splits an annotated classification dataset into a training dataset and images that are kept back for a check " +
		"(70 % of the images per class go into a new dataset, by default named <dataset>-train; the rest is kept back and " +
		"the gateway alone decides which). Train on the training dataset afterwards: only then can assess_model measure " +
		"the model on images it did not see. Needs approval: it creates a dataset and downloads the source, which also " +
		"syncs its CVAT annotations. The answer names both counts.",
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

// assessModel is the tool of R1.7: it starts the check of a platform model in the background.
var assessModel = Tool{
	Name: "assess_model", CLI: "assess-model",
	Desc: "Starts a check of a training result: the gateway runs the model in a container of its own on images the " +
		"model did not see in training and reports accuracy, the figures per class, the majority baseline and a verdict " +
		"(usable, questionable, not measured). It answers at once with a check_id; follow it with assess_status, whose " +
		"report starts with the header to begin your reply with (the gateway also wakes you when it is done). " +
		"final=true measures the hold-out set and works once per dataset and chat: use it only when the model will not " +
		"change any more; compare a retrained model with final=false. Needs approval; it downloads the model, the " +
		"training log and the datasets (which syncs their CVAT annotations) and reads the model's own figures.",
	Write: true,
	Params: []Param{
		{Name: "model_id", Type: "integer", Desc: "id of the platform model to check", Required: true},
		{Name: "eval_dataset_id", Type: "integer", Desc: "dataset to measure on (default: the dataset the training dataset was split from)"},
		{Name: "final", Type: "boolean", Desc: "true: measure the hold-out set (once per dataset and chat), default false"},
	},
	build: func(a Args) (Request, error) {
		id, ok, err := a.int("model_id")
		if err != nil {
			return Request{}, err
		}
		if !ok || id <= 0 {
			return Request{}, errors.New("model_id missing")
		}
		body := map[string]any{"model_id": id}
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
		b, err := json.Marshal(body)
		return Request{Method: http.MethodPost, Path: AssessCheckPath, Body: b}, err
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

// OnModelCreated is what the gateway does when a model was created from a training container in a chat
// (R1.6): it records where the model came from, so that a later check knows which images the model saw.
// The platform package does not know the store, so main.go wires it.
type OnModelCreated func(ctx context.Context, chatID string, modelID, trainID, datasetID int64)

var onModelCreated OnModelCreated

// SetOnModelCreated wires the callback that records the origin of a model (main.go). Not wired: no
// origin is recorded, and every check on such a model answers "origin unknown".
func SetOnModelCreated(f OnModelCreated) { onModelCreated = f }

// notifyModelCreated reports a model that was created from a training container of this chat.
func notifyModelCreated(ctx context.Context, modelID, trainID, datasetID int64) {
	if onModelCreated == nil || modelID <= 0 {
		return
	}
	if chatID := ChatIDFrom(ctx); chatID != "" {
		onModelCreated(ctx, chatID, modelID, trainID, datasetID)
	}
}

// modelIDOf reads the id of the model out of an answer of the platform (the model that was just
// created): modelSummary keeps the platform's answer under "model" and repeats id and name next to it.
func modelIDOf(res Result) int64 {
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

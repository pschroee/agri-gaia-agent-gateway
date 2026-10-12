// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Summaries of writing calls (issue #72). What an approval says will happen is written here, by the gateway, from the
// call that will go out (method, path, query, body, form, files; for a plan step the stored step) and from platform
// state the gateway reads itself. Text of the agent never enters a summary: a plan's title and a step's description
// stay separate fields that the UI marks as unverified.
//
// A summary is plain text: the first line says what happens, every further line is one detail ("Label: value").
// Every field of the call appears in it: a template names the fields it understands, and every other field follows as
// "Also sends …", so nothing the call carries is hidden behind a friendly sentence.

// SummaryFile is an upload of a call: its form field, its path in the sandbox, its size (-1: not read yet, as in a
// plan step) and its SHA-256.
type SummaryFile struct {
	Field  string
	Name   string
	Size   int64
	SHA256 string
}

// SummaryCall is what a summary describes. Path, body and query of a plan step may hold placeholders {{stepN.id}}.
// Tool is the platform tool that builds the call ("" for a raw request); it only selects what the tool itself
// guarantees (the owner of an image's repository, the fields update_model keeps).
type SummaryCall struct {
	Tool   string
	Method string
	Path   string
	Query  map[string]string
	Body   json.RawMessage
	Form   map[string][]string
	Files  []SummaryFile
}

// SummaryCallOf describes a request that is about to go out (a single approval). Uploads that were read carry their
// size and hash.
func SummaryCallOf(tool string, r Request) SummaryCall {
	c := SummaryCall{Tool: tool, Method: r.Method, Path: r.Path, Query: r.Query, Body: r.Body, Form: r.Form}
	for i, f := range r.Files {
		sf := SummaryFile{Field: f.Field, Name: f.Path, Size: -1}
		if i < len(r.Uploads) {
			sf.Size, sf.SHA256 = int64(len(r.Uploads[i].Data)), r.Uploads[i].SHA256
		}
		c.Files = append(c.Files, sf)
	}
	return c
}

// summaryReadTimeout bounds the reads of one summary; a slow platform gives a summary without current values.
const summaryReadTimeout = 10 * time.Second

var (
	summaryPlaceholderRe = regexp.MustCompile(`^\{\{step([1-9][0-9]?)\.id\}\}$`)
	sumDatasetRe         = regexp.MustCompile(`^/datasets/([^/]+)$`)
	sumModelRe           = regexp.MustCompile(`^/models/([^/]+)$`)
	sumFromTrainingRe    = regexp.MustCompile(`^/train/containers/([^/]+)/model$`)
	sumStartTrainingRe   = regexp.MustCompile(`^/train/containers/([^/]+)/run$`)
)

// Summarize writes the summary of a call. read reads the platform as the user (only reading GETs are sent through
// it); nil reads nothing, and the summary then names objects by id only.
func Summarize(ctx context.Context, read Doer, c SummaryCall) string {
	ctx, cancel := context.WithTimeout(ctx, summaryReadTimeout)
	defer cancel()
	s := &summarizer{ctx: ctx, read: read, call: c, cache: map[string]cached{}, usedBody: map[string]bool{}, usedForm: map[string]bool{}}
	s.decodeBody()
	head, details := s.describe()
	lines := append([]string{head}, details...)
	lines = append(lines, s.rest()...)
	return strings.Join(lines, "\n")
}

type cached struct {
	v   any
	err string
}

type summarizer struct {
	ctx      context.Context
	read     Doer
	call     SummaryCall
	cache    map[string]cached
	body     map[string]any // the body when it is a JSON object
	rawBody  bool           // a body that is not a JSON object
	usedBody map[string]bool
	usedForm map[string]bool
	usedFile bool
}

func (s *summarizer) decodeBody() {
	b := bytes.TrimSpace(s.call.Body)
	if len(b) == 0 {
		return
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var m map[string]any
	if err := d.Decode(&m); err != nil || m == nil {
		s.rawBody = true
		return
	}
	s.body = m
}

// describe picks the template of the route; the generic one lists every field.
func (s *summarizer) describe() (string, []string) {
	c := s.call
	switch {
	case c.Method == http.MethodPatch && sumDatasetRe.MatchString(c.Path):
		return s.patchDataset(sumDatasetRe.FindStringSubmatch(c.Path)[1])
	case c.Method == http.MethodDelete && sumDatasetRe.MatchString(c.Path):
		return "Delete " + s.named("dataset", sumDatasetRe.FindStringSubmatch(c.Path)[1]), []string{"This cannot be undone."}
	case c.Method == http.MethodPatch && sumModelRe.MatchString(c.Path):
		return s.patchModel(sumModelRe.FindStringSubmatch(c.Path)[1])
	case c.Method == http.MethodDelete && sumModelRe.MatchString(c.Path):
		return "Delete " + s.named("model", sumModelRe.FindStringSubmatch(c.Path)[1]), []string{"This cannot be undone."}
	case c.Method == http.MethodPost && c.Path == "/container-images/build":
		return s.buildImage()
	case c.Method == http.MethodGet && sumFromTrainingRe.MatchString(c.Path):
		id := sumFromTrainingRe.FindStringSubmatch(c.Path)[1]
		return "Create a new model from " + s.idText("training container", id),
			append(s.trainingDetails(id), "Every call creates another model, also from the same training container.")
	case c.Method == http.MethodPost && c.Path == "/train/config":
		return s.createTraining()
	case c.Method == http.MethodPost && sumStartTrainingRe.MatchString(c.Path):
		id := sumStartTrainingRe.FindStringSubmatch(c.Path)[1]
		return "Start " + s.idText("training container", id), s.trainingDetails(id)
	case c.Method == http.MethodPost && c.Path == "/datasets" && (len(c.Form) > 0 || len(c.Files) > 0):
		return s.uploadDataset()
	case c.Method == http.MethodPost && c.Path == "/models" && (len(c.Form) > 0 || len(c.Files) > 0):
		return s.uploadModel()
	case c.Method == http.MethodPost && c.Path == templatesPath+"/create/git":
		return s.importTemplate()
	}
	return "Send " + c.Method + " " + c.Path + " to the platform", []string{"No description for this call: every field it sends is listed."}
}

// rest lists everything of the call that the template did not name: query, body fields, form fields, files.
func (s *summarizer) rest() []string {
	var out []string
	prefix := "Also sends "
	keys := sortedKeys(s.call.Query)
	for _, k := range keys {
		out = append(out, fmt.Sprintf("%squery %s = %s", prefix, k, strconv.Quote(s.call.Query[k])))
	}
	if s.rawBody {
		out = append(out, prefix+"body "+compactJSON(s.call.Body))
	}
	for _, k := range sortedKeys(s.body) {
		if !s.usedBody[k] {
			out = append(out, fmt.Sprintf("%s%s = %s", prefix, k, value(s.body[k])))
		}
	}
	for _, k := range sortedKeys(s.call.Form) {
		if !s.usedForm[k] {
			out = append(out, fmt.Sprintf("%sform %s = %s", prefix, k, quoteAll(s.call.Form[k])))
		}
	}
	if !s.usedFile {
		out = append(out, s.fileLines()...)
	}
	return out
}

func (s *summarizer) fileLines() []string {
	s.usedFile = true
	out := make([]string, 0, len(s.call.Files))
	for _, f := range s.call.Files {
		line := fmt.Sprintf("File (%s): %s", f.Field, strconv.Quote(f.Name))
		var extra []string
		if f.Size >= 0 {
			extra = append(extra, sizeText(f.Size))
		}
		if f.SHA256 != "" {
			extra = append(extra, "sha256 "+f.SHA256)
		}
		if len(extra) > 0 {
			line += " (" + strings.Join(extra, ", ") + ")"
		}
		out = append(out, line)
	}
	return out
}

// --- templates ---

func (s *summarizer) patchDataset(id string) (string, []string) {
	name, ok := s.body["name"].(string)
	if !ok {
		return "Change " + s.named("dataset", id), nil
	}
	s.usedBody["name"] = true
	cur, has := s.datasetName(id)
	if !has {
		return fmt.Sprintf("Rename %s to %s", s.idText("dataset", id), strconv.Quote(name)), nil
	}
	if cur == name {
		return fmt.Sprintf("Rename %s %s to the same name (no change)", s.idText("dataset", id), strconv.Quote(cur)), nil
	}
	return fmt.Sprintf("Rename %s %s → %s", s.idText("dataset", id), strconv.Quote(cur), strconv.Quote(name)), nil
}

func (s *summarizer) patchModel(id string) (string, []string) {
	var cur map[string]any
	curErr := ""
	if p, isPh := placeholder(id); isPh {
		curErr = fmt.Sprintf("created by step %d", p)
	} else if plainID(id) {
		cur, curErr = s.object("/models/" + id)
	}
	curName, _ := cur["name"].(string)
	var head string
	newName, hasName := s.body["name"].(string)
	switch {
	case hasName && cur != nil && curName != newName:
		head = fmt.Sprintf("Rename %s %s → %s", s.idText("model", id), strconv.Quote(curName), strconv.Quote(newName))
	case hasName && cur == nil:
		head = fmt.Sprintf("Rename %s to %s", s.idText("model", id), strconv.Quote(newName))
	case cur != nil:
		head = fmt.Sprintf("Update %s %s", s.idText("model", id), strconv.Quote(curName))
	default:
		head = "Update " + s.idText("model", id)
	}
	if hasName {
		s.usedBody["name"] = true
	}
	var details, unchanged, missing []string
	for _, f := range ModelPatchFields {
		v, sent := s.body[f]
		if !sent {
			missing = append(missing, f)
			continue
		}
		s.usedBody[f] = true
		switch {
		case cur == nil:
			if f != "name" {
				details = append(details, fmt.Sprintf("%s: set to %s", f, value(v)))
			}
		case sameJSON(cur[f], v):
			unchanged = append(unchanged, f)
		case f != "name":
			details = append(details, fmt.Sprintf("%s: %s → %s", f, value(cur[f]), value(v)))
		}
	}
	if cur == nil && curErr != "" && !strings.HasPrefix(curErr, "created by") {
		details = append(details, "Current values not readable: "+curErr)
	}
	if cur != nil && len(details) == 0 && (!hasName || curName == newName) {
		details = append(details, "No field changes its value.")
	}
	if len(unchanged) > 0 {
		details = append(details, "Unchanged (sent with the current value): "+strings.Join(unchanged, ", "))
	}
	if len(missing) > 0 {
		if s.call.Tool == "update_model" {
			details = append(details, "Kept as they are: "+strings.Join(missing, ", ")+" (the gateway reads the model right before the change and sends their current values)")
		} else {
			details = append(details, "Not sent, so the platform sets them to null: "+strings.Join(missing, ", "))
		}
	}
	return head, details
}

func (s *summarizer) buildImage() (string, []string) {
	for _, k := range []string{"container_template_id", "model_id", "tag", "architecture", "repository"} {
		if _, ok := s.body[k]; ok {
			s.usedBody[k] = true
		}
	}
	mid := scalarText(s.body["model_id"])
	tid := scalarText(s.body["container_template_id"])
	tag, _ := s.body["tag"].(string)
	arch, _ := s.body["architecture"].(string)
	repo, _ := s.body["repository"].(string)

	var model map[string]any
	modelErr := ""
	if _, isPh := placeholder(mid); !isPh && mid != "" {
		model, modelErr = s.object("/models/" + mid)
	}
	repoNote := ""
	if s.call.Tool == "build_inference_image" && strings.HasPrefix(repo, "OWNER/") {
		name := strings.TrimPrefix(repo, "OWNER/")
		owner, _ := model["owner"].(string)
		switch {
		case owner != "":
			repo = owner + "/" + name
			repoNote = " (built by the gateway: owner of the model / name)"
		default:
			if p, isPh := placeholder(mid); isPh {
				repo = fmt.Sprintf("<owner of the model from step %d>/%s", p, name)
			} else {
				repo = "<owner of model " + mid + ">/" + name
			}
			repoNote = " (the gateway sets the owner of the model when the step runs)"
		}
	}
	image := repo + ":" + tag
	head := "Build inference image " + strconv.Quote(image)
	if arch != "" {
		head += " for " + arch
	}
	details := []string{
		"Model: " + s.modelText(mid, model, modelErr),
		"Template: " + s.templateText(tid),
		"Repository: " + strconv.Quote(repo) + repoNote,
		"Tag: " + strconv.Quote(tag),
		"Architecture: " + strconv.Quote(arch),
		"The image goes into the platform's registry.",
	}
	return head, details
}

func (s *summarizer) createTraining() (string, []string) {
	for _, k := range []string{"provider", "architecture", "category", "dataset_id", "train_config", "export_config"} {
		if _, ok := s.body[k]; ok {
			s.usedBody[k] = true
		}
	}
	provider, _ := s.body["provider"].(string)
	arch, _ := s.body["architecture"].(string)
	category, _ := s.body["category"].(string)
	ds := scalarText(s.body["dataset_id"])
	head := fmt.Sprintf("Create a training container %s %s", provider, arch)
	if category != "" {
		head += " (" + category + ")"
	}
	head += " on " + s.named("dataset", ds)
	var details []string
	if tc, ok := s.body["train_config"].(map[string]any); ok && len(tc) > 0 {
		details = append(details, "Training parameters: "+fieldList(tc))
	} else if v, ok := s.body["train_config"]; ok {
		details = append(details, "Training parameters: "+value(v))
	}
	if ec, ok := s.body["export_config"].(map[string]any); ok && len(ec) > 0 {
		details = append(details, "ONNX export: "+fieldList(ec))
	} else if v, ok := s.body["export_config"]; ok && v != nil {
		details = append(details, "ONNX export: "+value(v))
	} else {
		details = append(details, "ONNX export: none (the model is saved as model.pt without tensor metadata)")
	}
	details = append(details, "Uses compute time: the platform builds the training image in the background.")
	return head, details
}

func (s *summarizer) uploadDataset() (string, []string) {
	f := s.call.Form
	name := first(f["name"])
	head := fmt.Sprintf("Upload a new dataset %s with %s", strconv.Quote(name), s.filesText())
	var details []string
	s.formDetail(&details, "description", "Description")
	s.formDetail(&details, "annotation_labels", "Labels")
	s.formDetail(&details, "dataset_type", "Dataset type")
	s.formDetail(&details, "semantic_labels", "Keywords")
	s.formDetail(&details, "metadata", "Metadata")
	s.formDetail(&details, "includes_annotation_file", "Annotation file included (the last file)")
	s.formDetail(&details, "is_classification_dataset", "Classification dataset")
	s.usedForm["name"] = true
	details = append(details, s.fileLines()...)
	return head, details
}

func (s *summarizer) uploadModel() (string, []string) {
	f := s.call.Form
	head := fmt.Sprintf("Upload a new model %s", strconv.Quote(first(f["name"])))
	if fm := first(f["format"]); fm != "" {
		head += " (format " + fm + ")"
	}
	s.usedForm["name"], s.usedForm["format"] = true, true
	var details []string
	s.formDetail(&details, "description", "Description")
	s.formDetail(&details, "semantic_labels", "Keywords")
	details = append(details, s.fileLines()...)
	return head, details
}

func (s *summarizer) importTemplate() (string, []string) {
	f := s.call.Form
	head := fmt.Sprintf("Import the container template %s from %s", strconv.Quote(first(f["name"])), first(f["git_url"]))
	if ref := first(f["git_ref"]); ref != "" {
		head += " (" + ref + ")"
	}
	s.usedForm["name"], s.usedForm["git_url"], s.usedForm["git_ref"] = true, true, true
	var details []string
	s.formDetail(&details, "description", "Description")
	return head, details
}

// --- reads and names ---

// get reads a JSON value with one GET (cached per path); err is a short reason when it cannot be read.
func (s *summarizer) get(path string, query map[string]string) (any, string) {
	key := path + "?" + fmt.Sprint(query)
	if c, ok := s.cache[key]; ok {
		return c.v, c.err
	}
	var v any
	errText := ""
	if s.read == nil {
		errText = "not read"
	} else {
		r := Request{Method: http.MethodGet, Path: path, Query: query}
		if r.Writes() {
			errText = "not a reading call"
		} else if res, stop, err := ReadJSON(s.ctx, s.read, r, &v); stop {
			switch {
			case err != nil:
				errText = err.Error()
			case res.Message != "":
				errText = res.Message
			default:
				errText = fmt.Sprintf("HTTP %d", res.HTTPStatus)
			}
			v = nil
		}
	}
	s.cache[key] = cached{v: v, err: errText}
	return v, errText
}

func (s *summarizer) object(path string) (map[string]any, string) {
	v, err := s.get(path, nil)
	m, ok := v.(map[string]any)
	if !ok && err == "" {
		err = "unexpected answer"
	}
	return m, err
}

func (s *summarizer) list(path string) ([]any, string) {
	v, err := s.get(path, map[string]string{"limit": "1000"})
	l, ok := v.([]any)
	if !ok && err == "" {
		err = "unexpected answer"
	}
	return l, err
}

func (s *summarizer) datasetName(id string) (string, bool) {
	if _, isPh := placeholder(id); isPh || !plainID(id) {
		return "", false
	}
	m, _ := s.object("/datasets/" + id)
	n, ok := m["name"].(string)
	return n, ok
}

// idText: "dataset 16", or "the dataset created by step 1" for a placeholder.
func (s *summarizer) idText(kind, id string) string {
	if p, isPh := placeholder(id); isPh {
		return fmt.Sprintf("the %s created by step %d", kind, p)
	}
	if id == "" {
		return kind + " (no id)"
	}
	return kind + " " + id
}

// named: "dataset 16 \"mnist\"" with the name the platform reports now.
func (s *summarizer) named(kind, id string) string {
	t := s.idText(kind, id)
	if _, isPh := placeholder(id); isPh || !plainID(id) {
		return t
	}
	var path string
	switch kind {
	case "dataset":
		path = "/datasets/" + id
	case "model":
		path = "/models/" + id
	default:
		return t
	}
	m, err := s.object(path)
	if n, ok := m["name"].(string); ok {
		return t + " " + strconv.Quote(n)
	}
	if err != "" && s.read != nil {
		return t + " (name not readable: " + err + ")"
	}
	return t
}

func (s *summarizer) modelText(id string, m map[string]any, err string) string {
	t := s.idText("model", id)
	if n, ok := m["name"].(string); ok {
		t += " " + strconv.Quote(n)
		if f, ok := m["format"].(string); ok && f != "" {
			t += ", format " + f
		}
		return t
	}
	if err != "" && s.read != nil {
		return t + " (not readable: " + err + ")"
	}
	return t
}

func (s *summarizer) templateText(id string) string {
	t := "container template " + id
	if id == "" || !plainID(id) {
		return t
	}
	l, err := s.list(templatesPath)
	for _, e := range l {
		m, _ := e.(map[string]any)
		if scalarText(m["id"]) == id {
			if n, ok := m["name"].(string); ok {
				t += " " + strconv.Quote(n)
			}
			if u, ok := m["git_url"].(string); ok && u != "" {
				t += " from " + u
				if r, ok := m["git_ref"].(string); ok && r != "" {
					t += " (" + r + ")"
				}
			}
			return t
		}
	}
	if err != "" && s.read != nil {
		return t + " (not readable: " + err + ")"
	}
	if s.read != nil {
		return t + " (not installed)"
	}
	return t
}

// trainingDetails describes a training container from the platform's list (GET /train/containers/{id} fails there).
func (s *summarizer) trainingDetails(id string) []string {
	if _, isPh := placeholder(id); isPh || !plainID(id) || s.read == nil {
		return nil
	}
	l, err := s.list("/train/containers")
	for _, e := range l {
		m, _ := e.(map[string]any)
		if scalarText(m["id"]) != id {
			continue
		}
		provider, _ := m["provider"].(string)
		arch, _ := m["architecture"].(string)
		status, _ := m["status"].(string)
		line := fmt.Sprintf("Training: %s %s", provider, arch)
		if ds := scalarText(m["dataset_id"]); ds != "" {
			line += " on " + s.named("dataset", ds)
		}
		if status != "" {
			line += ", status " + status
		}
		return []string{line}
	}
	if err != "" {
		return []string{"Training container not readable: " + err}
	}
	return []string{"Training container not in the platform's list"}
}

func (s *summarizer) filesText() string {
	n := len(s.call.Files)
	var total int64
	known := n > 0
	for _, f := range s.call.Files {
		if f.Size < 0 {
			known = false
			break
		}
		total += f.Size
	}
	t := fileCount(n)
	if known {
		t += " (" + sizeText(total) + ")"
	}
	return t
}

func (s *summarizer) formDetail(details *[]string, key, label string) {
	if v, ok := s.call.Form[key]; ok {
		s.usedForm[key] = true
		*details = append(*details, label+": "+quoteAll(v))
	}
}

// --- values ---

func placeholder(s string) (int, bool) {
	m := summaryPlaceholderRe.FindStringSubmatch(s)
	if m == nil {
		return 0, false
	}
	n, _ := strconv.Atoi(m[1])
	return n, true
}

var plainIDRe = regexp.MustCompile(`^[0-9]+$`)

func plainID(s string) bool { return plainIDRe.MatchString(s) }

// scalarText gives a number or a string as text ("" for anything else).
func scalarText(v any) string {
	switch t := v.(type) {
	case json.Number:
		return t.String()
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	}
	return ""
}

// value renders a JSON value: strings quoted, numbers and literals as they are, the rest as compact JSON.
func value(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case string:
		return strconv.Quote(t)
	case json.Number:
		return t.String()
	case bool:
		return strconv.FormatBool(t)
	}
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	if err := e.Encode(v); err != nil {
		return fmt.Sprint(v)
	}
	return strings.TrimRight(b.String(), "\n")
}

func compactJSON(raw []byte) string {
	var b bytes.Buffer
	if json.Compact(&b, raw) == nil {
		return b.String()
	}
	return strconv.Quote(string(raw))
}

// fieldList: "a = 1, b = \"x\"", keys sorted.
func fieldList(m map[string]any) string {
	keys := sortedKeys(m)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+" = "+value(m[k]))
	}
	return strings.Join(parts, ", ")
}

func sameJSON(a, b any) bool {
	x, e1 := json.Marshal(a)
	y, e2 := json.Marshal(b)
	return e1 == nil && e2 == nil && bytes.Equal(x, y)
}

func quoteAll(v []string) string {
	q := make([]string, len(v))
	for i, s := range v {
		q[i] = strconv.Quote(s)
	}
	return strings.Join(q, ", ")
}

func first(v []string) string {
	if len(v) == 0 {
		return ""
	}
	return v[0]
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sizeText(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d bytes", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	case n < 1024*1024*1024:
		return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
	}
	return fmt.Sprintf("%.1f GB", float64(n)/(1024*1024*1024))
}

// SummaryHead is the first line of a summary.
func SummaryHead(summary string) string {
	head, _, _ := strings.Cut(summary, "\n")
	return head
}

// SummarizePlan writes the summaries of a plan's steps in order. A step that changes an object (PATCH of a dataset
// or model) changes what later steps of the same plan see: reads of that object return the platform's state with
// the earlier steps' fields applied, and the later step says so. Without that, a second rename would show the name
// from before the first one as the "old" name.
func SummarizePlan(ctx context.Context, read Doer, calls []SummaryCall) []string {
	changes := map[string]map[string]any{} // object path → fields set by earlier steps
	changedBy := map[string][]int{}        // object path → numbers of those steps
	out := make([]string, len(calls))
	for i, c := range calls {
		var used []int
		overlay := read
		if read != nil {
			overlay = func(ctx context.Context, r Request) (Result, error) {
				res, err := read(ctx, r)
				fields, ok := changes[r.Path]
				if err != nil || !ok || len(r.Query) > 0 || res.Status != "ok" || res.HTTPStatus < 200 || res.HTTPStatus >= 300 {
					return res, err
				}
				d := json.NewDecoder(strings.NewReader(res.Body))
				d.UseNumber()
				var obj map[string]any
				if d.Decode(&obj) != nil || obj == nil {
					return res, err
				}
				for k, v := range fields {
					obj[k] = v
				}
				b, merr := json.Marshal(obj)
				if merr != nil {
					return res, err
				}
				res.Body = string(b)
				used = append(used, changedBy[r.Path]...)
				return res, nil
			}
		}
		out[i] = Summarize(ctx, overlay, c)
		if len(used) > 0 {
			slices.Sort(used)
			used = slices.Compact(used)
			steps := make([]string, len(used))
			for j, n := range used {
				steps[j] = strconv.Itoa(n)
			}
			out[i] += "\nCurrent values as they will be after step " + strings.Join(steps, ", ") + " of this plan."
		}
		if c.Method == http.MethodPatch && (sumDatasetRe.MatchString(c.Path) || sumModelRe.MatchString(c.Path)) {
			d := json.NewDecoder(bytes.NewReader(c.Body))
			d.UseNumber()
			var fields map[string]any
			if d.Decode(&fields) == nil && fields != nil {
				if changes[c.Path] == nil {
					changes[c.Path] = map[string]any{}
				}
				for k, v := range fields {
					changes[c.Path][k] = v
				}
				changedBy[c.Path] = append(changedBy[c.Path], i+1)
			}
		}
	}
	return out
}

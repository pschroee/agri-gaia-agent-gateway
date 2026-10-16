// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package annotation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"agw/internal/platform"
	"agw/internal/store"
)

// The build of a pre-annotation function from a model of the platform (V2). The gateway packs the
// fixed template of this repository with the model and hands the archive to the platform, which
// calls nuctl with the file name of the archive; the name is built here, so no name the agent chose
// can reach that shell (E9, E10).

// WatchKindPlatformTask is the kind of a platform task (autonomy A-E2). The build of a function is
// observed with it; the gateway runs no polling of its own for it.
const WatchKindPlatformTask = "platform_task"

// FunctionRoutePath is the path of a call that builds a function; the kind above is bound to it.
var FunctionRoutePath = regexp.MustCompile(`^` + regexp.QuoteMeta(platform.AnnotationPrefix) + `/functions$`)

// platformTaskRe reads the task out of the Location of the platform, which its background tasks
// answer as /tasks/<id> (backend, routers/common.py).
var platformTaskRe = regexp.MustCompile(`^/tasks/([0-9]+)(?:$|\?)`)

// Defaults of the two bounds when the configuration does not carry them; they are the defaults of
// config.Config (AGW_ANNOTATION_MAX_FUNCTIONS, AGW_ANNOTATION_MODEL_MAX_MB).
const (
	defaultMaxFunctions = 3
	defaultModelMaxMB   = 300
)

// annotationArchivePath is the call the platform offers for a function archive; it is blocked for the
// agent itself (it passes the file name into a shell) and reached here through DoTrusted (E5).
const (
	annotationArchivePath  = "/datasets/auto-annotation"
	annotationArchiveField = "auto_annotation_archive"
)

// modelFormatONNX is the only format a function may be built from: ONNX is data, a .pt file is a
// pickle and therefore code (E10).
const modelFormatONNX = "onnx"

// zipMagic is the beginning of an archive: the download of a model with several objects is one.
var zipMagic = []byte("PK\x03\x04")

// FunctionRef reads the build task out of the answer of a built function (G1.7).
func FunctionRef(_ platform.Request, res platform.Result) (string, bool) {
	var a functionAnswer
	if json.Unmarshal([]byte(res.Body), &a) != nil || a.PlatformTask == "" {
		return "", false
	}
	return a.PlatformTask, true
}

// functionAnswer is the answer of a built function.
type functionAnswer struct {
	stepList
	Function     string `json:"function"`
	State        string `json:"state"`
	PlatformTask string `json:"platform_task"`
	Reused       bool   `json:"reused"`
	Note         string `json:"note"`
}

// maxFunctions is the number of functions one user may have built; a value that is not set falls
// back to the default of the configuration.
func (s *Service) maxFunctions() int {
	if s.MaxFunctions > 0 {
		return s.MaxFunctions
	}
	return defaultMaxFunctions
}

// modelMaxMB is the bound of the model file a function is built from.
func (s *Service) modelMaxMB() int64 {
	if s.ModelMaxMB > 0 {
		return s.ModelMaxMB
	}
	return defaultModelMaxMB
}

// modelMaxBytes is the same bound in bytes.
func (s *Service) modelMaxBytes() int64 { return s.modelMaxMB() << 20 }

// createFunction builds a function from a model of the user and provides it through the platform.
func (s *Service) createFunction(ctx context.Context, chatID string, req platform.Request) (platform.Result, error) {
	r := &recorder{chatID: chatID}
	var body struct {
		ModelID   int64    `json:"model_id"`
		Kind      string   `json:"kind"`
		Labels    []string `json:"labels"`
		InputSize *int     `json:"input_size"`
	}
	if err := json.Unmarshal(req.Body, &body); err != nil {
		return failed(errors.New("the body of the function is not readable"))
	}
	// A raw call over the request tool reaches this path as well: check everything the tool checks.
	if body.ModelID <= 0 {
		return failed(errors.New("model_id missing"))
	}
	if _, known := template(body.Kind); !known {
		return failed(fmt.Errorf("kind must be %s or %s", KindClassification, KindDetection))
	}
	if body.InputSize != nil && (*body.InputSize < minInputSize || *body.InputSize > maxInputSize) {
		return failed(fmt.Errorf("input_size must be between %d and %d pixels", minInputSize, maxInputSize))
	}
	user, err := s.user(ctx, chatID)
	if err != nil {
		return failed(err)
	}
	m, err := s.ownModel(ctx, r, body.ModelID, user)
	if err != nil {
		return failed(err)
	}
	// The format of the platform is the first signal, the file name the second; both must say ONNX.
	// A PyTorch file is a pickle and therefore code (E10).
	if m.Format != modelFormatONNX {
		return failed(fmt.Errorf("model %d has format %s; only ONNX models can become a function (PyTorch files are code)", body.ModelID, m.Format))
	}
	if m.FileName != "" && !strings.EqualFold(fileExt(m.FileName), ".onnx") {
		return failed(fmt.Errorf("model %d carries the file %s; only an ONNX file can become a function (PyTorch files are code)", body.ModelID, m.FileName))
	}
	if m.FileSize != nil && *m.FileSize > s.modelMaxBytes() {
		return failed(fmt.Errorf("the model file of model %d is %d bytes; at most %d MiB (AGW_ANNOTATION_MODEL_MAX_MB)",
			body.ModelID, *m.FileSize, s.modelMaxMB()))
	}
	labels, err := functionLabels(body.Labels, m.OutputLabels, m.OutputShape, body.Kind)
	if err != nil {
		return failed(err)
	}
	size, err := functionInputSize(body.InputSize, m.InputShape, body.Kind)
	if err != nil {
		return failed(err)
	}
	rows, err := s.St.AnnotationFunctionsOf(ctx, user)
	if err != nil {
		return platform.Result{}, err
	}
	if len(rows) >= s.maxFunctions() {
		return failed(deny("you already have %d agent functions (at most %d); ask the operator to remove one",
			len(rows), s.maxFunctions()))
	}
	// The file of the model is read by the gateway (E5); the bound of the configuration caps what is
	// held in memory here. An answer that is truncated (or empty) is no model.
	download := platform.Request{Method: http.MethodGet,
		Path: "/models/" + strconv.FormatInt(body.ModelID, 10) + "/download"}
	res, raw, err := s.platformCall(ctx, r, download, s.modelMaxBytes()+1)
	if err != nil {
		return failed(err)
	}
	if res.Status != "ok" || res.HTTPStatus < 200 || res.HTTPStatus >= 300 {
		return failed(platformAnswered{req: download.String(), status: res.HTTPStatus})
	}
	if int64(len(raw)) == 0 || int64(len(raw)) > s.modelMaxBytes() {
		return failed(fmt.Errorf("the model file of model %d is empty or larger than %d MiB (AGW_ANNOTATION_MODEL_MAX_MB)",
			body.ModelID, s.modelMaxMB()))
	}
	// The platform answers the bytes of the file only when the model holds one object; several
	// objects come as an archive (backend, routers/models.py), and an archive is no model file.
	if bytes.HasPrefix(raw, zipMagic) {
		return failed(fmt.Errorf("the download of model %d came as an archive; the function needs a model with a single ONNX file", body.ModelID))
	}
	sum := sha256.Sum256(raw)
	digest := hex.EncodeToString(sum[:])
	name := FunctionName(user, body.ModelID, digest)

	// The same model gives the same name: a function that is ready and listed is used instead of
	// deployed again (the same name would simply redeploy it).
	if row, known := findFunction(rows, name); known && row.State == store.AnnotationFunctionReady {
		ses, err := s.currentSession(ctx, chatID, r)
		if err != nil {
			return failed(err)
		}
		listed, err := s.C.Functions(ctx, ses)
		r.cvat("GET /api/lambda/functions", err)
		if err != nil {
			return failed(err)
		}
		if findListed(listed, name) {
			return okAnswer(functionAnswer{stepList: r.steps(), Function: name, State: store.AnnotationFunctionReady,
				PlatformTask: row.PlatformTask, Reused: true,
				Note: "The function is already built and listed by CVAT; nothing was deployed again."})
		}
	}

	archive, err := BuildZip(name, body.Kind, labels, size, raw, m.Name, body.ModelID)
	if err != nil {
		return failed(err)
	}
	archiveSum := sha256.Sum256(archive)
	upload := platform.Request{
		Method: http.MethodPost, Path: annotationArchivePath,
		Files:   []platform.File{{Field: annotationArchiveField, Path: "/" + name + ".zip"}},
		Uploads: []platform.Upload{{Field: annotationArchiveField, Name: name + ".zip", Data: archive, SHA256: hex.EncodeToString(archiveSum[:])}},
	}
	res, _, err = s.platformCall(ctx, r, upload, maxJSONBytes)
	if err != nil {
		return failed(err)
	}
	if res.Status != "ok" || res.HTTPStatus < 200 || res.HTTPStatus >= 300 {
		return failed(platformAnswered{req: upload.String(), status: res.HTTPStatus})
	}
	task := taskOfLocation(res.Location)
	row, err := s.St.AddAnnotationFunction(ctx, store.AnnotationFunction{
		Name: name, ChatID: chatID, Username: user, ModelID: body.ModelID, ModelSHA256: digest,
		Kind: body.Kind, Labels: labels, InputSize: size, TemplateVersion: TemplateVersion,
		PlatformTask: task, State: store.AnnotationFunctionDeploying,
	})
	if err != nil {
		return platform.Result{}, err
	}
	note := "Building the function takes several minutes. It is usable when annotation-functions shows state ready."
	if task != "" {
		note = fmt.Sprintf("Building the function takes several minutes. Follow it with task %s; it is usable when annotation-functions shows state ready.", task)
	}
	return okAnswer(functionAnswer{stepList: r.steps(), Function: row.Name, State: row.State,
		PlatformTask: task, Reused: false, Note: note})
}

// taskOfLocation reads the id of the platform's background task out of a Location answer.
func taskOfLocation(loc string) string {
	if m := platformTaskRe.FindStringSubmatch(loc); m != nil {
		return m[1]
	}
	return ""
}

// fileExt is the extension of a file name in lower case.
func fileExt(name string) string {
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		return name[i:]
	}
	return ""
}

// FunctionName is the name of the function built from a model: the user, the model and the first
// eight hex digits of the model file. The name reaches the shell of the backend (nuctl takes the
// file name of the archive), so it carries letters, digits and dashes only (E9).
func FunctionName(user string, modelID int64, digest string) string {
	if len(digest) > 8 {
		digest = digest[:8]
	}
	return fmt.Sprintf("agw-%s-m%d-%s", sanitizeUser(user), modelID, digest)
}

// platformModel is the part of a model of the platform the service needs (backend, schemas/model.py).
type platformModel struct {
	ID           int64    `json:"id"`
	Name         string   `json:"name"`
	Owner        string   `json:"owner"`
	Format       string   `json:"format"`
	FileName     string   `json:"file_name"`
	FileSize     *int64   `json:"file_size"`
	InputShape   []int    `json:"input_shape"`
	OutputShape  []int    `json:"output_shape"`
	OutputLabels []string `json:"output_labels"`
}

// ownModel reads a model and checks that it belongs to the chat's user (E6): the platform checks no
// owner on models, and a function of a foreign model would carry its file.
func (s *Service) ownModel(ctx context.Context, r *recorder, id int64, user string) (platformModel, error) {
	var m platformModel
	req := platform.Request{Method: http.MethodGet, Path: "/models/" + strconv.FormatInt(id, 10)}
	if err := s.platformJSON(ctx, r, req, maxJSONBytes, &m); err != nil {
		return platformModel{}, err
	}
	if m.Owner != user {
		return m, deny("model %d belongs to %s; only your own models", id, m.Owner)
	}
	return m, nil
}

// functionLabels decides the class names of a function: the labels of the call first, then the
// output_labels of the model, then — for a classification model — class_0 … class_{n-1} with n from
// output_shape. A detector without class names is refused: its output carries numbers only, and the
// mapping of a run would have nothing to hold on (E7).
func functionLabels(arg, model []string, outputShape []int, kind string) ([]string, error) {
	labels := arg
	if len(labels) == 0 {
		labels = model
	}
	if len(labels) == 0 && kind == KindClassification {
		n := lastShape(outputShape)
		if n > maxClasses {
			return nil, fmt.Errorf("the model has %d outputs; give the %d class names of the function (labels)", n, maxClasses)
		}
		if n > 0 {
			labels = make([]string, 0, n)
			for i := 0; i < n; i++ {
				labels = append(labels, "class_"+strconv.Itoa(i))
			}
		}
	}
	switch {
	case len(labels) == 0 && kind == KindDetection:
		return nil, errors.New("detection needs the class names in output order (labels or the model's output_labels)")
	case len(labels) == 0:
		return nil, errors.New("the model has no output_labels and no usable output_shape; give the class names of the function (labels)")
	case len(labels) > maxClasses:
		return nil, fmt.Errorf("a function has at most %d class names, got %d", maxClasses, len(labels))
	}
	seen := map[string]bool{}
	for _, l := range labels {
		if !classRe.MatchString(l) {
			return nil, fmt.Errorf("class name %q is not allowed (letters, digits, space, _ . - ; at most 64 characters)", l)
		}
		if seen[l] {
			return nil, fmt.Errorf("class name %q is given twice; the order of the output must be unambiguous", l)
		}
		seen[l] = true
	}
	return labels, nil
}

// lastShape is the number of outputs of a model: the last value of its output_shape.
func lastShape(shape []int) int {
	if len(shape) == 0 {
		return 0
	}
	if n := shape[len(shape)-1]; n > 0 {
		return n
	}
	return 0
}

// functionInputSize is the square input of a function: what the agent named, else the side of an
// input_shape [1,3,S,S], else the default of the kind. The templates feed one RGB image, so a model
// with another input cannot be served by them.
func functionInputSize(arg *int, inputShape []int, kind string) (int, error) {
	if arg != nil {
		if *arg < minInputSize || *arg > maxInputSize {
			return 0, fmt.Errorf("input_size must be between %d and %d pixels", minInputSize, maxInputSize)
		}
		return *arg, nil
	}
	if len(inputShape) == 0 {
		if kind == KindDetection {
			return defaultInputDetection, nil
		}
		return defaultInputClassification, nil
	}
	if len(inputShape) != 4 || inputShape[0] != 1 || inputShape[1] != 3 {
		return 0, fmt.Errorf("the model expects input_shape %v; the template feeds one RGB image (1,3,S,S). Only such a model can become a function here", inputShape)
	}
	if inputShape[2] != inputShape[3] || inputShape[2] < minInputSize || inputShape[2] > maxInputSize {
		return 0, fmt.Errorf("the model expects %dx%d; the template serves a square input between %d and %d pixels",
			inputShape[2], inputShape[3], minInputSize, maxInputSize)
	}
	return inputShape[2], nil
}

// findFunction names a row of the register by its name.
func findFunction(rows []store.AnnotationFunction, name string) (store.AnnotationFunction, bool) {
	for _, row := range rows {
		if row.Name == name {
			return row, true
		}
	}
	return store.AnnotationFunction{}, false
}

// findListed reports whether CVAT lists a function.
func findListed(all []Function, id string) bool {
	for _, f := range all {
		if f.ID == id {
			return true
		}
	}
	return false
}

// syncFunctionStates brings the register of a user together with the list CVAT answers (V2): a
// function that appears there is ready, one that disappeared is missing, and the failure of a build
// is read from the platform's task. What changed is written into the register.
//
// A task that cannot be read changes nothing: the catalogue of CVAT is the truth about a function
// that runs, and the task list of the platform is transient. The read still stands in the steps.
func (s *Service) syncFunctionStates(ctx context.Context, r *recorder, rows []store.AnnotationFunction, live map[string]bool) error {
	for i := range rows {
		state := rows[i].State
		switch {
		case live[rows[i].Name]:
			state = store.AnnotationFunctionReady
		case rows[i].State == store.AnnotationFunctionReady:
			// The catalogue of the platform is volatile (V1, 1.3): a function it no longer lists is
			// gone, whether it was removed by hand or lost with the store.
			state = store.AnnotationFunctionMissing
		case rows[i].State == store.AnnotationFunctionDeploying && rows[i].PlatformTask != "":
			status, err := s.platformTaskState(ctx, r, rows[i].PlatformTask)
			if err != nil {
				continue
			}
			if status == taskFailed {
				state = store.AnnotationFunctionFailed
			}
		}
		if state == rows[i].State {
			continue
		}
		if err := s.St.SetAnnotationFunctionState(ctx, rows[i].Name, state); err != nil {
			return err
		}
		rows[i].State = state
	}
	return nil
}

// taskFailed is the status of a build task that ended with an error.
const taskFailed = "failed"

// platformTaskState reads the status of a background task of the platform. The message of a failed
// task is not kept: it comes from the backend and can name paths of the instance.
func (s *Service) platformTaskState(ctx context.Context, r *recorder, id string) (string, error) {
	var task struct {
		Status string `json:"status"`
	}
	req := platform.Request{Method: http.MethodGet, Path: "/tasks/" + id}
	if err := s.platformJSON(ctx, r, req, maxJSONBytes, &task); err != nil {
		return "", err
	}
	return strings.ToLower(strings.TrimSpace(task.Status)), nil
}

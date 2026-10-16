// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package annotation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"agw/internal/delegation"
	"agw/internal/platform"
	"agw/internal/store"
	"agw/internal/watch"
)

// modelOf is the answer of GET /models/{id} for a model of the chat's user; extra overrides single
// fields (owner, format, file_name, file_size, input_shape, output_shape, output_labels).
func modelOf(id int64, extra map[string]any) string {
	m := map[string]any{
		"id": id, "name": "mnist_efficientnet", "owner": "test", "format": "onnx",
		"file_name": "model.onnx", "file_size": 1024, "input_shape": []int{1, 3, 64, 64},
		"output_shape": []int{1, 2}, "output_labels": []string{"class_0", "class_1"},
	}
	for k, v := range extra {
		m[k] = v
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// digestOf is the SHA-256 of a model file, as the gateway computes it.
func digestOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// functionsOf reads the register of a user.
func functionsOf(t *testing.T, e *testEnv, username string) []store.AnnotationFunction {
	t.Helper()
	rows, err := e.st.AnnotationFunctionsOf(context.Background(), username)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// functionsList asks the service for the function list of the chat's user.
func functionsList(t *testing.T, e *testEnv, chatID string) functionsAnswer {
	t.Helper()
	res := e.do(t, chatID, http.MethodGet, platform.AnnotationPrefix+"/functions", "")
	if res.Status != "ok" {
		t.Fatalf("annotation_functions: %+v", res)
	}
	var answer functionsAnswer
	if err := json.Unmarshal([]byte(res.Body), &answer); err != nil {
		t.Fatal(err)
	}
	return answer
}

// watchFor classifies a call and asks internal/watch for the kind and the reference of the job it
// started (G1.7).
func watchFor(req platform.Request, res platform.Result) (string, string, bool) {
	return watch.For(delegation.Classify(req), req, res)
}

// The name of a function is built from the user, the model and the hash of the model file: it is
// unique per model, and it stays inside the characters that may reach the shell of the backend.
func TestFunctionName(t *testing.T) {
	digest := digestOf("ONNX-MODEL")
	name := FunctionName("agent-test", 5, digest)
	if name != "agw-agent-test-m5-"+digest[:8] {
		t.Errorf("name: %q", name)
	}
	if !functionNameRe.MatchString(name) {
		t.Errorf("the name does not follow the rule of a function: %q", name)
	}
	// Every character a user name can carry ends up in [a-z0-9-]; the prefix of the user is the one
	// the listing and the run use (V1, FunctionNamePrefix).
	for _, user := range []string{"Philipp.Schröer", "a", "Ein.SEHR_langer--Name!!", "---", "Agent Test 2"} {
		got := FunctionName(user, 12, digest)
		if !functionNameRe.MatchString(got) {
			t.Errorf("%s: %q does not follow the rule", user, got)
		}
		if !strings.HasPrefix(got, FunctionNamePrefix(user)) {
			t.Errorf("%s: %q does not start with the prefix of the user", user, got)
		}
		if len(got) > 56 {
			t.Errorf("%s: %q is too long", user, got)
		}
	}
	// The full digest works as well as a short one, and a different file gives a different name.
	if got := FunctionName("test", 5, digest); got == FunctionName("test", 5, digestOf("OTHER")) {
		t.Error("two model files give the same name")
	}
}

// The classes of a function: the argument first, then the output_labels of the model, then class_i
// from the output shape; a detector without classes is refused, and a name CVAT could not carry.
func TestFunctionLabels(t *testing.T) {
	model := []string{"cat", "dog"}
	shape := []int{1, 3}
	if got, err := functionLabels([]string{"a", "b"}, model, shape, KindClassification); err != nil || !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("the argument wins: %v %v", got, err)
	}
	if got, err := functionLabels(nil, model, shape, KindClassification); err != nil || !reflect.DeepEqual(got, model) {
		t.Errorf("output_labels: %v %v", got, err)
	}
	if got, err := functionLabels(nil, nil, []int{1, 3}, KindClassification); err != nil || !reflect.DeepEqual(got, []string{"class_0", "class_1", "class_2"}) {
		t.Errorf("class_i: %v %v", got, err)
	}
	// A detector has no order it could guess from a shape.
	if _, err := functionLabels(nil, nil, []int{1, 5, 3}, KindDetection); err == nil ||
		!strings.Contains(err.Error(), "detection needs the class names in output order") {
		t.Errorf("detection without classes: %v", err)
	}
	// A classification model without either source.
	if _, err := functionLabels(nil, nil, nil, KindClassification); err == nil || !strings.Contains(err.Error(), "no output_labels") {
		t.Errorf("without any source: %v", err)
	}
	// Names CVAT and the YAML cannot carry, and one name twice.
	for _, labels := range [][]string{{`a"b`}, {"a:b"}, {""}, {strings.Repeat("c", 65)}, {"pig", "pig"}} {
		if _, err := functionLabels(labels, nil, nil, KindClassification); err == nil {
			t.Errorf("%v was accepted", labels)
		}
	}
	if _, err := functionLabels(make([]string, maxClasses+1), nil, nil, KindClassification); err == nil {
		t.Error("more classes than the limit were accepted")
	}
}

// The input side of a function comes from the argument, else from a square RGB input_shape, else
// from the default of the kind; a model the template cannot serve is refused.
func TestFunctionInputSize(t *testing.T) {
	size := 512
	if got, err := functionInputSize(&size, []int{1, 3, 64, 64}, KindClassification); err != nil || got != 512 {
		t.Errorf("the argument wins: %d %v", got, err)
	}
	if got, err := functionInputSize(nil, []int{1, 3, 64, 64}, KindDetection); err != nil || got != 64 {
		t.Errorf("input_shape: %d %v", got, err)
	}
	if got, err := functionInputSize(nil, nil, KindClassification); err != nil || got != defaultInputClassification {
		t.Errorf("classification default: %d %v", got, err)
	}
	if got, err := functionInputSize(nil, nil, KindDetection); err != nil || got != defaultInputDetection {
		t.Errorf("detection default: %d %v", got, err)
	}
	small, big := 8, 4096
	for _, arg := range []*int{&small, &big} {
		if _, err := functionInputSize(arg, nil, KindClassification); err == nil {
			t.Errorf("%d was accepted", *arg)
		}
	}
	for _, shape := range [][]int{{1, 1, 64, 64}, {1, 3, 64, 32}, {1, 3, 4096, 4096}, {2, 3, 64, 64}} {
		if _, err := functionInputSize(nil, shape, KindClassification); err == nil {
			t.Errorf("input_shape %v was accepted", shape)
		}
	}
}

// Building a function packs the archive, hands it to the platform and records the build with its
// task; the classes come from the model, the name from the file.
func TestFunctionBuild(t *testing.T) {
	e := newTestEnv(t)
	chatID := e.chat(t)
	e.pf.setModel(5, modelOf(5, nil))
	e.pf.setModelFile("ONNX-MODEL")
	e.pf.setTaskLocation("/tasks/7")

	res := e.do(t, chatID, http.MethodPost, platform.AnnotationPrefix+"/functions", `{"model_id":5,"kind":"classification"}`)
	var answer functionAnswer
	if err := json.Unmarshal([]byte(res.Body), &answer); err != nil {
		t.Fatal(err)
	}
	name := FunctionName("test", 5, digestOf("ONNX-MODEL"))
	if res.Status != "ok" || answer.Function != name || answer.State != store.AnnotationFunctionDeploying ||
		answer.PlatformTask != "7" || answer.Reused {
		t.Fatalf("build: %+v", answer)
	}
	if !strings.Contains(answer.Note, "task 7") || !strings.Contains(answer.Note, "annotation-functions shows state ready") {
		t.Errorf("the note does not say how to follow the build: %q", answer.Note)
	}
	if builds, sizes := e.pf.buildsOf(); builds != 1 || sizes[0] == 0 {
		t.Errorf("the archive did not reach the platform: %d %v", builds, sizes)
	}
	if !e.pf.sawCall("GET", "/models/5/download") {
		t.Errorf("the model file was not read: %v", e.pf.saw())
	}
	rows := functionsOf(t, e, "test")
	if len(rows) != 1 {
		t.Fatalf("register: %+v", rows)
	}
	row := rows[0]
	if row.Name != name || row.ChatID != chatID || row.ModelID != 5 || row.Kind != KindClassification ||
		row.InputSize != 64 || row.TemplateVersion != TemplateVersion || row.PlatformTask != "7" ||
		row.State != store.AnnotationFunctionDeploying {
		t.Errorf("row: %+v", row)
	}
	if row.ModelSHA256 != digestOf("ONNX-MODEL") {
		t.Errorf("the hash of the model is not the one of the file: %q", row.ModelSHA256)
	}
	// The classes came from output_labels, the input side from the square input_shape.
	if !reflect.DeepEqual(row.Labels, []string{"class_0", "class_1"}) {
		t.Errorf("classes: %v", row.Labels)
	}
	if len(stepsOf(t, res)) == 0 {
		t.Error("the answer carries no steps")
	}
}

// The build of a function is observed with the kind platform_task: the route hands the reference of
// the platform's task (autonomy A-E2), not a kind of its own (V1, E11).
func TestFunctionWatchRoute(t *testing.T) {
	e := newTestEnv(t)
	chatID := e.chat(t)
	e.pf.setModel(5, modelOf(5, nil))
	e.pf.setModelFile("ONNX-MODEL")
	e.pf.setTaskLocation("/tasks/7")
	e.svc.RegisterWatch()

	call := platform.Request{Method: http.MethodPost, Path: platform.AnnotationPrefix + "/functions"}
	res := e.do(t, chatID, http.MethodPost, platform.AnnotationPrefix+"/functions", `{"model_id":5,"kind":"classification"}`)
	kind, ref, ok := watchFor(call, res)
	if !ok || kind != WatchKindPlatformTask || ref != "7" {
		t.Fatalf("route: %q %q %v", kind, ref, ok)
	}
	// An answer without a task carries no reference; the build anyway (the platform answered a
	// Location the gateway cannot read) stays observable through the function list.
	if _, _, ok := watchFor(call, platform.Result{Status: "ok", Body: `{"function":"x"}`}); ok {
		t.Error("a function without a task gave a reference")
	}
}

// With the configuration of the gateway the bounds are the ones of the environment (V2): without a
// value the defaults hold, with one the value of the instance.
func TestFunctionBounds(t *testing.T) {
	e := newTestEnv(t)
	if got := e.svc.maxFunctions(); got != defaultMaxFunctions {
		t.Errorf("without a configuration: %d", got)
	}
	if got := e.svc.modelMaxMB(); got != defaultModelMaxMB || e.svc.modelMaxBytes() != defaultModelMaxMB<<20 {
		t.Errorf("without a configuration: %d %d", got, e.svc.modelMaxBytes())
	}
	e.svc.MaxFunctions, e.svc.ModelMaxMB = 5, 12
	if e.svc.maxFunctions() != 5 || e.svc.modelMaxMB() != 12 || e.svc.modelMaxBytes() != 12<<20 {
		t.Errorf("with a configuration: %d %d", e.svc.maxFunctions(), e.svc.modelMaxBytes())
	}
}

// Only the user's own ONNX model within the size bound becomes a function (E6, E10). Nothing of a
// refused model is read or uploaded.
func TestFunctionOwnerAndFormat(t *testing.T) {
	for _, tc := range []struct {
		name  string
		extra map[string]any
		mb    int64
		want  string
	}{
		{name: "foreign model", extra: map[string]any{"owner": "other"}, want: "model 5 belongs to other; only your own models"},
		{name: "pytorch", extra: map[string]any{"format": "pytorch", "file_name": "model.pt"}, want: "has format pytorch; only ONNX models can become a function (PyTorch files are code)"},
		{name: "onnx metadata, pickle file", extra: map[string]any{"file_name": "model.pt"}, want: "carries the file model.pt; only an ONNX file can become a function"},
		{name: "too large", extra: map[string]any{"file_size": 2 << 20}, mb: 1, want: "at most 1 MiB"},
	} {
		e := newTestEnv(t)
		chatID := e.chat(t)
		e.pf.setModel(5, modelOf(5, tc.extra))
		e.pf.setModelFile("ONNX-MODEL")
		e.pf.setTaskLocation("/tasks/7")
		if tc.mb > 0 {
			e.svc.ModelMaxMB = tc.mb
		}
		res := e.do(t, chatID, http.MethodPost, platform.AnnotationPrefix+"/functions", `{"model_id":5,"kind":"classification"}`)
		if !strings.Contains(res.Message, tc.want) {
			t.Errorf("%s: %+v", tc.name, res)
		}
		if res.Status != "error" && res.Status != "denied" {
			t.Errorf("%s: status %q", tc.name, res.Status)
		}
		if builds, _ := e.pf.buildsOf(); builds != 0 {
			t.Errorf("%s: an archive was uploaded", tc.name)
		}
		if e.pf.sawCall("GET", "/models/5/download") {
			t.Errorf("%s: the model file was read", tc.name)
		}
		if rows := functionsOf(t, e, "test"); len(rows) != 0 {
			t.Errorf("%s: a row was written: %+v", tc.name, rows)
		}
	}
	// Several objects under a model come as an archive: an archive is no model file.
	e := newTestEnv(t)
	chatID := e.chat(t)
	e.pf.setModel(5, modelOf(5, nil))
	e.pf.setModelFile("PK\x03\x04model.onnx and a second file")
	res := e.do(t, chatID, http.MethodPost, platform.AnnotationPrefix+"/functions", `{"model_id":5,"kind":"classification"}`)
	if res.Status != "error" || !strings.Contains(res.Message, "came as an archive") {
		t.Errorf("an archive as a model: %+v", res)
	}
	if builds, _ := e.pf.buildsOf(); builds != 0 {
		t.Error("an archive was packed as a model")
	}

	// The model of another user is never read; a denied call leaves CVAT alone.
	e = newTestEnv(t)
	chatID = e.chat(t)
	e.pf.setModel(5, modelOf(5, map[string]any{"owner": "other"}))
	e.do(t, chatID, http.MethodPost, platform.AnnotationPrefix+"/functions", `{"model_id":5,"kind":"classification"}`)
	if got := e.fc.saw(); len(got) != 0 {
		t.Errorf("CVAT was called for a foreign model: %v", got)
	}
}

// One user may have at most AGW_ANNOTATION_MAX_FUNCTIONS functions (E14): at the bound the agent has
// to ask the operator, and nothing is deployed.
func TestFunctionLimit(t *testing.T) {
	e := newTestEnv(t)
	chatID := e.chat(t)
	e.svc.MaxFunctions = 1
	e.pf.setModel(5, modelOf(5, nil))
	e.pf.setModelFile("ONNX-MODEL")
	e.pf.setTaskLocation("/tasks/7")

	if res := e.do(t, chatID, http.MethodPost, platform.AnnotationPrefix+"/functions", `{"model_id":5,"kind":"classification"}`); res.Status != "ok" {
		t.Fatalf("the first function: %+v", res)
	}
	res := e.do(t, chatID, http.MethodPost, platform.AnnotationPrefix+"/functions", `{"model_id":5,"kind":"classification"}`)
	if res.Status != "denied" || res.Message != "you already have 1 agent functions (at most 1); ask the operator to remove one" {
		t.Fatalf("beyond the limit: %+v", res)
	}
	if builds, _ := e.pf.buildsOf(); builds != 1 {
		t.Errorf("a function was deployed beyond the limit: %d", builds)
	}
	// Without the configuration the default of the gateway holds.
	if e.svc.MaxFunctions != 1 {
		t.Errorf("the test changed the configuration: %d", e.svc.MaxFunctions)
	}
}

// The same model gives the same name: a function that is ready and listed by CVAT is used instead of
// deployed again.
func TestFunctionReuse(t *testing.T) {
	e := newTestEnv(t)
	chatID := e.chat(t)
	e.pf.setModel(5, modelOf(5, nil))
	e.pf.setModelFile("ONNX-MODEL")
	e.pf.setTaskLocation("/tasks/7")
	res := e.do(t, chatID, http.MethodPost, platform.AnnotationPrefix+"/functions", `{"model_id":5,"kind":"classification"}`)
	var first functionAnswer
	if err := json.Unmarshal([]byte(res.Body), &first); err != nil {
		t.Fatal(err)
	}
	// The build is done when CVAT lists the function; the listing of the gateway says so.
	e.fc.setFunctions(`[{"id":"` + first.Function + `","name":"` + first.Function + `","kind":"detector","labels":["class_0","class_1"]}]`)
	e.pf.setTask("7", `{"id":7,"status":"completed","completion_percentage":100}`)
	list := functionsList(t, e, chatID)
	if len(list.Functions) != 1 || list.Functions[0].State != store.AnnotationFunctionReady || list.Functions[0].Source != "yours" {
		t.Fatalf("listing after the build: %+v", list)
	}
	if row := functionsOf(t, e, "test"); row[0].State != store.AnnotationFunctionReady {
		t.Fatalf("the register was not brought up to date: %+v", row)
	}

	res = e.do(t, chatID, http.MethodPost, platform.AnnotationPrefix+"/functions", `{"model_id":5,"kind":"classification"}`)
	var again functionAnswer
	if err := json.Unmarshal([]byte(res.Body), &again); err != nil {
		t.Fatal(err)
	}
	if res.Status != "ok" || !again.Reused || again.State != store.AnnotationFunctionReady || again.PlatformTask != "7" {
		t.Fatalf("second call: %+v", again)
	}
	if builds, _ := e.pf.buildsOf(); builds != 1 {
		t.Errorf("the function was deployed a second time: %d", builds)
	}
	if rows := functionsOf(t, e, "test"); len(rows) != 1 {
		t.Errorf("a second row was written: %+v", rows)
	}
}

// The register follows CVAT: a build that appears there is ready, one that fails is failed, and one
// that disappears is missing. Every change is written into the register.
func TestFunctionStateSync(t *testing.T) {
	e := newTestEnv(t)
	chatID := e.chat(t)
	e.pf.setModel(5, modelOf(5, nil))
	e.pf.setModelFile("ONNX-MODEL")
	e.pf.setTaskLocation("/tasks/7")
	res := e.do(t, chatID, http.MethodPost, platform.AnnotationPrefix+"/functions", `{"model_id":5,"kind":"classification"}`)
	var answer functionAnswer
	if err := json.Unmarshal([]byte(res.Body), &answer); err != nil {
		t.Fatal(err)
	}

	// Not listed yet, the task of the platform still running: it keeps building.
	e.fc.setFunctions(`[]`)
	e.pf.setTask("7", `{"id":7,"status":"inprogress","completion_percentage":40}`)
	list := functionsList(t, e, chatID)
	if len(list.Functions) != 1 || list.Functions[0].State != store.AnnotationFunctionDeploying || list.Functions[0].ID != answer.Function {
		t.Fatalf("while building: %+v", list)
	}
	if !e.pf.sawCall("GET", "/tasks/7") {
		t.Errorf("the task of the build was not read: %v", e.pf.saw())
	}

	// The task failed: the function is failed and stays so.
	e.pf.setTask("7", `{"id":7,"status":"failed","message":"nuctl deploy failed"}`)
	list = functionsList(t, e, chatID)
	if list.Functions[0].State != store.AnnotationFunctionFailed {
		t.Fatalf("a failed build: %+v", list.Functions)
	}
	if row := functionsOf(t, e, "test"); row[0].State != store.AnnotationFunctionFailed {
		t.Fatalf("the register was not brought up to date: %+v", row)
	}

	// A second function: CVAT lists it, then it disappears.
	e.pf.setModelFile("ONNX-MODEL-2")
	res = e.do(t, chatID, http.MethodPost, platform.AnnotationPrefix+"/functions", `{"model_id":5,"kind":"classification"}`)
	var second functionAnswer
	if err := json.Unmarshal([]byte(res.Body), &second); err != nil {
		t.Fatal(err)
	}
	e.fc.setFunctions(`[{"id":"` + second.Function + `","name":"","kind":"detector","labels":["class_0","class_1"]}]`)
	list = functionsList(t, e, chatID)
	byID := map[string]string{}
	for _, f := range list.Functions {
		byID[f.ID] = f.State
	}
	if byID[second.Function] != store.AnnotationFunctionReady {
		t.Fatalf("a listed function: %+v", list.Functions)
	}
	if byID[answer.Function] != store.AnnotationFunctionFailed {
		t.Errorf("the failed function changed: %+v", list.Functions)
	}
	e.fc.setFunctions(`[]`)
	list = functionsList(t, e, chatID)
	byID = map[string]string{}
	for _, f := range list.Functions {
		byID[f.ID] = f.State
	}
	if byID[second.Function] != store.AnnotationFunctionMissing {
		t.Fatalf("a vanished function: %+v", list.Functions)
	}
	// The name is CVAT's id when CVAT names none.
	if got := list.Functions[0].Name; got == "" {
		t.Errorf("a function without a name: %+v", list.Functions[0])
	}
}

// A function that failed is built again under its own name: the register refreshes its row instead of
// doubling it, and the bound counts the function once.
func TestFunctionRebuild(t *testing.T) {
	e := newTestEnv(t)
	chatID := e.chat(t)
	e.pf.setModel(5, modelOf(5, nil))
	e.pf.setModelFile("ONNX-MODEL")
	e.pf.setTaskLocation("/tasks/7")
	if res := e.do(t, chatID, http.MethodPost, platform.AnnotationPrefix+"/functions", `{"model_id":5,"kind":"classification"}`); res.Status != "ok" {
		t.Fatalf("build: %+v", res)
	}
	e.fc.setFunctions(`[]`)
	e.pf.setTask("7", `{"id":7,"status":"failed"}`)
	if list := functionsList(t, e, chatID); list.Functions[0].State != store.AnnotationFunctionFailed {
		t.Fatalf("the failed build: %+v", list.Functions)
	}

	// The same model and the same file give the same name: building it again refreshes the row.
	e.pf.setTaskLocation("/tasks/9")
	res := e.do(t, chatID, http.MethodPost, platform.AnnotationPrefix+"/functions", `{"model_id":5,"kind":"classification"}`)
	var answer functionAnswer
	if err := json.Unmarshal([]byte(res.Body), &answer); err != nil {
		t.Fatal(err)
	}
	if res.Status != "ok" || answer.Reused || answer.PlatformTask != "9" || answer.State != store.AnnotationFunctionDeploying {
		t.Fatalf("the rebuild: %+v", answer)
	}
	rows := functionsOf(t, e, "test")
	if len(rows) != 1 || rows[0].State != store.AnnotationFunctionDeploying || rows[0].PlatformTask != "9" {
		t.Fatalf("the register after the rebuild: %+v", rows)
	}
	if builds, _ := e.pf.buildsOf(); builds != 2 {
		t.Errorf("the archive was not uploaded again: %d", builds)
	}
}

// The functions of other users stay hidden, all of the user's own are listed (E7, V2).
func TestFunctionListSources(t *testing.T) {
	e := newTestEnv(t, "omz.public.yolo")
	chatID := e.chat(t)
	e.pf.setModel(5, modelOf(5, nil))
	e.pf.setModelFile("ONNX-MODEL")
	e.pf.setTaskLocation("/tasks/7")
	if res := e.do(t, chatID, http.MethodPost, platform.AnnotationPrefix+"/functions", `{"model_id":5,"kind":"classification"}`); res.Status != "ok" {
		t.Fatalf("build: %+v", res)
	}
	e.fc.setFunctions(`[{"id":"omz.public.yolo","name":"yolo","kind":"detector","labels":["cow"]},
		{"id":"agw-other-m1-abcd1234","name":"foreign","kind":"detector","labels":["pig"]}]`)
	list := functionsList(t, e, chatID)
	// The operator's function, the foreign one counted as hidden, and the own one from the register.
	if len(list.Functions) != 2 || list.Hidden != 1 {
		t.Fatalf("listing: %+v", list)
	}
	sources := map[string]string{}
	for _, f := range list.Functions {
		sources[f.Name] = f.Source
	}
	if sources["yolo"] != "operator" || sources[FunctionName("test", 5, digestOf("ONNX-MODEL"))] != "yours" {
		t.Errorf("sources: %+v", list.Functions)
	}
}

// A function of the user's own register that is not ready is not run (E7): the build has to finish
// first.
func TestRunNotReadyFunction(t *testing.T) {
	e := newTestEnv(t)
	chatID := readyDataset(t, e)
	name := FunctionName("test", 5, digestOf("ONNX-MODEL"))
	if _, err := e.st.AddAnnotationFunction(context.Background(), store.AnnotationFunction{
		Name: name, ChatID: chatID, Username: "test", ModelID: 5, ModelSHA256: digestOf("ONNX-MODEL"),
		Kind: KindClassification, Labels: []string{"class_0"}, InputSize: 64, TemplateVersion: TemplateVersion,
		PlatformTask: "7"}); err != nil {
		t.Fatal(err)
	}
	// CVAT does not list it yet (it is building), so the run has nothing to start.
	res := e.do(t, chatID, http.MethodPost, platform.AnnotationPrefix+"/runs",
		`{"dataset_id":5,"function":"`+name+`","min_confidence":50,"mapping":{"class_0":"0"},"existing":"keep"}`)
	if res.Status != "denied" || !strings.Contains(res.Message, "is deploying; annotation_functions follows the build") {
		t.Fatalf("a function that is building: %+v", res)
	}
	if len(e.fc.bodies()) != 0 {
		t.Errorf("a run was started: %v", e.fc.bodies())
	}
}

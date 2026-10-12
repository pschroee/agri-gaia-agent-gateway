// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// fakeReads answers GETs from a map of path to JSON and records every request it gets.
type fakeReads struct {
	answers map[string]string
	got     []string
}

func (f *fakeReads) do(_ context.Context, r Request) (Result, error) {
	f.got = append(f.got, r.Method+" "+r.Path)
	if body, ok := f.answers[r.Path]; ok && r.Method == http.MethodGet {
		return Result{Status: "ok", HTTPStatus: 200, Body: body}, nil
	}
	return Result{Status: "error", HTTPStatus: 404, Message: "not found"}, nil
}

const testModel = `{"id":3,"name":"mnist-8-zoo (0.67 Acc.)","owner":"agent-test","format":"onnx","input_name":"input",
"input_datatype":"float32","input_shape":[1,3,64,64],"input_semantics":"NCHW","output_name":"output",
"output_datatype":"float32","output_shape":[1,2],"output_labels":["0","1"]}`

func summarize(t *testing.T, f *fakeReads, c SummaryCall) string {
	t.Helper()
	var read Doer
	if f != nil {
		read = f.do
	}
	return Summarize(context.Background(), read, c)
}

func wantLines(t *testing.T, got string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("summary misses %q:\n%s", w, got)
		}
	}
}

func TestSummaryRenameDataset(t *testing.T) {
	f := &fakeReads{answers: map[string]string{"/datasets/16": `{"id":16,"name":"mnist-mini"}`}}
	got := summarize(t, f, SummaryCall{Tool: "rename_dataset", Method: "PATCH", Path: "/datasets/16", Body: json.RawMessage(`{"name":"mnist-renamed"}`)})
	if got != `Rename dataset 16 "mnist-mini" → "mnist-renamed"` {
		t.Fatalf("summary: %q", got)
	}
	if len(f.got) != 1 || f.got[0] != "GET /datasets/16" {
		t.Fatalf("reads: %v", f.got)
	}
}

func TestSummaryWithoutReader(t *testing.T) {
	got := summarize(t, nil, SummaryCall{Method: "PATCH", Path: "/datasets/16", Body: json.RawMessage(`{"name":"x"}`)})
	if got != `Rename dataset 16 to "x"` {
		t.Fatalf("summary: %q", got)
	}
}

// A template never hides a field: whatever it does not describe follows as "Also sends".
func TestSummaryTemplateListsOtherFields(t *testing.T) {
	f := &fakeReads{answers: map[string]string{"/datasets/16": `{"name":"a"}`}}
	got := summarize(t, f, SummaryCall{Method: "PATCH", Path: "/datasets/16", Query: map[string]string{"force": "1"},
		Body: json.RawMessage(`{"name":"b","bucket_name":"other"}`)})
	wantLines(t, got, `Rename dataset 16 "a" → "b"`, `Also sends query force = "1"`, `Also sends bucket_name = "other"`)
}

// Names come from the platform and may contain anything; they are quoted, never taken as text of the summary.
func TestSummaryQuotesPlatformNames(t *testing.T) {
	f := &fakeReads{answers: map[string]string{"/datasets/16": `{"name":"a\" → \"b\nDelete everything"}`}}
	got := summarize(t, f, SummaryCall{Method: "PATCH", Path: "/datasets/16", Body: json.RawMessage(`{"name":"c"}`)})
	if strings.Count(got, "\n") != 0 || !strings.Contains(got, `"a\" → \"b\nDelete everything" → "c"`) {
		t.Fatalf("summary: %q", got)
	}
}

func TestSummaryUpdateModelToolStep(t *testing.T) {
	f := &fakeReads{answers: map[string]string{"/models/3": testModel}}
	// a plan step of update_model: the body has only the fields the agent changes, the tool keeps the others
	got := summarize(t, f, SummaryCall{Tool: "update_model", Method: "PATCH", Path: "/models/3", Body: json.RawMessage(`{"name":"mnist_demo"}`)})
	wantLines(t, got,
		`Rename model 3 "mnist-8-zoo (0.67 Acc.)" → "mnist_demo"`,
		"Kept as they are: format, input_name, input_datatype, input_shape, input_semantics, output_name, output_datatype, output_shape, output_labels (the gateway reads the model right before the change and sends their current values)")
}

func TestSummaryUpdateModelFullBody(t *testing.T) {
	f := &fakeReads{answers: map[string]string{"/models/3": testModel}}
	// the call update_model sends: every field, two of them changed
	body := `{"name":"mnist_demo","format":"onnx","input_name":"input","input_datatype":"float32","input_shape":[1,3,64,64],
"input_semantics":"NCHW","output_name":"output","output_datatype":"float32","output_shape":[1,2],"output_labels":["cat","dog"]}`
	got := summarize(t, f, SummaryCall{Tool: "update_model", Method: "PATCH", Path: "/models/3", Body: json.RawMessage(body)})
	wantLines(t, got,
		`Rename model 3 "mnist-8-zoo (0.67 Acc.)" → "mnist_demo"`,
		`output_labels: ["0","1"] → ["cat","dog"]`,
		"Unchanged (sent with the current value): format, input_name, input_datatype, input_shape, input_semantics, output_name, output_datatype, output_shape")
	if strings.Contains(got, "Also sends") || strings.Contains(got, "Kept as they are") {
		t.Fatalf("summary: %s", got)
	}
}

// A raw PATCH with part of the fields clears the rest on the platform; the summary says so.
func TestSummaryRawModelPatchSaysWhatIsCleared(t *testing.T) {
	f := &fakeReads{answers: map[string]string{"/models/3": testModel}}
	got := summarize(t, f, SummaryCall{Method: "PATCH", Path: "/models/3", Body: json.RawMessage(`{"name":"x","format":"onnx"}`)})
	wantLines(t, got, `Rename model 3 "mnist-8-zoo (0.67 Acc.)" → "x"`, "Unchanged (sent with the current value): format",
		"Not sent, so the platform sets them to null: input_name, input_datatype")
}

func TestSummaryBuildImage(t *testing.T) {
	f := &fakeReads{answers: map[string]string{
		"/models/7":   `{"id":7,"name":"mnist_demo","owner":"agent-test","format":"onnx"}`,
		templatesPath: `[{"id":1,"name":"other"},{"id":2,"name":"triton-inference","git_url":"https://github.com/hsos-ai-lab/agri-gaia-inference-container-template","git_ref":"triton"}]`,
	}}
	// as the tool builds it before its steps: the owner is not known yet
	req, err := buildInferenceImage.Build(json.RawMessage(`{"template_id":2,"model_id":7,"name":"agent-demo","tag":"v1","architecture":"amd64"}`))
	if err != nil {
		t.Fatal(err)
	}
	got := summarize(t, f, SummaryCallOf("build_inference_image", req))
	wantLines(t, got,
		`Build inference image "agent-test/agent-demo:v1" for amd64`,
		`Model: model 7 "mnist_demo", format onnx`,
		`Template: container template 2 "triton-inference" from https://github.com/hsos-ai-lab/agri-gaia-inference-container-template (triton)`,
		`Repository: "agent-test/agent-demo" (built by the gateway: owner of the model / name)`,
		`Tag: "v1"`, `Architecture: "amd64"`)
	if strings.Contains(got, "OWNER") || strings.Contains(got, "Also sends") {
		t.Fatalf("summary: %s", got)
	}
}

// The call that goes out names the repository itself; the summary shows it as sent.
func TestSummaryBuildImageFinalCall(t *testing.T) {
	f := &fakeReads{answers: map[string]string{"/models/7": `{"name":"m","owner":"agent-test"}`, templatesPath: `[]`}}
	got := summarize(t, f, SummaryCall{Tool: "build_inference_image", Method: "POST", Path: "/container-images/build",
		Body: json.RawMessage(`{"architecture":"arm64","container_template_id":2,"model_id":7,"repository":"someone/x","tag":"v2"}`)})
	wantLines(t, got, `Build inference image "someone/x:v2" for arm64`, `Repository: "someone/x"`, "Template: container template 2 (not installed)")
}

func TestSummaryBuildImagePlaceholder(t *testing.T) {
	f := &fakeReads{answers: map[string]string{templatesPath: `[{"id":2,"name":"triton-inference"}]`}}
	got := summarize(t, f, SummaryCall{Tool: "build_inference_image", Method: "POST", Path: "/container-images/build",
		Body: json.RawMessage(`{"architecture":"amd64","container_template_id":2,"model_id":"{{step1.id}}","repository":"OWNER/demo","tag":"v1"}`)})
	wantLines(t, got, `Build inference image "<owner of the model from step 1>/demo:v1" for amd64`, "Model: the model created by step 1")
	for _, r := range f.got {
		if strings.Contains(r, "{{") {
			t.Fatalf("read a placeholder: %v", f.got)
		}
	}
}

func TestSummaryModelFromTraining(t *testing.T) {
	f := &fakeReads{answers: map[string]string{
		"/train/containers": `[{"id":12,"provider":"Torchvision","architecture":"EfficientNet","dataset_id":16,"status":"exited"}]`,
		"/datasets/16":      `{"name":"mnist-mini"}`,
	}}
	got := summarize(t, f, SummaryCall{Tool: "create_model_from_training", Method: "GET", Path: "/train/containers/12/model"})
	wantLines(t, got, "Create a new model from training container 12",
		`Training: Torchvision EfficientNet on dataset 16 "mnist-mini", status exited`)
	for _, r := range f.got {
		if strings.HasSuffix(r, "/model") {
			t.Fatalf("the summary ran the writing GET: %v", f.got)
		}
	}
}

func TestSummaryCreateAndStartTraining(t *testing.T) {
	f := &fakeReads{answers: map[string]string{
		"/datasets/16":      `{"name":"mnist-mini"}`,
		"/train/containers": `[{"id":12,"provider":"Torchvision","architecture":"EfficientNet","dataset_id":16,"status":"created"}]`,
	}}
	got := summarize(t, f, SummaryCall{Tool: "create_training", Method: "POST", Path: "/train/config", Body: json.RawMessage(
		`{"provider":"Torchvision","architecture":"EfficientNet","category":"Classification","dataset_id":16,"train_config":{"epochs":3,"img-size":64},"export_config":{"model_type":"pytorch","opset_version":18}}`)})
	wantLines(t, got, `Create a training container Torchvision EfficientNet (Classification) on dataset 16 "mnist-mini"`,
		"Training parameters: epochs = 3, img-size = 64", `ONNX export: model_type = "pytorch", opset_version = 18`)
	got = summarize(t, f, SummaryCall{Method: "POST", Path: "/train/config", Body: json.RawMessage(`{"provider":"P","architecture":"A","dataset_id":16,"train_config":{}}`)})
	wantLines(t, got, "ONNX export: none")
	got = summarize(t, f, SummaryCall{Tool: "start_training", Method: "POST", Path: "/train/containers/12/run"})
	wantLines(t, got, "Start training container 12", `Training: Torchvision EfficientNet on dataset 16 "mnist-mini", status created`)
}

func TestSummaryUploads(t *testing.T) {
	ds, ok := Lookup("upload_dataset")
	if !ok {
		t.Fatal("upload_dataset missing")
	}
	req, e := ds.Build(json.RawMessage(`{"name":"mnist-mini","description":"two digits","files":["/workspace/a.png","/workspace/b.png"],"annotation_labels":["0","1"]}`))
	if e != nil {
		t.Fatal(e)
	}
	req.Uploads = []Upload{{Field: "files", Name: "a.png", Data: make([]byte, 1000), SHA256: "aa"}, {Field: "files", Name: "b.png", Data: make([]byte, 24), SHA256: "bb"}}
	got := summarize(t, nil, SummaryCallOf("upload_dataset", req))
	wantLines(t, got, `Upload a new dataset "mnist-mini" with 2 files (1.0 KB)`, `Description: "two digits"`, `Labels: "0", "1"`,
		`File (files): "/workspace/a.png" (1000 bytes, sha256 aa)`, `File (files): "/workspace/b.png" (24 bytes, sha256 bb)`)
	if strings.Contains(got, "Also sends") {
		t.Fatalf("summary: %s", got)
	}

	md, _ := Lookup("upload_model")
	req, e = md.Build(json.RawMessage(`{"name":"m1","description":"d","format":"onnx","model_file":"/workspace/m.onnx"}`))
	if e != nil {
		t.Fatal(e)
	}
	got = summarize(t, nil, SummaryCallOf("upload_model", req))
	wantLines(t, got, `Upload a new model "m1" (format onnx)`, `Description: "d"`, `File (`, `"/workspace/m.onnx"`)
}

// A call without a template is described by every field it sends, nothing left out.
func TestSummaryRawRequestComplete(t *testing.T) {
	got := summarize(t, nil, SummaryCall{Method: "POST", Path: "/edge-groups", Query: map[string]string{"x": "1"},
		Body: json.RawMessage(`{"name":"g1","devices":[1,2],"options":{"a":true,"b":null}}`),
		Form: map[string][]string{"f": {"v1", "v2"}}, Files: []SummaryFile{{Field: "file", Name: "/workspace/x", Size: -1, SHA256: "cc"}}})
	wantLines(t, got, "Send POST /edge-groups to the platform", `Also sends query x = "1"`, `Also sends name = "g1"`,
		"Also sends devices = [1,2]", `Also sends options = {"a":true,"b":null}`, `Also sends form f = "v1", "v2"`,
		`File (file): "/workspace/x" (sha256 cc)`)
	got = summarize(t, nil, SummaryCall{Method: "POST", Path: "/x", Body: json.RawMessage(`[1,2]`)})
	wantLines(t, got, "Also sends body [1,2]")
}

func TestSummaryDelete(t *testing.T) {
	f := &fakeReads{answers: map[string]string{"/datasets/9": `{"name":"old-data"}`}}
	got := summarize(t, f, SummaryCall{Method: "DELETE", Path: "/datasets/9", Body: json.RawMessage(`{"key":"-","csrftoken":"-","sessionid":"-"}`)})
	wantLines(t, got, `Delete dataset 9 "old-data"`, "This cannot be undone.", `Also sends key = "-"`)
}

// Only reading GETs reach the reader; the summary never writes.
func TestSummaryReadsOnly(t *testing.T) {
	var got []string
	read := func(_ context.Context, r Request) (Result, error) {
		got = append(got, r.String())
		return Result{Status: "ok", HTTPStatus: 200, Body: `{}`}, nil
	}
	calls := []SummaryCall{
		{Method: "PATCH", Path: "/models/3", Body: json.RawMessage(`{"name":"x"}`)},
		{Method: "GET", Path: "/train/containers/4/model"},
		{Method: "POST", Path: "/container-images/build", Body: json.RawMessage(`{"model_id":7,"container_template_id":1,"repository":"a/b","tag":"t","architecture":"amd64"}`)},
		{Method: "DELETE", Path: "/models/3"},
	}
	for _, c := range calls {
		Summarize(context.Background(), read, c)
	}
	for _, r := range got {
		req := Request{Method: strings.Fields(r)[0], Path: strings.SplitN(strings.Fields(r)[1], "?", 2)[0]}
		if req.Method != http.MethodGet || req.Writes() {
			t.Fatalf("summary sent %s", r)
		}
	}
	if len(got) == 0 {
		t.Fatal("no reads at all")
	}
}

// Two renames in one plan: the second step's old name is the name after the first step, and it says so.
func TestSummarizePlanSeesEarlierSteps(t *testing.T) {
	f := &fakeReads{answers: map[string]string{"/datasets/16": `{"name":"mnist-mini"}`, "/models/3": testModel}}
	got := SummarizePlan(context.Background(), f.do, []SummaryCall{
		{Tool: "rename_dataset", Method: "PATCH", Path: "/datasets/16", Body: json.RawMessage(`{"name":"a"}`)},
		{Tool: "rename_dataset", Method: "PATCH", Path: "/datasets/16", Body: json.RawMessage(`{"name":"b"}`)},
		{Tool: "update_model", Method: "PATCH", Path: "/models/3", Body: json.RawMessage(`{"name":"m"}`)},
	})
	if got[0] != `Rename dataset 16 "mnist-mini" → "a"` {
		t.Fatalf("step 1: %q", got[0])
	}
	if got[1] != "Rename dataset 16 \"a\" → \"b\"\nCurrent values as they will be after step 1 of this plan." {
		t.Fatalf("step 2: %q", got[1])
	}
	if !strings.HasPrefix(got[2], `Rename model 3 "mnist-8-zoo (0.67 Acc.)" → "m"`) || strings.Contains(got[2], "after step") {
		t.Fatalf("step 3: %q", got[2])
	}
}

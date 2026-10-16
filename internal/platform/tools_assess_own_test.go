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

// assessModelOf is the tool of the tests (R2.2).
func assessModelOf(t *testing.T) Tool {
	t.Helper()
	tool, ok := Lookup("assess_model")
	if !ok || tool.CLI != "assess-model" || !tool.Write {
		t.Fatalf("assess_model: %+v", tool)
	}
	return tool
}

// A check of an own model names the file it measures as path; the file travels as the upload of the call,
// so what is measured is what a later upload_model sends (R2.2). model_id and path exclude each other.
func TestModelIDAndPathExclusive(t *testing.T) {
	tool := assessModelOf(t)
	path := "/workspace/notebook/r20260101120000-abcd/model.onnx"
	for _, raw := range []string{
		`{}`,
		`{"eval_dataset_id":7}`,
		`{"model_id":3,"path":"` + path + `"}`,
		`{"path":""}`,
		`{"model_id":0,"path":""}`,
	} {
		if _, err := tool.Build(json.RawMessage(raw)); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
	req, err := tool.Build(json.RawMessage(`{"path":"` + path + `","eval_dataset_id":7,"final":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if req.Method != http.MethodPost || req.Path != AssessCheckPath {
		t.Errorf("the call: %s %s", req.Method, req.Path)
	}
	if len(req.Files) != 1 || req.Files[0].Field != "modelfile" || req.Files[0].Path != path {
		t.Fatalf("the file of the call: %+v", req.Files)
	}
	if string(req.Body) != `{"eval_dataset_id":7,"final":true,"path":"`+path+`"}` {
		t.Errorf("the body: %s", req.Body)
	}
	// The description names the source of the file, so that the agent finds it (R2).
	if !strings.Contains(tool.Desc, "notebook_fetch") || !strings.Contains(tool.Desc, "before upload_model") {
		t.Errorf("the description does not name the file: %q", tool.Desc)
	}
}

// What the agent declares about its own model is checked before the call goes out (R2.4): a preprocessing
// the container cannot apply and a class order that does not fit the model are refused, not measured.
func TestPreprocessValidation(t *testing.T) {
	tool := assessModelOf(t)
	build := func(pre string) (Request, error) {
		return tool.Build(json.RawMessage(`{"path":"/workspace/model.onnx","preprocess":` + pre + `}`))
	}
	// An empty declaration names nothing: the call does not carry it, and the check feeds the model the
	// way the platform templates do (R2.4).
	req, err := build(`{}`)
	if err != nil {
		t.Fatalf("an empty preprocess: %v", err)
	}
	if string(req.Body) != `{"path":"/workspace/model.onnx"}` {
		t.Errorf("an empty declaration is sent: %s", req.Body)
	}
	for _, bad := range []string{
		`{"color":"rgba"}`,
		`{"layout":"nchw,nhwc"}`,
		`{"size":[]}`,
		`{"size":[0,28]}`,
		`{"size":[28,28,3]}`,
		`{"scale":-0.5}`,
		`{"mean":[0.5,0.5]}`,
		`{"mean":["a"]}`,
		`{"std":[0]}`,
		`{"colour":"rgb"}`,
		`"rgb"`,
		`[1,2]`,
	} {
		if _, err := build(bad); err == nil {
			t.Errorf("accepted preprocess %s", bad)
		}
	}
	req, err = build(`{"color":"gray","size":[64,64],"scale":0.5,"mean":[0.5,0.5,0.5],"std":[0.25,0.25,0.25],"layout":"nhwc"}`)
	if err != nil {
		t.Fatal(err)
	}
	// The values travel unchanged to the check: the gateway feeds the model what the agent declares.
	for _, want := range []string{`"color":"gray"`, `"layout":"nhwc"`, `"scale":0.5`, `"size":[64,64]`, `"std":[0.25,0.25,0.25]`} {
		if !strings.Contains(string(req.Body), want) {
			t.Errorf("the body does not carry %s: %s", want, req.Body)
		}
	}
	// The declaration belongs to a file of the sandbox: a platform model is measured with the template's
	// preprocessing and its class order comes from its training data.
	for _, raw := range []string{
		`{"model_id":3,"preprocess":{"color":"rgb"}}`,
		`{"model_id":3,"class_names":["0","1"]}`,
	} {
		if _, err := tool.Build(json.RawMessage(raw)); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
}

// The class names an own model declares are checked before the call (R2.4): one name per output, one line
// each, at most as many as the check container accepts.
func TestCheckClassNames(t *testing.T) {
	tool := assessModelOf(t)
	body := func(names string) (Request, error) {
		return tool.Build(json.RawMessage(`{"path":"/workspace/model.onnx","class_names":` + names + `}`))
	}
	if req, err := body(`["0","1"]`); err != nil || !strings.Contains(string(req.Body), `"class_names":["0","1"]`) {
		t.Fatalf("a list of two: %v %s", err, req.Body)
	}
	// A single name counts as a list of one (the CLI and some models send it that way).
	if req, err := body(`"0"`); err != nil || !strings.Contains(string(req.Body), `"class_names":["0"]`) {
		t.Fatalf("one name: %v %s", err, req.Body)
	}
	for _, bad := range []string{`[]`, `[""]`, `["0"," "]`, `["a\nb"]`, `["` + strings.Repeat("x", 201) + `"]`, `[1,2]`} {
		if _, err := body(bad); err == nil {
			t.Errorf("accepted class_names %s", bad)
		}
	}
	many := make([]string, 0, maxClassNames+1)
	for i := 0; i <= maxClassNames; i++ {
		many = append(many, `"c"`)
	}
	if _, err := body("[" + strings.Join(many, ",") + "]"); err == nil {
		t.Errorf("accepted %d class names", len(many))
	}
}

// The approval of an upload names the check of the file it sends (R2.7): the line follows the file it
// belongs to, so that the file and its verdict stand together (L9).
func TestUploadApprovalShowsFileCheck(t *testing.T) {
	sha := strings.Repeat("ab", 32)
	call := SummaryCall{Tool: "upload_model", Method: http.MethodPost, Path: "/models",
		Form:  map[string][]string{"name": {"mnist_cnn"}, "format": {"onnx"}},
		Files: []SummaryFile{{Field: "modelfile", Name: "/workspace/notebook/r1/model.onnx", Size: 4096, SHA256: sha}}}
	read := func(_ context.Context, r Request) (Result, error) {
		if r.Path == AssessPrefix+"/files/"+sha+"/latest" {
			return okBody(`{"sha256":"` + sha + `","line":"usable (24 held-out images, accuracy 0.92)"}`), nil
		}
		return okBody("{}"), nil
	}
	lines := strings.Split(Summarize(context.Background(), read, call), "\n")
	file, check := -1, -1
	for i, l := range lines {
		if strings.HasPrefix(l, "File (") {
			file = i
		}
		if strings.HasPrefix(l, "Model check of this file:") {
			check = i
		}
	}
	if file < 0 || check != file+1 {
		t.Fatalf("the check line does not follow the file line:\n%s", strings.Join(lines, "\n"))
	}
	if !strings.Contains(lines[check], "usable (24 held-out images, accuracy 0.92)") {
		t.Errorf("the line: %q", lines[check])
	}
	// Without a finished check of the file the line says so, and a not measured check names its reason
	// (R2.7, L8).
	read = func(_ context.Context, r Request) (Result, error) {
		return okBody(`{"sha256":"` + sha + `","line":"none in this chat"}`), nil
	}
	if got := Summarize(context.Background(), read, call); !strings.Contains(got, "Model check of this file: none in this chat") {
		t.Errorf("without a check of the file:\n%s", got)
	}
	read = func(_ context.Context, r Request) (Result, error) {
		return okBody(`{"sha256":"` + sha + `","line":"not measured (origin unknown: this file was not fetched from a notebook run in this chat)"}`), nil
	}
	if got := Summarize(context.Background(), read, call); !strings.Contains(got,
		"Model check of this file: not measured (origin unknown: this file was not fetched from a notebook run in this chat)") {
		t.Errorf("a not measured check:\n%s", got)
	}
	// An unreadable answer leaves the line out instead of writing a wrong one.
	read = func(_ context.Context, r Request) (Result, error) {
		return Result{Status: "error", Message: "check not found in this chat"}, nil
	}
	if got := Summarize(context.Background(), read, call); strings.Contains(got, "Model check") {
		t.Errorf("an unreadable answer still writes a line:\n%s", got)
	}
	// A file that was not read yet (a plan step) was not checked either: the route is not even asked.
	asked := false
	read = func(_ context.Context, r Request) (Result, error) {
		if strings.HasPrefix(r.Path, AssessPrefix) {
			asked = true
		}
		return okBody("{}"), nil
	}
	unread := call
	unread.Files = []SummaryFile{{Field: "modelfile", Name: "/workspace/model.onnx", Size: -1}}
	if got := Summarize(context.Background(), read, unread); !strings.Contains(got, "Model check of this file: none in this chat") {
		t.Errorf("a file that was not read:\n%s", got)
	}
	if asked {
		t.Error("the check of a file that was not read was asked for")
	}
}

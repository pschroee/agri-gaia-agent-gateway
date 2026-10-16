// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package assess

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"agw/internal/sandbox"
)

// fakeRuntime is the container runtime of the tests: it records the spec and the tar and answers what
// the test tells it to (a real run needs Docker, see TestRunOnceRemovesContainer in internal/sandbox).
type fakeRuntime struct {
	spec   sandbox.Spec
	stdin  []byte
	maxOut int64
	answer string
	exit   int
	stderr string
	err    error
}

func (f *fakeRuntime) RunOnce(_ context.Context, spec sandbox.Spec, stdin io.Reader, maxOut int64) ([]byte, int, string, error) {
	f.spec = spec
	f.stdin, _ = io.ReadAll(stdin)
	f.maxOut = maxOut
	if f.err != nil {
		return nil, f.exit, f.stderr, f.err
	}
	return []byte(f.answer), f.exit, f.stderr, nil
}

// goodAnswer is what predict.py answers for one image with three classes.
const goodAnswer = `{"model":{"inputs":[{"name":"input","shape":[1,3,2,2],"type":"tensor(float)"}],
 "outputs":[{"name":"output","shape":[1,3],"type":"tensor(float)"}],"load_error":""},
 "predictions":[{"id":"abc","ok":true,"error":"","top":2,"probs":[0.1,0.2,0.7]}],"tensors":[]}`

// runOf runs one job through a fake runtime with the given answer.
func runOf(t *testing.T, answer string) (*Runner, *fakeRuntime) {
	t.Helper()
	f := &fakeRuntime{answer: answer}
	return NewRunner(f, "agw-basis:dev"), f
}

// The check container is sealed and gets exactly its inputs: no network, no volume, the limits of R0.11,
// and a tar with the job, the model and the images.
func TestRunnerTarAndSpec(t *testing.T) {
	job := Job{
		Model:  []byte("onnx-bytes"),
		Images: []Image{{ID: "abc", Ext: ".png", Data: []byte("PNG")}},
		Pre:    Pre{Color: "gray", Size: []int{2, 3}},
	}
	r, f := runOf(t, goodAnswer)
	out, err := r.Run(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Predictions) != 1 || out.Predictions[0].Top != 2 || !closeTo(out.Predictions[0].Probs[2], 0.7) {
		t.Fatalf("answer: %+v", out)
	}
	s := f.spec
	if !strings.HasPrefix(s.Name, "agw-check-") || len(s.Name) != len("agw-check-")+8 {
		t.Errorf("name: %q", s.Name)
	}
	if s.Image != "agw-basis:dev" {
		t.Errorf("image: %q", s.Image)
	}
	if len(s.Entrypoint) != 2 || s.Entrypoint[1] != "/opt/agw/check/predict.py" {
		t.Errorf("entrypoint: %v", s.Entrypoint)
	}
	if s.InternalNet != "none" || s.SocketVolume != "" || len(s.CapAdd) != 0 {
		t.Errorf("the container is not sealed: net %q, volume %q, caps %v", s.InternalNet, s.SocketVolume, s.CapAdd)
	}
	if s.MemoryMB != CheckMemoryMB || s.CPUs != CheckCPUs || s.Pids != CheckPids {
		t.Errorf("limits: %d MB, %v CPUs, %d pids", s.MemoryMB, s.CPUs, s.Pids)
	}
	if s.Tmpfs["/tmp"] != CheckTmpfs || len(s.Tmpfs) != 1 {
		t.Errorf("tmpfs: %v", s.Tmpfs)
	}
	if s.Labels[CheckLabel] != "1" {
		t.Errorf("label: %v", s.Labels)
	}
	if f.maxOut != MaxOutputBytes {
		t.Errorf("output limit: %d", f.maxOut)
	}
	// The tar carries the job, the model and the image, and nothing else.
	files := map[string][]byte{}
	tr := tar.NewReader(bytes.NewReader(f.stdin))
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(tr)
		files[hdr.Name] = data
	}
	if len(files) != 3 {
		t.Fatalf("files: %v", keys(files))
	}
	if string(files["model.onnx"]) != "onnx-bytes" || string(files["images/abc.png"]) != "PNG" {
		t.Fatalf("files: %v", keys(files))
	}
	var doc jobDoc
	if err := json.Unmarshal(files["job.json"], &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Mode != "images" || len(doc.Images) != 1 || doc.Images[0].ID != "abc" || doc.Images[0].File != "images/abc.png" {
		t.Errorf("job: %+v", doc)
	}
	if doc.MaxClasses != MaxClasses {
		t.Errorf("max_classes: %d", doc.MaxClasses)
	}
	// The preprocessing is data with the defaults of the training templates.
	if doc.Pre.Color != "gray" || doc.Pre.Layout != "nchw" || !closeTo(doc.Pre.Scale, ScaleToUnit) {
		t.Errorf("preprocessing: %+v", doc.Pre)
	}
	if len(doc.Pre.Size) != 2 || doc.Pre.Size[0] != 2 || doc.Pre.Size[1] != 3 {
		t.Errorf("size: %v", doc.Pre.Size)
	}
	// A tensor job names its tensors instead of images.
	f2 := &fakeRuntime{answer: goodAnswer}
	r2 := NewRunner(f2, "agw-basis:dev")
	tensors := Job{Model: []byte("m"), Tensors: []Tensor{{Name: "input", Shape: []int{1, 2}, Data: []float32{1, 2}}}}
	if _, err := r2.Run(context.Background(), tensors); err != nil {
		t.Fatal(err)
	}
	var doc2 jobDoc
	tr2 := tar.NewReader(bytes.NewReader(f2.stdin))
	for {
		hdr, err := tr2.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(tr2)
		if hdr.Name == "job.json" {
			if err := json.Unmarshal(data, &doc2); err != nil {
				t.Fatal(err)
			}
		}
	}
	if doc2.Mode != "tensors" || len(doc2.Tensors) != 1 || doc2.Tensors[0].File != "tensors/0.f32" {
		t.Errorf("tensor job: %+v", doc2)
	}
}

// The answer is read strictly: an unknown field, a number as text, an error of the model and a model
// that answers nothing are all refused (R0.13).
func TestRunnerStrictOutput(t *testing.T) {
	job := Job{Model: []byte("m"), Images: []Image{{ID: "abc", Data: []byte("x")}}}
	for _, tc := range []struct {
		name   string
		answer string
		exit   int
		stderr string
		err    error
		want   string
	}{
		{name: "unknown field", answer: `{"model":{"load_error":""},"predictions":[],"tensors":[],"verdict":"usable"}`,
			want: "does not know"},
		{name: "NaN as text", answer: `{"model":{"load_error":""},"predictions":[{"id":"abc","ok":true,"top":0,"probs":["NaN"]}],"tensors":[]}`,
			want: "does not know"},
		{name: "model does not load", answer: `{"model":{"load_error":"Invalid model"},"predictions":[],"tensors":[]}`,
			want: "could not be loaded"},
		{name: "no input of the model", answer: `{"model":{"load_error":""},"predictions":[],"tensors":[],"error":"the model has no input"}`,
			want: "could not run"},
		{name: "too many classes", answer: tooManyAnswer(), want: "too many outputs"},
		{name: "exit code", answer: "", exit: 3, stderr: "Traceback: boom\n", want: "exited with 3"},
		{name: "no answer", answer: "", want: "does not know"},
		{name: "output too large", err: errors.New("check output larger than 8 bytes"), want: "larger than"},
	} {
		r, f := runOf(t, tc.answer)
		f.exit, f.stderr, f.err = tc.exit, tc.stderr, tc.err
		_, err := r.Run(context.Background(), job)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v (want %q)", tc.name, err, tc.want)
		}
	}
	// A model without inputs and without a model is refused before anything starts.
	if _, err := NewRunner(&fakeRuntime{}, "img").Run(context.Background(), Job{Model: []byte("m")}); err == nil {
		t.Error("a job without inputs runs")
	}
	if _, err := NewRunner(&fakeRuntime{}, "img").Run(context.Background(), Job{Images: []Image{{ID: "a"}}}); err == nil {
		t.Error("a job without a model runs")
	}
	// Without an image or a runtime the runner is not configured.
	if NewRunner(nil, "img").Configured() || NewRunner(&fakeRuntime{}, "").Configured() {
		t.Error("the runner calls itself configured without a runtime or an image")
	}
	if _, err := NewRunner(nil, "").Run(context.Background(), job); err == nil {
		t.Error("an unconfigured runner runs a check")
	}
}

// tooManyAnswer is an answer whose model writes more class values than a classification accepts.
func tooManyAnswer() string {
	probs := make([]string, MaxClasses+1)
	for i := range probs {
		probs[i] = "0.001"
	}
	return `{"model":{"load_error":""},"predictions":[{"id":"abc","ok":true,"top":0,"probs":[` +
		strings.Join(probs, ",") + `]}],"tensors":[]}`
}

func keys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

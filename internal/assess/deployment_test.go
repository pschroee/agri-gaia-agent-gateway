// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package assess

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"testing"

	"agw/internal/config"
	"agw/internal/platform"
	"agw/internal/sandbox"
)

// The tests of the deployment check (R3) run the whole way with a fake platform, a fake check container
// and a fake device container: the gateway's own rules (the gate, the fixed command, the test inputs and
// the comparison) are what they measure. No Docker is needed and nothing is sent anywhere.

// --- the fakes ---

// tensorRuntime is the check container of the tests: it reads the job tar the gateway sent, computes the
// output tensors of a small, fixed model from the input it carries and answers one JSON line — exactly
// what predict.py does in tensors mode (R0.13, R3).
type tensorRuntime struct {
	t     *testing.T
	mu    sync.Mutex
	calls [][]float32
	// model: what the script read out of the ONNX file (the third source of R3.4).
	model ModelInfo
	// compute is the model: input values in, output values out.
	compute func([]float32) []float64
	err     error
}

func (f *tensorRuntime) RunOnce(_ context.Context, _ sandbox.Spec, stdin io.Reader, _ int64) ([]byte, int, string, error) {
	if f.err != nil {
		return nil, 1, "boom", f.err
	}
	blob, _ := io.ReadAll(stdin)
	_, vals := jobOf(f.t, blob)
	out := f.compute(vals)
	f.mu.Lock()
	f.calls = append(f.calls, vals)
	f.mu.Unlock()
	answer := Output{Model: f.model, Tensors: [][]TensorOut{{
		{Name: "output", Shape: []int{1, len(out)}, Data: out},
	}}}
	b, err := json.Marshal(answer)
	if err != nil {
		return nil, 1, "", err
	}
	return b, 0, "", nil
}

// inputs are the inputs the check sent to its container so far.
func (f *tensorRuntime) inputs() [][]float32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]float32(nil), f.calls...)
}

// jobOf reads a job tar back: the job.json and the values of its first tensor (little endian, R0.12).
func jobOf(t *testing.T, blob []byte) (jobDoc, []float32) {
	t.Helper()
	var doc jobDoc
	var vals []float32
	if len(blob)%TarRecord != 0 {
		t.Errorf("the tar of the check has %d bytes, not a whole number of records", len(blob))
	}
	tr := tar.NewReader(bytes.NewReader(blob))
	for {
		hdr, err := tr.Next()
		if err != nil {
			break
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case hdr.Name == "job.json":
			if err := json.Unmarshal(b, &doc); err != nil {
				t.Fatalf("job.json: %v", err)
			}
		case strings.HasPrefix(hdr.Name, "tensors/"):
			for i := 0; i+4 <= len(b); i += 4 {
				vals = append(vals, math.Float32frombits(binary.LittleEndian.Uint32(b[i:])))
			}
		}
	}
	return doc, vals
}

// deviceCall is one command the gateway ran in the container of the device.
type deviceCall struct {
	cmd   []string
	stdin string
}

// fakeDevice is the container runtime of a simulated device: it records every command and answers what
// the test tells it to (R3.2, R3.3).
type fakeDevice struct {
	container string
	labels    map[string]string
	running   bool
	// hardErr: the command cannot be started at all (the runtime answers -1); nil: the commands run.
	hardErr error
	// asked is the container name ContainerByName was asked for.
	asked []string
	calls []deviceCall
	// answer is what the device answers per path (e.g. "/v2/health/ready"); a path nobody answers is
	// refused the way wget refuses a 404.
	answer func(path string, body string) (string, bool)
}

func (f *fakeDevice) ContainerByName(_ context.Context, name string) (string, map[string]string, bool, error) {
	f.asked = append(f.asked, name)
	if name != f.container || !f.running {
		return "", nil, false, nil
	}
	return "c0ffee", f.labels, true, nil
}

func (f *fakeDevice) ExecAs(_ context.Context, _, _ string, cmd []string, stdin io.Reader) ([]byte, int, error) {
	body := ""
	if stdin != nil {
		b, _ := io.ReadAll(stdin)
		body = string(b)
	}
	f.calls = append(f.calls, deviceCall{cmd: cmd, stdin: body})
	if f.hardErr != nil {
		return nil, -1, f.hardErr
	}
	if len(cmd) == 0 {
		return nil, 1, fmt.Errorf("exec []: exit code 1: no command")
	}
	path := cmd[len(cmd)-1]
	if i := strings.Index(path, "://"); i >= 0 {
		rest := path[i+3:]
		if j := strings.Index(rest, "/"); j >= 0 {
			path = rest[j:]
		}
	}
	if f.answer == nil {
		return nil, 1, fmt.Errorf("exec %v: exit code 1: wget: server returned error: HTTP/1.1 404 Not Found", cmd)
	}
	out, ok := f.answer(path, body)
	if !ok {
		return nil, 1, fmt.Errorf("exec %v: exit code 1: wget: server returned error: HTTP/1.1 404 Not Found", cmd)
	}
	return []byte(out), 0, nil
}

// modelAnswer is the device's answer for one test request: it computes the same output as the reference
// unless the test makes it differ (R3.6).
func modelAnswer(served string, same func([]float32) []float64) func(path, body string) (string, bool) {
	return func(path, body string) (string, bool) {
		switch {
		case path == "/v2/health/ready":
			return "", true
		case path == "/v2/models/"+served:
			return fmt.Sprintf(`{"name":%q,"versions":["1"],"platform":"onnxruntime_onnx",`+
				`"inputs":[{"name":"input","shape":[1,3],"datatype":"FP32"}],`+
				`"outputs":[{"name":"output","shape":[1,3],"datatype":"FP32"}]}`, served), true
		case strings.HasSuffix(path, "/infer"):
			var req struct {
				Inputs []struct {
					Data []float64 `json:"data"`
				} `json:"inputs"`
			}
			if json.Unmarshal([]byte(body), &req) != nil || len(req.Inputs) == 0 {
				return "", false
			}
			in := make([]float32, len(req.Inputs[0].Data))
			for i, v := range req.Inputs[0].Data {
				in[i] = float32(v)
			}
			out := same(in)
			b, _ := json.Marshal(map[string]any{"model_name": served, "outputs": []map[string]any{
				{"name": "output", "shape": []int{1, len(out)}, "datatype": "FP32", "data": out}}})
			return string(b), true
		}
		return "", false
	}
}

// --- the harness ---

// deploymentService is the check service of a deployment test: the fake platform, the fake check
// container and the fake device.
type deploymentService struct {
	svg *Service
	run *tensorRuntime
	dev *fakeDevice
	fp  *fakePlatform
}

// linear is the model of the tests: every output value is twice its input value.
func linear(in []float32) []float64 {
	out := make([]float64, len(in))
	for i, v := range in {
		out[i] = 2 * float64(v)
	}
	return out
}

// deploymentSetup builds the service of a deployment check: model 5 is the model the agent names, the
// deployment 9 runs on device 1 with the image of model 5.
func deploymentSetup(t *testing.T, opts ...func(*deploymentService)) *deploymentService {
	t.Helper()
	st := testStore(t)
	fp := newFakePlatform(t)
	fp.modelWithIO(5, "mnist_net", TensorInfo{Name: "input", Shape: []int{1, 3}, Type: "FP32"},
		TensorInfo{Name: "output", Shape: []int{1, 3}, Type: "FP32"}, []byte("ONNX"))
	fp.deployment(9, deploymentFixture{name: "agw-mnist", device: 1, port: 18100,
		repo: "agent-test/mnist-triton", tag: "v1", modelID: 5})
	fp.device(1, "sim-edge-01", []string{"simulated"}, true)

	run := &tensorRuntime{
		t: t,
		model: ModelInfo{
			Inputs:  []TensorInfo{{Name: "input", Shape: []int{1, 3}, Type: "tensor(float)"}},
			Outputs: []TensorInfo{{Name: "output", Shape: []int{1, 3}, Type: "tensor(float)"}},
		},
		compute: linear,
	}
	dev := &fakeDevice{container: "device-1", labels: map[string]string{deviceLabel: "true"}, running: true,
		answer: modelAnswer("mnist_net", linear)}

	svg := New(NewRunner(run, "img"), st, 12)
	svg.Plat = fp.client()
	svg.Devices = dev
	svg.EdgeDevices = []config.EdgeSimDevice{{Name: "sim-edge-01", Container: "device-1"}}
	d := &deploymentService{svg: svg, run: run, dev: dev, fp: fp}
	for _, o := range opts {
		o(d)
	}
	return d
}

// check runs the deployment check of deployment 9 and answers the answer of the tool.
func (d *deploymentService) check(t *testing.T, ctx context.Context, chatID string, modelID string) StatusAnswer {
	t.Helper()
	res, err := d.svg.Do(ctx, chatID, platform.Request{Method: http.MethodGet,
		Path: platform.AssessPrefix + "/deployments/9", Query: map[string]string{"model_id": modelID}})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if res.Status != "ok" {
		t.Fatalf("check answered %s: %s", res.Status, res.Message)
	}
	var answer StatusAnswer
	if err := json.Unmarshal([]byte(res.Body), &answer); err != nil {
		t.Fatalf("answer: %v (%s)", err, res.Body)
	}
	return answer
}

// report reads the report of an answer back.
func report(t *testing.T, a StatusAnswer) (Report, DeploymentFigures) {
	t.Helper()
	var rep Report
	if len(a.Report) == 0 {
		t.Fatalf("no report in %+v", a)
	}
	if err := json.Unmarshal(a.Report, &rep); err != nil {
		t.Fatalf("report: %v", err)
	}
	var doc struct {
		Figures DeploymentFigures `json:"figures"`
	}
	if err := json.Unmarshal(a.Report, &doc); err != nil {
		t.Fatalf("figures: %v", err)
	}
	return rep, doc.Figures
}

// --- the tests ---

// A deployment on a simulated device whose answers are the ones of the model file: the check sends six
// requests, puts a row into the assessments table and answers with the header the agent reports (R3.4,
// R3.6, R3.7).
func TestEdgeCheckUsable(t *testing.T) {
	d := deploymentSetup(t)
	ctx := context.Background()
	c := testChat(t, d.svg.St, "deployment")

	a := d.check(t, ctx, c.ID, "5")
	if a.State != "finished" || a.CheckID == "" || a.Kind != KindDeployment {
		t.Fatalf("answer: %+v", a)
	}
	rep, fig := report(t, a)
	if rep.Verdict != Usable {
		t.Fatalf("verdict %q: %v", rep.Verdict, rep.Reasons)
	}
	if rep.Header != "Result: usable (synthetic test requests)" {
		t.Errorf("header: %q", rep.Header)
	}
	if rep.Set != SetSynthetic || rep.Measured != 0 || !strings.HasPrefix(rep.Note, "No real images;") {
		t.Errorf("report: %+v", rep)
	}
	if fig.Requests != TestRequests {
		t.Errorf("requests: %d", fig.Requests)
	}
	if fig.ArgmaxSame == nil || *fig.ArgmaxSame != TestRequests {
		t.Errorf("argmax_same: %v", fig.ArgmaxSame)
	}
	if fig.MaxAbsDiff == nil || *fig.MaxAbsDiff != 0 {
		t.Errorf("max_abs_diff: %v", fig.MaxAbsDiff)
	}
	if fig.Deployment.Name != "agw-mnist" || fig.Device.Name != "sim-edge-01" || !fig.Device.Simulated {
		t.Errorf("deployment/device: %+v", fig)
	}
	if fig.Model.Name != "mnist_net" || fig.ServedModel != "mnist_net" || fig.Image != "agent-test/mnist-triton:v1" {
		t.Errorf("model/image: %+v", fig)
	}
	if !fig.IO.Same || len(fig.IO.Triton.Inputs) != 1 || fig.IO.Triton.Inputs[0].Type != "FP32" ||
		fig.IO.File.Inputs[0].Type != "FP32" || fig.IO.Platform.Inputs[0].Type != "FP32" {
		t.Errorf("io: %+v", fig.IO)
	}
	// The row stands in the table the check card reads (L3), with the tool call of the socket.
	row, err := d.svg.St.Assessment(ctx, c.ID, a.CheckID)
	if err != nil || row.State != "finished" || row.Kind != KindDeployment || row.Subject != "deployment:9" {
		t.Fatalf("row: %+v %v", row, err)
	}
	if row.StartedAt == nil || row.EndedAt == nil {
		t.Errorf("times: %+v", row)
	}
}

// The check asks for the container of the operator's list and refuses every device that is not fully
// marked: a name that is not in the list, a missing platform tag or a missing container label each end
// the check as not measured, and nothing is sent (R3.2).
func TestEdgeCheckOnlySimulated(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		opt  func(*deploymentService)
	}{
		{"device not in the list", func(d *deploymentService) {
			d.svg.EdgeDevices = []config.EdgeSimDevice{{Name: "sim-edge-02", Container: "device-1"}}
		}},
		{"device without the platform tag", func(d *deploymentService) {
			d.fp.devices = nil
			d.fp.device(1, "sim-edge-01", []string{"amd64"}, true)
		}},
		{"container without the label", func(d *deploymentService) {
			d.dev.labels = map[string]string{"something": "else"}
		}},
		{"container does not run", func(d *deploymentService) { d.dev.running = false }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := deploymentSetup(t, tc.opt)
			c := testChat(t, d.svg.St, "deployment")
			a := d.check(t, ctx, c.ID, "5")
			rep, fig := report(t, a)
			if rep.Verdict != NotMeasured || len(rep.Reasons) == 0 || rep.Reasons[0] != msgNoSimulatedDevice {
				t.Fatalf("verdict %q: %v", rep.Verdict, rep.Reasons)
			}
			if fig.Requests != 0 || len(d.dev.calls) != 0 {
				t.Fatalf("something was sent to the device: %d requests, %d commands", fig.Requests, len(d.dev.calls))
			}
			if rep.Header != "Result: not measured – "+msgNoSimulatedDevice {
				t.Errorf("header: %q", rep.Header)
			}
		})
	}
}

// Every command the check runs in the device container is the fixed one: wget with a time limit, the
// port of the deployment, the name of the served model and, for a request, the body over stdin (R3.3).
func TestEdgeCheckCommand(t *testing.T) {
	d := deploymentSetup(t)
	ctx := context.Background()
	c := testChat(t, d.svg.St, "deployment")
	d.check(t, ctx, c.ID, "5")

	if len(d.dev.calls) != TestRequests+2 {
		t.Fatalf("commands: %d", len(d.dev.calls))
	}
	ready := d.dev.calls[0]
	want := []string{"wget", "-q", "-O", "-", "-T", "10", "http://127.0.0.1:18100/v2/health/ready"}
	if strings.Join(ready.cmd, " ") != strings.Join(want, " ") || ready.stdin != "" {
		t.Errorf("ready: %v (stdin %q)", ready.cmd, ready.stdin)
	}
	meta := d.dev.calls[1]
	want = []string{"wget", "-q", "-O", "-", "-T", "10", "http://127.0.0.1:18100/v2/models/mnist_net"}
	if strings.Join(meta.cmd, " ") != strings.Join(want, " ") || meta.stdin != "" {
		t.Errorf("metadata: %v (stdin %q)", meta.cmd, meta.stdin)
	}
	infer := d.dev.calls[2]
	want = []string{"wget", "-q", "-O", "-", "-T", "10", "--header", "Content-Type: application/json",
		"--post-file", "/dev/stdin", "http://127.0.0.1:18100/v2/models/mnist_net/infer"}
	if strings.Join(infer.cmd, " ") != strings.Join(want, " ") {
		t.Errorf("infer: %v", infer.cmd)
	}
	var body struct {
		Inputs []struct {
			Name     string    `json:"name"`
			Shape    []int     `json:"shape"`
			Datatype string    `json:"datatype"`
			Data     []float64 `json:"data"`
		} `json:"inputs"`
	}
	if err := json.Unmarshal([]byte(infer.stdin), &body); err != nil {
		t.Fatalf("the body of a request is not JSON: %v (%q)", err, infer.stdin)
	}
	if len(body.Inputs) != 1 || body.Inputs[0].Name != "input" || body.Inputs[0].Datatype != "FP32" ||
		len(body.Inputs[0].Shape) != 2 || len(body.Inputs[0].Data) != 3 {
		t.Fatalf("body: %+v", body)
	}
	// The same input the check container got, value for value.
	got := d.run.inputs()
	if len(got) != TestRequests || len(got[0]) != 3 {
		t.Fatalf("inputs of the check container: %d", len(got))
	}
	for i, v := range got[0] {
		if body.Inputs[0].Data[i] != float64(v) {
			t.Fatalf("the device got another input than the check container: %v against %v", body.Inputs[0].Data, got[0])
		}
	}
	// Asking for the container of the device is what starts the check; the name comes from the
	// configuration and nowhere else.
	if len(d.dev.asked) == 0 || d.dev.asked[0] != "device-1" {
		t.Errorf("containers asked for: %v", d.dev.asked)
	}
}

// A device whose model does not answer yet is no defect: the check reports not measured with the hint to
// come back (R3.4).
func TestEdgeCheckNotReady(t *testing.T) {
	d := deploymentSetup(t, func(d *deploymentService) {
		d.dev.answer = func(path, body string) (string, bool) {
			if path == "/v2/health/ready" {
				return "", false // Triton answers 503 while the model loads
			}
			return modelAnswer("mnist_net", linear)(path, body)
		}
	})
	ctx := context.Background()
	c := testChat(t, d.svg.St, "deployment")
	a := d.check(t, ctx, c.ID, "5")
	rep, fig := report(t, a)
	if rep.Verdict != NotMeasured || rep.Reasons[0] != msgNotReady {
		t.Fatalf("verdict %q: %v", rep.Verdict, rep.Reasons)
	}
	if rep.Header != "Result: not measured – "+msgNotReady {
		t.Errorf("header: %q", rep.Header)
	}
	if !hasText(rep.Hints, hintNotReady) {
		t.Errorf("hints: %v", rep.Hints)
	}
	if !hasSuggestion(rep.Suggestions, "wait_and_recheck") {
		t.Errorf("suggestions: %v", rep.Suggestions)
	}
	if fig.Requests != 0 || len(d.dev.calls) != 1 {
		t.Errorf("the check kept talking to a device that is not ready: %d commands", len(d.dev.calls))
	}
	// Not ready is no reason to check the model: the check says what to do instead.
	if hasSuggestion(rep.Suggestions, "undeploy") {
		t.Errorf("suggestions: %v", rep.Suggestions)
	}
}

// The device serves another model than the one the agent named: the check says which one it serves and
// keeps testing it (L17, D-04).
func TestEdgeCheckWrongModel(t *testing.T) {
	d := deploymentSetup(t, func(d *deploymentService) {
		d.fp.models[6] = modelFixture{name: "mnist_other", format: "onnx"}
		d.fp.deployments[9] = deploymentFixture{name: "agw-mnist", device: 1, port: 18100,
			repo: "agent-test/mnist-triton", tag: "v1", modelID: 6}
		d.dev.answer = modelAnswer("mnist_other", linear)
	})
	ctx := context.Background()
	c := testChat(t, d.svg.St, "deployment")
	a := d.check(t, ctx, c.ID, "5")
	rep, fig := report(t, a)
	if rep.Verdict != Questionable {
		t.Fatalf("verdict %q: %v", rep.Verdict, rep.Reasons)
	}
	if !hasText(rep.Reasons, "the deployment serves mnist_other, not mnist_net") {
		t.Errorf("reasons: %v", rep.Reasons)
	}
	// The requests still go out, under the name the device serves (L17).
	if fig.Requests != TestRequests || fig.ServedModel != "mnist_other" {
		t.Errorf("requests %d, served %q", fig.Requests, fig.ServedModel)
	}
	for _, call := range d.dev.calls[2:] {
		if !strings.Contains(call.cmd[len(call.cmd)-1], "/v2/models/mnist_other/infer") {
			t.Errorf("request to the wrong model: %v", call.cmd)
		}
	}
	if !hasSuggestion(rep.Suggestions, "undeploy") {
		t.Errorf("suggestions: %v", rep.Suggestions)
	}
}

// The platform's model record and the device do not name the same inputs: nothing is sent and the
// suggestion is to build the image again (R3.4, D-06, L17).
func TestEdgeCheckMetadataMismatch(t *testing.T) {
	d := deploymentSetup(t, func(d *deploymentService) {
		d.fp.modelWithIO(5, "mnist_net", TensorInfo{Name: "input", Shape: []int{1, 28, 28}, Type: "FP32"},
			TensorInfo{Name: "output", Shape: []int{1, 3}, Type: "FP32"}, []byte("ONNX"))
	})
	ctx := context.Background()
	c := testChat(t, d.svg.St, "deployment")
	a := d.check(t, ctx, c.ID, "5")
	rep, fig := report(t, a)
	if rep.Verdict != Questionable || fig.Requests != 0 {
		t.Fatalf("verdict %q, requests %d: %v", rep.Verdict, fig.Requests, rep.Reasons)
	}
	if !hasText(rep.Reasons, "do not agree on the inputs") || !hasText(rep.Reasons, "[1 28 28]") {
		t.Errorf("reasons: %v", rep.Reasons)
	}
	if !hasSuggestion(rep.Suggestions, "rebuild_image") || hasSuggestion(rep.Suggestions, "undeploy") {
		t.Errorf("suggestions: %v", rep.Suggestions)
	}
	if len(d.dev.calls) != 2 { // ready and the metadata, nothing else
		t.Errorf("commands: %d", len(d.dev.calls))
	}
	if fig.IO.Same || fig.IO.Platform.Inputs[0].Shape[1] != 28 {
		t.Errorf("io: %+v", fig.IO)
	}
}

// The device answers another value than the model file: the report names the largest difference and the
// top classes that do not agree (R3.6).
func TestEdgeCheckDifferingAnswers(t *testing.T) {
	d := deploymentSetup(t, func(d *deploymentService) {
		d.dev.answer = modelAnswer("mnist_net", func(in []float32) []float64 {
			out := linear(in)
			out[0] += 0.5 // outside every tolerance
			return out
		})
	})
	ctx := context.Background()
	c := testChat(t, d.svg.St, "deployment")
	a := d.check(t, ctx, c.ID, "5")
	rep, fig := report(t, a)
	if rep.Verdict != Questionable {
		t.Fatalf("verdict %q: %v", rep.Verdict, rep.Reasons)
	}
	if fig.MaxAbsDiff == nil || *fig.MaxAbsDiff <= AbsTolerance {
		t.Errorf("max_abs_diff: %v", fig.MaxAbsDiff)
	}
	if fig.Requests != TestRequests {
		t.Errorf("requests: %d", fig.Requests)
	}
	if !hasSuggestion(rep.Suggestions, "undeploy") {
		t.Errorf("suggestions: %v", rep.Suggestions)
	}
}

// A command that cannot be started at all ends the check as failed, with the figures it read until then
// (L5, D-10).
func TestEdgeCheckCommandFails(t *testing.T) {
	d := deploymentSetup(t, func(d *deploymentService) {
		d.dev.hardErr = errors.New("cannot exec into the container of the device")
	})
	ctx := context.Background()
	c := testChat(t, d.svg.St, "deployment")
	a := d.check(t, ctx, c.ID, "5")
	if a.State != "failed" || a.Error == "" {
		t.Fatalf("answer: %+v", a)
	}
	if len(a.Report) == 0 {
		t.Fatal("a failed check kept no figures")
	}
	var doc struct {
		Verdict string            `json:"verdict"`
		Header  string            `json:"header"`
		Figures DeploymentFigures `json:"figures"`
	}
	if err := json.Unmarshal(a.Report, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Verdict != "" || doc.Header != "" {
		t.Errorf("a failed check carries a verdict: %q %q", doc.Verdict, doc.Header)
	}
	if doc.Figures.Deployment.ID != 9 || doc.Figures.Model.Name != "mnist_net" {
		t.Errorf("figures: %+v", doc.Figures)
	}
}

// The test inputs are fixed: the same run twice sends the same values, the first is zeros, the second is
// 0.5, and a dynamic dimension becomes 1 (R3.5).
func TestSyntheticInputsDeterministic(t *testing.T) {
	in := &deploymentRun{TritonIO: SourceIO{Inputs: []TensorInfo{
		{Name: "input", Shape: []int{-1, 3}, Type: "FP32"}}}}
	first, why := testInputs(in)
	if why != "" {
		t.Fatal(why)
	}
	second, why := testInputs(in)
	if why != "" {
		t.Fatal(why)
	}
	if len(first) != TestRequests {
		t.Fatalf("requests: %d", len(first))
	}
	if !slicesEqualFloat(first[0].Data, make([]float32, 3)) {
		t.Errorf("the first request is not zeros: %v", first[0].Data)
	}
	for _, v := range first[1].Data {
		if v != 0.5 {
			t.Errorf("the second request is not 0.5: %v", first[1].Data)
			break
		}
	}
	for i := range first {
		if !slicesEqualFloat(first[i].Data, second[i].Data) {
			t.Fatalf("request %d differs between two runs: %v against %v", i+1, first[i].Data, second[i].Data)
		}
		if first[i].Shape[0] != 1 || first[i].Shape[1] != 3 {
			t.Errorf("shape: %v", first[i].Shape)
		}
		if first[i].Name != "input" {
			t.Errorf("name: %q", first[i].Name)
		}
	}
	// The requests with drawn values carry different ones.
	if slicesEqualFloat(first[2].Data, first[3].Data) {
		t.Error("two drawn requests carry the same values")
	}

	// What the test cannot do, it says.
	if _, why := testInputs(&deploymentRun{TritonIO: SourceIO{Inputs: []TensorInfo{
		{Name: "input", Shape: []int{1, 3}, Type: "INT64"}}}}); why != "input type INT64 not supported by the test" {
		t.Errorf("type: %q", why)
	}
	if _, why := testInputs(&deploymentRun{TritonIO: SourceIO{Inputs: []TensorInfo{
		{Name: "a", Shape: []int{1}, Type: "FP32"}, {Name: "b", Shape: []int{1}, Type: "FP32"}}}}); why == "" {
		t.Error("two inputs were accepted")
	}
	if _, why := testInputs(&deploymentRun{TritonIO: SourceIO{Inputs: []TensorInfo{
		{Name: "input", Shape: []int{2000, 1000}, Type: "FP32"}}}}); why == "" {
		t.Error("a too large input was accepted")
	}
}

// The comparison of one answer with the model file: the tolerance, shapes, non-finite values and the top
// class (R3.6).
func TestCompareOutputs(t *testing.T) {
	file := []TensorOut{{Name: "output", Shape: []int{1, 3}, Data: []float64{0.1, 0.7, 0.2}}}
	same := CompareOutputs([]TensorOut{{Name: "output", Shape: []int{1, 3}, Data: []float64{0.1, 0.7, 0.2}}}, file)
	if same.Reason != "" || same.MaxAbs != 0 || same.Argmax == nil || !*same.Argmax {
		t.Errorf("same: %+v", same)
	}
	// Just inside the tolerance: 1e-4 + 1e-3*0.7 = 8e-4; 7e-4 passes.
	inside := CompareOutputs([]TensorOut{{Name: "output", Shape: []int{1, 3}, Data: []float64{0.1, 0.7007, 0.2}}}, file)
	if inside.Reason != "" {
		t.Errorf("inside the tolerance: %+v", inside)
	}
	if inside.MaxAbs < 6e-4 || inside.MaxAbs > 8e-4 {
		t.Errorf("max_abs: %v", inside.MaxAbs)
	}
	// Just outside it: 9e-4 fails.
	outside := CompareOutputs([]TensorOut{{Name: "output", Shape: []int{1, 3}, Data: []float64{0.1, 0.7009, 0.2}}}, file)
	if outside.Reason == "" || !strings.Contains(outside.Reason, "tolerance") {
		t.Errorf("outside the tolerance: %+v", outside)
	}
	// Another top class, inside the tolerance.
	top := CompareOutputs([]TensorOut{{Name: "output", Shape: []int{1, 3}, Data: []float64{0.7, 0.1, 0.2}}}, file)
	if top.Argmax == nil || *top.Argmax || top.Reason == "" {
		t.Errorf("top class: %+v", top)
	}
	// Another shape, another number of outputs, a non-finite value and a name of its own.
	shape := CompareOutputs([]TensorOut{{Name: "output", Shape: []int{1, 2}, Data: []float64{0.1, 0.7}}}, file)
	if shape.Reason == "" || !strings.Contains(shape.Reason, "shape") {
		t.Errorf("shape: %+v", shape)
	}
	count := CompareOutputs([]TensorOut{{Name: "output", Shape: []int{1, 3}, Data: []float64{0.1, 0.7, 0.2}},
		{Name: "second", Shape: []int{1}, Data: []float64{0}}}, file)
	if count.Reason == "" || !strings.Contains(count.Reason, "outputs") {
		t.Errorf("count: %+v", count)
	}
	nan := CompareOutputs([]TensorOut{{Name: "output", Shape: []int{1, 3}, Data: []float64{0.1, math.NaN(), 0.2}}}, file)
	if nan.Reason == "" || !strings.Contains(nan.Reason, "finite") {
		t.Errorf("nan: %+v", nan)
	}
	name := CompareOutputs([]TensorOut{{Name: "logits", Shape: []int{1, 3}, Data: []float64{0.1, 0.7, 0.2}}}, file)
	if name.Reason == "" {
		t.Errorf("name: %+v", name)
	}
	// No classification output: there is no top class to compare (L17: argmax_same is null then).
	plain := []TensorOut{{Name: "boxes", Shape: []int{2, 4}, Data: []float64{1, 2, 3, 4, 5, 6, 7, 8}}}
	if d := CompareOutputs(plain, plain); d.Argmax != nil || d.Reason != "" {
		t.Errorf("no classification: %+v", d)
	}
}

// A device that answers the same to every request with drawn values is questionable even if every single
// answer matches: the model may ignore its input (R3.6).
func TestEdgeCheckConstantAnswers(t *testing.T) {
	d := deploymentSetup(t, func(d *deploymentService) {
		// The device always answers the value of the first request: it ignores what it gets.
		d.run.compute = func(in []float32) []float64 { return linear(make([]float32, len(in))) }
		d.dev.answer = modelAnswer("mnist_net", func(in []float32) []float64 {
			return linear(make([]float32, len(in)))
		})
	})
	ctx := context.Background()
	c := testChat(t, d.svg.St, "deployment")
	a := d.check(t, ctx, c.ID, "5")
	rep, fig := report(t, a)
	if rep.Verdict != Questionable {
		t.Fatalf("verdict %q: %v", rep.Verdict, rep.Reasons)
	}
	if !hasText(rep.Reasons, "the same to every test request") {
		t.Errorf("reasons: %v", rep.Reasons)
	}
	if fig.Requests != TestRequests {
		t.Errorf("requests: %d", fig.Requests)
	}
}

// A model the platform saved without an ONNX export cannot be run on both sides: the check says so
// (R1.9 uses the same reason) and sends nothing.
func TestEdgeCheckNotONNX(t *testing.T) {
	d := deploymentSetup(t, func(d *deploymentService) {
		f := d.fp.models[5]
		f.format = "pt"
		d.fp.models[5] = f
	})
	ctx := context.Background()
	c := testChat(t, d.svg.St, "deployment")
	a := d.check(t, ctx, c.ID, "5")
	rep, fig := report(t, a)
	if rep.Verdict != NotMeasured || !hasText(rep.Reasons, ReasonNotONNX) || !hasText(rep.Reasons, "format pt") {
		t.Fatalf("verdict %q: %v", rep.Verdict, rep.Reasons)
	}
	if fig.Requests != 0 || len(d.run.inputs()) != 0 {
		t.Errorf("the model file was run: %d requests, %d runs", fig.Requests, len(d.run.inputs()))
	}
}

// The limit of the chat holds for a synchronous check as well, and a failed one does not count (P7).
func TestEdgeCheckLimit(t *testing.T) {
	d := deploymentSetup(t)
	ctx := context.Background()
	c := testChat(t, d.svg.St, "deployment")
	d.svg.Max = 1
	d.check(t, ctx, c.ID, "5")
	res, err := d.svg.Do(ctx, c.ID, platform.Request{Method: http.MethodGet,
		Path: platform.AssessPrefix + "/deployments/9", Query: map[string]string{"model_id": "5"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "error" || !strings.Contains(res.Message, "check limit reached") {
		t.Fatalf("second check: %+v", res)
	}
}

// Without a configured device or without the container runtime the tool says that this setup cannot
// probe a deployment (G1.13) — it is no error the agent should repeat.
func TestEdgeCheckUnavailable(t *testing.T) {
	ctx := context.Background()
	off := []func(*deploymentService){
		func(d *deploymentService) { d.svg.EdgeDevices = nil },
		func(d *deploymentService) { d.svg.Devices = nil },
	}
	for i, o := range off {
		d := deploymentSetup(t, o)
		c := testChat(t, d.svg.St, "deployment")
		res, err := d.svg.Do(ctx, c.ID, platform.Request{Method: http.MethodGet,
			Path: platform.AssessPrefix + "/deployments/9", Query: map[string]string{"model_id": "5"}})
		if err != nil || res.Status != platform.StatusUnavailable || !strings.Contains(res.Message, "checks is not available") {
			t.Fatalf("case %d: %+v %v", i, res, err)
		}
		if len(d.dev.calls) != 0 {
			t.Fatalf("case %d sent %d commands", i, len(d.dev.calls))
		}
	}
}

// --- helpers of the tests ---

func hasText(list []string, want string) bool {
	for _, s := range list {
		if strings.Contains(s, want) {
			return true
		}
	}
	return false
}

func hasSuggestion(list []Suggestion, id string) bool {
	for _, s := range list {
		if s.ID == id {
			return true
		}
	}
	return false
}

func slicesEqualFloat(a, b []float32) bool {
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

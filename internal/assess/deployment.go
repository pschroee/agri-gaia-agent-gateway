// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package assess

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"agw/internal/platform"
	"agw/internal/store"
)

// The deployment check (R3): after a deployment has started, the gateway sends a few fixed test requests
// to the container on the device and compares the answers with what the model file computes for the same
// inputs. The verdict says whether the device serves exactly this model; how good the model is comes from
// a training check (R1) and is quoted next to it.
//
// **The only way to a device is the operator's list** (AGW_EDGE_SIM_DEVICES, RF1 of the planning): the
// gateway asks the container runtime for the container of that list, never for one it guessed, and only
// when the platform marks the device as simulated (tag) and the container carries the label of a
// simulated device. A real device is out of reach: the platform knows no address for it and the gateway
// has no Portainer access (D2.6). Every value that goes into a command is checked before the command does
// (the model name with platform.TritonNameRe, the port as a number of the agent's port range).

// KindDeployment is the kind of a check on a deployment (R3; the column assessments.kind).
const KindDeployment = "deployment"

// The rules of the test requests (R3.5, R3.6) are constants, so that a report, a test and the
// documentation name the same numbers.
const (
	// TestRequests is the number of requests the check sends (R3.5): one with zeros, one with 0.5
	// everywhere and four with values of fixed random generators.
	TestRequests = 6
	// MaxTestInputValues bounds one input: a larger one says no more than a smaller one and would push
	// megabytes through the socket of the device.
	MaxTestInputValues = 1_000_000
	// AbsTolerance and ScaleTolerance bound the difference between the answer of the device and of the
	// model file, per output tensor and request: max|a - b| <= AbsTolerance + ScaleTolerance*s, where the
	// scale s is the largest absolute value of that output in the model file. The rounding error of a
	// sum grows with the magnitude of its terms, not with the magnitude of its result, so another
	// summation order in another onnxruntime moves even a small element of a vector of large values by
	// an amount of the order of the whole vector (#232). Measured: 2.6e-6 absolute on an output of scale
	// ~2, and 0.0826 on an output of scale ~300 (about 2.8e-4 of it), both for the identical model file;
	// another model differs in the order of the logits themselves (>= 1e-1 of the scale). 1e-2 lies a
	// factor 35 above the first and a factor 10 below the second (R3.6).
	AbsTolerance   = 1e-4
	ScaleTolerance = 1e-2
	// MaxDeviceBytes is the largest answer one device request may have (8 MiB, as for the check
	// container).
	MaxDeviceBytes = MaxOutputBytes
	// deviceTimeout bounds one command in the container of the device (wget's own limit is 10 s).
	deviceTimeout = 30 * time.Second
)

// Fixed wordings of the check. msgNoSimulatedDevice is the reason of a device the gateway may not
// address (R3.2); tests and the documentation quote it.
const (
	msgNoSimulatedDevice = "the gateway can send test requests only to simulated devices (AGW_EDGE_SIM_DEVICES); the platform offers no way to reach a real device"
	msgNotReady          = "not ready yet"
	hintNotReady         = "the container is still starting or still pulling the image; check again in about a minute"
	hintRoundtrip        = "the round trip is measured in the gateway and includes the docker exec into the container of the device"
	noteDeployment       = "No real images; whether the model is good comes from the model check ({{tool:assess_model}})."

	// deviceUser: the user of the docker:dind-rootless container of a simulated device.
	deviceUser = "rootless"
	// deviceLabel marks the containers of a simulated edge device (edge D-E0: the device is recognisable
	// on the platform, in Portainer and in `docker ps`). The check reads it before every command.
	deviceLabel = "agri-gaia.simulated"
	// deviceTag is the platform's tag of a simulated device.
	deviceTag = "simulated"
)

// DeploymentRef, DeviceRef and the figures of a deployment check (R3.7, L17). The check card of the
// frontend reads these fields; a field it does not find it does not paint.
type (
	// DeploymentRef names the deployment that was checked.
	DeploymentRef struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	// DeviceRef names the device the deployment runs on and whether the platform marks it as simulated.
	DeviceRef struct {
		Name      string `json:"name"`
		Simulated bool   `json:"simulated"`
	}
	// SourceIO is what one source says about the inputs and outputs of the model (R3.4). The types are
	// written alike from all three sources (normalType), so that the comparison sees a difference in the
	// model and not one in the spelling.
	SourceIO struct {
		Inputs  []TensorInfo `json:"inputs"`
		Outputs []TensorInfo `json:"outputs"`
	}
	// DeploymentIO compares the three sources: same is true when they name the same inputs and outputs.
	DeploymentIO struct {
		Same     bool     `json:"same"`
		Triton   SourceIO `json:"triton"`
		Platform SourceIO `json:"platform"`
		File     SourceIO `json:"file"`
	}
	// RoundtripFigures are the round trips of the test requests, measured in the gateway (R3.6).
	RoundtripFigures struct {
		MedianMS int64 `json:"median"`
		MaxMS    int64 `json:"max"`
	}
	// DeploymentFigures is what a deployment check measured (R3.7, L17).
	DeploymentFigures struct {
		Deployment  DeploymentRef `json:"deployment"`
		Device      DeviceRef     `json:"device"`
		Image       string        `json:"image,omitempty"`
		Model       SubjectRef    `json:"model"`
		Ready       bool          `json:"ready"`
		ServedModel string        `json:"served_model,omitempty"`
		IO          DeploymentIO  `json:"io"`
		Requests    int           `json:"requests"`
		MaxAbsDiff  *float64      `json:"max_abs_diff"`
		// MaxRelDiff is the largest difference as a share of the scale of its output (nil without a
		// comparison or when every scale is 0); ToleranceUsed is the largest share of the tolerance
		// that a request used (<= 1 means the comparison held).
		MaxRelDiff    *float64         `json:"max_rel_diff"`
		ToleranceUsed *float64         `json:"tolerance_used"`
		Tolerance     ToleranceRule    `json:"tolerance"`
		PerRequest    []RequestFigures `json:"per_request"`
		ArgmaxSame    *int             `json:"argmax_same"`
		Roundtrip     RoundtripFigures `json:"roundtrip_ms"`
		ModelCheck    *ModelCheck      `json:"model_check"`
	}
	// ToleranceRule names the numbers of the comparison, so that a report is readable without the code.
	ToleranceRule struct {
		Abs   float64 `json:"abs"`
		Scale float64 `json:"scale"`
	}
	// RequestFigures is the comparison of one test request (E5 of #232).
	RequestFigures struct {
		Input         string   `json:"input"`
		MaxAbsDiff    float64  `json:"max_abs_diff"`
		Scale         float64  `json:"scale"`
		RelDiff       *float64 `json:"rel_diff"`
		ToleranceUsed float64  `json:"tolerance_used"`
		ArgmaxSame    *bool    `json:"argmax_same"`
		Tie           bool     `json:"tie,omitempty"`
	}
)

// --- the handler ---

// registerDeployment adds the path of the deployment check (R3.1): the deployment stands in the path, the
// model the agent expects in the query. It lives under the check prefix, so delegation, effect class,
// approval and log apply to it like to every other call.
func (s *Service) registerDeployment() {
	prefix := regexp.QuoteMeta(platform.AssessPrefix)
	s.Handle(http.MethodGet, regexp.MustCompile(`^`+prefix+`/deployments/([0-9]+)$`), s.deploymentCheck)
}

// deploymentCheck is GET /_agw/assess/deployments/<id>?model_id=<m> (R3.1): the check runs at once (a few
// seconds) and the answer carries the report in the form assess_status uses, so that the check card
// finds the row through the check id (L3).
func (s *Service) deploymentCheck(ctx context.Context, chatID string, req platform.Request, groups []string) (platform.Result, error) {
	if !s.Configured() || s.Plat == nil || s.Devices == nil || len(s.EdgeDevices) == 0 {
		return unavailableResult(), nil
	}
	depID, err := strconv.ParseInt(groups[1], 10, 64)
	if err != nil {
		return errResult("not a deployment id"), nil
	}
	modelID, merr := strconv.ParseInt(strings.TrimSpace(req.Query["model_id"]), 10, 64)
	if merr != nil || modelID <= 0 {
		return errResult("model_id missing"), nil
	}
	answer, err := s.Record(ctx, chatID, KindDeployment, "deployment:"+strconv.FormatInt(depID, 10),
		store.ToolCallFrom(ctx), func(ctx context.Context) (Report, error) {
			return s.runDeploymentCheck(ctx, chatID, depID, modelID)
		})
	if err != nil {
		return errResult(err.Error()), nil
	}
	return jsonResult(answer)
}

// TestRequestDevices names the simulated devices the gateway may send test requests to (R3.9): the entry
// "edge" of the capability overview names them, so that the agent knows before it plans a deployment
// whether the result can be checked on the device afterwards.
func (s *Service) TestRequestDevices() []string {
	if s == nil {
		return nil
	}
	out := make([]string, 0, len(s.EdgeDevices))
	for _, d := range s.EdgeDevices {
		out = append(out, d.Name)
	}
	return out
}

// --- the check ---

// deploymentRun collects everything the report of one deployment check rests on (R3.4). It is filled
// step by step; a step that cannot go on names the reason, and the report is written from what stands.
type deploymentRun struct {
	Deployment DeploymentRef
	Device     DeviceRef
	Image      string
	Model      SubjectRef
	// Served is the model the deployment's image was built from (the platform's container_images row
	// names it): the name the device serves. Empty while the image names no model.
	Served SubjectRef
	// ContainerID is the container of the simulated device the commands run in (R3.2); empty while the
	// gate has not let the check through.
	ContainerID string
	// Port is the host port of the deployment's Triton HTTP port on the device.
	Port int

	Ready    bool
	TritonIO SourceIO
	PlatIO   SourceIO
	FileIO   SourceIO
	// Answers is one entry per request that ran: the answer of the device and of the model file.
	Answers []requestAnswer
	// Sent counts the requests that went out (a refused one counts as well, L17).
	Sent int
	// Reasons holds what was found before the verdict (a wrong model, a missing signal); every one
	// becomes part of the report.
	Reasons []string
	// check is the last final check of the model in this chat (L8, W4); nil when there is none.
	check *ModelCheck
}

// requestAnswer is what one test request produced (R3.6).
type requestAnswer struct {
	// Diff: how far the answer of the device is from the model file.
	Diff OutputDiff
	// N and Input: the number (1-based) and the name of the test request (requestLabel).
	N     int
	Input string
	// Took: the round trip of this request, measured in the gateway around the command in the device's
	// container (R3.6).
	Took time.Duration
	// Drawn: this request carried drawn values (R3.5); its answer counts for the rule that the device
	// must not answer the same to every request.
	Drawn bool
	// Device: the answer of the device, for that rule.
	Device []TensorOut
}

// runDeploymentCheck is the work of one deployment check (R3.4): the gate to the device, the readiness,
// the three sources of the model's interface, the test requests against the device and against the model
// file, and the comparison. An error means the check broke (a command failed, a read went wrong); what it
// found until then stands in the figures it returns with the error (L5).
func (s *Service) runDeploymentCheck(ctx context.Context, chatID string, depID, modelID int64) (Report, error) {
	in := &deploymentRun{Model: SubjectRef{ID: modelID}}
	in.Deployment.ID = depID

	dep, err := s.deploymentOf(ctx, chatID, depID)
	if err != nil {
		return in.report(NotMeasured, nil), err
	}
	in.Deployment.Name = dep.Name
	in.Image = dep.Image.ref()
	if port, ok := dep.httpPort(); ok {
		in.Port = port
	}
	// The image names the model it was built from: that is the name the device serves (R3.4).
	if mid, ok := dep.Image.modelID(); ok {
		in.Served.ID = mid
		if m, err := s.modelInfoFull(ctx, chatID, mid); err == nil {
			in.Served.Name = m.Name
		}
	}
	// The model the agent named: its platform record is one of the three sources (R3.4).
	m, err := s.modelInfoFull(ctx, chatID, modelID)
	if err != nil {
		return in.report(NotMeasured, nil), err
	}
	in.Model.Name = m.Name
	in.PlatIO = platformIO(m)
	// The last check of the model stands next to the verdict (L8, W4): it says how good the model is,
	// which this check does not.
	if c, err := s.ModelCheckOf(ctx, chatID, modelID); err == nil && c.Found {
		in.check = &c
	}

	if why, err := s.deviceGate(ctx, chatID, in, dep); err != nil {
		return in.report(NotMeasured, nil), err
	} else if why != "" {
		return in.report(NotMeasured, []string{why}), nil
	}
	if in.Port == 0 {
		rep := in.report(Questionable, []string{"the deployment binds no host port to the inference server's HTTP port 8000"})
		rep.Suggestions = append(rep.Suggestions, suggestionUndeploy)
		return rep, nil
	}

	// The device has to answer before anything is sent (R3.4).
	ready, err := s.deviceReady(ctx, in)
	if err != nil {
		return in.report(NotMeasured, nil), err
	}
	if !ready {
		rep := in.report(NotMeasured, []string{msgNotReady})
		rep.Hints = append(rep.Hints, hintNotReady)
		rep.Suggestions = append(rep.Suggestions, Suggestion{ID: "wait_and_recheck",
			Text: "wait for the container to finish starting and run the deployment check again"})
		return rep, nil
	}
	in.Ready = true

	// What the device serves: the metadata of the served model (R3.4).
	why, err := s.readDeviceModel(ctx, in)
	if err != nil {
		return in.report(NotMeasured, nil), err
	}
	if why != "" {
		rep := in.report(Questionable, []string{why})
		rep.Suggestions = append(rep.Suggestions, suggestionRebuild)
		return rep, nil
	}
	if in.Served.Name != "" && in.Served.Name != in.Model.Name {
		in.Reasons = append(in.Reasons,
			fmt.Sprintf("the deployment serves %s, not %s", in.Served.Name, in.Model.Name))
	}

	// The three sources have to name the same inputs and outputs before a request goes out (R3.4, L17).
	if why := compareInterface(in); why != "" {
		rep := in.report(Questionable, []string{why})
		rep.Suggestions = append(rep.Suggestions, suggestionRebuild)
		return rep, nil
	}

	inputs, why := testInputs(in)
	if why != "" {
		return in.report(NotMeasured, []string{why}), nil
	}
	// Only an ONNX model can be run the same way on both sides (R0.13); the platform's record says what
	// the file is (R1.9 uses the same reason).
	if m.Format != "onnx" {
		return in.report(NotMeasured, []string{ReasonNotONNX + " (format " + m.Format + ")"}), nil
	}
	model, _, err := s.modelFile(ctx, chatID, modelID)
	if err != nil {
		return in.report(NotMeasured, nil), err
	}
	for i := range inputs {
		if err := s.testRequest(ctx, in, model, inputs[i], i); err != nil {
			return in.report(Questionable, in.Reasons), err
		}
	}
	return in.judged(), nil
}

// testRequest sends one synthetic input to the model file and to the device and compares the two answers
// (R3.4, R3.6). The model file runs first: it is the reference, and its interface is the third source.
func (s *Service) testRequest(ctx context.Context, in *deploymentRun, model []byte, t Tensor, i int) error {
	drawn := i >= 2
	label := requestLabel(i)
	out, err := s.runner().Run(ctx, Job{Model: model, Tensors: []Tensor{t}})
	if err != nil {
		return err
	}
	if len(in.FileIO.Inputs) == 0 {
		in.FileIO = SourceIO{Inputs: tensorInfos(out.Model.Inputs), Outputs: tensorInfos(out.Model.Outputs)}
	}
	if len(out.Tensors) == 0 {
		return errors.New("the check container answered no output tensor for a test request")
	}
	body, err := inferBody(t)
	if err != nil {
		return err
	}
	in.Sent++
	start := time.Now()
	answer, ok, detail, err := s.deviceCall(ctx, in, "/v2/models/"+in.servedName()+"/infer", body)
	took := time.Since(start)
	if err != nil {
		return err
	}
	if !ok {
		in.Reasons = append(in.Reasons, fmt.Sprintf(
			"the device answered no result for test request %d%s", in.Sent, reasonDetail(detail)))
		return nil
	}
	device, err := parseInfer(answer)
	if err != nil {
		in.Reasons = append(in.Reasons, fmt.Sprintf(
			"the device answered test request %d in a form the gateway does not know (%s)", in.Sent, err))
		return nil
	}
	diff := CompareOutputs(device, out.Tensors[0])
	if diff.Reason != "" {
		in.Reasons = append(in.Reasons, fmt.Sprintf("test request %d (%s): %s", i+1, label, diff.Reason))
	}
	in.Answers = append(in.Answers, requestAnswer{Diff: diff, N: i + 1, Input: label, Took: took, Drawn: drawn, Device: device})
	return nil
}

// runner is the container runtime of the checks (R0); the field is called Run, so this keeps the call
// sites readable.
func (s *Service) runner() *Runner { return s.Run }

// servedName is the model every request goes to: the model the deployment's image was built from, or the
// model the agent named when the image names none (the device has one model either way).
func (in *deploymentRun) servedName() string {
	if in.Served.Name != "" {
		return in.Served.Name
	}
	return in.Model.Name
}

// --- the gate to the device ---

// deviceGate decides whether the gateway may send test requests to the device of this deployment (R3.2):
// the device has to be the one the operator configured (by its name), it has to carry the platform's tag
// `simulated`, and its container has to run with the label of a simulated device. It answers the reason
// when it may not; then nothing goes to the device.
func (s *Service) deviceGate(ctx context.Context, chatID string, in *deploymentRun, dep deploymentRec) (string, error) {
	if d, err := s.edgeDevice(ctx, chatID, dep); err != nil {
		return "", err
	} else if d != nil {
		in.Device.Name = d.Name
		in.Device.Simulated = slices.Contains(d.Tags, deviceTag)
	}
	container, configured := s.simDevice(in.Device.Name)
	if !configured || !in.Device.Simulated {
		return msgNoSimulatedDevice, nil
	}
	id, labels, running, err := s.Devices.ContainerByName(ctx, container)
	if err != nil {
		return "", fmt.Errorf("the container of the simulated device is not readable: %w", err)
	}
	if !running || labels[deviceLabel] != "true" {
		return msgNoSimulatedDevice, nil
	}
	in.ContainerID = id
	return "", nil
}

// simDevice names the container of a configured device; ok is false when the operator did not configure
// this device (R3.2).
func (s *Service) simDevice(name string) (string, bool) {
	for _, d := range s.EdgeDevices {
		if d.Name == name {
			return d.Container, true
		}
	}
	return "", false
}

// deviceReady asks the device whether its model answers (R3.4). A device that does not answer yet is no
// defect of the deployment: the check reports it as not measured and tells the agent to come back. An
// error means the command could not be run at all — that is a broken check, not a device that is late.
func (s *Service) deviceReady(ctx context.Context, in *deploymentRun) (bool, error) {
	_, ok, _, err := s.deviceCall(ctx, in, "/v2/health/ready", "")
	return ok, err
}

// readDeviceModel reads the metadata of the served model from the device (R3.4) and answers the reason
// when the device does not serve it.
func (s *Service) readDeviceModel(ctx context.Context, in *deploymentRun) (string, error) {
	served := in.servedName()
	if !platform.TritonNameRe.MatchString(served) {
		return fmt.Sprintf("the name %q of the model cannot be addressed by the inference server over HTTP", served), nil
	}
	data, ok, detail, err := s.deviceCall(ctx, in, "/v2/models/"+served, "")
	if err != nil {
		return "", err
	}
	if !ok {
		return fmt.Sprintf("the device does not serve a model named %s (the inference server refused its metadata%s)",
			served, reasonDetail(detail)), nil
	}
	var meta tritonModel
	if json.Unmarshal(data, &meta) != nil {
		return fmt.Sprintf("the device answered the metadata of %s in a form the gateway does not know", served), nil
	}
	in.TritonIO = SourceIO{Inputs: tritonInfos(meta.Inputs), Outputs: tritonInfos(meta.Outputs)}
	return "", nil
}

// deviceCall runs one fixed command in the container of the device (R3.3) and answers its output. The
// command is built here and nowhere else: wget with a time limit and, with a body, the JSON over stdin.
// ok is false when the device refused the request (wget ends with a non-zero code for every status it
// refuses); detail carries what wget said about it. err is set when the command itself could not run.
func (s *Service) deviceCall(ctx context.Context, in *deploymentRun, path, body string) (data []byte, ok bool, detail string, err error) {
	if in.ContainerID == "" {
		return nil, false, "", errors.New("the check has no container on the device")
	}
	if in.Port < platform.EdgeHostPortMin || in.Port > platform.EdgeHostPortMax {
		return nil, false, "", errors.New("the deployment has no host port in the range the agent may use")
	}
	args := []string{"wget", "-q", "-O", "-", "-T", "10"}
	var stdin io.Reader
	if body != "" {
		args = append(args, "--header", "Content-Type: application/json", "--post-file", "/dev/stdin")
		stdin = strings.NewReader(body)
	}
	args = append(args, fmt.Sprintf("http://127.0.0.1:%d%s", in.Port, path))

	callCtx, cancel := context.WithTimeout(ctx, deviceTimeout)
	defer cancel()
	out, exit, cerr := s.Devices.ExecAs(callCtx, in.ContainerID, deviceUser, args, stdin)
	if exit < 0 {
		// -1 comes from the runtime itself (the command could not be started), not from the command.
		return nil, false, "", cerr
	}
	if exit != 0 {
		return nil, false, commandText(cerr), nil
	}
	if len(out) > MaxDeviceBytes {
		return nil, false, "", fmt.Errorf("the device answered more than %d MiB", MaxDeviceBytes>>20)
	}
	return out, true, "", nil
}

// commandText is what the command said about its failure: the runtime's message without the command
// itself (it is the gateway's own fixed command and says nothing to the agent), so that a reason can
// name the status the device answered (404, 503).
func commandText(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	const marker = "]: exit code "
	i := strings.Index(s, marker)
	if i < 0 {
		return ""
	}
	rest := s[i+len(marker):]
	j := strings.Index(rest, ": ")
	if j < 0 {
		return ""
	}
	return rest[j+2:]
}

// reasonDetail puts the text of a refused command behind a reason; empty when it said nothing.
func reasonDetail(detail string) string {
	if detail == "" {
		return ""
	}
	return ": " + detail
}

// --- the test inputs (R3.5) ---

// testInputs builds the fixed test requests from the input of the served model (R3.5): the first is all
// zeros, the second is 0.5 everywhere, and the remaining four are drawn uniformly from [0, 1) with a
// generator of their own, so that a run can be repeated. It answers the reason when the model cannot be
// tested this way.
func testInputs(in *deploymentRun) ([]Tensor, string) {
	info := in.TritonIO.Inputs
	if len(info) == 0 {
		return nil, "the device named no input of the model"
	}
	if len(info) > 1 {
		return nil, fmt.Sprintf("the model has %d inputs; the test requests carry one", len(info))
	}
	t := info[0]
	if t.Type != "FP32" {
		return nil, fmt.Sprintf("input type %s not supported by the test", t.Type)
	}
	shape, n := fixedShape(t.Shape)
	if n <= 0 {
		return nil, "the input of the model has no values"
	}
	if n > MaxTestInputValues {
		return nil, fmt.Sprintf("the input of the model has %d values; test requests carry at most %d",
			n, MaxTestInputValues)
	}
	out := make([]Tensor, 0, TestRequests)
	for i := 0; i < TestRequests; i++ {
		data := make([]float32, n)
		switch {
		case i == 0: // zeros
		case i == 1: // 0.5 everywhere
			for j := range data {
				data[j] = 0.5
			}
		default:
			r := rand.New(rand.NewPCG(uint64(i-1), 0x9E3779B97F4A7C15))
			for j := range data {
				data[j] = float32(r.Float64())
			}
		}
		out = append(out, Tensor{Name: t.Name, Shape: shape, Data: data})
	}
	return out, ""
}

// fixedShape turns a declared shape into the shape of a test request: a dynamic dimension becomes 1
// (R3.5). n is the number of values behind it.
func fixedShape(shape []int) ([]int, int) {
	out := make([]int, len(shape))
	n := 1
	for i, d := range shape {
		if d <= 0 {
			d = 1
		}
		out[i] = d
		n *= d
	}
	return out, n
}

// inferBody is the body of one test request to Triton (KServe v2): the input with its shape, its type and
// its values. Only FP32 is sent (R3.5).
func inferBody(t Tensor) (string, error) {
	b, err := json.Marshal(map[string]any{"inputs": []map[string]any{{
		"name": t.Name, "shape": t.Shape, "datatype": "FP32", "data": t.Data,
	}}})
	return string(b), err
}

// --- what the device answers ---

// tritonModel is the part of the answer of GET /v2/models/<name> the check reads (R3.4).
type tritonModel struct {
	Name    string       `json:"name"`
	Inputs  []tritonInfo `json:"inputs"`
	Outputs []tritonInfo `json:"outputs"`
}

type tritonInfo struct {
	Name     string `json:"name"`
	Shape    []int  `json:"shape"`
	Datatype string `json:"datatype"`
}

// parseInfer reads the output tensors out of the answer of one test request.
func parseInfer(data []byte) ([]TensorOut, error) {
	var doc struct {
		Outputs []struct {
			Name  string    `json:"name"`
			Shape []int     `json:"shape"`
			Data  []float64 `json:"data"`
		} `json:"outputs"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if len(doc.Outputs) == 0 {
		return nil, errors.New("no output")
	}
	out := make([]TensorOut, 0, len(doc.Outputs))
	for _, o := range doc.Outputs {
		out = append(out, TensorOut{Name: o.Name, Shape: o.Shape, Data: o.Data})
	}
	return out, nil
}

// OutputDiff is the comparison of one answer of the device with the answer of the model file (R3.6).
type OutputDiff struct {
	// MaxAbs: the largest difference between a value of the device and of the model file.
	MaxAbs float64
	// Argmax: whether the device named the same top classes (strictly); nil when the output is no
	// classification (there is no class to compare).
	Argmax *bool
	// ArgmaxOK: the top classes agree or differ only inside a tie (see Tie).
	ArgmaxOK bool
	// Scale: the largest absolute value of the output in the model file, of the output that used most of
	// its tolerance.
	Scale float64
	// RelDiff: the difference of that output as a share of Scale; nil when Scale is 0.
	RelDiff *float64
	// Used: the largest share of the tolerance that one output used (<= 1 means it held).
	Used float64
	// Tie: the device names another top class, but the two values of the model file are no further apart
	// than twice the tolerance; TieGap is that distance.
	Tie    bool
	TieGap float64
	// Reason: what did not hold; empty when the two answers agree.
	Reason string
}

// CompareOutputs compares one answer of the device with the answer of the model file (R3.6): the same
// outputs in the same order, the same shapes, every value finite, per output tensor the largest
// difference d inside AbsTolerance + ScaleTolerance*s (s is the largest absolute value of the model
// file's output) and, for a classification output, the same top class unless the two values of the model
// file are a tie inside twice the tolerance. It is a pure function: no device and no container is needed
// to test it.
func CompareOutputs(device, file []TensorOut) OutputDiff {
	out := OutputDiff{}
	if len(device) != len(file) {
		out.Reason = fmt.Sprintf("the device answered %d outputs, the model file %d", len(device), len(file))
		return out
	}
	var deviceTop, fileTop []int
	classification := true
	strictSame := true
	tie := false
	var tieGap float64
	topReason := ""
	var worstD, worstS float64
	worstUsed := -1.0
	for i := range file {
		a, b := device[i], file[i]
		if a.Name != b.Name {
			out.Reason = fmt.Sprintf("the device answered the output %q, the model file %q", a.Name, b.Name)
			return out
		}
		if !slices.Equal(a.Shape, b.Shape) {
			out.Reason = fmt.Sprintf("the output %s has the shape %s on the device and %s in the model file",
				b.Name, ints(a.Shape), ints(b.Shape))
			return out
		}
		if len(a.Data) != len(b.Data) {
			out.Reason = fmt.Sprintf("the output %s has %d values on the device and %d in the model file",
				b.Name, len(a.Data), len(b.Data))
			return out
		}
		var d, s float64
		for j := range b.Data {
			if !finite(a.Data[j]) || !finite(b.Data[j]) {
				out.Reason = fmt.Sprintf("the output %s carries a value that is not a finite number", b.Name)
				return out
			}
			d = math.Max(d, math.Abs(a.Data[j]-b.Data[j]))
			s = math.Max(s, math.Abs(b.Data[j]))
		}
		tol := AbsTolerance + ScaleTolerance*s
		used := d / tol
		out.MaxAbs = math.Max(out.MaxAbs, d)
		if used > worstUsed {
			worstUsed, worstD, worstS = used, d, s
		}
		if isDistribution(b.Shape) && len(b.Data) > 0 {
			k, top := topOf(a.Data), topOf(b.Data)
			deviceTop = append(deviceTop, k)
			fileTop = append(fileTop, top)
			if k != top {
				strictSame = false
				if gap := b.Data[top] - b.Data[k]; gap <= 2*tol {
					tie, tieGap = true, gap
				} else if topReason == "" {
					topReason = fmt.Sprintf("the device names class %d as the top class, the model file class %d", k, top)
				}
			}
		} else {
			classification = false
		}
	}
	if worstUsed >= 0 {
		out.Used, out.Scale = worstUsed, worstS
		if worstS > 0 {
			rel := worstD / worstS
			out.RelDiff = &rel
		}
	}
	out.ArgmaxOK = true
	if out.Used > 1 {
		tie = false // a tie is only rounding where the values agree inside the tolerance anyway
	}
	if classification && len(deviceTop) > 0 {
		out.Argmax = &strictSame
		out.Tie, out.TieGap = tie, tieGap
		out.ArgmaxOK = strictSame || (tie && topReason == "")
	}
	switch {
	case out.Used > 1:
		if out.RelDiff == nil {
			out.Reason = fmt.Sprintf("the device differs from the model file by up to %.3g while the output of the model file is all zero; "+
				"rounding in another runtime stays below %g of the largest value, so the device serves another model or computes it differently",
				worstD, AbsTolerance)
		} else {
			out.Reason = fmt.Sprintf("the device differs from the model file by up to %.3g, %.2g of the largest value of the output (%.3g); "+
				"rounding in another runtime stays below %g of it, so the device serves another model or computes it differently",
				worstD, *out.RelDiff, worstS, ScaleTolerance)
		}
	case topReason != "":
		out.Reason = topReason
	}
	return out
}

// requestLabel names test request i (0-based) the way messages and figures do (E6 of #232).
func requestLabel(i int) string {
	switch i {
	case 0:
		return "zeros"
	case 1:
		return "0.5 everywhere"
	}
	return fmt.Sprintf("drawn %d", i-1)
}

// isDistribution reports whether a shape is a classification output: [1, C] or [C].
func isDistribution(shape []int) bool {
	return len(shape) == 1 || (len(shape) == 2 && shape[0] == 1)
}

// topOf is the index of the largest value (the class of a classification output).
func topOf(data []float64) int {
	top := 0
	for i, v := range data {
		if v > data[top] {
			top = i
		}
	}
	return top
}

// finite reports whether a number is usable (neither NaN nor infinite).
func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

// ints renders a shape for a message: [1 3 28 28].
func ints(shape []int) string {
	parts := make([]string, 0, len(shape))
	for _, d := range shape {
		parts = append(parts, strconv.Itoa(d))
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// --- the judgement (R3.6, R3.7, R3.8) ---

// judged turns what stands in the run into the report: the verdict, the reasons, the figures and the
// suggestions (R3.6). Usable needs every one of the six requests to have gone out and every comparison
// to hold.
func (in *deploymentRun) judged() Report {
	reasons := append([]string(nil), in.Reasons...)
	if len(in.FileIO.Inputs) > 0 {
		if why := diffIO("the model file", in.FileIO, "the device", in.TritonIO); why != "" {
			reasons = append(reasons, why)
		}
	}
	if why := in.constantReason(); why != "" {
		reasons = append(reasons, why)
	}
	if in.requests() < TestRequests {
		reasons = append(reasons, fmt.Sprintf("only %d of %d test requests were answered by the device",
			in.requests(), TestRequests))
	}
	labelsWrong := in.check != nil && in.check.LabelOrderWrong
	if labelsWrong {
		reasons = append(reasons, ReasonLabelOrderFromCheck)
	}
	verdict := Questionable
	if len(reasons) == 0 && in.requests() == TestRequests && in.ioSame() {
		verdict = Usable
	}
	rep := in.report(verdict, reasons)
	for _, a := range in.Answers {
		if a.Diff.Tie {
			rep.Hints = append(rep.Hints, fmt.Sprintf(
				"test request %d (%s): the two largest values of the model file are only %.3g apart, inside the tolerance; "+
					"that the device names another top class there is rounding, not another model",
				a.N, a.Input, a.Diff.TieGap))
		}
	}
	if f := rep.Figures.(*DeploymentFigures); verdict == Usable && f.MaxAbsDiff != nil && *f.MaxAbsDiff > AbsTolerance && f.MaxRelDiff != nil {
		rep.Hints = append(rep.Hints, fmt.Sprintf(
			"the device differs from the model file by up to %.3g, %.2g of the largest value of the output; "+
				"differences below %g of it come from rounding in another runtime and do not mean another model",
			*f.MaxAbsDiff, *f.MaxRelDiff, ScaleTolerance))
	}
	if labelsWrong {
		rep.Suggestions = append(rep.Suggestions, suggestionFixOutputLabels, suggestionRebuild)
	}
	if verdict == Questionable && in.Device.Name != "" {
		rep.Suggestions = append(rep.Suggestions, suggestionUndeploy)
	}
	return rep
}

// report builds the report of the check (R3.7): the verdict, the reasons it was given, the figures and
// the note; the header comes from Finish (R0.2).
func (in *deploymentRun) report(verdict Verdict, reasons []string) Report {
	rep := NewReport(KindDeployment, SetSynthetic, 0)
	rep.Verdict = verdict
	if verdict != Usable {
		rep.Reasons = reasons
	}
	rep.ImageReview = ImageReview{Done: false}
	rep.Note = noteDeployment
	if in.Sent > 0 {
		rep.Hints = append(rep.Hints, hintRoundtrip)
	}
	rep.Figures = in.figures()
	if verdict == Usable || in.Ready {
		rep.Suggestions = append(rep.Suggestions, Suggestion{ID: "assess_model",
			Text: "check the model itself with {{tool:assess_model}}: the deployment check says whether the device serves it, not whether it is good"})
	}
	return rep
}

// figures is what the report carries (R3.7, L17), including the last final model check of this chat
// (L8, W4) when there is one.
func (in *deploymentRun) figures() *DeploymentFigures {
	maxAbs, argmaxSame := in.metrics()
	maxRel, used, perRequest := in.perRequest()
	f := &DeploymentFigures{
		Deployment: in.Deployment, Device: in.Device, Image: in.Image, Model: in.Model,
		Ready: in.Ready, ServedModel: in.servedName(),
		IO:       DeploymentIO{Same: in.ioSame(), Triton: in.TritonIO, Platform: in.PlatIO, File: in.FileIO},
		Requests: in.requests(), MaxAbsDiff: maxAbs, ArgmaxSame: argmaxSame,
		MaxRelDiff: maxRel, ToleranceUsed: used, PerRequest: perRequest,
		Tolerance: ToleranceRule{Abs: AbsTolerance, Scale: ScaleTolerance},
		Roundtrip: roundtripOf(in.times()),
	}
	f.ModelCheck = in.check
	return f
}

// metrics are the figures over all requests: the largest difference (nil while nothing was compared) and
// in how many requests the device named the same top class (nil when the output is no classification,
// L17).
func (in *deploymentRun) metrics() (*float64, *int) {
	var maxAbs *float64
	var argmax *int
	classified := false
	for _, a := range in.Answers {
		if maxAbs == nil || a.Diff.MaxAbs > *maxAbs {
			v := a.Diff.MaxAbs
			maxAbs = &v
		}
		if a.Diff.Argmax != nil {
			classified = true
			if argmax == nil {
				v := 0
				argmax = &v
			}
			if *a.Diff.Argmax {
				*argmax++
			}
		}
	}
	if !classified {
		argmax = nil
	}
	return maxAbs, argmax
}

// perRequest are the figures of every compared request (E5 of #232): the largest share of the scale and
// of the tolerance (nil while nothing was compared) and one entry per request, so that every threshold
// can be checked against the stored numbers.
func (in *deploymentRun) perRequest() (*float64, *float64, []RequestFigures) {
	var maxRel, used *float64
	out := make([]RequestFigures, 0, len(in.Answers))
	for _, a := range in.Answers {
		d := a.Diff
		if d.RelDiff != nil && (maxRel == nil || *d.RelDiff > *maxRel) {
			v := *d.RelDiff
			maxRel = &v
		}
		if used == nil || d.Used > *used {
			v := d.Used
			used = &v
		}
		out = append(out, RequestFigures{Input: a.Input, MaxAbsDiff: d.MaxAbs, Scale: d.Scale,
			RelDiff: d.RelDiff, ToleranceUsed: d.Used, ArgmaxSame: d.Argmax, Tie: d.Tie})
	}
	return maxRel, used, out
}

// requests is how many requests the device answered.
func (in *deploymentRun) requests() int { return len(in.Answers) }

// times are the round trips of the answered requests.
func (in *deploymentRun) times() []time.Duration {
	out := make([]time.Duration, 0, len(in.Answers))
	for _, a := range in.Answers {
		out = append(out, a.Took)
	}
	return out
}

// ioSame reports whether the three sources name the same inputs and outputs (R3.4). A platform record
// without tensor metadata names nothing and is not a difference: the check says so with the sources it
// has.
func (in *deploymentRun) ioSame() bool {
	if len(in.TritonIO.Inputs) == 0 && len(in.TritonIO.Outputs) == 0 {
		return false
	}
	if len(in.FileIO.Inputs) > 0 && !sameIO(in.TritonIO, in.FileIO) {
		return false
	}
	if (len(in.PlatIO.Inputs) > 0 || len(in.PlatIO.Outputs) > 0) && !sameIO(in.TritonIO, in.PlatIO) {
		return false
	}
	return true
}

// constantReason is the rule that the device must not answer the same to every request with drawn values
// (R3.6): a model that ignores its input would pass every single comparison.
func (in *deploymentRun) constantReason() string {
	var drawn [][]TensorOut
	for _, a := range in.Answers {
		if a.Drawn {
			drawn = append(drawn, a.Device)
		}
	}
	if len(drawn) < 2 {
		return ""
	}
	for _, a := range drawn[1:] {
		if !sameOutputs(drawn[0], a) {
			return ""
		}
	}
	return "the device answers the same to every test request with drawn values; the model may ignore its input"
}

// roundtripOf is the median and the largest round trip of the test requests (R3.6): the median is the
// middle of the sorted list (even count: the upper of the two middle ones, as the plan asks).
func roundtripOf(list []time.Duration) RoundtripFigures {
	if len(list) == 0 {
		return RoundtripFigures{}
	}
	sorted := append([]time.Duration(nil), list...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return RoundtripFigures{
		MedianMS: sorted[len(sorted)/2].Milliseconds(),
		MaxMS:    sorted[len(sorted)-1].Milliseconds(),
	}
}

// suggestionUndeploy and suggestionRebuild are the suggestions of R3.8: taking a deployment back is a
// write and asks the user (or runs inside a plan), so the gateway proposes it instead of doing it.
const ReasonLabelOrderFromCheck = "the last check of the model found output_labels in another order than the model's outputs, so the device answers with wrong class names"

var (
	suggestionFixOutputLabels = Suggestion{ID: "fix_output_labels",
		Text: "correct output_labels with {{tool:update_model}} as the model check says"}
	suggestionUndeploy = Suggestion{ID: "undeploy",
		Text: "take the deployment back ({{tool:undeploy}}) and deploy the right image again"}
	suggestionRebuild = Suggestion{ID: "rebuild_image",
		Text: "build the inference image again from this model and deploy the new one"}
)

// sameOutputs reports whether two answers of the device carry the same tensors (names, shapes and
// values). The rule that a model must not answer the same to every request (R3.6) rests on it.
func sameOutputs(a, b []TensorOut) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Name != b[i].Name || !slices.Equal(a[i].Shape, b[i].Shape) || !slices.Equal(a[i].Data, b[i].Data) {
			return false
		}
	}
	return true
}

// --- the three sources of the model's interface (R3.4) ---

// sameIO reports whether two sources name the same inputs and outputs (name, shape and type), one for
// one and in the same order.
func sameIO(a, b SourceIO) bool {
	return sameInfos(a.Inputs, b.Inputs) && sameInfos(a.Outputs, b.Outputs)
}

func sameInfos(a, b []TensorInfo) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Name != b[i].Name || a[i].Type != b[i].Type || !slices.Equal(a[i].Shape, b[i].Shape) {
			return false
		}
	}
	return true
}

// compareInterface checks the three sources against each other (R3.4) and answers the reason of the first
// difference; empty when they agree.
func compareInterface(in *deploymentRun) string {
	if len(in.PlatIO.Inputs) > 0 || len(in.PlatIO.Outputs) > 0 {
		if why := diffIO("the platform's model record", in.PlatIO, "the device", in.TritonIO); why != "" {
			return why
		}
	}
	return ""
}

// diffIO names the first difference between two sources of the model's interface.
func diffIO(whatA string, a SourceIO, whatB string, b SourceIO) string {
	if why := diffInfos("inputs", a.Inputs, b.Inputs); why != "" {
		return fmt.Sprintf("%s and %s do not agree on the inputs: %s", whatA, whatB, why)
	}
	if why := diffInfos("outputs", a.Outputs, b.Outputs); why != "" {
		return fmt.Sprintf("%s and %s do not agree on the outputs: %s", whatA, whatB, why)
	}
	return ""
}

func diffInfos(what string, a, b []TensorInfo) string {
	if len(a) != len(b) {
		return fmt.Sprintf("the %s are %d there and %d here", what, len(a), len(b))
	}
	for i := range a {
		if a[i].Name != b[i].Name {
			return fmt.Sprintf("the %s is named %q there and %q here", strings.TrimSuffix(what, "s"), a[i].Name, b[i].Name)
		}
		if a[i].Type != b[i].Type {
			return fmt.Sprintf("the %s %s has the type %s there and %s here",
				strings.TrimSuffix(what, "s"), a[i].Name, a[i].Type, b[i].Type)
		}
		if !slices.Equal(a[i].Shape, b[i].Shape) {
			return fmt.Sprintf("the %s %s has the shape %s there and %s here",
				strings.TrimSuffix(what, "s"), a[i].Name, ints(a[i].Shape), ints(b[i].Shape))
		}
	}
	return ""
}

// tensorInfos turns what the check container read out of the model file into the shape of a source.
func tensorInfos(list []TensorInfo) []TensorInfo {
	out := make([]TensorInfo, 0, len(list))
	for _, t := range list {
		out = append(out, TensorInfo{Name: t.Name, Shape: t.Shape, Type: normalType(t.Type)})
	}
	return out
}

// tritonInfos turns the metadata of the device into the shape of a source.
func tritonInfos(list []tritonInfo) []TensorInfo {
	out := make([]TensorInfo, 0, len(list))
	for _, t := range list {
		out = append(out, TensorInfo{Name: t.Name, Shape: t.Shape, Type: normalType(t.Datatype)})
	}
	return out
}

// platformIO reads the inputs and outputs out of a model record of the platform (R3.4). A record without
// tensor metadata (nothing written by the build) names nothing.
func platformIO(m modelMetaFull) SourceIO {
	out := SourceIO{}
	if m.InputName != "" {
		out.Inputs = []TensorInfo{{Name: m.InputName, Shape: m.InputShape, Type: normalType(m.InputDatatype)}}
	}
	if m.OutputName != "" {
		out.Outputs = []TensorInfo{{Name: m.OutputName, Shape: m.OutputShape, Type: normalType(m.OutputDatatype)}}
	}
	return out
}

// normalType writes a datatype of the three sources alike: the platform's enum (float32, int64), Triton's
// protocol (FP32, INT64) and onnxruntime's (tensor(float)) all become the protocol's spelling, so that the
// comparison sees a difference in the model and not one in the spelling.
func normalType(t string) string {
	s := strings.ToLower(strings.TrimSpace(t))
	s = strings.TrimPrefix(s, "tensor(")
	s = strings.TrimSuffix(s, ")")
	switch s {
	case "fp32", "float", "float32":
		return "FP32"
	case "fp16", "float16":
		return "FP16"
	case "fp64", "double", "float64":
		return "FP64"
	case "int8", "int16", "int32", "int64", "uint8", "uint16", "uint32", "uint64":
		return strings.ToUpper(s)
	case "bool":
		return "BOOL"
	case "string", "bytes":
		return "STRING"
	case "":
		return ""
	default:
		return strings.ToUpper(s)
	}
}

// --- the parts of the platform the check reads ---

// deploymentRec is the part of a container deployment the check reads
// (schemas/container_deployment.py).
type deploymentRec struct {
	ID           json.Number `json:"id"`
	Name         string      `json:"name"`
	Status       string      `json:"status"`
	EdgeDeviceID json.Number `json:"edge_device_id"`
	PortBindings []portBind  `json:"port_bindings"`
	Image        imageRecord `json:"container_image"`
}

type portBind struct {
	HostPort      string `json:"host_port"`
	ContainerPort string `json:"container_port"`
	Protocol      string `json:"protocol"`
}

// imageRecord is the part of a container image a deployment embeds; model_id names the model the image
// was built from (schemas/container_image.py).
type imageRecord struct {
	ID         json.Number `json:"id"`
	Repository string      `json:"repository"`
	Tag        string      `json:"tag"`
	ModelID    json.Number `json:"model_id"`
}

// ref names the image as <repository>:<tag>.
func (i imageRecord) ref() string {
	if i.Repository == "" && i.Tag == "" {
		return ""
	}
	return i.Repository + ":" + i.Tag
}

// modelID reads the model the image was built from; ok is false when the image names none.
func (i imageRecord) modelID() (int64, bool) {
	if i.ModelID == "" {
		return 0, false
	}
	n, err := i.ModelID.Int64()
	return n, err == nil && n > 0
}

// deviceID is the device the deployment runs on.
func (d deploymentRec) deviceID() string { return d.EdgeDeviceID.String() }

// httpPort is the host port of the deployment's Triton HTTP port on the device (R3.3).
func (d deploymentRec) httpPort() (int, bool) {
	for _, b := range d.PortBindings {
		if strings.TrimSpace(b.ContainerPort) != "8000" {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(b.HostPort))
		if err != nil {
			return 0, false
		}
		return n, true
	}
	return 0, false
}

// edgeDeviceRec is the part of an edge device the check reads.
type edgeDeviceRec struct {
	ID   json.Number `json:"id"`
	Name string      `json:"name"`
	Tags []string    `json:"tags"`
}

// deploymentOf reads one deployment with the user's token (a reading call).
func (s *Service) deploymentOf(ctx context.Context, chatID string, id int64) (deploymentRec, error) {
	var out deploymentRec
	req := platform.Request{Method: http.MethodGet, Path: "/container-deployments/" + strconv.FormatInt(id, 10)}
	res, stop, err := platform.ReadJSON(ctx, s.doer(chatID), req, &out)
	s.log(ctx, chatID, "GET "+req.Path, callResult(res, err))
	if stop {
		return out, callError("GET "+req.Path, res, err)
	}
	return out, nil
}

// edgeDevice reads the device of a deployment (its name and tags); nil when the platform does not know
// it.
func (s *Service) edgeDevice(ctx context.Context, chatID string, dep deploymentRec) (*edgeDeviceRec, error) {
	var devices []edgeDeviceRec
	req := platform.Request{Method: http.MethodGet, Path: "/edge-devices"}
	res, stop, err := platform.ReadJSON(ctx, s.doer(chatID), req, &devices)
	s.log(ctx, chatID, "GET "+req.Path, callResult(res, err))
	if stop {
		return nil, callError("GET "+req.Path, res, err)
	}
	want := dep.deviceID()
	for i := range devices {
		if devices[i].ID.String() == want {
			return &devices[i], nil
		}
	}
	return nil, nil
}

// modelMetaFull is a model record with the tensor metadata of the platform (R3.4); assess.modelMeta (R1)
// reads only id, name and format.
type modelMetaFull struct {
	modelMeta
	InputName      string `json:"input_name"`
	InputShape     []int  `json:"input_shape"`
	InputDatatype  string `json:"input_datatype"`
	OutputName     string `json:"output_name"`
	OutputShape    []int  `json:"output_shape"`
	OutputDatatype string `json:"output_datatype"`
}

// modelInfoFull reads a model record with its tensor metadata (a reading call with the user's token).
func (s *Service) modelInfoFull(ctx context.Context, chatID string, id int64) (modelMetaFull, error) {
	var out modelMetaFull
	req := platform.Request{Method: http.MethodGet, Path: "/models/" + strconv.FormatInt(id, 10)}
	res, stop, err := platform.ReadJSON(ctx, s.doer(chatID), req, &out)
	s.log(ctx, chatID, "GET "+req.Path, callResult(res, err))
	if stop {
		return out, callError("GET "+req.Path, res, err)
	}
	return out, nil
}

// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"agw/internal/delegation"
	"agw/internal/platform"
	"agw/internal/store"
)

// #237: in an experiment chat inference images use the runtime of the setup only.

const (
	expTemplatesTriton = `{"id":2,"name":"triton","git_url":"https://github.com/hsos-ai-lab/agri-gaia-inference-container-template","git_ref":"triton"}`
	expTemplatesOVMS   = `{"id":3,"name":"ovms","git_url":"https://github.com/pschroee/agri-gaia-inference-container-template","git_ref":"ovms-onnx"}`
	expTemplatesOther  = `{"id":4,"name":"own","git_url":"https://example.org/own-template","git_ref":"main"}`
	expOpenDelegation  = `{"rules":[],"enforce":false,"confirm":"none"}`
)

// expRuntimeFake is a fake platform with a template list, one edge device tagged intel-cpu and a build and
// an import that succeed. templatesStatus and templates can be changed by a test.
type expRuntimeFake struct {
	mu             sync.Mutex
	calls          []string
	templates      string
	templatesState int
}

func (f *expRuntimeFake) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *expRuntimeFake) setTemplates(list string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.templates = list
}

func (f *expRuntimeFake) posts() []string {
	var out []string
	for _, c := range f.seen() {
		if strings.HasPrefix(c, "POST ") {
			out = append(out, c)
		}
	}
	return out
}

func withExpRuntimePlatform(t *testing.T, e *env) *expRuntimeFake {
	t.Helper()
	f := &expRuntimeFake{templates: "[" + expTemplatesTriton + "," + expTemplatesOVMS + "," + expTemplatesOther + "]", templatesState: http.StatusOK}
	device := edgeDeviceStateJSON("sim-edge-01", "amd64", []string{"simulated", platform.IntelCPUTag},
		time.Now().UTC().Add(-time.Minute).Format(edgeHeartbeatLayout), true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			fmt.Fprintf(w, `{"access_token":%q,"expires_in":3600}`, edgeJWT("agent-test"))
			return
		}
		f.mu.Lock()
		f.calls = append(f.calls, r.Method+" "+r.URL.Path)
		list, status := f.templates, f.templatesState
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/inference-container-templates":
			w.WriteHeader(status)
			fmt.Fprint(w, list)
		case r.Method == http.MethodGet && r.URL.Path == "/edge-devices":
			fmt.Fprint(w, "["+device+"]")
		case r.Method == http.MethodGet && r.URL.Path == "/edge-devices/1":
			fmt.Fprint(w, device)
		case r.Method == http.MethodPost && r.URL.Path == "/container-images/build":
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":7}`)
		case r.Method == http.MethodPost && r.URL.Path == "/inference-container-templates/create/git":
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":8}`)
		default:
			fmt.Fprint(w, `{}`)
		}
	}))
	t.Cleanup(srv.Close)
	c, err := platform.New(platform.Config{APIURL: srv.URL, TokenURL: srv.URL + "/token", User: "u", Password: "p"})
	if err != nil {
		t.Fatal(err)
	}
	e.m.opt.Platform = c
	return f
}

// expRuntimeChat creates an experiment chat (or a product chat) that no delegation holds back.
func expRuntimeChat(t *testing.T, e *env, experiment bool) (string, string) {
	t.Helper()
	nc := NewChat{Delegation: json.RawMessage(expOpenDelegation), Owner: "sub-1", OwnerName: experimentUser}
	if experiment {
		allowExperiment(e)
		nc.Experiment = experimentProfile("none")
	}
	c, err := e.m.Create(context.Background(), nc)
	if err != nil {
		t.Fatal(err)
	}
	return c.ID, e.m.live[c.ID].slot.ID
}

func expBuildCall(templateID int) platform.Request {
	return platform.Request{Method: http.MethodPost, Path: "/container-images/build",
		Body: json.RawMessage(fmt.Sprintf(`{"container_template_id":%d,"model_id":3,"repository":"test/x","tag":"v1","architecture":"amd64"}`, templateID))}
}

func expImportCall(url, ref string) platform.Request {
	return platform.Request{Method: http.MethodPost, Path: "/inference-container-templates/create/git",
		Form: map[string][]string{"git_url": {url}, "git_ref": {ref}}}
}

// expCallApproving makes a call and approves what asks for an approval, so that a test needs not know whether a
// build asks.
func expCallApproving(t *testing.T, e *env, chatID, slot, via string, req platform.Request) platform.Result {
	t.Helper()
	done := make(chan platform.Result, 1)
	go func() {
		r, _ := e.m.PlatformCall(store.WithSession(context.Background(), "main"), chatID, slot, via, req)
		done <- r
	}()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case r := <-done:
			return r
		case <-deadline:
			t.Fatal("the call did not return")
		case <-time.After(20 * time.Millisecond):
			if aps, _ := e.st.ListApprovals(context.Background(), store.ApprovalPending, chatID); len(aps) > 0 {
				_, _ = e.m.Decide(context.Background(), aps[0].ID, true)
			}
		}
	}
}

const (
	expTritonURL = "https://github.com/hsos-ai-lab/agri-gaia-inference-container-template"
	expOVMSURL   = "https://github.com/pschroee/agri-gaia-inference-container-template"
)

// A product chat may still build with the OpenVINO template (#230).
func TestExpRuntimeGuardProductChatUntouched(t *testing.T) {
	e := setup(t)
	f := withExpRuntimePlatform(t, e)
	id, slot := expRuntimeChat(t, e, false)
	if r := expCallApproving(t, e, id, slot, "cli", expBuildCall(3)); r.Status != "ok" {
		t.Fatalf("product chat build with the OVMS template: %+v", r)
	}
	if r := expCallApproving(t, e, id, slot, "cli", expImportCall(expOVMSURL, "ovms-onnx")); r.Status != "ok" {
		t.Fatalf("product chat import of the OVMS template: %+v", r)
	}
	if len(f.posts()) != 2 {
		t.Fatalf("posts: %v", f.posts())
	}
}

// The build from the template of another runtime is refused with the wording of the plan; nothing is posted.
func TestExpRuntimeGuardRefusesOVMSBuild(t *testing.T) {
	t.Cleanup(func() { _ = platform.SetDefaultRuntime(platform.RuntimeTriton) })
	e := setup(t)
	f := withExpRuntimePlatform(t, e)
	id, slot := expRuntimeChat(t, e, true)
	r := expCallApproving(t, e, id, slot, "cli", expBuildCall(3))
	want := platform.RefusedPrefix + fmt.Sprintf(msgExpRuntimeBuild, "triton", "3", "ovms", "triton")
	if r.Status != "denied" || r.Message != want || r.Decision != platform.DecisionDeniedFixed || r.Reason != "guard" {
		t.Fatalf("OVMS template in an experiment chat: %+v\nwant %q", r, want)
	}
	if len(f.posts()) != 0 {
		t.Fatalf("a refused build reached the platform: %v", f.posts())
	}
	if aps, _ := e.st.ListApprovals(context.Background(), "", id); len(aps) != 0 {
		t.Fatalf("a refused build asked the user: %+v", aps)
	}
}

// The template of the setup's runtime and a template of no official source go through.
func TestExpRuntimeGuardAllowsDefaultAndOther(t *testing.T) {
	e := setup(t)
	f := withExpRuntimePlatform(t, e)
	id, slot := expRuntimeChat(t, e, true)
	for _, tid := range []int{2, 4} {
		if r := expCallApproving(t, e, id, slot, "cli", expBuildCall(tid)); r.Status != "ok" {
			t.Fatalf("template %d: %+v", tid, r)
		}
	}
	if len(f.posts()) != 2 {
		t.Fatalf("posts: %v", f.posts())
	}
	if r := expCallApproving(t, e, id, slot, "cli", expImportCall(expTritonURL, "triton")); r.Status != "ok" {
		t.Fatalf("import of the Triton template: %+v", r)
	}
}

// The import of the OpenVINO template is refused by its repository, whatever the ref (raw REST import of another
// branch of the fork included).
func TestExpRuntimeGuardRefusesOVMSImport(t *testing.T) {
	e := setup(t)
	f := withExpRuntimePlatform(t, e)
	id, slot := expRuntimeChat(t, e, true)
	want := platform.RefusedPrefix + fmt.Sprintf(msgExpRuntimeImport, "triton", "ovms")
	for _, ref := range []string{"ovms-onnx", "main", ""} {
		r := expCallApproving(t, e, id, slot, "api", expImportCall(expOVMSURL, ref))
		if r.Status != "denied" || r.Message != want {
			t.Fatalf("import with ref %q: %+v", ref, r)
		}
	}
	if len(f.posts()) != 0 {
		t.Fatalf("a refused import reached the platform: %v", f.posts())
	}
}

// The setting decides: with AGW_INFERENCE_RUNTIME=ovms the Triton template is the refused one.
func TestExpRuntimeGuardFollowsSetting(t *testing.T) {
	t.Cleanup(func() { _ = platform.SetDefaultRuntime(platform.RuntimeTriton) })
	e := setup(t)
	f := withExpRuntimePlatform(t, e)
	id, slot := expRuntimeChat(t, e, true)
	if err := platform.SetDefaultRuntime(platform.RuntimeOVMS); err != nil {
		t.Fatal(err)
	}
	r := expCallApproving(t, e, id, slot, "cli", expBuildCall(2))
	want := platform.RefusedPrefix + fmt.Sprintf(msgExpRuntimeBuild, "ovms", "2", "triton", "ovms")
	if r.Status != "denied" || r.Message != want {
		t.Fatalf("Triton template with the ovms setting: %+v", r)
	}
	if r := expCallApproving(t, e, id, slot, "cli", expBuildCall(3)); r.Status != "ok" {
		t.Fatalf("OVMS template with the ovms setting: %+v", r)
	}
	if len(f.posts()) != 1 {
		t.Fatalf("posts: %v", f.posts())
	}
}

// An unreadable template list or profile does not lift the condition: the call fails closed, nothing is posted.
func TestExpRuntimeGuardFailsClosed(t *testing.T) {
	e := setup(t)
	f := withExpRuntimePlatform(t, e)
	id, slot := expRuntimeChat(t, e, true)
	f.mu.Lock()
	f.templatesState = http.StatusInternalServerError
	f.mu.Unlock()
	r, err := e.m.PlatformCall(context.Background(), id, slot, "cli", expBuildCall(2))
	if err == nil {
		t.Fatalf("an unreadable template list let the build go on: %+v", r)
	}
	if len(f.posts()) != 0 {
		t.Fatalf("posts: %v", f.posts())
	}
	// The profile: a read that fails (here a cancelled context, with a cold cache) is an error, not a product chat.
	e.m.invalidateChatGate(id)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a := delegation.Access{Also: []delegation.Access{{Resource: delegation.ContainerTemplate, ID: "3"}}}
	if refusal, err := expRuntimeGuard(ctx, e.m, id, expBuildCall(3), a); err == nil {
		t.Fatalf("unreadable profile: refusal %q, no error", refusal)
	}
}

// A raw REST build (no tool, no plan) is held too.
func TestExpRuntimeGuardRESTRaw(t *testing.T) {
	e := setup(t)
	f := withExpRuntimePlatform(t, e)
	id, slot := expRuntimeChat(t, e, true)
	r := expCallApproving(t, e, id, slot, "api", expBuildCall(3))
	if r.Status != "denied" || !strings.Contains(r.Message, "leave out runtime") {
		t.Fatalf("raw REST build: %+v", r)
	}
	if len(f.posts()) != 0 {
		t.Fatalf("posts: %v", f.posts())
	}
}

// A step of an approved plan is held at its execution too; the plan does not complete.
func TestExpRuntimeGuardPlanStep(t *testing.T) {
	e := setup(t)
	f := withExpRuntimePlatform(t, e)
	ctx := store.WithSession(context.Background(), "main")
	id, slot := expRuntimeChat(t, e, true)
	body := `{"title":"Build","steps":[{"method":"POST","path":"/container-images/build","body":{"container_template_id":3,"model_id":3,"repository":"test/x","tag":"v1","architecture":"amd64"}}]}`
	approvedPlan(t, e, ctx, id, body)
	r := expCallApproving(t, e, id, slot, "cli", expBuildCall(3))
	if r.Status != "denied" {
		t.Fatalf("approved step with the OVMS template: %+v", r)
	}
	if len(f.posts()) != 0 {
		t.Fatalf("posts: %v", f.posts())
	}
	if p := onlyPlan(t, e, ctx, id); p.State == "completed" {
		t.Fatalf("the plan completed: %s", p.State)
	}
}

// proposeInExperiment proposes a plan in an experiment chat and returns the answer; an accepted proposal asks the
// user for the plan, which is approved.
func proposeInExperiment(t *testing.T, e *env, body string) platform.Result {
	t.Helper()
	id, slot := expRuntimeChat(t, e, true)
	req, err := platform.Normalize(platform.Request{Method: "POST", Path: platform.PlanPath, Body: json.RawMessage(body)})
	if err != nil {
		t.Fatal(err)
	}
	return expCallApproving(t, e, id, slot, "cli", req)
}

// A proposal that names another runtime, imports its template or builds from a literally named template of
// another runtime is refused with every problem; the setup's runtime, with or without the argument, is accepted.
func TestProposePlanExperimentRuntime(t *testing.T) {
	e := setup(t)
	withExpRuntimePlatform(t, e)
	cases := map[string]string{
		"runtime ovms":       `{"title":"T","steps":[{"tool":"import_template","args":{"name":"tpl","runtime":"ovms"}}]}`,
		"literal OVMS build": `{"title":"T","steps":[{"method":"POST","path":"/container-images/build","body":{"container_template_id":3,"model_id":3,"repository":"test/x","tag":"v1","architecture":"amd64"}}]}`,
	}
	for name, body := range cases {
		res := proposeInExperiment(t, e, body)
		if res.Status != "error" || !strings.Contains(res.Message, "experiment run: ") || !strings.Contains(res.Message, "leave out runtime") {
			t.Errorf("%s: %+v", name, res)
		}
	}
	for name, body := range map[string]string{
		"runtime triton": `{"title":"T","steps":[{"tool":"import_template","args":{"name":"tpl","runtime":"triton"}}]}`,
		"no runtime":     `{"title":"T","steps":[{"tool":"import_template","args":{"name":"tpl"}}]}`,
	} {
		res := proposeInExperiment(t, e, body)
		if strings.Contains(res.Message, "experiment run: ") {
			t.Errorf("%s was refused for its runtime: %+v", name, res)
		}
	}
}

// expRuntimeStepProblems on its own: the problems carry the step number, a product chat has none.
func TestExpRuntimeStepProblemsNumbered(t *testing.T) {
	e := setup(t)
	withExpRuntimePlatform(t, e)
	id, _ := expRuntimeChat(t, e, true)
	steps := []store.PlanStep{
		{Idx: 1, Method: http.MethodPost, Path: "/datasets"},
		{Idx: 2, Method: http.MethodPost, Path: "/container-images/build", Args: json.RawMessage(`{"runtime":"ovms"}`)},
	}
	err := e.m.expRuntimeStepProblems(context.Background(), id, steps)
	if err == nil || !strings.HasPrefix(err.Error(), "step 2: ") {
		t.Fatalf("problems: %v", err)
	}
	pid, _ := expRuntimeChat(t, e, false)
	if err := e.m.expRuntimeStepProblems(context.Background(), pid, steps); err != nil {
		t.Fatalf("product chat: %v", err)
	}
}

// The tag intel-cpu is cut from the answer of the device reads, wherever "tags" stands; the rest is kept.
func TestExpHideDeviceTags(t *testing.T) {
	digest := func(req platform.Request, in string) string {
		t.Helper()
		if req.Digest == nil {
			return in
		}
		out, err := req.Digest([]byte(in))
		if err != nil {
			t.Fatal(err)
		}
		return string(out)
	}
	list := platform.Request{Method: http.MethodGet, Path: "/edge-devices"}
	got := digest(expHideDeviceTags(list), `[{"id":1,"name":"a","tags":["simulated","intel-cpu"]},{"id":2,"tags":["intel-cpu"]},{"id":3,"tags":null}]`)
	if strings.Contains(got, "intel-cpu") || !strings.Contains(got, `"simulated"`) || !strings.Contains(got, `"name":"a"`) {
		t.Fatalf("array: %s", got)
	}
	one := platform.Request{Method: http.MethodGet, Path: "/edge-devices/1"}
	if got := digest(expHideDeviceTags(one), `{"id":1,"tags":["intel-cpu","x"]}`); strings.Contains(got, "intel-cpu") || !strings.Contains(got, `"x"`) {
		t.Fatalf("object: %s", got)
	}
	if got := digest(expHideDeviceTags(list), `not json intel-cpu`); got != "not json intel-cpu" {
		t.Fatalf("no JSON: %s", got)
	}
	// a digest the request already had runs first
	chained := list
	chained.Digest = func(b []byte) ([]byte, error) { return []byte(strings.ReplaceAll(string(b), "XX", `"intel-cpu"`)), nil }
	if got := digest(expHideDeviceTags(chained), `[{"tags":[XX,"y"]}]`); strings.Contains(got, "intel-cpu") || !strings.Contains(got, `"y"`) {
		t.Fatalf("chained: %s", got)
	}
	// other calls stay as they are
	for _, r := range []platform.Request{
		{Method: http.MethodPost, Path: "/edge-devices"},
		{Method: http.MethodGet, Path: "/edge-devices/1/containers"},
		{Method: http.MethodGet, Path: "/datasets"},
	} {
		if expHideDeviceTags(r).Digest != nil {
			t.Fatalf("%s %s got a digest", r.Method, r.Path)
		}
	}
}

// Through PlatformCall the experiment chat does not see the tag; the product chat does.
func TestExpHideDeviceTagsThroughPlatformCall(t *testing.T) {
	e := setup(t)
	withExpRuntimePlatform(t, e)
	expID, expSlot := expRuntimeChat(t, e, true)
	prodID, prodSlot := expRuntimeChat(t, e, false)
	for _, path := range []string{"/edge-devices", "/edge-devices/1"} {
		req := platform.Request{Method: http.MethodGet, Path: path}
		r, err := e.m.PlatformCall(context.Background(), expID, expSlot, "api", req)
		if err != nil || r.Status != "ok" || strings.Contains(r.Body, platform.IntelCPUTag) || !strings.Contains(r.Body, "simulated") {
			t.Fatalf("experiment chat %s: %+v %v", path, r, err)
		}
		r, err = e.m.PlatformCall(context.Background(), prodID, prodSlot, "api", req)
		if err != nil || r.Status != "ok" || !strings.Contains(r.Body, platform.IntelCPUTag) {
			t.Fatalf("product chat %s: %+v %v", path, r, err)
		}
	}
}

// The capability edge in an experiment chat names neither the tag nor ovms.
func TestEdgeCapabilityExperimentNoIntelCPU(t *testing.T) {
	e := setup(t)
	withExpRuntimePlatform(t, e)
	id, _ := expRuntimeChat(t, e, true)
	cap := byName(e.m.Capabilities(context.Background(), id))["edge"]
	if !cap.Available || strings.Contains(cap.Detail, platform.IntelCPUTag) || strings.Contains(cap.Detail, platform.RuntimeOVMS) ||
		!strings.Contains(cap.Detail, "1 device online: sim-edge-01 (simulated, amd64); inference runtime: triton;") {
		t.Fatalf("experiment chat: %+v", cap)
	}
}

// A product chat still gets the hint about the tag (#230).
func TestEdgeCapabilityProductUnchanged(t *testing.T) {
	e := setup(t)
	withExpRuntimePlatform(t, e)
	id, _ := expRuntimeChat(t, e, false)
	cap := byName(e.m.Capabilities(context.Background(), id))["edge"]
	if !strings.Contains(cap.Detail, "sim-edge-01 (simulated, amd64, intel-cpu)") ||
		!strings.Contains(cap.Detail, "inference runtime: triton by default, ovms for devices tagged intel-cpu") {
		t.Fatalf("product chat: %+v", cap)
	}
}

// The summary leaves the tag out on request.
func TestEdgeSummaryHidesIntelCPU(t *testing.T) {
	online := []edgeDeviceState{{Name: "sim-edge-01", Arch: "amd64", Tags: []string{"simulated", platform.IntelCPUTag}}}
	if got := edgeDeviceSummary(online, true); got != "1 device online: sim-edge-01 (simulated, amd64)" {
		t.Fatalf("summary = %q", got)
	}
}

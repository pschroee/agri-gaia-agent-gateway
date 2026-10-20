// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package delegation

import (
	"testing"

	"agw/internal/platform"
)

func okResult(body, location string) platform.Result {
	return platform.Result{Status: "ok", HTTPStatus: 200, Body: body, Location: location}
}

// changeSamples: one call per writing route (action other than read), with what Changes must report
// for the main access. Written for issue #167; the guard below keeps the table complete.
var changeSamples = []struct {
	method, path, body string
	resource, action   string
	id                 string
}{
	{"POST", "/datasets", `{"name":"x"}`, Dataset, Create, ""},
	{"POST", "/datasets/import", `{}`, Dataset, Create, ""},
	{"GET", "/datasets/5/download", "", Dataset, Update, "5"},
	{"PATCH", "/datasets/5", `{"name":"y"}`, Dataset, Update, "5"},
	{"PATCH", "/datasets/5/toggle-public", "", Dataset, Update, "5"},
	{"POST", "/datasets/5/annotate", "", Dataset, Update, "5"},
	{"DELETE", "/datasets/5", "", Dataset, Delete, "5"},
	{"POST", "/models", "", Model, Create, ""},
	{"PATCH", "/models/6", `{"name":"m"}`, Model, Update, "6"},
	{"PATCH", "/models/6/toggle-public", "", Model, Update, "6"},
	{"DELETE", "/models/6", "", Model, Delete, "6"},
	{"POST", "/train/config", `{"dataset_id":1}`, Training, Create, ""},
	{"PUT", "/train/config", `{"container_id":7,"dataset_id":1}`, Training, Update, "7"},
	{"GET", "/train/containers/7/model", "", Model, Create, ""},
	{"POST", "/train/containers/7/run", "", Training, Run, "7"},
	{"POST", "/train/containers/7/stop", "", Training, Run, "7"},
	{"POST", "/train/containers/7/clone", `{"train_config":{}}`, Training, Create, ""},
	{"POST", platform.SearchPrefix + "/s0123456789ab/trials", `{"params":{}}`, Search, Update, ""},
	{"POST", platform.SearchPrefix + "/s0123456789ab/end", `{}`, Search, Update, ""},
	{"PATCH", "/train/containers/7", `{}`, Training, Update, "7"},
	{"PATCH", "/train/containers/7/score", `{"score":1}`, Training, Update, "7"},
	{"DELETE", "/train/containers/7", "", Training, Delete, "7"},
	{"DELETE", "/tasks/abc", "", Task, Delete, "abc"},
	{"POST", "/edge-devices", `{}`, EdgeDevice, Create, ""},
	{"GET", "/edge-devices/3", "", EdgeDevice, Update, "3"},
	{"DELETE", "/edge-devices/3", "", EdgeDevice, Delete, "3"},
	{"DELETE", "/container-images/me/img:1", "", ContainerImage, Delete, "me/img:1"},
	{"POST", "/container-images/build", `{"repository":"me/img","tag":"1","model_id":6,"container_template_id":2}`, ContainerImage, Create, "me/img:1"},
	{"POST", "/inference-container-templates/create/git", `{}`, ContainerTemplate, Create, ""},
	{"POST", "/container-deployments", `{"edge_device_id":1,"container_image_id":2,"port_bindings":{},"name":"d"}`, ContainerDeployment, Create, ""},
	{"PUT", "/container-deployments/4/deploy", "", ContainerDeployment, Run, "4"},
	{"PUT", "/container-deployments/4/undeploy", "", ContainerDeployment, Run, "4"},
	{"DELETE", "/container-deployments/4", "", ContainerDeployment, Delete, "4"},
	{"POST", platform.NotebookPrefix + "/runs", `{}`, Notebook, Run, ""},
	{"POST", platform.NotebookPrefix + "/stop", `{}`, Notebook, Update, ""},
	{"POST", platform.AnnotationPrefix + "/functions", `{"model_id":6}`, AnnotationFunction, Create, ""},
	{"POST", platform.AnnotationPrefix + "/tasks", `{"dataset_id":5}`, Dataset, Update, "5"},
	{"POST", platform.AnnotationPrefix + "/runs", `{"dataset_id":5}`, Dataset, Run, "5"},
	{"POST", platform.AnnotationPrefix + "/checks", `{"dataset_id":5}`, Dataset, Run, "5"},
	{"POST", platform.AnnotationPrefix + "/fills", `{"dataset_id":5}`, Dataset, Run, "5"},
	{"POST", platform.AnnotationPrefix + "/rollbacks", `{"source":"auto"}`, Dataset, Update, ""},
	{"POST", platform.AnnotationPrefix + "/datasets/5/sync", "", Dataset, Update, "5"},
	{"POST", platform.AssessPrefix + "/splits", `{"dataset_id":5}`, Dataset, Create, ""},
	{"POST", platform.AssessPrefix + "/checks", `{"model_id":6}`, Assessment, Create, ""},
}

// writingRouteIndex returns the index of the route a call is classified by (the first match).
func routeIndex(method, path string) int {
	for i, rt := range routes {
		if rt.method == method && rt.re.MatchString(path) {
			return i
		}
	}
	return -1
}

func TestChangesCoverEveryWritingRoute(t *testing.T) {
	covered := map[int]bool{}
	for _, s := range changeSamples {
		req := platform.Request{Method: s.method, Path: s.path, Body: []byte(s.body)}
		if i := routeIndex(s.method, s.path); i >= 0 {
			covered[i] = true
		} else {
			t.Errorf("%s %s: sample matches no route", s.method, s.path)
		}
		a := Classify(req)
		if a.Effect == EffectRead {
			t.Errorf("%s %s: sample is a read", s.method, s.path)
		}
		var found *Change
		for _, c := range Changes(a, req, platform.Result{Status: "ok", HTTPStatus: 200}) {
			if c.Resource == s.resource && c.Action == s.action {
				c := c
				found = &c
				break
			}
		}
		if found == nil {
			t.Errorf("%s %s: no change %s %s in %+v", s.method, s.path, s.action, s.resource, Changes(a, req, platform.Result{Status: "ok", HTTPStatus: 200}))
			continue
		}
		if found.ID != s.id {
			t.Errorf("%s %s: id = %q, want %q", s.method, s.path, found.ID, s.id)
		}
	}
	// Guard: every route that writes has a sample, so that a new route cannot be forgotten.
	for i, rt := range routes {
		if rt.action != Read && rt.effect != EffectRead && !covered[i] {
			t.Errorf("route without a change sample: %s %s", rt.method, rt.re.String())
		}
	}
}

func TestChangesAlsoAndCreated(t *testing.T) {
	// Split: the source dataset is changed, the new one created.
	req := platform.Request{Method: "POST", Path: platform.AssessPrefix + "/splits", Body: []byte(`{"dataset_id":5}`)}
	got := Changes(Classify(req), req, okResult(`{"id":9}`, ""))
	has := func(c Change) bool {
		for _, x := range got {
			if x == c {
				return true
			}
		}
		return false
	}
	if !has(Change{Resource: Dataset, Action: Create, ID: "9"}) {
		t.Errorf("split: created dataset missing: %+v", got)
	}
	// A model created from a training container.
	req = platform.Request{Method: "GET", Path: "/train/containers/7/model"}
	got = Changes(Classify(req), req, okResult(`{"id":12}`, ""))
	if !has(Change{Resource: Model, Action: Create, ID: "12"}) {
		t.Errorf("model from training: %+v", got)
	}
	// Image build: the reference from the body.
	req = platform.Request{Method: "POST", Path: "/container-images/build", Body: []byte(`{"repository":"me/img","tag":"1","model_id":6,"container_template_id":2}`)}
	got = Changes(Classify(req), req, okResult(`{}`, ""))
	if !has(Change{Resource: ContainerImage, Action: Create, ID: "me/img:1"}) {
		t.Errorf("image build: %+v", got)
	}
	// A read changes nothing.
	req = platform.Request{Method: "GET", Path: "/datasets"}
	if got := Changes(Classify(req), req, okResult(`[]`, "")); len(got) != 0 {
		t.Errorf("read: %+v", got)
	}
	// An unknown identifier is left out; duplicates are merged.
	req = platform.Request{Method: "POST", Path: "/datasets", Body: []byte(`{"name":"x"}`)}
	got = Changes(Classify(req), req, okResult(`{"id":3}`, ""))
	if len(got) != 1 || got[0] != (Change{Resource: Dataset, Action: Create, ID: "3"}) {
		t.Errorf("create dataset: %+v", got)
	}
}

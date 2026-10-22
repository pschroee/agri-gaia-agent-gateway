// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// writesWithoutBody: the writing operations of testdata/openapi-2026-10-21.json that take no body (the backend
// reads none). A new write that is neither here nor carries a body fails TestEveryWriteHasBody.
var writesWithoutBody = []string{
	"DELETE /applications/{application_id}",
	"DELETE /container-deployments/{container_deployment_id}",
	"PUT /container-deployments/{container_deployment_id}/deploy",
	"PUT /container-deployments/{container_deployment_id}/undeploy",
	"POST /container-images/download/{repository}/{tag}/{arch}",
	"DELETE /container-images/{repository_first}/{repository_second}:{image_tag}",
	"POST /cvat/auth/login",
	"POST /cvat/user/create",
	"POST /datasets/{dataset_id}/annotate",
	"PATCH /datasets/{dataset_id}/toggle-public",
	"DELETE /edge-devices/{edge_device_id}",
	"DELETE /edge-groups/{edge_group_id}",
	"DELETE /inference-container-templates/{template_id}",
	"DELETE /integrated-services/{service_id}",
	"PATCH /integrated-services/{service_id}/toggle-public",
	"DELETE /model-deployments/{model_deployment_id}",
	"PUT /model-deployments/{model_deployment_id}/deploy",
	"PUT /model-deployments/{model_deployment_id}/undeploy",
	"DELETE /models/{model_id}",
	"PATCH /models/{model_id}/toggle-public",
	"DELETE /tasks/{task_id}",
	"DELETE /train/containers/{train_container_id}",
	"POST /train/containers/{train_container_id}/run",
	"PATCH /train/containers/{train_container_id}/score",
	"POST /train/containers/{train_container_id}/stop",
}

func digestOf(t *testing.T, prefix string) string {
	t.Helper()
	raw, err := os.ReadFile("testdata/openapi-2026-10-21.json")
	if err != nil {
		t.Fatal(err)
	}
	d, err := openAPIDigest(raw, prefix)
	if err != nil {
		t.Fatal(err)
	}
	return string(d)
}

func lineOf(digest, start string) string {
	for _, l := range strings.Split(digest, "\n") {
		if strings.HasPrefix(l, start+" ") {
			return l
		}
	}
	return ""
}

func TestDigestBodyFieldsFromSchema(t *testing.T) {
	l := lineOf(digestOf(t, ""), "POST /container-images/build")
	want := " · body: container_template_id*, repository*, tag*, model_id*, architecture, edge_device_id"
	if !strings.HasSuffix(l, want) {
		t.Errorf("line %q lacks %q", l, want)
	}
}

func TestDigestBodyFromNotes(t *testing.T) {
	d := digestOf(t, "")
	if l := lineOf(d, "POST /train/config"); !strings.Contains(l, " · body: provider*, architecture*, category*, dataset_id*, train_config*, export_config · gateway: train_config values are ") {
		t.Errorf("POST /train/config: %q", l)
	}
	if l := lineOf(d, "PATCH /datasets/{dataset_id}"); !strings.Contains(l, " · body: name* · gateway: ") {
		t.Errorf("PATCH /datasets: %q", l)
	}
	if l := lineOf(d, "GET /train/containers/{train_container_id}"); !strings.Contains(l, " · gateway: answers HTTP 500") || strings.Contains(l, "body:") {
		t.Errorf("GET /train/containers/{id}: %q", l)
	}
	if l := lineOf(d, "PATCH /models/{model_id}"); !strings.Contains(l, " · body: name*, ") || !strings.Contains(l, "gateway: sets every field") {
		t.Errorf("PATCH /models: %q", l)
	}
	t.Logf("digest: %d bytes", len(d))
}

// Every writing operation names its body, or is known to take none.
func TestEveryWriteHasBody(t *testing.T) {
	none := map[string]bool{}
	for _, k := range writesWithoutBody {
		none[k] = true
	}
	for _, l := range strings.Split(digestOf(t, ""), "\n") {
		f := strings.Fields(l)
		if len(f) < 2 || strings.HasPrefix(l, "(") || PathBlocked(f[1]) || f[0] == "GET" {
			continue
		}
		key := f[0] + " " + f[1]
		hasBody := strings.Contains(l, " · body: ")
		switch {
		case !hasBody && !none[key]:
			t.Errorf("%s has no body and is not in writesWithoutBody", key)
		case hasBody && none[key]:
			t.Errorf("%s is listed as having no body but has one", key)
		}
	}
}

func openAPIOperations(t *testing.T) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile("testdata/openapi-2026-10-21.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	ops := map[string]bool{}
	for p, m := range doc.Paths {
		for method := range m {
			ops[strings.ToUpper(method)+" "+p] = true
		}
	}
	return ops
}

func TestAPIPathNotesInOpenAPI(t *testing.T) {
	ops := openAPIOperations(t)
	for key := range apiPathNotes {
		if !ops[key] {
			t.Errorf("note %q is no operation of the description", key)
		}
	}
}

// A note with a tool names the same fields as the tool's body, with the same required marks.
func TestAPIPathNotesMatchTools(t *testing.T) {
	for key, n := range apiPathNotes {
		if n.Tool == "" {
			continue
		}
		tool, ok := Lookup(n.Tool)
		if !ok {
			t.Errorf("%s: unknown tool %s", key, n.Tool)
			continue
		}
		if tool.APIRef() != key {
			t.Errorf("%s: tool %s has APIRef %q", key, n.Tool, tool.APIRef())
		}
		args, ok := argsFor[n.Tool]
		if !ok {
			t.Errorf("%s: no sample arguments for %s", key, n.Tool)
			continue
		}
		req, err := tool.Build(json.RawMessage(args))
		if err != nil {
			t.Errorf("%s: %v", key, err)
			continue
		}
		var body map[string]json.RawMessage
		if err := json.Unmarshal(req.Body, &body); err != nil {
			t.Errorf("%s: body of the tool: %v", key, err)
			continue
		}
		fields := noteFields(n)
		if len(fields) == 0 {
			t.Errorf("%s: note names no fields", key)
		}
		for _, f := range fields {
			if f.Required {
				if _, ok := body[f.Name]; !ok {
					t.Errorf("%s: required field %s is not in the body of %s", key, f.Name, n.Tool)
				}
			}
			for _, p := range tool.Params {
				// a field the note names required must be a required parameter, the body carries it renamed or not
				if p.Name == f.Name && p.Required != f.Required {
					t.Errorf("%s: field %s required=%v, parameter required=%v", key, f.Name, f.Required, p.Required)
				}
			}
		}
		for k := range body {
			found := false
			for _, f := range fields {
				if f.Name == k {
					found = true
				}
			}
			if !found {
				t.Errorf("%s: the body of %s carries %s, the note does not name it", key, n.Tool, k)
			}
		}
	}
}

func TestDigestDetailFields(t *testing.T) {
	with := digestOf(t, "/train/config")
	if !strings.Contains(with, "\n    train_config* (object) — ") {
		t.Errorf("detail line for train_config missing:\n%s", with)
	}
	if without := digestOf(t, ""); strings.Contains(without, "\n    train_config*") {
		t.Error("detail lines without a prefix")
	}
}

// The index stays small (H1): the fields add well under a kilobyte per ten operations.
func TestIndexSizeWithBodies(t *testing.T) {
	raw, err := os.ReadFile("testdata/openapi-2026-10-21.json")
	if err != nil {
		t.Fatal(err)
	}
	idx, err := APIIndexFrom(raw)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("api index: %d bytes", len(idx))
	if len(idx) > 10<<10 {
		t.Errorf("api index is %d bytes, expected under 10 KiB", len(idx))
	}
	if strings.Contains(idx, apiIndexTruncMsg) {
		t.Error("index truncated")
	}
}

func TestAPIPathNoteModelFromTraining(t *testing.T) {
	l := lineOf(digestOf(t, ""), "GET /train/containers/{train_container_id}/model")
	if !strings.Contains(l, " · gateway: creates a NEW model on every call") {
		t.Errorf("line %q", l)
	}
}

// The platform answers HTTP 500 to POST /train/config without the key export_config (live run, 22.10.2026): the index line
// says so, gives the export settings and is not cut off by the line limit.
func TestIndexLineTrainConfigNamesExportConfig(t *testing.T) {
	raw, err := os.ReadFile("testdata/openapi-2026-10-21.json")
	if err != nil {
		t.Fatal(err)
	}
	idx, err := APIIndexFrom(raw)
	if err != nil {
		t.Fatal(err)
	}
	l := lineOf(idx, "POST /train/config")
	for _, want := range []string{"Always send the key export_config", "HTTP 500", `"input_shapes":[[1,3,H,W]]`, `"opset_version":18}`} {
		if !strings.Contains(l, want) {
			t.Errorf("POST /train/config lacks %q: %q", want, l)
		}
	}
	if strings.HasSuffix(l, "…") {
		t.Errorf("line is cut off: %q", l)
	}
	t.Logf("api index: %d bytes", len(idx))
}

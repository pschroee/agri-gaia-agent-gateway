// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package annotation

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"reflect"
	"sort"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// zipEntries reads an archive into its file names and contents.
func zipEntries(t *testing.T, raw []byte) (map[string][]byte, []string) {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("the archive is not readable: %v", err)
	}
	names := make([]string, 0, len(zr.File))
	out := map[string][]byte{}
	for _, f := range zr.File {
		if f.UncompressedSize64 > 1<<20 {
			t.Fatalf("%s is unexpectedly large", f.Name)
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, f.Name)
		out[f.Name] = data
	}
	sort.Strings(names)
	return out, names
}

// yamlKeys names the keys of a mapping read from the YAML of a function.
func yamlKeys(t *testing.T, m map[string]any) []string {
	t.Helper()
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// yamlMap reads a nested mapping; a missing or unexpected node fails the test.
func yamlMap(t *testing.T, m map[string]any, keys ...string) map[string]any {
	t.Helper()
	cur := m
	for i, k := range keys {
		v, ok := cur[k]
		if !ok {
			t.Fatalf("yaml: %s missing", strings.Join(keys[:i+1], "."))
		}
		next, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("yaml: %s is not a mapping (%T)", strings.Join(keys[:i+1], "."), v)
		}
		cur = next
	}
	return cur
}

// The archive has the layout the backend expects: one folder named like the archive (it calls nuctl
// with the file name without its extension) with the four files, and the function.yaml carries only
// the agreed fields.
func TestBuildZipLayout(t *testing.T) {
	const name = "agw-agent-test-m5-ab12cd34"
	model := []byte("ONNX-MODEL-BYTES")
	raw, err := BuildZip(name, KindClassification, []string{"class_0", "class_1"}, 64, model, "mnist_efficientnet", 5)
	if err != nil {
		t.Fatal(err)
	}
	entries, names := zipEntries(t, raw)
	want := []string{name + "/config.json", name + "/function.yaml", name + "/main.py", name + "/model.onnx"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("the archive holds %v, want %v", names, want)
	}
	if !bytes.Equal(entries[name+"/model.onnx"], model) {
		t.Error("the model file changed on the way into the archive")
	}
	if !bytes.Equal(entries[name+"/main.py"], classificationTemplate) {
		t.Error("the template of the classification kind is not the file of the repository")
	}

	// function.yaml parses as YAML and carries exactly the agreed fields.
	var y map[string]any
	if err := yaml.Unmarshal(entries[name+"/function.yaml"], &y); err != nil {
		t.Fatalf("function.yaml is not readable: %v", err)
	}
	if got := yamlKeys(t, y); !reflect.DeepEqual(got, []string{"metadata", "spec"}) {
		t.Fatalf("top-level fields: %v", got)
	}
	md := yamlMap(t, y, "metadata")
	if got := yamlKeys(t, md); !reflect.DeepEqual(got, []string{"annotations", "name", "namespace"}) {
		t.Errorf("metadata fields: %v", got)
	}
	if md["name"] != name || md["namespace"] != "cvat" {
		t.Errorf("metadata: %v", md)
	}
	an := yamlMap(t, md, "annotations")
	if got := yamlKeys(t, an); !reflect.DeepEqual(got, []string{"framework", "name", "spec", "type"}) {
		t.Errorf("annotations fields: %v", got)
	}
	if an["type"] != "detector" || an["framework"] != "onnx" {
		t.Errorf("annotations: %v", an)
	}
	if an["name"] != "Agent: mnist_efficientnet (#5)" {
		t.Errorf("annotation name: %v", an["name"])
	}
	var classes []classSpec
	spec, _ := an["spec"].(string)
	if err := json.Unmarshal([]byte(spec), &classes); err != nil {
		t.Fatalf("the classes of the function are not JSON: %q %v", spec, err)
	}
	if !reflect.DeepEqual(classes, []classSpec{{0, "class_0"}, {1, "class_1"}}) {
		t.Errorf("classes: %v", classes)
	}

	sp := yamlMap(t, y, "spec")
	if got := yamlKeys(t, sp); !reflect.DeepEqual(got, []string{"build", "eventTimeout", "handler", "platform", "runtime", "triggers"}) {
		t.Errorf("spec fields: %v", got)
	}
	if sp["runtime"] != "python:3.11" || sp["handler"] != "main:handler" || sp["eventTimeout"] != "30s" {
		t.Errorf("spec: %v", sp)
	}
	build := yamlMap(t, sp, "build")
	if got := yamlKeys(t, build); !reflect.DeepEqual(got, []string{"baseImage", "directives", "image"}) {
		t.Errorf("build fields: %v", got)
	}
	if build["image"] != "cvat."+name || build["baseImage"] != "python:3.11-slim" {
		t.Errorf("build: %v", build)
	}
	post := yamlMap(t, build, "directives")["postCopy"]
	pkgs, ok := post.([]any)
	if !ok || len(pkgs) != 1 || !strings.Contains(pkgs[0].(string), functionPackages) {
		t.Errorf("the packages of the image are not pinned: %v", post)
	}
	tr := yamlMap(t, sp, "triggers")
	if got := yamlKeys(t, tr); !reflect.DeepEqual(got, []string{"myHttpTrigger"}) {
		t.Errorf("triggers: %v", got)
	}
	http := yamlMap(t, tr, "myHttpTrigger")
	if got := yamlKeys(t, http); !reflect.DeepEqual(got, []string{"attributes", "kind", "maxWorkers"}) {
		t.Errorf("trigger fields: %v", got)
	}
	if http["maxWorkers"] != 1 || http["kind"] != "http" {
		t.Errorf("trigger: %v", http)
	}
	if sz := yamlMap(t, http, "attributes")["maxRequestBodySize"]; sz != maxRequestBodySize {
		t.Errorf("maxRequestBodySize: %v (%T)", sz, sz)
	}
	attrs := yamlMap(t, sp, "platform", "attributes")
	if got := yamlKeys(t, attrs); !reflect.DeepEqual(got, []string{"mountMode", "restartPolicy"}) {
		t.Errorf("platform attributes: %v", got)
	}
	if attrs["mountMode"] != "volume" {
		t.Errorf("mountMode: %v", attrs["mountMode"])
	}
	rp := yamlMap(t, attrs, "restartPolicy")
	if rp["name"] != "always" || rp["maximumRetryCount"] != 3 {
		t.Errorf("restartPolicy: %v", rp)
	}

	// config.json is what the template reads at its start.
	var cfg functionConfig
	if err := json.Unmarshal(entries[name+"/config.json"], &cfg); err != nil {
		t.Fatalf("config.json is not readable: %v", err)
	}
	if cfg.Kind != KindClassification || cfg.InputSize != 64 || cfg.TemplateVersion != TemplateVersion ||
		!reflect.DeepEqual(cfg.Labels, []string{"class_0", "class_1"}) {
		t.Errorf("config.json: %+v", cfg)
	}
}

// The detection archive carries its own template; the model name of the title is reduced to the
// characters CVAT may show, and an empty one falls back to "model".
func TestBuildZipDetection(t *testing.T) {
	raw, err := BuildZip("agw-x-m7-00112233", KindDetection, []string{"pig"}, 640, []byte("m"), "Schwein/Pen #2", 7)
	if err != nil {
		t.Fatal(err)
	}
	entries, _ := zipEntries(t, raw)
	if !bytes.Equal(entries["agw-x-m7-00112233/main.py"], detectionTemplate) {
		t.Error("the detection archive does not carry the detection template")
	}
	if got := string(entries["agw-x-m7-00112233/function.yaml"]); !strings.Contains(got, `name: "Agent: SchweinPen 2 (#7)"`) {
		t.Errorf("the title of the function was not reduced: %s", got)
	}
	if got := cvatTitle("", 9); got != "Agent: model (#9)" {
		t.Errorf("without a model name: %q", got)
	}
	// A long model name is shortened.
	if got := cvatTitle(strings.Repeat("a", 100), 1); len(got) > 90 {
		t.Errorf("the title is not shortened: %d characters", len(got))
	}
}

// Everything the agent can influence is checked while the archive is built: the name reaches the
// shell of the backend, the class names reach the YAML, and the size reaches the model run.
func TestBuildZipChecks(t *testing.T) {
	labels := []string{"a"}
	for _, tc := range []struct {
		name          string
		kind          string
		labels        []string
		size          int
		model         []byte
		wantInMessage string
	}{
		{name: "agw-x-m1-00112233", kind: "segmentation", labels: labels, size: 64, model: []byte("m"), wantInMessage: "unknown function kind"},
		{name: "agw-x-m1-00112233; rm -rf /", kind: KindClassification, labels: labels, size: 64, model: []byte("m"), wantInMessage: "not valid"},
		{name: "AGW-Upper", kind: KindClassification, labels: labels, size: 64, model: []byte("m"), wantInMessage: "not valid"},
		{name: "agw-x-m1-00112233", kind: KindClassification, labels: nil, size: 64, model: []byte("m"), wantInMessage: "class names"},
		{name: "agw-x-m1-00112233", kind: KindClassification, labels: []string{`a"b`}, size: 64, model: []byte("m"), wantInMessage: "not allowed"},
		{name: "agw-x-m1-00112233", kind: KindClassification, labels: []string{strings.Repeat("c", 65)}, size: 64, model: []byte("m"), wantInMessage: "not allowed"},
		{name: "agw-x-m1-00112233", kind: KindClassification, labels: labels, size: 8, model: []byte("m"), wantInMessage: "input side"},
		{name: "agw-x-m1-00112233", kind: KindClassification, labels: labels, size: 4096, model: []byte("m"), wantInMessage: "input side"},
		{name: "agw-x-m1-00112233", kind: KindClassification, labels: labels, size: 64, model: nil, wantInMessage: "empty"},
	} {
		_, err := BuildZip(tc.name, tc.kind, tc.labels, tc.size, tc.model, "n", 1)
		if err == nil || !strings.Contains(err.Error(), tc.wantInMessage) {
			t.Errorf("%s/%s/%v/%d: %v", tc.name, tc.kind, tc.labels, tc.size, err)
		}
	}
}

// Both templates of the gateway are embedded and self-contained: they read the two files of the
// archive, define the two functions nuclio calls and pin their runtime settings.
func TestTemplatesEmbedded(t *testing.T) {
	for kind, tpl := range map[string][]byte{KindClassification: classificationTemplate, KindDetection: detectionTemplate} {
		if len(tpl) == 0 {
			t.Fatalf("%s: template missing", kind)
		}
		text := string(tpl)
		for _, want := range []string{
			"SPDX-FileCopyrightText: 2026 Philipp Schröer",
			"SPDX-License-Identifier: MIT",
			"def init_context(context):",
			"def handler(context, event):",
			`open("config.json"`,
			`open("model.onnx"`,
			"intra_op_num_threads = 2",
			"labels = list(config[\"labels\"])",
			"input_size = int(config[\"input_size\"])",
			`data["image"]`,
			`data.get("threshold", 0.5)`,
			"context.Response(",
		} {
			if !strings.Contains(text, want) {
				t.Errorf("%s: the template lacks %q", kind, want)
			}
		}
		// A template of the gateway carries no placeholder and no second source of the classes.
		for _, bad := range []string{"TODO", "FIXME", "{{", "%VERSION%"} {
			if strings.Contains(text, bad) {
				t.Errorf("%s: the template still carries %q", kind, bad)
			}
		}
	}
	if !strings.Contains(string(classificationTemplate), "_softmax") {
		t.Error("the classification template does not soften its output")
	}
	if !strings.Contains(string(detectionTemplate), "_letterbox") || !strings.Contains(string(detectionTemplate), "_nms") {
		t.Error("the detection template lacks letterbox or suppression")
	}
	if !strings.Contains(string(detectionTemplate), "unsupported output shape") {
		t.Error("the detection template does not refuse an output it cannot read")
	}
}

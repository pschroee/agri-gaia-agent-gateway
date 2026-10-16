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
	if got := yamlKeys(t, sp); !reflect.DeepEqual(got, []string{"build", "description", "eventTimeout", "handler", "platform", "runtime", "triggers"}) {
		t.Errorf("spec fields: %v", got)
	}
	if sp["runtime"] != "python:3.11" || sp["handler"] != "main:handler" || sp["eventTimeout"] != "30s" {
		t.Errorf("spec: %v", sp)
	}
	// CVAT reads spec.description without a default when it lists functions; without the field its
	// list answers 500 (KeyError 'description') and the whole pre-annotation toolset fails.
	if sp["description"] != an["name"] || sp["description"] == "" {
		t.Errorf("spec.description does not carry the title of the function: %v", sp["description"])
	}
	build := yamlMap(t, sp, "build")
	if got := yamlKeys(t, build); !reflect.DeepEqual(got, []string{"baseImage", "directives", "image"}) {
		t.Errorf("build fields: %v", got)
	}
	if build["image"] != "cvat."+name || build["baseImage"] != "python:3.11-slim" {
		t.Errorf("build: %v", build)
	}
	// The directives are the shape nuclio reads: a map from a phase to a list of {kind, value},
	// kind the Dockerfile instruction and value its argument alone (no repeated kind).
	directives := yamlMap(t, build, "directives")
	if got := yamlKeys(t, directives); !reflect.DeepEqual(got, []string{"postCopy", "preCopy"}) {
		t.Fatalf("directive phases: %v", got)
	}
	pre := directives["preCopy"]
	preItems, ok := pre.([]any)
	if !ok || len(preItems) != 1 {
		t.Fatalf("directives.preCopy is not a one-element list: %v (%T)", pre, pre)
	}
	preItem, ok := preItems[0].(map[string]any)
	if !ok {
		t.Fatalf("the preCopy directive is not a mapping: %v (%T)", preItems[0], preItems[0])
	}
	if got := yamlKeys(t, preItem); !reflect.DeepEqual(got, []string{"kind", "value"}) {
		t.Errorf("preCopy directive fields: %v", got)
	}
	if preItem["kind"] != "WORKDIR" || preItem["value"] != functionWorkdir {
		t.Errorf("the working directory is not set: %v", preItem)
	}
	post := directives["postCopy"]
	items, ok := post.([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("directives.postCopy is not a one-element list: %v (%T)", post, post)
	}
	item, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("a directive is not a mapping: %v (%T)", items[0], items[0])
	}
	if got := yamlKeys(t, item); !reflect.DeepEqual(got, []string{"kind", "value"}) {
		t.Errorf("directive fields: %v", got)
	}
	if item["kind"] != "RUN" {
		t.Errorf("directive kind: %v", item["kind"])
	}
	value, _ := item["value"].(string)
	if !strings.Contains(value, functionPackages) {
		t.Errorf("the packages of the image are not pinned: %v", value)
	}
	if strings.HasPrefix(strings.TrimSpace(value), "RUN ") {
		t.Errorf("the directive value repeats the kind: %q", value)
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

// nuclioDirective mirrors functionconfig.Directive of nuclio 1.16.4: the Dockerfile instruction and
// its argument, written into the Dockerfile as "<kind> <value>".
type nuclioDirective struct {
	Kind  string `yaml:"kind"`
	Value string `yaml:"value"`
}

// The directives of function.yaml are unmarshalled the way nuctl 1.16.4 unmarshals them: a map from
// a phase ("preCopy"/"postCopy") to a list of {kind, value}. A strict decoder refuses a bare string
// where a directive mapping is expected - exactly the failure nuctl reported ("cannot unmarshal
// string into Go struct field ... of type functionconfig.Directive") - so a regression fails here
// instead of on the instance.
func TestFunctionYAMLDirectives(t *testing.T) {
	const name = "agw-agent-test-m1-00112233"
	raw, err := BuildZip(name, KindDetection, []string{"pig"}, 640, []byte("m"), "m", 1)
	if err != nil {
		t.Fatal(err)
	}
	entries, _ := zipEntries(t, raw)

	// Every key function.yaml writes is declared, so the strict decoder checks the whole file.
	var cfg struct {
		Metadata map[string]any `yaml:"metadata"`
		Spec     struct {
			Description  string `yaml:"description"`
			Runtime      string `yaml:"runtime"`
			Handler      string `yaml:"handler"`
			EventTimeout string `yaml:"eventTimeout"`
			Build        struct {
				Image      string                       `yaml:"image"`
				BaseImage  string                       `yaml:"baseImage"`
				Directives map[string][]nuclioDirective `yaml:"directives"`
			} `yaml:"build"`
			Triggers map[string]any `yaml:"triggers"`
			Platform map[string]any `yaml:"platform"`
		} `yaml:"spec"`
	}
	dec := yaml.NewDecoder(bytes.NewReader(entries[name+"/function.yaml"]))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		t.Fatalf("nuctl 1.16.4 would refuse this function.yaml: %v", err)
	}

	if got := cfg.Spec.Build.Directives; len(got) != 2 || got["postCopy"] == nil || got["preCopy"] == nil {
		t.Fatalf("directives carry %d phases, want preCopy and postCopy: %v", len(got), got)
	}
	pre := cfg.Spec.Build.Directives["preCopy"]
	if len(pre) != 1 || pre[0].Kind != "WORKDIR" || pre[0].Value != functionWorkdir {
		t.Errorf("preCopy directives = %+v, want one WORKDIR %s", pre, functionWorkdir)
	}
	post := cfg.Spec.Build.Directives["postCopy"]
	if len(post) != 1 {
		t.Fatalf("postCopy holds %d directives, want 1", len(post))
	}
	if post[0].Kind != "RUN" {
		t.Errorf("directive kind = %q, want RUN", post[0].Kind)
	}
	want := "pip install --no-cache-dir " + functionPackages
	if post[0].Value != want {
		t.Errorf("directive value = %q, want %q", post[0].Value, want)
	}
	if strings.HasPrefix(post[0].Value, "RUN") {
		t.Errorf("the directive value repeats the kind: %q", post[0].Value)
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

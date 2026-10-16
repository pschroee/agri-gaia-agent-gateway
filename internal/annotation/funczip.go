// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package annotation

import (
	"archive/zip"
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// TemplateVersion is the version of the templates of the gateway (V2). It stands in config.json and
// in the gateway's register, so that a change of a template is visible on the instance.
const TemplateVersion = 1

// The templates are Python files of this repository and never come from the agent (E9, E10). The
// archive carries the one of the kind that was asked for.
//
//go:embed templates/classification/main.py
var classificationTemplate []byte

//go:embed templates/detection/main.py
var detectionTemplate []byte

// Kinds of a function and the input side each uses when the model says nothing about it.
const (
	KindClassification = "classification"
	KindDetection      = "detection"

	defaultInputClassification = 224
	defaultInputDetection      = 640

	// minInputSize and maxInputSize bound the square input of a function.
	minInputSize = 32
	maxInputSize = 2048
	// maxClasses bounds the class list of a function.
	maxClasses = 1000
	// maxRequestBodySize is what the trigger of the function accepts; CVAT sends one image.
	maxRequestBodySize = 32 << 20
)

// functionPackages are the packages the templates need, pinned: an unpinned build of the function
// would break with the next release of a package (the lesson of graphql-core, CLAUDE.md).
const functionPackages = "onnxruntime==1.20.1 numpy==1.26.4 pillow==10.4.0"

// classRe is the shape of a class name: it stands in function.yaml and in CVAT, so it stays simple
// (no quoting and no shell risk).
var classRe = regexp.MustCompile(`^[A-Za-z0-9 _.\-]{1,64}$`)

// functionNameRe is the shape of a function name: the folder of the archive and the id of the
// function in CVAT. The gateway builds the name itself; the regexpr bounds what can reach the shell
// of the backend (it calls nuctl with the file name of the archive).
var functionNameRe = regexp.MustCompile(`^[a-z0-9-]{1,56}$`)

// template returns the Python template of a kind.
func template(kind string) ([]byte, bool) {
	switch kind {
	case KindClassification:
		return classificationTemplate, true
	case KindDetection:
		return detectionTemplate, true
	}
	return nil, false
}

// functionConfig is the config.json of a function: what the template reads when it starts.
type functionConfig struct {
	Kind            string   `json:"kind"`
	Labels          []string `json:"labels"`
	InputSize       int      `json:"input_size"`
	TemplateVersion int      `json:"template_version"`
}

// classSpec is one class as CVAT's function.yaml carries it: the index of the output and its name.
type classSpec struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// BuildZip builds the archive POST /datasets/auto-annotation takes: a folder named like the archive
// (the backend calls nuctl with the file name without its extension), holding the template, the
// model and the two description files.
//
// Every input the agent can influence is checked here as well, because a raw call over the request
// tool reaches the service without the tool's checks.
func BuildZip(name, kind string, labels []string, size int, model []byte, modelName string, modelID int64) ([]byte, error) {
	tpl, ok := template(kind)
	if !ok {
		return nil, fmt.Errorf("unknown function kind %q (%s or %s)", kind, KindClassification, KindDetection)
	}
	if !functionNameRe.MatchString(name) {
		return nil, fmt.Errorf("function name %q is not valid", name)
	}
	if len(labels) == 0 || len(labels) > maxClasses {
		return nil, fmt.Errorf("a function needs 1 to %d class names, got %d", maxClasses, len(labels))
	}
	for _, l := range labels {
		if !classRe.MatchString(l) {
			return nil, fmt.Errorf("class name %q is not allowed", l)
		}
	}
	if size < minInputSize || size > maxInputSize {
		return nil, fmt.Errorf("the input side must be between %d and %d pixels, got %d", minInputSize, maxInputSize, size)
	}
	if len(model) == 0 {
		return nil, fmt.Errorf("the model file is empty")
	}
	config, err := json.Marshal(functionConfig{Kind: kind, Labels: labels, InputSize: size, TemplateVersion: TemplateVersion})
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	files := []struct {
		name string
		data []byte
	}{
		{"main.py", tpl},
		{"function.yaml", functionYAML(name, labels, modelName, modelID)},
		{"config.json", config},
		{"model.onnx", model},
	}
	for _, f := range files {
		w, err := zw.Create(name + "/" + f.name)
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(f.data); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// functionYAML writes the function.yaml of a function: the conventions CVAT reads (metadata.name,
// the annotations with type detector, framework onnx and the classes as JSON) plus the runtime and
// the packages of the template. Only the fields below are written.
//
// The build directives carry the shape nuclio 1.16.4 reads: spec.build.directives is a map from a
// phase ("preCopy"/"postCopy") to a list of {kind, value}, where kind is the Dockerfile
// instruction (RUN) and value its argument alone - the builder writes "<kind> <value>" into the
// Dockerfile (pkg/functionconfig/types.go, pkg/processor/build/builder.go). nuctl refuses a list of
// bare strings ("cannot unmarshal string into Go struct field ... of type
// functionconfig.Directive"), so the packages must not repeat the RUN.
func functionYAML(name string, labels []string, modelName string, modelID int64) []byte {
	spec, _ := json.Marshal(classSpecs(labels))
	var b strings.Builder
	fmt.Fprintf(&b, "metadata:\n  name: %s\n  namespace: cvat\n", name)
	fmt.Fprintf(&b, "  annotations:\n    name: %s\n    type: detector\n    framework: onnx\n    spec: %s\n",
		strconv.Quote(cvatTitle(modelName, modelID)), strconv.Quote(string(spec)))
	b.WriteString("spec:\n  runtime: \"python:3.11\"\n  handler: main:handler\n  eventTimeout: 30s\n")
	fmt.Fprintf(&b, "  build:\n    image: cvat.%s\n    baseImage: python:3.11-slim\n"+
		"    directives:\n      postCopy:\n        - kind: RUN\n          value: pip install --no-cache-dir %s\n",
		name, functionPackages)
	fmt.Fprintf(&b, "  triggers:\n    myHttpTrigger:\n      maxWorkers: 1\n      kind: \"http\"\n"+
		"      attributes:\n        maxRequestBodySize: %d\n", maxRequestBodySize)
	b.WriteString("  platform:\n    attributes:\n      restartPolicy:\n        name: always\n        maximumRetryCount: 3\n" +
		"      mountMode: volume\n")
	return []byte(b.String())
}

// classSpecs numbers the classes in the order the model outputs them.
func classSpecs(labels []string) []classSpec {
	out := make([]classSpec, 0, len(labels))
	for i, l := range labels {
		out = append(out, classSpec{ID: i, Name: l})
	}
	return out
}

// cvatTitle is the name CVAT shows for a function: the agent's function, the model it came from.
func cvatTitle(modelName string, modelID int64) string {
	label := modelLabel(modelName)
	if label == "" {
		label = "model"
	}
	return fmt.Sprintf("Agent: %s (#%d)", label, modelID)
}

// modelLabel reduces a model name to the characters the title of a function may carry.
func modelLabel(name string) string {
	var b strings.Builder
	for _, c := range name {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == ' ', c == '_', c == '.', c == '-':
			b.WriteRune(c)
		}
	}
	out := strings.TrimSpace(b.String())
	if len(out) > 64 {
		out = strings.TrimSpace(out[:64])
	}
	return out
}

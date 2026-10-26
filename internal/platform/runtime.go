// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

import (
	"fmt"
	"slices"
)

// Runtimes of an inference image (#230). Both serve KServe v2 over HTTP on port 8000.
const (
	RuntimeTriton = "triton"
	RuntimeOVMS   = "ovms"
	OVMSTagSuffix = "-ovms"     // tag suffix of an OVMS image, so that both images of one model stand side by side
	IntelCPUTag   = "intel-cpu" // edge device tag: an Intel/x86 CPU device, OVMS fits
)

// TemplateSource is a repository the agent may import an inference template from, and the branch that holds
// the template of one runtime.
type TemplateSource struct{ Runtime, GitURL, GitRef string }

// TemplateSources is the fixed list of allowed templates (#230; #219 relies on it for the origin of an image).
var TemplateSources = []TemplateSource{
	{RuntimeTriton, "https://github.com/hsos-ai-lab/agri-gaia-inference-container-template", "triton"},
	{RuntimeOVMS, "https://github.com/pschroee/agri-gaia-inference-container-template", "ovms-onnx"},
}

// AllowedTemplateURL tells whether a template may be imported from url and counts as known origin (exact match).
func AllowedTemplateURL(url string) bool {
	return slices.ContainsFunc(TemplateSources, func(s TemplateSource) bool { return s.GitURL == url })
}

// TemplateRuntime is the runtime of an installed template, "" when url and ref are none of TemplateSources.
func TemplateRuntime(url, ref string) string {
	for _, s := range TemplateSources {
		if s.GitURL == url && s.GitRef == ref {
			return s.Runtime
		}
	}
	return ""
}

// TemplateSourceOf answers the source of a runtime.
func TemplateSourceOf(runtime string) (TemplateSource, bool) {
	for _, s := range TemplateSources {
		if s.Runtime == runtime {
			return s, true
		}
	}
	return TemplateSource{}, false
}

// ParseRuntime checks a runtime name: triton or ovms.
func ParseRuntime(s string) (string, error) {
	if s != RuntimeTriton && s != RuntimeOVMS {
		return "", fmt.Errorf("runtime %q: triton or ovms", s)
	}
	return s, nil
}

var defaultRuntime = RuntimeTriton

// SetDefaultRuntime sets the runtime used when a call names none (AGW_INFERENCE_RUNTIME); main calls it once before serving.
func SetDefaultRuntime(s string) error {
	rt, err := ParseRuntime(s)
	if err != nil {
		return err
	}
	defaultRuntime = rt
	return nil
}

// DefaultRuntime is the runtime used when a call names none.
func DefaultRuntime() string { return defaultRuntime }

// runtimeParam is the shared argument of import_template and build_inference_image.
var runtimeParam = Param{Name: "runtime", Type: "string", Desc: "triton or ovms (default: the setup's, see capabilities)",
	Schema: map[string]any{"type": "string", "enum": []string{RuntimeTriton, RuntimeOVMS}}}

// runtimeArg reads the runtime argument; given tells whether the call named one.
func runtimeArg(a Args) (rt string, given bool, err error) {
	s, ok, err := a.str("runtime")
	if err != nil {
		return "", false, err
	}
	if !ok {
		return DefaultRuntime(), false, nil
	}
	rt, err = ParseRuntime(s)
	return rt, true, err
}

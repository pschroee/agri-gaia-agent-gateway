// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package plan

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"agw/internal/delegation"
	"agw/internal/platform"
)

// createSamples are arguments for the tools the description names as creating an object.
var createSamples = map[string]string{
	"upload_dataset":             `{"name":"n","description":"d","files":["/workspace/a.png"]}`,
	"upload_model":               `{"name":"m","description":"d","format":"onnx","model_file":"/workspace/m.onnx"}`,
	"split_dataset":              `{"dataset_id":1}`,
	"create_training":            `{"provider":"Torchvision","architecture":"EfficientNet","category":"Classification","dataset_id":1,"train_config":{"epochs":1}}`,
	"clone_training":             `{"container_id":7,"train_config":{"epochs":2}}`,
	"create_model_from_training": `{"train_container_id":7}`,
	"build_inference_image":      `{"template_id":3,"model_id":1,"name":"mnist-triton","tag":"v1","architecture":"amd64"}`,
	"create_deployment":          `{"device_id":1,"name":"mnist","host_port":18100,"image_id":5}`,
	"import_template":            `{"name":"triton-inference"}`,
	// A search is no create call on the platform; in a plan it creates the container of its winner (issue #244).
	"propose_search": `{"title":"t","path":"template","space":{"lr":{"type":"float","min":0.001,"max":0.1,"scale":"log"}},"base_container_id":7}`,
}

// Every tool the description lists as creating an object is classified create by the delegation (it is the
// action a placeholder may point at), and no tool that creates is missing from the list.
func TestPlanDescriptionCreatesList(t *testing.T) {
	text := platform.PlanToolDescription
	i := strings.Index(text, "Steps that create an object: ")
	if i < 0 {
		t.Fatal("the description has no list of creating steps")
	}
	rest := text[i+len("Steps that create an object: "):]
	listed := strings.Split(rest[:strings.Index(rest, "; for create_training")], ",")
	var got []string
	for _, n := range listed {
		got = append(got, strings.TrimSpace(n))
	}
	var want []string
	for name, args := range createSamples {
		want = append(want, name)
		tool, ok := platform.Lookup(name)
		if !ok {
			t.Errorf("%s: no such tool", name)
			continue
		}
		req, err := tool.Build(json.RawMessage(args))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if a := delegation.Classify(req); a.Action != delegation.Create && name != SearchTool {
			t.Errorf("%s is listed as creating but the delegation says %q", name, a.Action)
		}
	}
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("the description lists\n  %v\nthe samples are\n  %v", got, want)
	}
}

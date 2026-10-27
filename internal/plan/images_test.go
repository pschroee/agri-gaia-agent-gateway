// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package plan

import (
	"strings"
	"testing"
)

const (
	imgModelStep = `{"tool":"create_model_from_training","args":{"train_container_id":7}}`
	// The build takes an existing model (35): a model of step 1 would need a rename first (checkModelNames).
	imgBuildStep = `{"tool":"build_inference_image","args":{"template_id":3,"model_id":35,"name":"mnist-triton","tag":"v1","architecture":"amd64"}}`
)

func deployToolStep(args string) string {
	return `{"tool":"create_deployment","args":{"device_id":1,"name":"mnist","host_port":18100,` + args + `}}`
}

func imagePlan(steps ...string) string {
	return `{"title":"t","steps":[` + strings.Join(steps, ",") + `]}`
}

// After a build step, a deployment names the image by the placeholder of that build step and carries no
// repository and tag (issue #218); every other way is refused with the way that works.
func TestImageStepsRequirePlaceholder(t *testing.T) {
	if _, err := problemsOf(t, imagePlan(imgModelStep, imgBuildStep, deployToolStep(`"image_id":"{{step2.id}}"`))); err != nil {
		t.Fatalf("the plan of the demo is refused: %v", err)
	}
	for name, c := range map[string]struct{ step, want string }{
		"repository and tag only": {deployToolStep(`"repository":"agent-test/mnist-triton","tag":"v1"`),
			`step 3: the plan builds the image in step 2; set "image_id": "{{step2.id}}" (the image that step builds) and leave out repository and tag`},
		"a fixed image id": {deployToolStep(`"image_id":5`),
			`step 3: the plan builds the image in step 2; set "image_id": "{{step2.id}}"`},
		"the wrong step": {deployToolStep(`"image_id":"{{step1.id}}"`),
			`step 3: image_id "{{step1.id}}" is not an image; the plan builds the image in step 2, use "{{step2.id}}"`},
		"placeholder and repository": {deployToolStep(`"image_id":"{{step2.id}}","repository":"agent-test/mnist-triton","tag":"v1"`),
			`step 3: with image_id "{{step2.id}}" leave out repository and tag; the gateway takes them from the image`},
	} {
		_, err := problemsOf(t, imagePlan(imgModelStep, imgBuildStep, c.step))
		if err == nil || !strings.HasPrefix(err.Error(), c.want) {
			t.Errorf("%s:\n got  %v\n want %s", name, err, c.want)
		}
	}
}

func TestImageStepsRawBody(t *testing.T) {
	deploy := func(image string) string {
		return `{"method":"POST","path":"/container-deployments","body":{"edge_device_id":1,"container_image_id":` + image +
			`,"name":"agw-mnist","port_bindings":[{"host_port":"18100","container_port":"8000","protocol":"tcp"}]}}`
	}
	if _, err := problemsOf(t, imagePlan(imgModelStep, imgBuildStep, deploy(`"{{step2.id}}"`))); err != nil {
		t.Fatalf("a raw deployment of the built image: %v", err)
	}
	_, err := problemsOf(t, imagePlan(imgModelStep, imgBuildStep, deploy(`5`)))
	if err == nil || err.Error() != `step 3: the plan builds the image in step 2; set "container_image_id": "{{step2.id}}" (the image that step builds)` {
		t.Errorf("fixed id: %v", err)
	}
	_, err = problemsOf(t, imagePlan(imgModelStep, imgBuildStep, deploy(`"{{step1.id}}"`)))
	if err == nil || !strings.Contains(err.Error(), `container_image_id "{{step1.id}}" is not an image`) {
		t.Errorf("wrong step: %v", err)
	}
}

// Without a build step in the plan, or when the deployment comes first, the rule does not apply.
func TestImageStepsNoBuildNoRule(t *testing.T) {
	own := deployToolStep(`"repository":"agent-test/mnist-triton","tag":"v1"`)
	if _, err := problemsOf(t, imagePlan(own)); err != nil {
		t.Errorf("a deployment of an existing image: %v", err)
	}
	if _, err := problemsOf(t, imagePlan(imgModelStep, own)); err != nil {
		t.Errorf("a deployment next to a model step: %v", err)
	}
	if _, err := problemsOf(t, imagePlan(`{"tool":"create_model_from_training","args":{"train_container_id":7}}`, own,
		`{"tool":"build_inference_image","args":{"template_id":3,"model_id":35,"name":"mnist-triton","tag":"v2","architecture":"amd64"}}`)); err != nil {
		t.Errorf("a deployment before the build: %v", err)
	}
}

// The stored deployment step carries the placeholder of the build step in container_image_id, which
// DeploymentImage (origin.go) reads for the origin check of the later steps.
func TestDeploymentImageStep(t *testing.T) {
	steps := build(t, imagePlan(imgModelStep, imgBuildStep, deployToolStep(`"image_id":"{{step2.id}}"`)))
	if !IsBuildStep(steps[1]) || !IsCreateDeploymentStep(steps[2]) {
		t.Fatalf("steps: %+v", steps)
	}
	if ref, lit := DeploymentImage(steps[2]); ref != 2 || lit != "" {
		t.Fatalf("image of the deployment: %d %q (body %s)", ref, lit, steps[2].Body)
	}
}

// A reference to a step that did not build is not reported twice.
func TestImageStepsSkipFailedSteps(t *testing.T) {
	ps, err := problemsOf(t, imagePlan(imgModelStep, `{"tool":"nonsense_tool"}`, deployToolStep(`"image_id":"{{step2.id}}"`)))
	if err == nil || len(ps) != 1 || ps[0].Step != 2 {
		t.Fatalf("problems: %v", err)
	}
}

// A model taken over in the plan keeps the training's name, which the inference server cannot address: the build of it needs an
// update_model step with only "name" in between (demo run of 26.10.2026).
func TestModelNamesRenameBeforeBuild(t *testing.T) {
	build := `{"tool":"build_inference_image","args":{"template_id":3,"model_id":"{{step1.id}}","name":"mnist-triton","tag":"v1","architecture":"amd64"}}`
	_, err := problemsOf(t, imagePlan(imgModelStep, build))
	want := `step 2: the model of step 1 keeps the training's name, which the inference server cannot address (spaces, brackets); add an {{tool:update_model}} step with model_id "{{step1.id}}" and only "name"`
	if err == nil || !strings.HasPrefix(err.Error(), want) {
		t.Fatalf("no rename:\n got  %v\n want %s", err, want)
	}
	rename := `{"tool":"update_model","args":{"model_id":"{{step1.id}}","name":"mnist_effnet"}}`
	if _, err := problemsOf(t, imagePlan(imgModelStep, rename, build)); err != nil {
		t.Fatalf("with update_model: %v", err)
	}
	if _, err := problemsOf(t, imagePlan(imgModelStep, build, rename)); err == nil || !strings.Contains(err.Error(), "inference server cannot address") {
		t.Fatalf("a rename after the build does not count: %v", err)
	}
}

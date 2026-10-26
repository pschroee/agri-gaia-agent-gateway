// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package plan

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"

	"agw/internal/store"
)

// checkImageSteps applies the rule for a plan that builds an image and deploys it (issue #218): the repository
// and the id of the image are known only when the build has run, so a deployment step after a build step names
// its image by the placeholder of that build step ("image_id": "{{stepB.id}}" of the tool create_deployment,
// "container_image_id" of a raw body) and carries no repository and tag. A plan that builds and also deploys
// another, existing image is refused: it can be proposed separately. steps are the steps that built; a step
// that did not build was reported already. The helpers of origin.go read the stored steps.
func checkImageSteps(steps map[int]store.PlanStep, problems *Problems) {
	idxs := make([]int, 0, len(steps))
	for idx := range steps {
		idxs = append(idxs, idx)
	}
	sort.Ints(idxs)
	lastBuild := 0 // the nearest build step before the step looked at
	for _, idx := range idxs {
		st := steps[idx]
		switch {
		case IsBuildStep(st):
			lastBuild = idx
		case IsCreateDeploymentStep(st) && lastBuild > 0:
			if why := imageStepProblem(st, lastBuild, steps); why != "" {
				problems.add(idx, fmt.Errorf("%s", why))
			}
		}
	}
}

// imageStepProblem checks one deployment step after a build step; build is the number of the nearest one.
func imageStepProblem(st store.PlanStep, build int, steps map[int]store.PlanStep) string {
	tool := st.Tool == "create_deployment"
	field := "container_image_id"
	if tool {
		field = "image_id"
	}
	ref, _ := DeploymentImage(st)
	switch {
	case ref == 0:
		return fmt.Sprintf("the plan builds the image in step %d; set %q: \"{{step%d.id}}\" (the image that step builds)%s",
			build, field, build, leaveOut(tool))
	case ref >= st.Idx:
		return "" // a placeholder that points forward is reported with the references
	}
	target, ok := steps[ref]
	if !ok {
		return "" // the step it points at did not build: reported already
	}
	if !IsBuildStep(target) {
		return fmt.Sprintf("%s \"{{step%d.id}}\" is not an image; the plan builds the image in step %d, use \"{{step%d.id}}\"",
			field, ref, build, build)
	}
	if tool && hasRepoOrTag(st) {
		return fmt.Sprintf("with image_id \"{{step%d.id}}\" leave out repository and tag; the gateway takes them from the image", ref)
	}
	return ""
}

// leaveOut is the end of the advice for a tool step: repository and tag stay out.
func leaveOut(tool bool) string {
	if tool {
		return " and leave out repository and tag"
	}
	return ""
}

// hasRepoOrTag tells whether the arguments of a stored tool step name a repository or a tag.
func hasRepoOrTag(st store.PlanStep) bool {
	var args map[string]json.RawMessage
	if json.Unmarshal(st.Args, &args) != nil {
		return false
	}
	_, repo := args["repository"]
	_, tag := args["tag"]
	return repo || tag
}

// fromTrainingRe is the stored call of create_model_from_training (GET /train/containers/<id>/model).
var fromTrainingRe = regexp.MustCompile(`^/train/containers/[^/]+/model$`)

// checkModelNames: a model taken over from a training gets the platform's name "Architecture (Provider) [dataset,
// score]", which Triton cannot address (spaces, brackets); validate_model reports it not ready and the agent renames
// it outside the plan, a second approval (demo run of 26.10.2026). A plan that builds an image of such a model
// therefore renames it before the build: an update_model step with model_id "{{stepM.id}}" and only "name".
func checkModelNames(steps map[int]store.PlanStep, problems *Problems) {
	idxs := make([]int, 0, len(steps))
	for idx := range steps {
		idxs = append(idxs, idx)
	}
	sort.Ints(idxs)
	for _, idx := range idxs {
		st := steps[idx]
		if !IsBuildStep(st) {
			continue
		}
		ref, _ := bodyRef(st.Body, "model_id")
		from, ok := steps[ref]
		if ref == 0 || ref >= idx || !ok || from.Method != "GET" || !fromTrainingRe.MatchString(from.Path) {
			continue
		}
		renamed := false
		for _, j := range idxs {
			r := steps[j]
			if j > ref && j < idx && r.Method == "PATCH" && r.Path == fmt.Sprintf("/models/{{step%d.id}}", ref) {
				renamed = true
			}
		}
		if !renamed {
			problems.add(idx, fmt.Errorf("the model of step %d keeps the training's name, which Triton cannot address "+
				"(spaces, brackets); add an {{tool:update_model}} step with model_id \"{{step%d.id}}\" and only \"name\" "+
				"(letters, digits, _ . -) between step %d and this build", ref, ref, ref))
		}
	}
}

// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package plan

import (
	"encoding/json"
	"fmt"
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

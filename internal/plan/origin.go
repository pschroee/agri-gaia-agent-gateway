// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package plan

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"

	"agw/internal/delegation"
	"agw/internal/platform"
	"agw/internal/store"
)

// IsBuildStep tells whether the step builds an image: POST /container-images/build, as the tool
// build_inference_image or as a raw call.
func IsBuildStep(st store.PlanStep) bool {
	return st.Method == http.MethodPost && st.Path == "/container-images/build"
}

// IsCreateDeploymentStep tells whether the step creates a deployment: POST /container-deployments.
func IsCreateDeploymentStep(st store.PlanStep) bool {
	return st.Method == http.MethodPost && st.Path == "/container-deployments"
}

// IsDeployStep tells whether the step starts a deployment: PUT /container-deployments/{id}/deploy.
func IsDeployStep(st store.PlanStep) bool { return store.IsDeployCall(st.Method, st.Path) }

// DeploymentImage reads container_image_id of a create-deployment step from st.Body (tool steps carry the
// body the tool built from image_id): ref = N for "{{stepN.id}}", literal = the digits otherwise; "0" and
// absent give (0, "").
func DeploymentImage(st store.PlanStep) (ref int, literal string) {
	return bodyRef(st.Body, "container_image_id")
}

// bodyRef reads one id field of a JSON body: ref = N for "{{stepN.id}}", literal = the digits otherwise.
func bodyRef(rawBody json.RawMessage, key string) (ref int, literal string) {
	if len(bytes.TrimSpace(rawBody)) == 0 {
		return 0, ""
	}
	var body map[string]json.RawMessage
	d := json.NewDecoder(bytes.NewReader(rawBody))
	d.UseNumber()
	if err := d.Decode(&body); err != nil {
		return 0, ""
	}
	raw, ok := body[key]
	if !ok {
		return 0, ""
	}
	var v any
	d = json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := d.Decode(&v); err != nil {
		return 0, ""
	}
	var text string
	switch t := v.(type) {
	case json.Number:
		text = t.String()
	case string:
		if n, ok := wholePlaceholder(t); ok {
			return n, ""
		}
		text = t
	default:
		return 0, ""
	}
	if !digitsRe.MatchString(text) {
		return 0, ""
	}
	for len(text) > 1 && text[0] == '0' {
		text = text[1:]
	}
	if text == "0" {
		return 0, ""
	}
	return 0, text
}

// TemplateOK tells whether the installed template with this id comes from one of the allowed repositories (platform.TemplateSources).
type TemplateOK func(id string) bool

// ImageOrigins sets ImageOrigin, Covered and UncoveredReason of every deployment start whose image the gateway can
// trace (#219): start -> {{stepM.id}} create -> container_image_id {{stepN.id}} -> build step N of this plan, or a
// literal id that a done build step of replaced created. replaced is nil without "replaces" or for a plan of
// another chat. Every other step only gets Covered/UncoveredReason recomputed with store.StepCovered.
func ImageOrigins(steps []store.PlanStep, replaced *store.Plan, templateOK TemplateOK) {
	byIdx := make(map[int]store.PlanStep, len(steps))
	for _, st := range steps {
		byIdx[st.Idx] = st
	}
	for i := range steps {
		dep := &steps[i]
		dep.ImageOrigin = nil
		if dep.ConfirmEach && IsDeployStep(*dep) {
			dep.ImageOrigin = traceImage(*dep, byIdx, steps, replaced, templateOK)
		}
	}
	for i := range steps {
		steps[i].Covered = store.StepCovered(steps[i])
		steps[i].UncoveredReason = store.UncoveredReason(steps[i])
	}
}

// traceImage follows start -> create -> build; nil when any link is missing.
func traceImage(dep store.PlanStep, byIdx map[int]store.PlanStep, steps []store.PlanStep, replaced *store.Plan, templateOK TemplateOK) *store.ImageOrigin {
	// "/container-deployments/{{stepM.id}}/deploy": the segment must be exactly one placeholder.
	parts := strings.Split(dep.Path, "/")
	if len(parts) != 4 {
		return nil
	}
	m, ok := wholePlaceholder(parts[2])
	if !ok || m >= dep.Idx {
		return nil
	}
	create, ok := byIdx[m]
	if !ok || !IsCreateDeploymentStep(create) {
		return nil
	}
	ref, literal := DeploymentImage(create)
	switch {
	case ref > 0:
		if ref >= m {
			return nil
		}
		build, ok := byIdx[ref]
		if !ok || !IsBuildStep(build) || !templateKnown(build, steps, false, templateOK) {
			return nil
		}
		return &store.ImageOrigin{Step: ref}
	case literal != "" && replaced != nil:
		for _, b := range replaced.Steps {
			if IsBuildStep(b) && b.State == StepDone && b.CreatedResource == delegation.ContainerImage &&
				b.CreatedID == literal && templateKnown(b, replaced.Steps, true, templateOK) {
				return &store.ImageOrigin{PlanID: replaced.ID, Step: b.Idx}
			}
		}
	}
	return nil
}

// templateKnown tells whether the template of a build step is the official one: a placeholder must point at an
// import step of the same plan with the allowed git_url (done, when the plan is the replaced one); a literal id
// is looked up with templateOK.
func templateKnown(build store.PlanStep, in []store.PlanStep, mustBeDone bool, templateOK TemplateOK) bool {
	ref, literal := bodyRef(build.Body, "container_template_id")
	switch {
	case ref > 0:
		for _, st := range in {
			if st.Idx != ref {
				continue
			}
			if st.Method != http.MethodPost || st.Path != "/inference-container-templates/create/git" || (mustBeDone && st.State != StepDone) {
				return false
			}
			var form map[string][]string
			if json.Unmarshal(st.Form, &form) != nil {
				return false
			}
			return len(form["git_url"]) == 1 && platform.AllowedTemplateURL(form["git_url"][0])
		}
		return false
	case literal != "":
		return templateOK != nil && templateOK(literal)
	}
	return false
}

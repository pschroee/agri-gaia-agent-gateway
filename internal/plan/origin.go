// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package plan

import (
	"bytes"
	"encoding/json"
	"net/http"

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
	if len(bytes.TrimSpace(st.Body)) == 0 {
		return 0, ""
	}
	var body map[string]json.RawMessage
	d := json.NewDecoder(bytes.NewReader(st.Body))
	d.UseNumber()
	if err := d.Decode(&body); err != nil {
		return 0, ""
	}
	raw, ok := body["container_image_id"]
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

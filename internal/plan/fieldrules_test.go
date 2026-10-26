// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package plan

import (
	"strings"
	"testing"

	"agw/internal/delegation"
)

// modelFullBody is a complete body of PATCH /models/{id}: every field of platform.ModelPatchFields.
const modelFullBody = `{"name":"n","format":"onnx","input_name":"i","input_datatype":"float32","input_shape":[1,3],` +
	`"input_semantics":"NCHW","output_name":"o","output_datatype":"float32","output_shape":[1,2],"output_labels":["0","1"]}`

// update_model reads the model when the step runs and adds the fields the plan leaves out, so a plan step with
// only the name is complete (issue #218). It is accepted in an autonomous plan too, and the grant derived from it
// is "update model, own" for the model the plan created.
func TestBuildUpdateModelNameOnlyAutonomous(t *testing.T) {
	raw := `{"title":"t","mode":"autonomous","steps":[
 {"tool":"create_model_from_training","args":{"train_container_id":7}},
 {"tool":"update_model","args":{"model_id":"{{step1.id}}","name":"pigs v1"}}]}`
	steps := build(t, raw)
	g, err := Grant(steps, SampleStep)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if !hasRule(g, delegation.Update, delegation.Model, delegation.Own) {
		t.Errorf("grant misses update model own: %+v", g.Rules)
	}
}

// A raw PATCH is the body that goes out: with only some fields the platform would clear the others, so it is
// refused at the proposal, in both modes (issue #65 stays in force). A complete body is accepted.
func TestBuildRawPatchModelPartialRefused(t *testing.T) {
	for _, mode := range []string{`"mode":"interactive"`, `"mode":"autonomous"`} {
		raw := `{"title":"t",` + mode + `,"steps":[{"method":"PATCH","path":"/models/5","body":{"name":"x"}}]}`
		ps, err := problemsOf(t, raw)
		if err == nil || len(ps) != 1 || ps[0].Step != 1 || !strings.Contains(ps[0].Msg, "fields missing") {
			t.Errorf("%s: partial raw PATCH accepted or wrong problem: %v", mode, err)
		}
		full := `{"title":"t",` + mode + `,"steps":[{"method":"PATCH","path":"/models/5","body":` + modelFullBody + `}]}`
		if _, err := problemsOf(t, full); err != nil {
			t.Errorf("%s: complete body refused: %v", mode, err)
		}
	}
}

// The allowlist of a route applies to an interactive plan as well (before #218 only an autonomous plan checked it).
func TestBuildDatasetPatchAllowlistStillRefused(t *testing.T) {
	for _, mode := range []string{`"mode":"interactive"`, `"mode":"autonomous"`} {
		raw := `{"title":"t",` + mode + `,"steps":[{"method":"PATCH","path":"/datasets/5","body":{"name":"x","bucket_name":"b"}}]}`
		_, err := problemsOf(t, raw)
		if err == nil || !strings.Contains(err.Error(), `step 1: field "bucket_name" not allowed`) {
			t.Errorf("%s: got %v", mode, err)
		}
	}
	// The one field of the list is fine.
	if _, err := problemsOf(t, `{"title":"t","steps":[{"method":"PATCH","path":"/datasets/5","body":{"name":"ok"}}]}`); err != nil {
		t.Errorf("name only is the allowlist: %v", err)
	}
}

func TestBuildDeploymentNameRuleAtProposal(t *testing.T) {
	body := func(name string) string {
		return `{"edge_device_id":1,"container_image_id":5,"name":"` + name + `","port_bindings":[{"host_port":"18100","container_port":"8000","protocol":"tcp"}]}`
	}
	raw := func(name string) string {
		return `{"title":"t","steps":[{"method":"POST","path":"/container-deployments","body":` + body(name) + `}]}`
	}
	_, err := problemsOf(t, raw("mnist"))
	if err == nil || !strings.Contains(err.Error(), delegation.MsgDeploymentName) {
		t.Fatalf("a name without the prefix must be refused at the proposal: %v", err)
	}
	if _, err := problemsOf(t, raw("agw-mnist")); err != nil {
		t.Fatalf("a valid name refused: %v", err)
	}
}
